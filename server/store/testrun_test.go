package store

import (
	"errors"
	"strings"
	"testing"
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

// caseStageGraph dispatches a regression-only graph — a clone, the virtual
// regression stage and one real task per named case — and returns the stored
// nodes. The cases hang off the clone (a container can never gate its
// children), so a test that wants to claim one reports the clone first.
//
// It is the shape the run-level tests work on: every case is a task of its
// own with its own attempt, its own run and its own log, and the stage is the
// container that rolls those up — which is what the old per-case child runs
// used to model in one row.
func caseStageGraph(t *testing.T, s *Store, name string, names ...string) (root, clone, stage *Task, cases []*Task) {
	t.Helper()
	env, commit := newGraphFixture(t, s, name)
	nodes := []TaskNode{
		{Task: &Task{Kind: TaskKindClone, NodeKey: TaskKindClone, Name: "clone repositories"}},
		{Task: &Task{Kind: TaskKindRegressionStage, NodeKey: TaskKindRegressionStage, Name: "regression"}},
	}
	for _, c := range names {
		nodes = append(nodes, TaskNode{
			Task:      &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey(c), Name: "regression: " + c},
			Deps:      []int64{TaskSubPlaceholderBase + 0},
			ParentKey: TaskKindRegressionStage,
		})
	}
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), nodes)
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	cases = append(cases, stored[3:]...)
	return stored[0], stored[1], stored[2], cases
}

// caseRun returns the run of a case's current attempt, failing the test when
// the node has none.
func caseRun(t *testing.T, s *Store, task *Task) *TestRun {
	t.Helper()
	run, err := s.FindTaskRun(task.ID, task.Attempts)
	if err != nil {
		t.Fatalf("run of %s (attempt %d): %v", task.NodeKey, task.Attempts, err)
	}
	return run
}

// A regression case's report lands on the case's own run, and the virtual
// stage above it aggregates the cases: the container is the matrix cell, and
// its counts are the cases' tallies, so the dashboard reads one row per
// stage without joining runs.
func TestFinishAttemptRollsCaseRunsUp(t *testing.T) {
	s := newTestTaskStore(t)
	root, _, stage, cases := caseStageGraph(t, s, "rollup",
		"lj-argon-nve", "water-tip4p-npt", "argon-liquid-nvt")

	reportTask(t, s, cases[0].ID, StatusPassed, "max rel err 2e-9")
	reportTask(t, s, cases[1].ID, StatusFailed, "drift above threshold")
	reportTask(t, s, cases[2].ID, StatusPassed, "")

	// Each case reports one unit of its own: one attempt, one run, its own
	// outcome.
	for i, want := range []struct {
		status string
		passed int
	}{
		{StatusPassed, 1}, {StatusFailed, 0}, {StatusPassed, 1},
	} {
		task := reloadTask(t, s, cases[i].ID)
		if task.Status != want.status || task.Attempts != 1 {
			t.Errorf("case %d after its report: %+v", i, task)
		}
		run := caseRun(t, s, task)
		if run.Attempt != 1 || run.Status != want.status || run.Total != 1 || run.Passed != want.passed {
			t.Errorf("case %d run: %+v", i, run)
		}
		if run.Kind != RunKindRegression || run.TaskID != task.ID {
			t.Errorf("case %d run should be this case's regression run: %+v", i, run)
		}
		if run.EnvironmentID == 0 || run.CommitID == 0 {
			t.Errorf("a run carries its graph's env/commit so the matrix needs no join: %+v", run)
		}
	}

	// The stage is a container: three cases, two green, one red — and the
	// failure is named, so the cell explains itself.
	got := reloadTask(t, s, stage.ID)
	if got.Total != 3 || got.Passed != 2 || got.Failed != 1 || got.Skipped != 0 {
		t.Errorf("stage counts = %d/%d passed (%d failed): %+v", got.Passed, got.Total, got.Failed, got)
	}
	if got.Status != StatusFailed {
		t.Errorf("stage status = %q, want %q", got.Status, StatusFailed)
	}
	if !strings.Contains(got.Summary, "water-tip4p-npt") {
		t.Errorf("stage summary should name the failed case, got %q", got.Summary)
	}
	// And the root above it follows.
	if rootGot := reloadTask(t, s, root.ID); rootGot.Status != StatusFailed {
		t.Errorf("root status = %q, want %q", rootGot.Status, StatusFailed)
	}
	// The container itself never recorded a run: a run is what makes a task
	// real.
	if _, err := s.FindTaskRun(stage.ID, 1); !errors.Is(err, ErrTestRunNotFound) {
		t.Errorf("virtual stage run = %v, want %v", err, ErrTestRunNotFound)
	}
}

// Descriptions are dispatch data now: they live on the node (the stage's and
// the case's label from md-builder.yaml) and a re-dispatch refreshes them on
// the same rows, because the yaml may have changed between dispatches.
func TestNodeDescriptionsFollowTheDispatch(t *testing.T) {
	s := newTestTaskStore(t)
	env, commit := newGraphFixture(t, s, "describe")

	dispatch := func(stageDesc, caseDesc string) []*Task {
		t.Helper()
		stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
			{Task: &Task{Kind: TaskKindClone, NodeKey: TaskKindClone, Name: "clone repositories"}},
			{Task: &Task{Kind: TaskKindRegressionStage, NodeKey: TaskKindRegressionStage, Name: "regression", Description: stageDesc}},
			{
				Task:      &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("simple"), Name: "regression: simple", Description: caseDesc},
				Deps:      []int64{TaskSubPlaceholderBase + 0},
				ParentKey: TaskKindRegressionStage,
			},
		})
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		return []*Task{stored[2], stored[3]}
	}

	nodes := dispatch("Run regression tests", "Simple regression test")
	stage, simple := nodes[0], nodes[1]
	if stage.Description != "Run regression tests" {
		t.Errorf("stage description: %q", stage.Description)
	}
	if simple.Description != "Simple regression test" {
		t.Errorf("case description: %q", simple.Description)
	}

	// The yaml changed: the re-dispatch overwrites the labels on the same rows
	// (nothing was retired, nothing was recreated).
	again := dispatch("Run regression tests (v2)", "Simple regression test (v2)")
	if again[0].ID != stage.ID || again[1].ID != simple.ID {
		t.Fatalf("a re-dispatch must reuse the node rows: %d/%d -> %d/%d",
			stage.ID, simple.ID, again[0].ID, again[1].ID)
	}
	if got := reloadTask(t, s, stage.ID); got.Description != "Run regression tests (v2)" {
		t.Errorf("stage description not refreshed: %q", got.Description)
	}
	if got := reloadTask(t, s, simple.ID); got.Description != "Simple regression test (v2)" {
		t.Errorf("case description not refreshed: %q", got.Description)
	}
}

func TestFinishAttemptAllPassed(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, _, cases := caseStageGraph(t, s, "allpass", "TestForce", "TestIntegrate")

	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	for _, c := range cases {
		reportTask(t, s, c.ID, StatusPassed, "")
	}

	// A stage whose cases all passed is green, and so is the root above it.
	stageID := cases[0].ParentID
	got := reloadTask(t, s, stageID)
	if got.Status != StatusPassed || got.Total != 2 || got.Passed != 2 || got.Failed != 0 {
		t.Fatalf("unexpected stage: %+v", got)
	}
	if got := reloadTask(t, s, root.ID); got.Status != StatusPassed {
		t.Fatalf("unexpected root: %+v", got)
	}
}

// A report without case results is valid (a command-only stage, or a test
// binary that only yields its counts): it derives its status from the counts
// it was given, and an empty report is a pass of nothing rather than a
// failure.
func TestFinishAttemptWithoutResults(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, _ := caseStageGraph(t, s, "nocases")

	run, err := s.FinishAttempt(clone.ID, AttemptResult{})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if run.Total != 0 || run.Status != StatusPassed {
		t.Fatalf("unexpected empty run: %+v", run)
	}
	if got := reloadTask(t, s, clone.ID); got.Total != 0 || got.Status != StatusPassed {
		t.Fatalf("unexpected empty task: %+v", got)
	}
	// Nothing was invented: the node's one attempt is the one the dispatch
	// opened, and no other run hangs off it.
	runs, err := s.ListTaskRuns(clone.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want the one attempt, got %+v (err %v)", runs, err)
	}
}

// A report for a task whose attempt already ended opens the next attempt
// instead of overwriting the previous one: that is the retry path, and both
// attempts stay readable — the node, however, is a cache of the newest.
func TestReReportOpensANewAttempt(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, cases := caseStageGraph(t, s, "retry", "a")

	first := reportTask(t, s, clone.ID, StatusFailed, "clone/upload failed")
	second := reportTask(t, s, clone.ID, StatusPassed, "cloned")

	if second.ID == first.ID {
		t.Fatal("the retry must not overwrite the previous attempt's run")
	}
	if second.Attempt != 2 {
		t.Fatalf("retry attempt = %d, want 2", second.Attempt)
	}
	if got := reloadTask(t, s, clone.ID); got.Attempts != 2 || got.Status != StatusPassed ||
		got.Total != 1 || got.Failed != 0 {
		t.Fatalf("the task caches its newest attempt: %+v", got)
	}

	runs, err := s.ListTaskRuns(clone.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 2 || runs[0].Attempt != 2 || runs[1].Attempt != 1 {
		t.Fatalf("attempts should be listed newest first: %+v", runs)
	}
	if runs[1].Status != StatusFailed || runs[1].Summary != "clone/upload failed" {
		t.Fatalf("the first attempt keeps its outcome: %+v", runs[1])
	}

	// A different node is a different identity, so it has its own attempt 1.
	other := caseRun(t, s, reloadTask(t, s, cases[0].ID))
	if other.ID == first.ID || other.TaskID == clone.ID || other.Attempt != 1 {
		t.Fatalf("a case must not share the clone's run: %+v", other)
	}
	// Reading one attempt back by (task, attempt) is the run page's lookup.
	got, err := s.FindTaskRun(clone.ID, 1)
	if err != nil || got.Status != StatusFailed {
		t.Fatalf("FindTaskRun(clone, 1) = %+v (err %v)", got, err)
	}
	if _, err := s.FindTaskRun(clone.ID, 3); !errors.Is(err, ErrTestRunNotFound) {
		t.Fatalf("an attempt that never happened = %v, want %v", err, ErrTestRunNotFound)
	}
}

func TestFinishAttemptValidation(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, stage, cases := caseStageGraph(t, s, "validate", "a")

	// Only passed/failed/skipped end an attempt: "running" and a typo are
	// rejected before anything is written.
	for _, status := range []string{StatusPending, StatusRunning, "bogus"} {
		if _, err := s.FinishAttempt(clone.ID, AttemptResult{Status: status}); !errors.Is(err, ErrInvalidRunStatus) {
			t.Fatalf("status %q: err = %v, want %v", status, err, ErrInvalidRunStatus)
		}
	}
	if got := reloadTask(t, s, clone.ID); got.Status != StatusPending {
		t.Fatalf("a rejected report must leave the node alone: %+v", got)
	}

	// Containers never execute: a report for one, or an attempt opened for one,
	// is refused. (The rollup derives their state from the children.)
	for _, id := range []int64{root.ID, stage.ID} {
		if _, err := s.FinishAttempt(id, AttemptResult{Status: StatusPassed}); !errors.Is(err, ErrVirtualTask) {
			t.Errorf("finish on virtual task %d = %v, want %v", id, err, ErrVirtualTask)
		}
		if _, err := s.BeginAttempt(id); !errors.Is(err, ErrVirtualTask) {
			t.Errorf("begin on virtual task %d = %v, want %v", id, err, ErrVirtualTask)
		}
	}

	// An unknown task is not a run target either.
	if _, err := s.FinishAttempt(cases[0].ID+9999, AttemptResult{Status: StatusPassed}); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("unknown task = %v, want %v", err, ErrTaskNotFound)
	}

	// Two cases with one name would fight over one node key (the identity a
	// re-dispatch matches on), so the graph is rejected before any of it is
	// stored.
	env, commit := newGraphFixture(t, s, "validate-dup")
	_, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
		{Task: &Task{Kind: TaskKindClone, NodeKey: TaskKindClone, Name: "clone"}},
		{Task: &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("dup"), Name: "regression: dup"}},
		{Task: &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("dup"), Name: "regression: dup"}},
	})
	if err == nil {
		t.Fatal("duplicate case names should collide on the (root, node key) index")
	}
	if _, err := s.FindRootTaskByCommitEnv(commit.ID, env.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("the rejected dispatch left a root behind: %v", err)
	}
}

// LatestRunsByTaskIDs is the lookup behind a matrix cell: it resolves the
// nodes of a (commit, environment) graph to the run each one shows. Containers
// and unknown ids have no run and are simply absent, and a retried node
// resolves to its newest attempt.
func TestLatestRunsByTaskIDs(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "matrix")
	clone, build, unit, stage := subs[0], subs[1], subs[2], subs[3]
	cases := caseNodes(t, s, stage)

	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	reportTask(t, s, unit.ID, StatusFailed, "1/4 tests failed")
	// The webhook re-dispatched the commit: the clone's cell is attempt 2.
	reportTask(t, s, clone.ID, StatusPassed, "cloned again")

	runs, err := s.LatestRunsByTaskIDs([]int64{clone.ID, build.ID, unit.ID, stage.ID, cases[0].ID, root.ID, 9999})
	if err != nil {
		t.Fatalf("latest runs: %v", err)
	}
	if len(runs) != 4 {
		t.Fatalf("want the four real nodes, got %+v", runs)
	}
	if got := runs[clone.ID]; got.Attempt != 2 || got.Status != StatusPassed {
		t.Errorf("the newest attempt is the cell: %+v", got)
	}
	if got := runs[unit.ID]; got.Kind != RunKindUnit || got.Status != StatusFailed {
		t.Errorf("unit cell: %+v", got)
	}
	if got := runs[build.ID]; got.Status != StatusPending || got.Attempt != 1 {
		t.Errorf("a node nobody reported yet shows its dispatch-time attempt: %+v", got)
	}
	if got := runs[cases[0].ID]; got.Kind != RunKindRegression || got.Status != StatusPending {
		t.Errorf("case cell: %+v", got)
	}
	for _, absent := range []int64{root.ID, stage.ID, 9999} {
		if r, ok := runs[absent]; ok {
			t.Errorf("task %d should have no run, got %+v", absent, r)
		}
	}

	// No ids at all is not an error, just an empty lookup.
	if empty, err := s.LatestRunsByTaskIDs(nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty lookup: %v (%v)", empty, err)
	}
}

// Deleting an environment takes its tasks, runs, logs and artifacts with it:
// the dashboard shows site-wide history, so a dangling row would outlive the
// machine it ran on.
func TestDeleteEnvironmentCascadesTasksAndRuns(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, _, cases := caseStageGraph(t, s, "cascade", "a")

	run := reportTask(t, s, clone.ID, StatusPassed, "cloned")
	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "<x/>"},
	}); err != nil {
		t.Fatalf("append artifact: %v", err)
	}
	caseRunRow := caseRun(t, s, reloadTask(t, s, cases[0].ID))

	if err := s.DeleteEnvironment(root.EnvironmentID); err != nil {
		t.Fatalf("delete env: %v", err)
	}

	// The run and its artifacts are gone; so is the case's own run and the
	// node row it belonged to.
	if _, err := s.GetTestRun(run.ID); !errors.Is(err, ErrTestRunNotFound) {
		t.Fatalf("expected run gone, got %v", err)
	}
	if artifacts, err := s.ListRunArtifacts(run.ID); err != nil || len(artifacts) != 0 {
		t.Fatalf("artifacts should be deleted with the run: %+v (err %v)", artifacts, err)
	}
	if _, err := s.GetTestRun(caseRunRow.ID); !errors.Is(err, ErrTestRunNotFound) {
		t.Fatalf("expected the case run gone, got %v", err)
	}
	if _, err := s.GetTask(clone.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("expected the node gone, got %v", err)
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

// The unit path: counts and a stored results file, with no case tasks at all.
// A reporter that knows better than its counts passes the status explicitly
// (ctest exited non-zero while the parsed counts still look green).
func TestFinishAttemptCountsAndArtifacts(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, _ := caseStageGraph(t, s, "unitcounts")

	run, err := s.FinishAttempt(clone.ID, AttemptResult{
		Status: StatusFailed, // e.g. ctest exited non-zero
		Total:  12, Passed: 9, Failed: 2, Skipped: 1,
		Artifacts: []ArtifactInput{
			{Kind: ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
		},
	})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if run.Total != 12 || run.Passed != 9 || run.Failed != 2 || run.Skipped != 1 {
		t.Fatalf("counts wrong: %+v", run)
	}
	if run.Status != StatusFailed {
		t.Fatalf("the explicit status should win over the counts: %+v", run)
	}
	// The node caches the same values, so the graph and the run never disagree.
	if got := reloadTask(t, s, clone.ID); got.Total != 12 || got.Passed != 9 ||
		got.Failed != 2 || got.Skipped != 1 || got.Status != StatusFailed {
		t.Fatalf("task should mirror its newest attempt: %+v", got)
	}

	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Kind != ArtifactKindResults ||
		artifacts[0].Name != "build/test_detail.xml" || artifactText(t, s, &artifacts[0]) != "<testsuites/>" {
		t.Fatalf("artifact wrong: %+v", artifacts)
	}

	// The next attempt starts clean: it keeps no artifact of the previous one,
	// which in turn keeps its own — an attempt is history, not a scratch row.
	next, err := s.FinishAttempt(clone.ID, AttemptResult{Total: 3, Passed: 3})
	if err != nil {
		t.Fatalf("second report: %v", err)
	}
	if next.ID == run.ID || next.Attempt != 2 {
		t.Fatalf("the second report should open attempt 2: %+v", next)
	}
	if artifacts, err := s.ListRunArtifacts(next.ID); err != nil || len(artifacts) != 0 {
		t.Fatalf("the new attempt carries no artifacts: %+v (err %v)", artifacts, err)
	}
	if artifacts, err := s.ListRunArtifacts(run.ID); err != nil || len(artifacts) != 1 {
		t.Fatalf("the first attempt keeps its artifacts: %+v (err %v)", artifacts, err)
	}
}

func TestFinishAttemptInvalidArtifactKind(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, _ := caseStageGraph(t, s, "badart")

	if _, err := s.FinishAttempt(clone.ID, AttemptResult{
		Status:    StatusPassed,
		Artifacts: []ArtifactInput{{Kind: "screenshot"}},
	}); err == nil {
		t.Fatal("invalid artifact kind should error")
	}
	// The rejected report left neither the artifact nor the outcome behind.
	var artifacts int64
	if err := s.DB.Model(&TestArtifact{}).Count(&artifacts).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("no artifact should have been stored, got %d", artifacts)
	}
	if got := reloadTask(t, s, clone.ID); got.Status != StatusPending {
		t.Fatalf("the node should still be queued: %+v", got)
	}
}

func TestGetArtifact(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, _ := caseStageGraph(t, s, "getartifact")
	run := reportTask(t, s, clone.ID, StatusPassed, "ok")
	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "r.json", Content: "{}"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	list, _ := s.ListRunArtifacts(run.ID)
	a, err := s.GetArtifact(list[0].ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if artifactText(t, s, a) != "{}" || a.RunID != run.ID {
		t.Fatalf("artifact wrong: %+v", a)
	}
}

// A case's outcome is recorded on the case, and the stage aggregates the
// cases' tallies incrementally: the container is never written directly, it
// is derived (store.rollupTx).
func TestCaseRunsRollUpIntoTheStage(t *testing.T) {
	s := newTestTaskStore(t)
	_, _, stage, cases := caseStageGraph(t, s, "aggregate", "heat", "poisson", "laplace")

	// First case: green. The container counts it and stays queued for the two
	// cases nobody has started yet.
	reportTask(t, s, cases[0].ID, StatusPassed, "max rel err 2e-9")
	got := reloadTask(t, s, stage.ID)
	if got.Total != 3 || got.Passed != 1 || got.Failed != 0 || got.Status != StatusPending {
		t.Fatalf("one green case: %+v", got)
	}

	// Second case: red, and the failure is named in the container's summary.
	reportTask(t, s, cases[1].ID, StatusFailed, "err 1e-3 > 1e-5")
	got = reloadTask(t, s, stage.ID)
	if got.Total != 3 || got.Passed != 1 || got.Failed != 1 || got.Status != StatusFailed {
		t.Fatalf("two-case aggregate: %+v", got)
	}
	if !strings.Contains(got.Summary, "poisson") {
		t.Fatalf("summary should name the failed case: %q", got.Summary)
	}

	// The last case passes: the shared counts settle, the failure does not.
	reportTask(t, s, cases[2].ID, StatusPassed, "")
	got = reloadTask(t, s, stage.ID)
	if got.Total != 3 || got.Passed != 2 || got.Failed != 1 {
		t.Fatalf("three-case aggregate wrong: %+v", got)
	}

	// Re-running one case is a new attempt on that case, and the container
	// follows: the case's tally is its newest attempt's, so the stage goes
	// green without any of its rows having been rewritten by hand.
	reportTask(t, s, cases[1].ID, StatusPassed, "")
	got = reloadTask(t, s, stage.ID)
	if got.Total != 3 || got.Passed != 3 || got.Failed != 0 || got.Status != StatusPassed {
		t.Fatalf("re-run aggregate wrong: %+v", got)
	}
	runs, err := s.ListTaskRuns(cases[1].ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("the re-run should be a second attempt: %+v (err %v)", runs, err)
	}
	// The other cases were not touched by it.
	if other, err := s.ListTaskRuns(cases[0].ID); err != nil || len(other) != 1 {
		t.Fatalf("an unrelated case keeps its single attempt: %+v (err %v)", other, err)
	}
	if other := reloadTask(t, s, cases[0].ID); other.Status != StatusPassed || other.Attempts != 1 {
		t.Fatalf("an unrelated case keeps its outcome: %+v", other)
	}
}

// A case runs as its own sub-task, so the attempt its dispatch opened has to
// follow the task: pending while it is queued, running from the moment the
// scheduler claims it, terminal once the outcome lands. The run page (case
// list, detail, live log) reads this row, and the container above follows it.
func TestClaimFlipsTheCaseAttemptToRunning(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, stage, cases := caseStageGraph(t, s, "claim", "heat", "poisson")

	// Both cases are queued until the clone they depend on is done: reporting
	// the clone is what makes them ready.
	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	for _, c := range cases {
		if got := caseRun(t, s, c); got.Status != StatusPending || !got.StartedAt.IsZero() {
			t.Fatalf("a queued case starts pending: %+v", got)
		}
	}

	// The scheduler claims the oldest ready node: the first case.
	claimed := claimTask(t, s)
	if claimed.ID != cases[0].ID {
		t.Fatalf("claimed %s, want the first case", claimed.NodeKey)
	}
	run := caseRun(t, s, claimed)
	if run.Status != StatusRunning || run.StartedAt.IsZero() {
		t.Fatalf("claimed case should be running: %+v", run)
	}
	if other := caseRun(t, s, cases[1]); other.Status != StatusPending {
		t.Fatalf("the queued case stays pending: %+v", other)
	}
	// The container follows a running child: containers are derived, so the
	// rollup is what turns "a case is running" into "the stage is running"
	// (the claim itself only moves the node and its run).
	if err := s.RollupTaskTree(root.ID); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	if got := reloadTask(t, s, stage.ID); got.Status != StatusRunning {
		t.Fatalf("stage should be running: %+v", got)
	}
	if got := reloadTask(t, s, root.ID); got.Status != StatusRunning {
		t.Fatalf("root should be running: %+v", got)
	}

	// The case finishes: the recorded outcome replaces the running row, and
	// the container follows.
	reportTask(t, s, claimed.ID, StatusPassed, "max rel err 2e-9")
	run = caseRun(t, s, reloadTask(t, s, claimed.ID))
	if run.Status != StatusPassed || run.FinishedAt.IsZero() {
		t.Fatalf("recorded case should be terminal: %+v", run)
	}

	// Reporting the other case completes the container.
	reportTask(t, s, cases[1].ID, StatusPassed, "")
	if got := reloadTask(t, s, stage.ID); got.Status != StatusPassed || got.Passed != 2 {
		t.Fatalf("container should be green: %+v", got)
	}
	if got := reloadTask(t, s, root.ID); got.Status != StatusPassed {
		t.Fatalf("root should be green: %+v", got)
	}
}

// An all-skipped stage is skipped — the real status, not a summary
// convention: the stage never ran because something upstream failed, and the
// container reads exactly that instead of looking green.
func TestSkippedCasesRollUpAsSkipped(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, stage, cases := caseStageGraph(t, s, "allskip", "heat", "poisson")

	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	for _, c := range cases {
		reportTask(t, s, c.ID, StatusSkipped, "clone failed: no route to host")
	}
	if got := reloadTask(t, s, cases[0].ID); got.Status != StatusSkipped {
		t.Fatalf("a skipped case is skipped: %+v", got)
	}
	got := reloadTask(t, s, stage.ID)
	if got.Total != 2 || got.Skipped != 2 || got.Passed != 0 || got.Failed != 0 {
		t.Fatalf("all-skipped aggregate wrong: %+v", got)
	}
	if got.Status != StatusSkipped {
		t.Fatalf("an all-skipped stage must not read as green: %+v", got)
	}
	if !strings.Contains(got.Summary, "skipped") {
		t.Fatalf("summary should say the cases were skipped: %q", got.Summary)
	}
	// Nothing about the skipped cases is a failure, so the root above them is
	// skipped too — not green.
	if got := reloadTask(t, s, root.ID); got.Status != StatusSkipped {
		t.Fatalf("the root follows: %+v", got)
	}
}

// A case's fetched files ride its own attempt's run: the case is the task, so
// its artifacts belong to the run of that task — the case detail page reads
// them straight off the node's run.
func TestCaseArtifactsRideTheCasesRun(t *testing.T) {
	s := newTestTaskStore(t)
	_, _, stage, cases := caseStageGraph(t, s, "caseart", "heat")
	heat := cases[0]

	reportTask(t, s, heat.ID, StatusPassed, "ok")
	run := caseRun(t, s, reloadTask(t, s, heat.ID))
	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "<x/>"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Name != "out.xml" || artifactText(t, s, &artifacts[0]) != "<x/>" {
		t.Fatalf("case artifacts wrong: %+v", artifacts)
	}

	// The container lists its subtree's files: a stage's bundle is its cases'.
	refs, err := s.ListSubtreeArtifacts(stage.ID)
	if err != nil {
		t.Fatalf("subtree artifacts: %v", err)
	}
	if len(refs) != 1 || refs[0].TaskID != heat.ID || refs[0].Artifact.Name != "out.xml" {
		t.Fatalf("a stage's bundle should carry its cases' files: %+v", refs)
	}

	// Re-running the case is a new attempt: the new run starts with no
	// artifact, and the old attempt keeps its file as history.
	reportTask(t, s, heat.ID, StatusFailed, "")
	next := caseRun(t, s, reloadTask(t, s, heat.ID))
	if next.ID == run.ID {
		t.Fatal("expected a fresh attempt")
	}
	if artifacts, err := s.ListRunArtifacts(next.ID); err != nil || len(artifacts) != 0 {
		t.Fatalf("the new attempt starts clean: %+v (err %v)", artifacts, err)
	}
	if artifacts, err := s.ListRunArtifacts(run.ID); err != nil || len(artifacts) != 1 {
		t.Fatalf("the previous attempt keeps its artifacts: %+v (err %v)", artifacts, err)
	}
}

func TestAppendRunArtifactsByRun(t *testing.T) {
	s := newTestTaskStore(t)
	_, clone, _, _ := caseStageGraph(t, s, "appendart")
	run := reportTask(t, s, clone.ID, StatusPassed, "ok")

	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "all.log", Content: "log"},
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "<x/>"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(artifacts))
	}
	// Appending does not touch the run's result.
	got, err := s.GetTestRun(run.ID)
	if err != nil || got.Status != StatusPassed || got.Total != 1 {
		t.Fatalf("appending must not change the outcome: %+v (err %v)", got, err)
	}

	// Unknown kind is rejected.
	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: "bogus", Name: "x"},
	}); err == nil {
		t.Fatal("invalid artifact kind should error")
	}
	// Nothing is appended for an empty artifact list.
	if err := s.AppendRunArtifactsByRun(run.ID, nil); err != nil {
		t.Fatalf("empty append should be a no-op, got %v", err)
	}
}

// A re-dispatch of the same (commit, environment) pair re-arms the nodes: the
// stage's cases are queued again on a fresh attempt, and the outcome of the
// previous attempt stays on its own run — which is what replaced the old
// "reset the regression run" path.
func TestRedispatchQueuesFreshCaseAttempts(t *testing.T) {
	s := newTestTaskStore(t)
	root, clone, stage, cases := caseStageGraph(t, s, "redispatch", "heat", "poisson")

	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	reportTask(t, s, cases[0].ID, StatusPassed, "ok")
	if got := reloadTask(t, s, stage.ID); got.Passed != 1 || got.Total != 2 {
		t.Fatalf("container in flight: %+v", got)
	}

	// The same graph is dispatched again (the yaml did not change).
	env, commit := root.EnvironmentID, root.CommitID
	nodes := []TaskNode{
		{Task: &Task{Kind: TaskKindClone, NodeKey: TaskKindClone, Name: "clone repositories"}},
		{Task: &Task{Kind: TaskKindRegressionStage, NodeKey: TaskKindRegressionStage, Name: "regression"}},
		{
			Task:      &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("heat"), Name: "regression: heat"},
			Deps:      []int64{TaskSubPlaceholderBase + 0},
			ParentKey: TaskKindRegressionStage,
		},
		{
			Task:      &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("poisson"), Name: "regression: poisson"},
			Deps:      []int64{TaskSubPlaceholderBase + 0},
			ParentKey: TaskKindRegressionStage,
		},
	}
	again, err := s.UpsertTaskGraph(graphRoot(commit, env), nodes)
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if again[0].ID != root.ID {
		t.Fatalf("the re-dispatch should reuse the root: %d != %d", again[0].ID, root.ID)
	}

	// The heat case is queued on attempt 2; its attempt 1 is untouched.
	heat := reloadTask(t, s, cases[0].ID)
	if heat.Status != StatusPending || heat.Attempts != 2 {
		t.Fatalf("a re-dispatch re-arms the case: %+v", heat)
	}
	runs, err := s.ListTaskRuns(heat.ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("want two attempts, got %+v (err %v)", runs, err)
	}
	if runs[0].Attempt != 2 || runs[0].Status != StatusPending {
		t.Errorf("the newest attempt is the queued one: %+v", runs[0])
	}
	if runs[1].Attempt != 1 || runs[1].Status != StatusPassed {
		t.Errorf("the first attempt keeps its outcome: %+v", runs[1])
	}
	// Nothing was retired, and every node is on its second attempt.
	if retired, err := s.ListRetiredNodes(root.ID); err != nil || len(retired) != 0 {
		t.Fatalf("a re-dispatch retires nothing: %+v (err %v)", retired, err)
	}
	for _, node := range []*Task{clone, stage} {
		if got := reloadTask(t, s, node.ID); got.Attempts != 2 {
			t.Errorf("%s attempts = %d, want 2", node.NodeKey, got.Attempts)
		}
	}
}

// GetTestRunWithTask is the run page's lookup: the run and the task it
// belongs to, so the page can title itself without a second call.
func TestGetTestRunWithTask(t *testing.T) {
	s := newTestTaskStore(t)
	_, _, _, cases := caseStageGraph(t, s, "withtask", "heat")
	reportTask(t, s, cases[0].ID, StatusPassed, "ok")
	run := caseRun(t, s, reloadTask(t, s, cases[0].ID))

	gotRun, gotTask, err := s.GetTestRunWithTask(run.ID)
	if err != nil {
		t.Fatalf("get run with task: %v", err)
	}
	if gotRun.ID != run.ID || gotTask.ID != cases[0].ID || gotTask.Name != "regression: heat" {
		t.Fatalf("run/task mismatch: %+v %+v", gotRun, gotTask)
	}
	if _, _, err := s.GetTestRunWithTask(run.ID + 9999); !errors.Is(err, ErrTestRunNotFound) {
		t.Fatalf("unknown run = %v, want %v", err, ErrTestRunNotFound)
	}
}
