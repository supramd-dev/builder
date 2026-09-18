package store

import (
	"errors"
	"strings"
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
		Cases: []CaseInput{
			{Name: "lj-argon-nve", Status: StatusPassed, Message: "max rel err"},
			{Name: "water-tip4p-npt", Status: StatusFailed, Message: "drift above threshold"},
			{Name: "argon-liquid-nvt", Status: StatusPassed},
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

	// Each case became a child run in submission order.
	children, err := s.ListChildRuns(run.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 child runs, got %d", len(children))
	}
	if children[0].Name != "lj-argon-nve" || children[0].Position != 0 {
		t.Fatalf("unexpected first child: %+v", children[0])
	}
	if children[1].Position != 1 || children[2].Position != 2 {
		t.Fatal("children not in submission order")
	}
	if children[0].Message != "max rel err" || children[1].Status != StatusFailed {
		t.Fatalf("child fields not carried over: %+v", children)
	}
	for _, c := range children {
		if c.ParentID != run.ID || c.Kind != RunKindRegression {
			t.Fatalf("child should link its parent run: %+v", c)
		}
		if c.Total != 1 || c.Passed != boolInt(c.Status == StatusPassed) {
			t.Fatalf("child counts should reflect its own status: %+v", c)
		}
	}
}

// Descriptions are stored on every trigger (they may change between
// dispatches): the placeholder seed carries them, execution reports refresh
// them, and an empty report description keeps the stored one.
func TestRunDescriptionPersistence(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// Dispatch-time placeholder: top-level and per-case descriptions.
	run, err := s.UpsertPlaceholderRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		TaskID:        7,
		Status:        StatusPending,
		Description:   "Run regression tests",
		Cases: []CaseInput{
			{Name: "simple", Description: "Simple regression test"},
		},
	})
	if err != nil {
		t.Fatalf("placeholder: %v", err)
	}
	if run.Description != "Run regression tests" {
		t.Errorf("placeholder description: %q", run.Description)
	}
	children, err := s.ListChildRuns(run.ID)
	if err != nil {
		t.Fatalf("children: %v", err)
	}
	if len(children) != 1 || children[0].Description != "Simple regression test" {
		t.Errorf("case placeholder description: %+v", children)
	}

	// Execution report: UpsertCaseRun replaces the child with the case's
	// (possibly changed) description.
	if _, child, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		TaskID:        8,
		Name:          "simple",
		Description:   "Simple regression test (v2)",
		Status:        StatusPassed,
	}); err != nil {
		t.Fatalf("case run: %v", err)
	} else if child.Description != "Simple regression test (v2)" {
		t.Errorf("case description not refreshed: %q", child.Description)
	}

	// A report WITHOUT a description keeps the stored one (skipped-stage
	// paths and external reporters may not know it).
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Status:        StatusPassed,
		Description:   "Run unit tests",
	}); err != nil {
		t.Fatalf("unit run: %v", err)
	}
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Status:        StatusPassed,
	}); err != nil {
		t.Fatalf("unit re-run: %v", err)
	}
	runs, err := s.FindRunsByCommits(RunKindUnit, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatalf("find runs: %v", err)
	}
	unit := runs[EnvCommit{Env: env.ID, Commit: commit.ID}]
	if unit.Description != "Run unit tests" {
		t.Errorf("empty report description should keep the stored one: %q", unit.Description)
	}

	// A re-dispatch placeholder overwrites with the fresh yaml description.
	if _, err := s.UpsertPlaceholderRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		TaskID:        9,
		Status:        StatusPending,
		Description:   "Run regression tests (new yaml)",
	}); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	again, err := s.GetTestRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if again.Description != "Run regression tests (new yaml)" {
		t.Errorf("re-dispatch should overwrite the description: %q", again.Description)
	}
}

func TestUpsertTestRunAllPassed(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases: []CaseInput{
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
	if children, _ := s.ListChildRuns(run.ID); len(children) != 0 {
		t.Fatalf("case-less run should have no children: %+v", children)
	}
}

func TestUpsertTestRunReplaces(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	first, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases: []CaseInput{
			{Name: "a", Status: StatusFailed},
			{Name: "b", Status: StatusFailed},
		},
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// Re-report for the same triple: same row, replaced child runs.
	second, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindRegression,
		Cases:         []CaseInput{{Name: "a", Status: StatusPassed}},
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

	children, err := s.ListChildRuns(second.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 1 || children[0].Name != "a" {
		t.Fatalf("expected old children replaced, got %+v", children)
	}

	// A different kind creates a separate run.
	unit, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID,
		CommitID:      commit.ID,
		Kind:          RunKindUnit,
		Cases:         []CaseInput{{Name: "TestX", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("unit upsert: %v", err)
	}
	if unit.ID == first.ID {
		t.Fatal("expected separate row for different kind")
	}

	// Top-level run count for the triple's environment: 2 (regression + unit);
	// child runs are rows too but not top-level ones.
	var count int64
	s.DB.Model(&TestRun{}).Where("environment_id = ? AND parent_id = 0", env.ID).Count(&count)
	if count != 2 {
		t.Fatalf("expected 2 top-level runs, got %d", count)
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
		Cases: []CaseInput{{Name: "a", Status: "pending"}},
	}); !errors.Is(err, ErrInvalidCaseStatus) {
		t.Fatalf("expected ErrInvalidCaseStatus, got %v", err)
	}
	// Missing case name.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []CaseInput{{Status: StatusPassed}},
	}); err == nil {
		t.Fatal("expected error for missing case name")
	}
	// Duplicate case names collide on the (parent, name) unique index.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []CaseInput{
			{Name: "dup", Status: StatusPassed},
			{Name: "dup", Status: StatusPassed},
		},
	}); err == nil {
		t.Fatal("duplicate case names should error")
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
		Cases: []CaseInput{{Name: "a", Status: StatusPassed}},
	}); err != nil {
		t.Fatalf("upsert c1: %v", err)
	}
	run2, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: c2.ID, Kind: RunKindRegression,
		Cases: []CaseInput{{Name: "a", Status: StatusFailed}},
	})
	if err != nil {
		t.Fatalf("upsert c2: %v", err)
	}
	// A unit run with one child: the child is a test_runs row too, but must
	// never surface as a matrix cell.
	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: c1.ID, Kind: RunKindUnit,
		Cases: []CaseInput{{Name: "TestX", Status: StatusPassed}},
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

	// Child runs are excluded: only the unit parent matches, not "TestX".
	unitRuns, err := s.FindRunsByCommits(RunKindUnit, []int64{env.ID}, []int64{c1.ID})
	if err != nil {
		t.Fatalf("find unit: %v", err)
	}
	if len(unitRuns) != 1 {
		t.Fatalf("expected 1 unit run (child excluded), got %d", len(unitRuns))
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
		Cases: []CaseInput{{Name: "a", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	children, err := s.ListChildRuns(run.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("expected 1 child, got %d (%v)", len(children), err)
	}

	if err := s.DeleteEnvironment(env.OwnerID, env.ID); err != nil {
		t.Fatalf("delete env: %v", err)
	}

	// The run and its children are gone.
	if _, err := s.GetTestRun(run.ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected run gone, got %v", err)
	}
	if _, err := s.GetTestRun(children[0].ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected child run gone, got %v", err)
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

	// The unit path: counts + a stored results file, no child runs.
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
		artifacts[0].Name != "build/test_detail.xml" || artifactText(t, s, &artifacts[0]) != "<testsuites/>" {
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

	// Deleting a regression run also removes its child runs.
	reg, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindRegression,
		Cases: []CaseInput{{Name: "a", Status: StatusPassed}},
	})
	if err != nil {
		t.Fatalf("reg upsert: %v", err)
	}
	children, _ := s.ListChildRuns(reg.ID)
	if err := s.DeleteTestRun(env.ID, commit.ID, RunKindRegression); err != nil {
		t.Fatalf("reg delete: %v", err)
	}
	if _, err := s.GetTestRun(children[0].ID); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected child run gone with the parent, got %v", err)
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
	if artifactText(t, s, a) != "{}" || a.RunID != run.ID {
		t.Fatalf("artifact wrong: %+v", a)
	}
}

func TestUpsertCaseRunAggregatesIncrementally(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// First case creates the parent run.
	parent, child, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "heat", Status: StatusPassed, Message: "max rel err 2e-9",
		DurationMillis: 1200,
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if child.ID == 0 || child.ParentID != parent.ID || child.Name != "heat" {
		t.Fatalf("child run should link the parent: %+v", child)
	}
	if child.Total != 1 || child.Position != 0 {
		t.Fatalf("first child wrong: %+v", child)
	}
	if parent.Total != 1 || parent.Passed != 1 || parent.Status != StatusPassed {
		t.Fatalf("first case aggregate wrong: %+v", parent)
	}

	// Second case aggregates into the same parent row.
	parentID := parent.ID
	parent, _, err = s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "poisson", Status: StatusFailed, Message: "err 1e-3 > 1e-5",
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if parent.ID != parentID {
		t.Fatalf("expected same parent row (%d), got %d", parentID, parent.ID)
	}
	if parent.Total != 2 || parent.Passed != 1 || parent.Failed != 1 || parent.Status != StatusFailed {
		t.Fatalf("two-case aggregate wrong: %+v", parent)
	}
	if !strings.Contains(parent.Summary, "poisson") {
		t.Fatalf("summary should name the failed case: %q", parent.Summary)
	}

	// A skipped case (upstream failure) counts as skipped, not failed.
	parent, _, err = s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "laplace", Status: StatusSkipped, Message: "build failed",
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if parent.Total != 3 || parent.Skipped != 1 || parent.Failed != 1 {
		t.Fatalf("mixed aggregate wrong: %+v", parent)
	}

	// Re-running one case replaces its child run (no duplicate).
	parent, child, err = s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "poisson", Status: StatusPassed,
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if parent.Total != 3 || parent.Passed != 2 || parent.Failed != 0 || parent.Status != StatusPassed {
		t.Fatalf("replace-by-name aggregate wrong: %+v", parent)
	}
	children, err := s.ListChildRuns(parent.ID)
	if err != nil {
		t.Fatalf("list children: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 child runs after replace, got %d", len(children))
	}
	// The replaced case keeps its original slot; the others are untouched.
	if children[0].Name != "heat" || children[0].Position != 0 ||
		children[1].Name != "poisson" || children[1].Position != 1 ||
		children[2].Name != "laplace" || children[2].Position != 2 {
		t.Fatalf("unexpected child order after replace: %+v", children)
	}
	if child.ID == 0 || child.Status != StatusPassed {
		t.Fatalf("re-reported child wrong: %+v", child)
	}

	// Validation: empty name and invalid status are rejected.
	if _, _, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "", Status: StatusPassed,
	}); err == nil {
		t.Fatal("empty case name should error")
	}
	if _, _, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "x", Status: "bogus",
	}); !errors.Is(err, ErrInvalidCaseStatus) {
		t.Fatalf("invalid case status should error, got %v", err)
	}
}

func TestUpsertCaseRunAllSkipped(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	_, _, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "heat", Status: StatusSkipped, Message: "clone failed: no route to host",
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	parent, _, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "poisson", Status: StatusSkipped, Message: "clone failed: no route to host",
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if parent.Total != 2 || parent.Skipped != 2 || parent.Passed != 0 {
		t.Fatalf("all-skipped aggregate wrong: %+v", parent)
	}
	// The all-skipped placeholder must not read as a green cell: the stored
	// status stays failed so the dashboard's "skipped:" translation kicks in.
	if parent.Status != StatusFailed {
		t.Fatalf("all-skipped run should stay failed (⤼ via the skipped summary): %+v", parent)
	}
	if !strings.HasPrefix(parent.Summary, "skipped: ") {
		t.Fatalf("summary should carry the skipped prefix: %q", parent.Summary)
	}
}

func TestUpsertCaseRunChildArtifacts(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	// The case's fetched artifacts ride its own child run.
	_, child, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "heat", Status: StatusPassed,
		Artifacts: []ArtifactInput{
			{Kind: ArtifactKindResults, Name: "out.xml", Content: "<x/>"},
		},
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	artifacts, err := s.ListRunArtifacts(child.ID)
	if err != nil {
		t.Fatalf("list child artifacts: %v", err)
	}
	if len(artifacts) != 1 || artifacts[0].Name != "out.xml" || artifactText(t, s, &artifacts[0]) != "<x/>" {
		t.Fatalf("child artifacts wrong: %+v", artifacts)
	}
	oldChildID := child.ID

	// Re-reporting the case replaces the child run and its artifacts.
	_, child, err = s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "heat", Status: StatusFailed,
	})
	if err != nil {
		t.Fatalf("re-upsert case: %v", err)
	}
	if child.ID == oldChildID {
		t.Fatal("expected a fresh child run row")
	}
	if artifacts, _ := s.ListRunArtifacts(oldChildID); len(artifacts) != 0 {
		t.Fatalf("old child artifacts should be gone: %+v", artifacts)
	}
}

func TestAppendRunArtifactsByRun(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

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

func TestResetRegressionRun(t *testing.T) {
	s := newTestStore(t)
	env, commit := seedEnvAndCommit(t, s)

	parent, child, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "heat", Status: StatusPassed,
		Artifacts: []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "<x/>"}},
	})
	if err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if _, _, err := s.UpsertCaseRun(&CaseRunInput{
		EnvironmentID: env.ID, CommitID: commit.ID,
		Name: "poisson", Status: StatusFailed,
	}); err != nil {
		t.Fatalf("upsert case: %v", err)
	}
	if err := s.AppendRunArtifactsByRun(parent.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "all.log", Content: "log"},
	}); err != nil {
		t.Fatalf("append: %v", err)
	}
	childID := child.ID

	if err := s.ResetRegressionRun(env.ID, commit.ID); err != nil {
		t.Fatalf("reset: %v", err)
	}
	got, err := s.GetTestRun(parent.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Total != 0 || got.Passed != 0 || got.Summary != "" {
		t.Fatalf("run should be reset: %+v", got)
	}
	if children, _ := s.ListChildRuns(got.ID); len(children) != 0 {
		t.Fatalf("child runs should be gone, got %d", len(children))
	}
	if artifacts, _ := s.ListRunArtifacts(got.ID); len(artifacts) != 0 {
		t.Fatalf("artifacts should be gone, got %d", len(artifacts))
	}
	// The reset child's artifacts are gone with the child row.
	if artifacts, _ := s.ListRunArtifacts(childID); len(artifacts) != 0 {
		t.Fatalf("child artifacts should be gone, got %d", len(artifacts))
	}

	// Resetting a never-recorded run is a no-op, not an error.
	if err := s.ResetRegressionRun(env.ID, commit.ID+999); err != nil {
		t.Fatalf("reset missing run: %v", err)
	}
}
