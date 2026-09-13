package store

import (
	"errors"
	"testing"

	"gorm.io/gorm"
)

func seedEnvAndCommit(t *testing.T, s *Store) (*TestEnvironment, *Commit) {
	t.Helper()
	u := &User{Username: "runowner", Email: "run@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	env := &TestEnvironment{OwnerID: u.ID, Name: "cpu-node-1", Host: "h", Username: "u", PrivateKey: "k"}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatalf("create env: %v", err)
	}
	commit := &Commit{Repo: "group/code", SHA: "c0ffee", Ref: "main"}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("create commit: %v", err)
	}
	return env, commit
}

func TestUpsertTestRunCreates(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	in := &RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases: []TestCaseResult{
			{Name: "lj-argon-nve", Status: StatusPassed, ErrorValue: 1.2e-07, Message: "max rel err"},
			{Name: "water-tip4p-npt", Status: StatusFailed, ErrorValue: 0.02, Message: "drift above threshold"},
			{Name: "argon-liquid-nvt", Status: StatusPassed, ErrorValue: 3e-06},
		},
	}
	run, err := s.UpsertTestRun(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if run.ID == 0 {
		t.Fatal("expected run ID set")
	}
	if run.Total != 3 || run.Passed != 2 || run.Failed != 1 {
		t.Fatalf("unexpected counts: %+v", run)
	}
	if run.Status != StatusFailed {
		t.Fatalf("expected failed status, got %q", run.Status)
	}

	cases, err := s.ListCaseResults(run.ID)
	if err != nil {
		t.Fatalf("list cases: %v", err)
	}
	if len(cases) != 3 {
		t.Fatalf("expected 3 cases, got %d", len(cases))
	}
	if cases[0].Name != "lj-argon-nve" || cases[0].Position != 0 {
		t.Fatalf("unexpected first case: %+v", cases[0])
	}
	if cases[1].Position != 1 || cases[2].Position != 2 {
		t.Fatal("cases not in submission order")
	}
	if cases[0].ErrorValue != 1.2e-07 {
		t.Fatalf("unexpected error value: %v", cases[0].ErrorValue)
	}
}

func TestUpsertTestRunAllPassed(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Cases: []TestCaseResult{
			{Name: "TestForce", Status: StatusPassed},
			{Name: "TestIntegrate", Status: StatusPassed},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if run.Status != StatusPassed || run.Total != 2 || run.Passed != 2 || run.Failed != 0 {
		t.Fatalf("unexpected run: %+v", run)
	}
}

func TestUpsertTestRunNoCases(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// A case-less report (e.g. unit tests reported as counts only) is valid.
	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if run.Total != 0 || run.Status != StatusPassed {
		t.Fatalf("unexpected empty run: %+v", run)
	}
}

func TestUpsertTestRunReplaces(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	first, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases: []TestCaseResult{
			{Name: "a", Status: StatusFailed},
			{Name: "b", Status: StatusFailed},
		},
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Re-report for the same triple: same row, replaced cases.
	second, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases:         []TestCaseResult{{Name: "a", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("expected same run row (%d), got %d", first.ID, second.ID)
	}
	if second.Total != 1 || second.Passed != 1 || second.Status != StatusPassed {
		t.Fatalf("unexpected replacement: %+v", second)
	}

	cases, err := s.ListCaseResults(second.ID)
	if err != nil {
		t.Fatalf("list cases: %v", err)
	}
	if len(cases) != 1 || cases[0].Name != "a" {
		t.Fatalf("expected old cases replaced, got %+v", cases)
	}

	// A different kind creates a separate run.
	unit, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Cases:         []TestCaseResult{{Name: "TestX", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("unit upsert: %v", err)
	}
	if unit.ID == first.ID {
		t.Fatal("expected separate row for different kind")
	}

	// Total run count for the triple's environment: 2 (regression + unit).
	var count int64
	s.DB.Model(&TestRun{}).Where("environment_id = ?", env.ID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 runs, got %d", count)
	}
}

func TestUpsertTestRunValidation(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// Invalid kind.
	if _, err := s.UpsertTestRun(&RunInput{EnvironmentID: env.ID, CommitID: commit.ID, Kind: "perf"}); !errors.Is(err, ErrInvalidRunKind) {
		t.Fatalf("expected ErrInvalidRunKind, got %v", err)
	}
	// Invalid case status.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []TestCaseResult{{Name: "a", Status: "skipped"}},
	}); !errors.Is(err, ErrInvalidCaseStatus) {
		t.Fatalf("expected ErrInvalidCaseStatus, got %v", err)
	}
	// Missing case name.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []TestCaseResult{{Status: StatusPassed}},
	}); err == nil {
		t.Fatal("expected error for missing case name")
	}
}

func TestFindRunsByCommits(t *testing.T) {
	s := newTestStore(t)
	env, c1 := seedEnvAndCommit(t, s)
	c2 := &Commit{Repo: "group/code", SHA: "deadbee"}
	if _, err := s.GetOrCreateCommit(c2); err != nil {
		t.Fatalf("create c2: %v", err)
	}

	// Runs for (env, c1, regression) and (env, c2, regression).
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: c1.ID, Kind: RunKindRegression,
		Cases: []TestCaseResult{{Name: "a", Status: StatusPassed}},
	}); err != nil {
		t.Fatalf("upsert c1: %v", err)
	}
	run2, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: c2.ID, Kind: RunKindRegression,
		Cases: []TestCaseResult{{Name: "a", Status: StatusFailed}},
	})
	if err != nil {
		t.Fatalf("upsert c2: %v", err)
	}
	// A unit run that must not appear in a regression query.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: c1.ID, Kind: RunKindUnit,
		Cases: []TestCaseResult{{Name: "TestX", Status: StatusPassed}},
	}); err != nil {
		t.Fatalf("upsert unit: %v", err)
	}

	runs, err := s.FindRunsByCommits(RunKindRegression, []int64{env.ID}, []int64{c1.ID, c2.ID})
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 regression runs, got %d", len(runs))
	}
	if runs[EnvCommit{Env: env.ID, Commit: c1.ID}].Status != StatusPassed {
		t.Fatal("expected passed run at (env, c1)")
	}
	if runs[EnvCommit{Env: env.ID, Commit: c2.ID}].ID != run2.ID {
		t.Fatal("expected failed run at (env, c2)")
	}

	// Empty inputs short-circuit without error.
	runs, err = s.FindRunsByCommits(RunKindRegression, nil, []int64{c1.ID})
	if err != nil || len(runs) != 0 {
		t.Fatalf("expected empty map for empty envs, got %v (%v)", runs, err)
	}
}

func TestDeleteEnvironmentCascadesRuns(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)
	u := &User{Username: "findowner", Email: "find@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []TestCaseResult{{Name: "a", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	if err := s.DeleteEnvironment(env.OwnerID, env.ID); err != nil {
		t.Fatalf("delete env: %v", err)
	}

	// The run and its cases are gone.
	if _, err := s.GetTestRun(run.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected run gone, got %v", err)
	}
	cases, err := s.ListCaseResults(run.ID)
	if err != nil || len(cases) != 0 {
		t.Fatalf("expected cases gone, got %d (%v)", len(cases), err)
	}
}

func TestListAllEnvironments(t *testing.T) {
	s := newTestStore(t)
	u1 := &User{Username: "listowner1", Email: "l1@example.com", PasswordHash: "hash"}
	u2 := &User{Username: "listowner2", Email: "l2@example.com", PasswordHash: "hash"}
	for _, u := range []*User{u1, u2} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create user: %v", err)
		}
	}
	for _, env := range []*TestEnvironment{
		{OwnerID: u1.ID, Name: "gpu-a100", Host: "h", Username: "u", PrivateKey: "k"},
		{OwnerID: u2.ID, Name: "cpu-node-1", Host: "h", Username: "u", PrivateKey: "k"},
		{OwnerID: u1.ID, Name: "cpu-node-2", Host: "h", Username: "u", PrivateKey: "k"},
	} {
		if err := s.CreateEnvironment(env); err != nil {
			t.Fatalf("create env: %v", err)
		}
	}

	// Site-wide list, name-ordered, across owners.
	envs, err := s.ListAllEnvironments()
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(envs) != 3 {
		t.Fatalf("expected 3 environments, got %d", len(envs))
	}
	if envs[0].Name != "cpu-node-1" || envs[1].Name != "cpu-node-2" || envs[2].Name != "gpu-a100" {
		t.Fatalf("unexpected order: %v", envs)
	}
}

func TestUpsertTestRunAggregateWithArtifact(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// The unit path: counts + a stored results file, no per-case rows.
	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		TaskID:        42,
		Total:         12,
		Passed:        9,
		Failed:        2,
		Skipped:       1,
		StatusFailed:  true, // e.g. ctest exited non-zero
		Artifacts: []ArtifactInput{
			{Kind: ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if run.Total != 12 || run.Passed != 9 || run.Failed != 2 || run.Skipped != 1 {
		t.Fatalf("counts wrong: %+v", run)
	}
	if run.Status != StatusFailed {
		t.Fatalf("StatusFailed override should force failed: %+v", run)
	}
	if run.TaskID != 42 {
		t.Fatalf("TaskID = %d", run.TaskID)
	}

	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Kind != ArtifactKindResults ||
		artifacts[0].Name != "build/test_detail.xml" || artifacts[0].Content != "<testsuites/>" {
		t.Fatalf("artifact wrong: %+v", artifacts)
	}

	// Re-report without an artifact: the old artifact is replaced (gone).
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Total:         3,
		Passed:        3,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	artifacts, err = s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list artifacts after replace: %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("old artifacts should be replaced: %+v", artifacts)
	}
}

func TestUpsertTestRunInvalidArtifactKind(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)
	_, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Artifacts:     []ArtifactInput{{Kind: "screenshot"}},
	})
	if err == nil {
		t.Fatal("invalid artifact kind should error")
	}
}

func TestDeleteTestRunRemovesArtifacts(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)
	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Total:         1,
		Artifacts:     []ArtifactInput{{Kind: ArtifactKindResults, Name: "r.xml", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.DeleteTestRun(env.ID, commit.ID, RunKindUnit); err != nil {
		t.Fatalf("delete: %v", err)
	}
	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("artifacts should be deleted with the run: %+v", artifacts)
	}
}

func TestGetArtifact(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)
	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Artifacts:     []ArtifactInput{{Kind: ArtifactKindResults, Name: "r.json", Content: "{}"}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	list, _ := s.ListRunArtifacts(run.ID)
	a, err := s.GetArtifact(list[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if a.Content != "{}" || a.RunID != run.ID {
		t.Fatalf("artifact wrong: %+v", a)
	}
}
