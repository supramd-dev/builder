package store

import (
	"testing"
	"time"
)

// perfStageGraph dispatches a graph with one performance stage on the given
// (commit, environment) pair and finishes its attempt with the given series
// artifacts — the state the performance history module reads.
func perfStageGraph(t *testing.T, s *Store, env *TestEnvironment, commit *Commit, files map[string]string) *Task {
	t.Helper()
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
		{Task: &Task{Kind: TaskKindPerformance, NodeKey: TaskKindPerformance, Name: "performance"}},
	})
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	task := stored[1]
	artifacts := make([]ArtifactInput, 0, len(files))
	for name, content := range files {
		artifacts = append(artifacts, ArtifactInput{Kind: ArtifactKindSeries, Name: name, Content: content})
	}
	if _, err := s.FinishAttempt(task.ID, AttemptResult{Status: StatusPassed, Artifacts: artifacts}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	return task
}

func TestListPerformanceObservations(t *testing.T) {
	s, _ := newTestStoreWithObjects(t)

	u := &User{Username: "perfowner", Email: "perf@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	env1 := &TestEnvironment{OwnerID: u.ID, Name: "cpu-node-1", Host: "h1", Username: "u", PrivateKey: "k"}
	env2 := &TestEnvironment{OwnerID: u.ID, Name: "gpu-a100", Host: "h2", Username: "u", PrivateKey: "k"}
	for _, env := range []*TestEnvironment{env1, env2} {
		if err := s.CreateEnvironment(env); err != nil {
			t.Fatalf("create environment: %v", err)
		}
	}

	base := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	mkCommit := func(repo, sha string, at time.Time) *Commit {
		c := &Commit{Repo: repo, SHA: sha, Ref: "main", Message: "m " + sha, PushedAt: at}
		if err := s.CreateCommit(c); err != nil {
			t.Fatalf("create commit: %v", err)
		}
		return c
	}
	c1 := mkCommit("group/code", "aaa111", base.Add(-48*time.Hour))
	c2 := mkCommit("group/code", "bbb222", base)

	perfStageGraph(t, s, env1, c1, map[string]string{"perf.json": `{"schema":1}`})
	perfStageGraph(t, s, env2, c2, map[string]string{"perf.json": `{"schema":1}`})

	// Noise the query must not pick up: a series artifact of a unit stage (a
	// regression case may store series files too), and a non-series artifact
	// of a performance stage.
	env3 := &TestEnvironment{OwnerID: u.ID, Name: "unit-node", Host: "h3", Username: "u", PrivateKey: "k"}
	if err := s.CreateEnvironment(env3); err != nil {
		t.Fatalf("create environment: %v", err)
	}
	unitTask, _ := unitRun(t, s, env3, c2)
	reportArtifacts(t, s, unitTask, []ArtifactInput{
		{Kind: ArtifactKindSeries, Name: "case.series.json", Content: `{"schema":1}`},
	})
	resultCommit := mkCommit("group/code", "ccc333", base.Add(24*time.Hour))
	stored, err := s.UpsertTaskGraph(graphRoot(resultCommit.ID, env1.ID), []TaskNode{
		{Task: &Task{Kind: TaskKindPerformance, NodeKey: TaskKindPerformance, Name: "performance"}},
	})
	if err != nil {
		t.Fatalf("dispatch result-only graph: %v", err)
	}
	if _, err := s.FinishAttempt(stored[1].ID, AttemptResult{Status: StatusPassed, Artifacts: []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "results.json", Content: "{}"},
	}}); err != nil {
		t.Fatalf("finish result-only attempt: %v", err)
	}

	all, err := s.ListPerformanceObservations(base.AddDate(0, 0, -30), 0, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d observations, want the 2 performance reports: %+v", len(all), all)
	}
	// Oldest first, with the context the module plots with.
	if all[0].Artifact.Name != "perf.json" || all[0].CommitSHA != "aaa111" || all[0].EnvName != "cpu-node-1" {
		t.Fatalf("observation[0] = %+v", all[0])
	}
	if !all[0].CommitAt.Equal(c1.PushedAt) || all[0].Artifact.Size == 0 {
		t.Fatalf("observation[0] lost its commit time or size: %+v", all[0])
	}
	if all[1].CommitID != c2.ID || all[1].EnvID != env2.ID {
		t.Fatalf("observation[1] = %+v", all[1])
	}
	if all[0].Artifact.ObjectKey == "" {
		t.Fatal("the artifact should be a reference into object storage")
	}

	// The environment filter.
	only2, err := s.ListPerformanceObservations(base.AddDate(0, 0, -30), env2.ID, "", 100)
	if err != nil {
		t.Fatalf("list by environment: %v", err)
	}
	if len(only2) != 1 || only2[0].EnvName != "gpu-a100" {
		t.Fatalf("environment filter: %+v", only2)
	}

	// The repository filter.
	if got, err := s.ListPerformanceObservations(base.AddDate(0, 0, -30), 0, "group/code", 100); err != nil || len(got) != 2 {
		t.Fatalf("repo filter: %d rows, err %v", len(got), err)
	}
	if got, err := s.ListPerformanceObservations(base.AddDate(0, 0, -30), 0, "other/code", 100); err != nil || len(got) != 0 {
		t.Fatalf("unknown repo: %d rows, err %v", len(got), err)
	}

	// The window starts at the commit's arrival.
	recent, err := s.ListPerformanceObservations(base.Add(-24*time.Hour), 0, "", 100)
	if err != nil {
		t.Fatalf("list by window: %v", err)
	}
	if len(recent) != 1 || recent[0].CommitSHA != "bbb222" {
		t.Fatalf("window: %+v", recent)
	}

	// Truncation keeps the newest observations, still in chronological order.
	newest, err := s.ListPerformanceObservations(base.AddDate(0, 0, -30), 0, "", 1)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(newest) != 1 || newest[0].CommitSHA != "bbb222" {
		t.Fatalf("limit 1: %+v", newest)
	}
}
