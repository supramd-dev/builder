package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"md-builder/server/storage"
)

// newTestTaskStore opens an in-memory store for the task-graph tests. It is
// newTestStore under the name these tests have always used: the store carries
// the in-memory artifact backend too, so a graph test may attach a results
// file to a run without a second helper.
func newTestTaskStore(t *testing.T) *Store {
	t.Helper()
	return newTestStore(t)
}

// newGraphFixture creates the user, environment and commit a dispatch needs,
// without persisting any task. Tests that build their own node list use it;
// seedTaskGraph is the shared fixture on top.
func newGraphFixture(t *testing.T, s *Store, name string) (*TestEnvironment, *Commit) {
	t.Helper()
	u := &User{Username: "u-" + name, Email: name + "@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &TestEnvironment{
		OwnerID: u.ID, Name: "env-" + name, Host: "h", Username: "u", PrivateKey: "k", Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	commit := &Commit{Repo: "group/code", SHA: "sha-" + name, PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	return env, commit
}

// graphRoot returns the root row of one dispatch. The caller may keep the
// pointer across the call: UpsertTaskGraph fills its id in — and, for a
// re-dispatch, reuses the id of the (commit, environment) root that is already
// stored.
func graphRoot(commitID, envID int64) *Task {
	return &Task{
		Kind: TaskKindRoot, Name: "root", CommitID: commitID, EnvironmentID: envID,
		Tags: "cpu", Config: `{"tags":["cpu"]}`,
	}
}

// caseNodeKey mirrors runner.RegressionCaseKey ("<stage>:<preset>"). The store
// tests cannot import runner: it imports store, so the reference would be an
// import cycle.
func caseNodeKey(name string) string { return TaskKindRegressionStage + ":" + name }

// graphNodes is the fixture graph, in creation order:
//
//	root (virtual)                 [graphRoot — not part of this list]
//	 ├── clone
//	 ├── build                     <- clone
//	 ├── unit                      <- build
//	 └── regression (virtual)
//	      ├── regression:heat       <- build
//	      └── regression:poisson    <- build
//
// It is a function and not a literal so a test can dispatch the same graph
// twice: that is what re-dispatching a commit does.
func graphNodes(commitID, envID int64) []TaskNode {
	node := func(kind, key, name, description string) *Task {
		return &Task{
			Kind: kind, NodeKey: key, Name: name, Description: description,
			CommitID: commitID, EnvironmentID: envID, Config: "{}",
		}
	}
	return []TaskNode{
		{Task: node(TaskKindClone, TaskKindClone, "clone repositories", "fetch both repositories")},
		{Task: node(TaskKindBuild, TaskKindBuild, "build", "make -j"), Deps: []int64{TaskSubPlaceholderBase + 0}},
		{Task: node(TaskKindUnit, TaskKindUnit, "unit tests", "Run unit tests"), Deps: []int64{TaskSubPlaceholderBase + 1}},
		// The stage is virtual: it runs nothing, so nothing may depend on it —
		// which is why the cases below depend on build.
		{Task: node(TaskKindRegressionStage, TaskKindRegressionStage, "regression", "Run regression tests")},
		{
			Task:      node(TaskKindRegressionCase, caseNodeKey("heat"), "regression: heat", "thermal conductivity"),
			Deps:      []int64{TaskSubPlaceholderBase + 1},
			ParentKey: TaskKindRegressionStage,
		},
		{
			Task:      node(TaskKindRegressionCase, caseNodeKey("poisson"), "regression: poisson", "poisson solver"),
			Deps:      []int64{TaskSubPlaceholderBase + 1},
			ParentKey: TaskKindRegressionStage,
		},
	}
}

// seedTaskGraph dispatches the fixture graph and returns the stored root and
// its four direct children — clone, build, unit and the virtual regression
// stage — in creation order. The stage's cases are caseNodes.
func seedTaskGraph(t *testing.T, s *Store, name string) (*Task, []*Task) {
	t.Helper()
	env, commit := newGraphFixture(t, s, name)
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID))
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	// stored is [root, clone, build, unit, stage, heat, poisson].
	return stored[0], []*Task{stored[1], stored[2], stored[3], stored[4]}
}

// caseNodes returns the regression stage's cases, in creation order.
func caseNodes(t *testing.T, s *Store, stage *Task) []*Task {
	t.Helper()
	kids, err := s.ListChildren(stage.ID)
	if err != nil {
		t.Fatalf("list cases of stage %d: %v", stage.ID, err)
	}
	if len(kids) == 0 {
		t.Fatalf("the regression stage %d has no cases", stage.ID)
	}
	out := make([]*Task, len(kids))
	for i := range kids {
		out[i] = &kids[i]
	}
	return out
}

// reportTask records one attempt's outcome for a task the way the runner
// reports a finished stage: FinishAttempt is the only writer of a task's
// status and counts. The counts follow the status, as a one-case stage's
// report would.
func reportTask(t *testing.T, s *Store, taskID int64, status, summary string) *TestRun {
	t.Helper()
	res := AttemptResult{Status: status, Summary: summary, Total: 1}
	switch status {
	case StatusPassed:
		res.Passed = 1
	case StatusFailed:
		res.Failed = 1
	case StatusSkipped:
		res.Skipped = 1
	}
	run, err := s.FinishAttempt(taskID, res)
	if err != nil {
		t.Fatalf("finish attempt of task %d: %v", taskID, err)
	}
	return run
}

// reloadTask reads a task back through the store, so an assertion sees what is
// stored rather than what a caller's struct happens to hold.
func reloadTask(t *testing.T, s *Store, id int64) *Task {
	t.Helper()
	got, err := s.GetTask(id)
	if err != nil {
		t.Fatalf("get task %d: %v", id, err)
	}
	return got
}

// claimTask claims the next ready task, failing the test when none is ready.
func claimTask(t *testing.T, s *Store) *Task {
	t.Helper()
	got, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got == nil {
		t.Fatal("no task was ready")
	}
	return got
}

// requireNoReadyTask asserts that the scheduler has nothing left to hand out.
func requireNoReadyTask(t *testing.T, s *Store) {
	t.Helper()
	got, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if got != nil {
		t.Fatalf("expected no ready task, got %+v", got)
	}
}

func TestUpsertTaskGraphResolvesDeps(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "graph")
	clone, build, stage := subs[0], subs[1], subs[3]

	// The root is a container that belongs to its own graph: RootID is patched
	// to its own id because a root row is inserted before that id exists.
	if root.ID == 0 || root.RootID != root.ID {
		t.Errorf("root RootID: want %d, got %d", root.ID, root.RootID)
	}
	if !root.Virtual || root.NodeKey != TaskKindRoot || root.ParentID != 0 {
		t.Errorf("root shape wrong: %+v", root)
	}
	if root.Status != StatusPending || root.Attempts != 1 {
		t.Errorf("a freshly dispatched root is pending on attempt 1: %+v", root)
	}
	if root.CreatedAt.IsZero() {
		t.Error("the root should carry its creation time")
	}

	// clone has no dependencies: the root is a container, never a gate.
	if deps := clone.DependsOnIDs(); len(deps) != 0 {
		t.Errorf("clone deps: want none, got %v", deps)
	}
	// build depends on clone.
	if deps := build.DependsOnIDs(); len(deps) != 1 || deps[0] != clone.ID {
		t.Errorf("build deps: want [%d], got %v", clone.ID, deps)
	}
	// unit and both cases depend on build (they may not depend on the virtual
	// stage: a container's status is derived from its children).
	for _, sub := range []*Task{subs[2], caseNodes(t, s, stage)[0], caseNodes(t, s, stage)[1]} {
		if deps := sub.DependsOnIDs(); len(deps) != 1 || deps[0] != build.ID {
			t.Errorf("%s deps: want [%d], got %v", sub.NodeKey, build.ID, deps)
		}
	}
	if deps := stage.DependsOnIDs(); len(deps) != 0 {
		t.Errorf("the virtual stage depends on nothing, got %v", deps)
	}

	// The tree nests the cases under the stage, and every node belongs to the
	// graph the root identifies.
	kids, err := s.ListChildren(root.ID)
	if err != nil || len(kids) != 4 {
		t.Fatalf("root children: want 4, got %d (err %v)", len(kids), err)
	}
	for i := range kids {
		if kids[i].ID != subs[i].ID || kids[i].ParentID != root.ID {
			t.Errorf("child %d: want id %d under %d, got %+v", i, subs[i].ID, root.ID, kids[i])
		}
	}
	if !stage.Virtual || stage.Config != "{}" {
		t.Errorf("the regression stage is a virtual container holding no config: %+v", stage)
	}
	for i, c := range caseNodes(t, s, stage) {
		if c.ParentID != stage.ID || c.Kind != TaskKindRegressionCase {
			t.Errorf("case %d should nest under the stage: %+v", i, c)
		}
		if c.RootID != root.ID || c.Retired {
			t.Errorf("case %d should be part of the active graph: %+v", i, c)
		}
	}
	active, err := s.ListActiveNodes(root.ID)
	if err != nil || len(active) != 6 {
		t.Fatalf("the graph is its root's six nodes: got %d (err %v)", len(active), err)
	}
	if retired, err := s.ListRetiredNodes(root.ID); err != nil || len(retired) != 0 {
		t.Errorf("a fresh graph retires nothing: %d (err %v)", len(retired), err)
	}

	// Every real node carries its attempt's run from dispatch time (the matrix
	// cell and the run page exist while the stage is still queued); the virtual
	// nodes have none — a container is not a cell.
	runKinds := map[string]string{
		TaskKindClone: RunKindClone, TaskKindBuild: RunKindBuild,
		TaskKindUnit: RunKindUnit, TaskKindRegressionCase: RunKindRegression,
	}
	for _, node := range active {
		runs, err := s.ListTaskRuns(node.ID)
		if err != nil {
			t.Fatalf("list runs of %s: %v", node.NodeKey, err)
		}
		if node.Virtual {
			if len(runs) != 0 {
				t.Errorf("virtual node %s must not have runs: %+v", node.NodeKey, runs)
			}
			continue
		}
		if len(runs) != 1 {
			t.Fatalf("node %s: want 1 run, got %d", node.NodeKey, len(runs))
		}
		r := runs[0]
		if r.Attempt != 1 || r.Status != StatusPending || r.TaskID != node.ID {
			t.Errorf("node %s run: %+v", node.NodeKey, r)
		}
		if r.Kind != runKinds[node.Kind] {
			t.Errorf("node %s run kind: want %s, got %s", node.NodeKey, runKinds[node.Kind], r.Kind)
		}
		// Denormalized so the dashboard reads a commit's column without a join.
		if r.EnvironmentID != root.EnvironmentID || r.CommitID != root.CommitID {
			t.Errorf("node %s run should carry the graph's env/commit: %+v", node.NodeKey, r)
		}
	}
	if runs, err := s.ListTaskRuns(root.ID); err != nil || len(runs) != 0 {
		t.Errorf("the root never runs: %d (err %v)", len(runs), err)
	}
}

// A dispatch is all-or-nothing: a graph the runner cannot express is rejected
// before any of it is stored, which is what keeps a container from deadlocking
// the scheduler (an edge to a node whose status is derived from its children
// could never become ready).
func TestUpsertTaskGraphRejectsBadDeps(t *testing.T) {
	s := newTestTaskStore(t)

	cloneNode := func(commitID, envID int64) *Task {
		return &Task{Kind: TaskKindClone, NodeKey: TaskKindClone, Name: "clone", CommitID: commitID, EnvironmentID: envID}
	}

	cases := []struct {
		name  string
		nodes func(commitID, envID int64) []TaskNode
	}{
		{
			// The placeholder indexes the call's own nodes; there is no third
			// entry to point at.
			name: "placeholder past the end of the graph",
			nodes: func(commitID, envID int64) []TaskNode {
				return []TaskNode{{Task: cloneNode(commitID, envID), Deps: []int64{TaskSubPlaceholderBase + 3}}}
			},
		},
		{
			name: "dependency on the root",
			nodes: func(commitID, envID int64) []TaskNode {
				return []TaskNode{{Task: cloneNode(commitID, envID), Deps: []int64{0}}}
			},
		},
		{
			name: "dependency on the virtual regression stage",
			nodes: func(commitID, envID int64) []TaskNode {
				return []TaskNode{
					{Task: cloneNode(commitID, envID)},
					{Task: &Task{Kind: TaskKindRegressionStage, NodeKey: TaskKindRegressionStage, Name: "regression", CommitID: commitID, EnvironmentID: envID}},
					{
						Task:      &Task{Kind: TaskKindRegressionCase, NodeKey: caseNodeKey("heat"), Name: "case", CommitID: commitID, EnvironmentID: envID},
						Deps:      []int64{TaskSubPlaceholderBase + 1},
						ParentKey: TaskKindRegressionStage,
					},
				}
			},
		},
		{
			name: "parent key that no node defines",
			nodes: func(commitID, envID int64) []TaskNode {
				return []TaskNode{{Task: cloneNode(commitID, envID), ParentKey: "nope"}}
			},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, commit := newGraphFixture(t, s, fmt.Sprintf("bad%d", i))
			if _, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), tc.nodes(commit.ID, env.ID)); err == nil {
				t.Fatal("the graph should be rejected")
			}
			// The rejected dispatch left no root (and therefore no node) behind.
			if _, err := s.FindRootTaskByCommitEnv(commit.ID, env.ID); !errors.Is(err, ErrTaskNotFound) {
				t.Errorf("a rejected graph must store nothing, got %v", err)
			}
		})
	}

	// The head of an UpsertTaskGraph call has to be a root row.
	if _, err := s.UpsertTaskGraph(&Task{Kind: TaskKindClone, Name: "clone"}, nil); err == nil {
		t.Error("the graph's head must be a root task")
	}
}

func TestClaimReadyTaskRespectsDependencies(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "claim")
	clone, build, unit := subs[0], subs[1], subs[2]

	// Clone has no dependencies: ready immediately (the root is a derived
	// container, never a gate). Claiming flips the task and the attempt's run
	// in one transaction, so the cell follows the stage from the start.
	got := claimTask(t, s)
	if got.ID != clone.ID || got.Status != StatusRunning || got.StartedAt == nil {
		t.Fatalf("want the clone claimed and running, got %+v", got)
	}
	runs, err := s.ListTaskRuns(clone.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("clone runs: want 1, got %d (err %v)", len(runs), err)
	}
	if runs[0].Status != StatusRunning || runs[0].StartedAt.IsZero() {
		t.Errorf("the claimed attempt's run should be running too: %+v", runs[0])
	}
	// The containers follow the stage they contain: a running node is a
	// running graph, not a queued one.
	if got := reloadTask(t, s, clone.RootID); got.Status != StatusRunning {
		t.Errorf("the root should read running while the clone runs: %+v", got)
	}

	// Nothing else is ready while the clone runs.
	requireNoReadyTask(t, s)

	// Clone passed: build becomes ready.
	reportTask(t, s, clone.ID, StatusPassed, "2 repositories fetched")
	if got := claimTask(t, s); got.ID != build.ID {
		t.Fatalf("want build claimed, got %+v", got)
	}

	// Build failed: unit and the cases never become ready (a broken build
	// cannot be tested). The report skips them in the same transaction — a
	// dependent can never be claimed once its dependency failed — so nothing
	// is left queued and the scheduler hands out nothing at all.
	reportTask(t, s, build.ID, StatusFailed, "make: *** [all] Error 2")
	requireNoReadyTask(t, s)
	if got := reloadTask(t, s, unit.ID); got.Status != StatusSkipped {
		t.Errorf("a stage behind a failed build should be skipped, got %s", got.Status)
	}
}

func TestClaimReadyTaskOldestFirst(t *testing.T) {
	s := newTestTaskStore(t)
	// Two independent graphs with ready clone tasks (clones have no deps).
	_, _ = seedTaskGraph(t, s, "old")
	_, _ = seedTaskGraph(t, s, "new")

	first := claimTask(t, s)
	second := claimTask(t, s)
	if first.ID >= second.ID {
		t.Errorf("claims should follow id ASC order: %d then %d", first.ID, second.ID)
	}
}

// A node reported failed skips everything queued behind it, in the same
// report: nothing downstream can ever be claimed (readiness waits for a passed
// dependency), so a graph whose node ended badly must not keep a queue that
// can never drain. Skipping a dependent closes its attempt as skipped, not as
// failed: the node never ran, so a dashboard must not report a failure it
// never saw. The containers are never skipped directly — the rollup derives
// them.
func TestFailedReportSkipsDependents(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skip")
	clone, build := subs[0], subs[1]
	stage := subs[3]

	reportTask(t, s, clone.ID, StatusFailed, "git fetch: no route to host")

	// Everything downstream of clone: build, unit and both cases.
	blocked := append([]*Task{build, subs[2]}, caseNodes(t, s, stage)...)
	for _, task := range blocked {
		got := reloadTask(t, s, task.ID)
		if got.Status != StatusSkipped {
			t.Errorf("%s: want skipped, got %s", got.NodeKey, got.Status)
		}
		// The reason is free text on a real status now, not a summary prefix:
		// it names the task that stopped the node.
		if !strings.HasPrefix(got.Summary, "upstream task ") || !strings.Contains(got.Summary, clone.Name) {
			t.Errorf("%s summary should name the failed task: %q", got.NodeKey, got.Summary)
		}
		if got.FinishedAt == nil {
			t.Errorf("%s should be closed: %+v", got.NodeKey, got)
		}
		runs, err := s.ListTaskRuns(got.ID)
		if err != nil || len(runs) != 1 {
			t.Fatalf("%s runs: want 1, got %d (err %v)", got.NodeKey, len(runs), err)
		}
		if runs[0].Status != StatusSkipped || runs[0].Summary != got.Summary {
			t.Errorf("%s's in-flight run should read skipped with the task's reason: %+v", got.NodeKey, runs[0])
		}
	}

	// The stage's cases are all skipped, so the container reads skipped; the
	// root has a failed child, which fails it.
	if got := reloadTask(t, s, stage.ID); got.Status != StatusSkipped {
		t.Errorf("the virtual stage should roll up to skipped, got %s", got.Status)
	}
	gotRoot := reloadTask(t, s, root.ID)
	if gotRoot.Status != StatusFailed {
		t.Errorf("a failed child fails the graph, got %s", gotRoot.Status)
	}
	if !strings.Contains(gotRoot.Summary, "clone repositories") {
		t.Errorf("the container's summary should name the failed node: %q", gotRoot.Summary)
	}

	// A skipped dependency does not satisfy readiness: running a node on a
	// broken tree would test nothing.
	requireNoReadyTask(t, s)
}

// A node reported skipped stops its dependents just as a failed one does: it
// did not pass, and readiness waits for passed — a report from an external
// tester (POST /api/test-runs) must not leave the rest of the graph queued for
// ever.
func TestSkippedReportSkipsDependents(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skipreport")
	build, stage := subs[1], subs[3]

	reportTask(t, s, subs[0].ID, StatusPassed, "cloned")
	reportTask(t, s, build.ID, StatusSkipped, "the build was not run")
	requireNoReadyTask(t, s)

	blocked := append([]*Task{subs[2]}, caseNodes(t, s, stage)...)
	for _, task := range blocked {
		got := reloadTask(t, s, task.ID)
		if got.Status != StatusSkipped {
			t.Errorf("%s: want skipped, got %s", got.NodeKey, got.Status)
		}
		if !strings.Contains(got.Summary, build.Name) {
			t.Errorf("%s summary should name the skipped task: %q", got.NodeKey, got.Summary)
		}
	}
	// Nothing is green: the root reports the skip, not a pass.
	if got := reloadTask(t, s, root.ID); got.Status != StatusSkipped {
		t.Errorf("the root of a skipped graph should read skipped, got %s", got.Status)
	}
}

// SkipDependents remains the manual entry point: a caller that skips a node
// for a reason of its own passes that reason on to everything behind it. The
// node it names is not touched — it is the caller's to end.
func TestSkipDependentsWithExplicitReason(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skipexplicit")
	clone, build, unit, stage := subs[0], subs[1], subs[2], subs[3]
	const reason = "upstream clone failed: no route to host"

	if err := s.SkipDependents(root.ID, clone.ID, reason); err != nil {
		t.Fatal(err)
	}
	if got := reloadTask(t, s, clone.ID); got.Status != StatusPending || got.Summary != "" {
		t.Errorf("SkipDependents must not end the node it names: %+v", got)
	}
	for _, task := range append([]*Task{build, unit}, caseNodes(t, s, stage)...) {
		got := reloadTask(t, s, task.ID)
		if got.Status != StatusSkipped || got.Summary != reason {
			t.Errorf("%s: want skipped with %q, got %s %q", got.NodeKey, reason, got.Status, got.Summary)
		}
	}
}

// A re-dispatch while a node is in flight: the attempt it was running is
// closed with the dispatch (nothing else would ever close it), and the outcome
// the goroutine reports afterwards lands on the attempt it actually ran — not
// on the fresh attempt the dispatch queued, which nobody has executed.
func TestRedispatchSupersedesTheAttemptInFlight(t *testing.T) {
	s := newTestTaskStore(t)
	env, commit := newGraphFixture(t, s, "supersede")
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	root, clone := stored[0], stored[1]

	claimed := claimTask(t, s)
	if claimed.ID != clone.ID {
		t.Fatalf("want the clone claimed, got %+v", claimed)
	}
	// The clone is running attempt 1 when the same commit is re-dispatched.
	if _, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID)); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}

	got := reloadTask(t, s, clone.ID)
	if got.Status != StatusPending || got.Attempts != 2 {
		t.Fatalf("the re-dispatch should re-arm the clone on attempt 2: %+v", got)
	}
	superseded, err := s.FindTaskRun(clone.ID, 1)
	if err != nil {
		t.Fatalf("attempt 1 of the clone: %v", err)
	}
	if !TaskStatusTerminal(superseded.Status) {
		t.Errorf("the superseded attempt must not be left open: %s", superseded.Status)
	}
	checkGraph(t, s, root.ID, "after the re-dispatch")

	// The goroutine that was running attempt 1 reports its outcome.
	if _, err := s.FinishAttempt(clone.ID, AttemptResult{
		Status: StatusPassed, Passed: 1, Total: 1, Attempt: 1,
	}); err != nil {
		t.Fatalf("report of the superseded attempt: %v", err)
	}
	if got := reloadTask(t, s, clone.ID); got.Status != StatusPending {
		t.Errorf("a stale report must not close the new attempt: %s", got.Status)
	}
	run1, err := s.FindTaskRun(clone.ID, 1)
	if err != nil {
		t.Fatalf("attempt 1 of the clone: %v", err)
	}
	if run1.Status != StatusPassed {
		t.Errorf("the report should have landed on the attempt that ran: %+v", run1)
	}
	// And the node is still there for the scheduler to run for real.
	if again := claimTask(t, s); again.ID != clone.ID || again.Attempts != 2 {
		t.Errorf("attempt 2 should still be claimable, got %+v", again)
	}
	checkGraph(t, s, root.ID, "after the stale report")
}

// A cascade closes the attempt a node is on now, whatever the caller's
// snapshot said: a re-dispatch between the two must not leave the current
// attempt's run open, and the reason must land on it.
func TestCascadeUsesTheCurrentAttempt(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "cascadecurrent")
	clone, build := subs[0], subs[1]

	// The clone passes, the build is queued; then the same graph is
	// re-dispatched, so every node moves to attempt 2.
	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	if _, err := s.UpsertTaskGraph(graphRoot(root.CommitID, root.EnvironmentID),
		graphNodes(root.CommitID, root.EnvironmentID)); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if got := reloadTask(t, s, build.ID); got.Attempts != 2 {
		t.Fatalf("the build should be on attempt 2: %+v", got)
	}

	// The clone of the new attempt fails: everything behind it is skipped.
	reportTask(t, s, clone.ID, StatusFailed, "git fetch: no route to host")
	got := reloadTask(t, s, build.ID)
	if got.Status != StatusSkipped {
		t.Fatalf("the build should be skipped: %+v", got)
	}
	run, err := s.FindTaskRun(build.ID, got.Attempts)
	if err != nil {
		t.Fatalf("the current attempt's run: %v", err)
	}
	if run.Status != StatusSkipped || run.Summary != got.Summary {
		t.Errorf("the skip should land on the current attempt: %+v", run)
	}
	checkGraph(t, s, root.ID, "after the cascade")
}

// The skip helper is the last line of defence for the same race: it re-reads
// the node inside the transaction and refuses to touch one that is no longer
// pending, even when the caller's snapshot said it was. Skipping a running
// node would close the attempt that is executing and leave the report of the
// goroutine that ran it landing on an attempt it never touched.
func TestSkipTasksLeavesAClaimedNodeAlone(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "skipclaimed")
	clone, build, unit := subs[0], subs[1], subs[2]

	// The caller's view of the unit node: pending, as it was when the graph
	// was read.
	snapshot := reloadTask(t, s, unit.ID)
	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	reportTask(t, s, build.ID, StatusPassed, "built")
	// The scheduler claims the unit while the caller still holds the snapshot.
	if got := claimTask(t, s); got.ID != unit.ID {
		t.Fatalf("want the unit claimed, got %+v", got)
	}

	tx := s.DB.Begin()
	if err := skipTasksTx(tx, []*Task{snapshot}, "upstream failed"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}

	got := reloadTask(t, s, unit.ID)
	if got.Status != StatusRunning {
		t.Errorf("a node claimed since the snapshot must not be skipped: %s", got.Status)
	}
	run, err := s.FindTaskRun(unit.ID, got.Attempts)
	if err != nil {
		t.Fatalf("the unit's run: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("its attempt must stay open for the report: %+v", run)
	}
	if run.LogPrefix != "" || run.LogBytes != 0 {
		t.Errorf("no skip reason should have been logged: %+v", run)
	}
}

// The scheduler's scan is not a fixed window: a ready node is found even when
// more unclaimable nodes than one batch sit in front of it. Unclaimable nodes
// accumulate site-wide — the query is not scoped to one graph — so a window
// would stall every queue on the site behind them.
func TestClaimReadyTaskScansPastBlockedNodes(t *testing.T) {
	s := newTestTaskStore(t)
	// Graphs whose clone is claimed and left running: the build, the unit and
	// both cases stay queued with no way forward (four per graph).
	const graphs = 20
	for i := 0; i < graphs; i++ {
		_, subs := seedTaskGraph(t, s, fmt.Sprintf("blocked%d", i))
		if got := claimTask(t, s); got.ID != subs[0].ID {
			t.Fatalf("graph %d: want the clone claimed, got %+v", i, got)
		}
	}
	var queued int64
	if err := s.DB.Model(&Task{}).Where("status = ? AND virtual = ?", StatusPending, false).
		Count(&queued).Error; err != nil {
		t.Fatal(err)
	}
	if queued <= claimBatch {
		t.Fatalf("the fixture should queue more than one batch (%d), got %d", claimBatch, queued)
	}

	// A fresh graph, ready to start, defined last.
	root, subs := seedTaskGraph(t, s, "arriving")
	got := claimTask(t, s)
	if got.ID != subs[0].ID {
		t.Fatalf("clone #%d of graph #%d is ready but was not handed out: got %+v",
			subs[0].ID, root.ID, got)
	}
}

// Attempt numbers come from the runs on record, not from the counter a caller
// holds: a writer working from a stale copy of the task must not propose an
// attempt that is taken. The stale copy below is what a second writer holds —
// the store's own callers read the task inside their transaction, so only two
// concurrent transactions can both see one state, and the unique index on
// (task_id, attempt) is what keeps the loser from failing outright.
func TestAttemptNumberingSurvivesAStaleCopy(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "numbering")
	build := subs[1]

	first, err := s.GetTask(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.GetTask(build.ID)
	if err != nil {
		t.Fatal(err)
	}

	tx1 := s.DB.Begin()
	runs, err := beginAttemptsTx(tx1, []*Task{first})
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if err := tx1.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx2 := s.DB.Begin()
	runs2, err := beginAttemptsTx(tx2, []*Task{second})
	if err != nil {
		tx2.Rollback()
		t.Fatalf("a second writer must not fail on the attempt index: %v", err)
	}
	if err := tx2.Commit().Error; err != nil {
		t.Fatalf("commit: %v", err)
	}
	for _, r := range append(runs, runs2...) {
		if r.ID == 0 {
			t.Errorf("a run was created without a row: %+v", r)
		}
	}
	stored, err := s.ListTaskRuns(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, r := range stored {
		if seen[r.Attempt] {
			t.Errorf("two runs share attempt %d: %+v", r.Attempt, stored)
		}
		seen[r.Attempt] = true
	}
	got := reloadTask(t, s, build.ID)
	if !seen[got.Attempts] {
		t.Errorf("the task points at attempt %d, which has no run: %+v", got.Attempts, stored)
	}
}

func TestSkipDependentsKeepsRunningAndTerminal(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skiplive")
	clone, build, unit, stage := subs[0], subs[1], subs[2], subs[3]

	// clone and build passed, so the unit task is claimed and running; a
	// later attempt of the clone then fails.
	reportTask(t, s, clone.ID, StatusPassed, "ok")
	reportTask(t, s, build.ID, StatusPassed, "ok")
	if got := claimTask(t, s); got.ID != unit.ID {
		t.Fatalf("want the unit task claimed, got %+v", got)
	}
	reportTask(t, s, clone.ID, StatusFailed, "clone failed")
	if err := s.SkipDependents(root.ID, clone.ID, "upstream clone failed"); err != nil {
		t.Fatal(err)
	}

	// unit is in flight: its outcome is the runner's to report, so a sibling's
	// failure does not touch it.
	if got := reloadTask(t, s, unit.ID); got.Status != StatusRunning {
		t.Errorf("a running task must not be skipped: %s", got.Status)
	}
	// The cases depend on build, not on clone: they stay pending (and the
	// scheduler may still claim them).
	for _, c := range caseNodes(t, s, stage) {
		if got := reloadTask(t, s, c.ID); got.Status != StatusPending {
			t.Errorf("%s does not depend on clone and should stay pending: %s", c.NodeKey, got.Status)
		}
	}
	if got := claimTask(t, s); got.Kind != TaskKindRegressionCase {
		t.Errorf("a case should be claimable, got %+v", got)
	}
}

func TestSkipTaskClosesTheAttempt(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "skipsingle")
	clone, build := subs[0], subs[1]

	// A pending node skipped for a reason of its own (no build command, an
	// unsupported preset): the node and its attempt read skipped.
	if err := s.SkipTask(build.ID, "no build command configured"); err != nil {
		t.Fatal(err)
	}
	got := reloadTask(t, s, build.ID)
	if got.Status != StatusSkipped || got.Summary != "no build command configured" || got.FinishedAt == nil {
		t.Errorf("skipped task wrong: %+v", got)
	}
	runs, err := s.ListTaskRuns(build.ID)
	if err != nil || len(runs) != 1 || runs[0].Status != StatusSkipped {
		t.Errorf("the attempt's run should read skipped: %+v (err %v)", runs, err)
	}

	// A running task is left alone: it is executing, and only a report may
	// close it.
	claimTask(t, s) // the clone is the only ready task
	if err := s.SkipTask(clone.ID, "too late"); err != nil {
		t.Fatal(err)
	}
	if got := reloadTask(t, s, clone.ID); got.Status != StatusRunning || got.Summary == "too late" {
		t.Errorf("skip must not touch a running task: %+v", got)
	}
}

func TestRollupTaskTree(t *testing.T) {
	t.Run("queued children keep the container queued", func(t *testing.T) {
		s := newTestTaskStore(t)
		root, subs := seedTaskGraph(t, s, "rolluprun")

		// The dispatch ends with a rollup of its own: a fresh graph has nothing
		// started yet, so it reads queued — not running, and certainly not
		// green.
		got := reloadTask(t, s, root.ID)
		if got.Status != StatusPending {
			t.Errorf("a queued graph reads pending, got %s", got.Status)
		}
		// Five leaves: clone, build, unit and the stage's two cases (a
		// container contributes its children's tally, never itself).
		if got.Total != 5 || got.Passed != 0 || got.Failed != 0 || got.Skipped != 0 {
			t.Errorf("the graph counts its leaves: %+v", got)
		}
		if got.Summary != "0/5 stages passed; 4 queued" {
			t.Errorf("summary: %q", got.Summary)
		}

		// One node done is not the graph done.
		reportTask(t, s, subs[0].ID, StatusPassed, "2 repositories fetched")
		got = reloadTask(t, s, root.ID)
		if got.Status != StatusPending || got.Passed != 1 {
			t.Errorf("still queued after one node: %+v", got)
		}
		if got.Summary != "1/5 stages passed; 3 queued" {
			t.Errorf("summary: %q", got.Summary)
		}

		// A child the scheduler claimed is what makes the container running:
		// until one of them actually starts, there is nothing in flight.
		if claimed := claimTask(t, s); claimed.ID != subs[1].ID {
			t.Fatalf("want the build claimed, got %+v", claimed)
		}
		if err := s.RollupTaskTree(root.ID); err != nil {
			t.Fatal(err)
		}
		got = reloadTask(t, s, root.ID)
		if got.Status != StatusRunning || got.Passed != 1 {
			t.Errorf("a running child makes the container running: %+v", got)
		}
		if got.Summary != "1/5 stages passed; 1 in progress" {
			t.Errorf("summary: %q", got.Summary)
		}

		// Every leaf terminal and green: the graph passes.
		reportTask(t, s, subs[1].ID, StatusPassed, "ok")
		reportTask(t, s, subs[2].ID, StatusPassed, "12/12 tests passed")
		for _, c := range caseNodes(t, s, subs[3]) {
			reportTask(t, s, c.ID, StatusPassed, "ok")
		}
		got = reloadTask(t, s, root.ID)
		if got.Status != StatusPassed || got.Passed != 5 || got.Total != 5 {
			t.Errorf("want a green graph, got %+v", got)
		}
		if got.Summary != "5/5 stages passed" {
			t.Errorf("summary: %q", got.Summary)
		}
		if got.FinishedAt == nil {
			t.Error("a finished container should be timestamped")
		}
	})

	t.Run("a failed child fails the container and names it", func(t *testing.T) {
		s := newTestTaskStore(t)
		root, subs := seedTaskGraph(t, s, "rollupfail")

		reportTask(t, s, subs[0].ID, StatusPassed, "ok")
		reportTask(t, s, subs[2].ID, StatusFailed, "1 of 12 tests failed")

		got := reloadTask(t, s, root.ID)
		if got.Status != StatusFailed {
			t.Errorf("any failed child fails the container, got %s", got.Status)
		}
		if got.Total != 5 || got.Passed != 1 || got.Failed != 1 {
			t.Errorf("counts: %+v", got)
		}
		// The summary names the culprit, so the graph page needs no drill-down
		// to show what broke.
		if !strings.Contains(got.Summary, "unit tests") {
			t.Errorf("summary should name the failed node: %q", got.Summary)
		}
	})

	t.Run("a virtual child contributes its own totals", func(t *testing.T) {
		s := newTestTaskStore(t)
		root, subs := seedTaskGraph(t, s, "rollupvirtual")
		stage := subs[3]
		cases := caseNodes(t, s, stage)

		reportTask(t, s, cases[0].ID, StatusPassed, "max rel err 2e-9")
		reportTask(t, s, cases[1].ID, StatusFailed, "drift above threshold")

		// The stage reports its cases and never counts itself as one.
		got := reloadTask(t, s, stage.ID)
		if got.Status != StatusFailed || got.Total != 2 || got.Passed != 1 || got.Failed != 1 {
			t.Errorf("stage rollup wrong: %+v", got)
		}
		if got.Summary != "1/2 cases passed; failed: regression: poisson" {
			t.Errorf("summary: %q", got.Summary)
		}

		// The root counts the two cases where the stage sits: clone, build,
		// unit and the cases — five leaves, not four children.
		got = reloadTask(t, s, root.ID)
		if got.Total != 5 || got.Passed != 1 || got.Failed != 1 {
			t.Errorf("the root should see the stage's leaves: %+v", got)
		}
	})

	t.Run("skipped children make the container skipped", func(t *testing.T) {
		s := newTestTaskStore(t)
		root, subs := seedTaskGraph(t, s, "rollupskip")
		stage := subs[3]
		for _, c := range caseNodes(t, s, stage) {
			if err := s.SkipTask(c.ID, "clone failed: no route to host"); err != nil {
				t.Fatal(err)
			}
		}
		// SkipTask closes the node and its attempt; a container is derived, so
		// it takes a rollup to see the consequence.
		if err := s.RollupTaskTree(root.ID); err != nil {
			t.Fatal(err)
		}

		got := reloadTask(t, s, stage.ID)
		// skipped, not failed: nothing broke here, an upstream task never let
		// these cases run.
		if got.Status != StatusSkipped || got.Skipped != 2 || got.Failed != 0 {
			t.Errorf("want a skipped stage, got %+v", got)
		}
		if got.Summary != "2/2 cases skipped (upstream failure)" {
			t.Errorf("summary: %q", got.Summary)
		}
		// The root still has nodes queued (clone, build, unit), and a queued
		// child outranks a skipped one: the graph is pending, not skipped —
		// two skipped cases do not condemn the whole tree.
		if gotRoot := reloadTask(t, s, root.ID); gotRoot.Status != StatusPending {
			t.Errorf("want a queued graph, got %s", gotRoot.Status)
		}
	})

	t.Run("timestamps span the children", func(t *testing.T) {
		s := newTestTaskStore(t)
		root, subs := seedTaskGraph(t, s, "rolluptime")
		cases := caseNodes(t, s, subs[3])

		// The first case ran early and briefly, the second one late: the
		// container's window is the union of its children's, so "how long did
		// the regression stage take?" needs no join.
		base := time.Now().Add(-time.Hour).Truncate(time.Second)
		if _, err := s.FinishAttempt(cases[0].ID, AttemptResult{
			Status: StatusPassed, StartedAt: base, FinishedAt: base.Add(time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.FinishAttempt(cases[1].ID, AttemptResult{
			Status: StatusPassed, StartedAt: base.Add(10 * time.Minute), FinishedAt: base.Add(11 * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}

		got := reloadTask(t, s, subs[3].ID)
		if got.StartedAt == nil || !got.StartedAt.Equal(base) {
			t.Errorf("the stage should start with its earliest case: %v", got.StartedAt)
		}
		if got.FinishedAt == nil || !got.FinishedAt.Equal(base.Add(11*time.Minute)) {
			t.Errorf("the stage should end with its latest case: %v", got.FinishedAt)
		}
		if gotRoot := reloadTask(t, s, root.ID); gotRoot.StartedAt == nil || !gotRoot.StartedAt.Equal(base) {
			t.Errorf("the root should start with its earliest node: %v", gotRoot.StartedAt)
		}
	})

	t.Run("a container with no children stays pending", func(t *testing.T) {
		s := newTestTaskStore(t)
		env, commit := newGraphFixture(t, s, "rollupempty")
		stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), nil)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing to report is not a pass: the container says so instead of
		// inventing a green graph.
		got := reloadTask(t, s, stored[0].ID)
		if got.Status != StatusPending || got.Summary != "no stages" {
			t.Errorf("an empty graph should stay pending: %+v", got)
		}
	})
}

// Re-dispatching a commit reuses its (commit, environment) root and re-arms
// every node for a new attempt. Nothing is deleted: the previous attempt —
// its run, its log — stays readable, which is what makes the history of a
// flaky stage usable.
func TestRedeployRootTaskRebuilds(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "redeploy")
	cases := caseNodes(t, s, subs[3])

	// Run the graph to completion, leaving a log behind on the clone's attempt.
	for _, task := range []*Task{subs[0], subs[1], subs[2], cases[0], cases[1]} {
		reportTask(t, s, task.ID, StatusPassed, "ok")
	}
	firstRun, err := s.FindTaskRun(subs[0].ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetRunLogPrefix(firstRun.ID, "runs/1/log/", 24); err != nil || !ok {
		t.Fatalf("record the attempt's log: ok=%t err=%v", ok, err)
	}
	if before := reloadTask(t, s, root.ID); before.Status != StatusPassed {
		t.Fatalf("the finished graph should be green: %+v", before)
	}

	newRoot := graphRoot(root.CommitID, root.EnvironmentID)
	newRoot.Description = "merged entry (v2)"
	newRoot.Config = `{"tags":["cpu"],"version":2}`
	again, err := s.UpsertTaskGraph(newRoot, graphNodes(root.CommitID, root.EnvironmentID))
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if again[0].ID != root.ID {
		t.Errorf("the re-dispatch should reuse the root: want %d, got %d", root.ID, again[0].ID)
	}

	gotRoot := reloadTask(t, s, root.ID)
	if gotRoot.Attempts != 2 {
		t.Errorf("the root's attempts should count the dispatches: %+v", gotRoot)
	}
	// The children are queued again, so the container is queued again; its own
	// outcome is cleared with the attempt.
	if gotRoot.Status != StatusPending || gotRoot.Summary != "0/5 stages passed; 4 queued" {
		t.Errorf("re-armed root wrong: %+v", gotRoot)
	}
	if gotRoot.StartedAt != nil || gotRoot.FinishedAt != nil {
		t.Errorf("a re-armed root carries no attempt timestamps: %+v", gotRoot)
	}
	// The root carries the entry snapshot, so a re-dispatch refreshes it.
	if gotRoot.Description != "merged entry (v2)" || !strings.Contains(gotRoot.Config, `"version":2`) {
		t.Errorf("the root's snapshot should be refreshed: %+v", gotRoot)
	}
	if gotRoot.CreatedAt.IsZero() {
		t.Error("the root keeps its creation time across dispatches")
	}

	// Every node kept its row and gained an attempt, with the counts cleared.
	for _, task := range []*Task{again[1], again[2], again[3]} {
		got := reloadTask(t, s, task.ID)
		if got.Attempts != 2 || got.Status != StatusPending || got.Retired {
			t.Errorf("%s should be re-armed: %+v", got.NodeKey, got)
		}
		if got.Total != 0 || got.Passed != 0 || got.Failed != 0 || got.Skipped != 0 {
			t.Errorf("%s counts should be cleared: %+v", got.NodeKey, got)
		}
		if got.StartedAt != nil || got.FinishedAt != nil {
			t.Errorf("%s should carry no attempt timestamps: %+v", got.NodeKey, got)
		}
	}
	// No second graph was created for the pair.
	active, err := s.ListActiveNodes(root.ID)
	if err != nil || len(active) != 6 {
		t.Fatalf("the graph should still be its six nodes: %d (err %v)", len(active), err)
	}
	if retired, err := s.ListRetiredNodes(root.ID); err != nil || len(retired) != 0 {
		t.Errorf("a re-dispatch retires nothing: %d (err %v)", len(retired), err)
	}

	// Both attempts of a node stay readable, newest first.
	runs, err := s.ListTaskRuns(subs[0].ID)
	if err != nil || len(runs) != 2 {
		t.Fatalf("want 2 attempts on the clone, got %d (err %v)", len(runs), err)
	}
	if runs[0].Attempt != 2 || runs[0].Status != StatusPending {
		t.Errorf("the newest attempt is the live one: %+v", runs[0])
	}
	if runs[1].Attempt != 1 || runs[1].Status != StatusPassed {
		t.Errorf("the first attempt keeps its outcome: %+v", runs[1])
	}
	// And so does the first attempt's log: the run keeps the reference its
	// parts live under, while the new attempt starts with none.
	if kept, err := s.GetTestRun(firstRun.ID); err != nil || kept.LogPrefix != "runs/1/log/" || kept.LogBytes != 24 {
		t.Errorf("the first attempt's log should survive: %+v (err %v)", kept, err)
	}
	if fresh, err := s.GetTestRun(runs[0].ID); err != nil || fresh.LogPrefix != "" || fresh.LogBytes != 0 {
		t.Errorf("the new attempt starts with no output: %+v (err %v)", fresh, err)
	}
}

// A node the new graph no longer defines is retired: it is never scheduled
// again and takes no part in its parent's rollup, but its runs, logs and
// artifacts are kept. Retired is not a tombstone — a later dispatch that
// defines the key again brings the same row back.
func TestUpsertTaskGraphRetiresDroppedNodes(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "retire")
	unit := subs[2]

	run := reportTask(t, s, unit.ID, StatusPassed, "12/12 tests passed")
	if ok, err := s.SetRunLogPrefix(run.ID, "runs/9/log/", 5); err != nil || !ok {
		t.Fatalf("record the attempt's log: ok=%t err=%v", ok, err)
	}

	// The yaml lost its unit stage: dispatch the nodes that remain.
	nodes := graphNodes(root.CommitID, root.EnvironmentID)
	reduced := append(nodes[:2:2], nodes[3:]...)
	if _, err := s.UpsertTaskGraph(graphRoot(root.CommitID, root.EnvironmentID), reduced); err != nil {
		t.Fatalf("dispatch without the unit stage: %v", err)
	}

	got := reloadTask(t, s, unit.ID)
	if !got.Retired {
		t.Fatalf("a node the dispatch did not mention should be retired: %+v", got)
	}
	// Retired is history: the node keeps the outcome of its last attempt.
	if got.Status != StatusPassed || got.Attempts != 1 {
		t.Errorf("a retired node keeps its last attempt: %+v", got)
	}
	if runs, err := s.ListTaskRuns(unit.ID); err != nil || len(runs) != 1 || runs[0].Status != StatusPassed {
		t.Errorf("a retired node keeps its runs: %+v (err %v)", runs, err)
	}
	if kept, err := s.GetTestRun(run.ID); err != nil || kept.LogPrefix != "runs/9/log/" {
		t.Errorf("a retired node keeps its logs: %+v (err %v)", kept, err)
	}

	// But it is out of the current graph: not listed as active, not listed as
	// a child of the root for the graph view... it stays a child structurally,
	// while the overlay builds the graph from the active nodes only.
	active, err := s.ListActiveNodes(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range active {
		if node.ID == unit.ID {
			t.Error("a retired node must not be listed as active")
		}
	}
	retired, err := s.ListRetiredNodes(root.ID)
	if err != nil || len(retired) != 1 || retired[0].ID != unit.ID {
		t.Errorf("the retired node should be listed as such: %+v (err %v)", retired, err)
	}
	kids, err := s.ListChildren(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range kids {
		found = found || kids[i].ID == unit.ID
	}
	if !found {
		t.Error("ListChildren is the structural view and includes retired nodes")
	}
	// The rollup counts the current graph: clone, build and the two cases.
	gotRoot := reloadTask(t, s, root.ID)
	if gotRoot.Total != 4 || gotRoot.Passed != 0 {
		t.Errorf("a retired node must not be rolled up: %+v", gotRoot)
	}
	// And the scheduler never hands one out.
	for {
		claimed, err := s.ClaimReadyTask()
		if err != nil {
			t.Fatal(err)
		}
		if claimed == nil {
			break
		}
		if claimed.ID == unit.ID {
			t.Fatal("a retired node must never be scheduled")
		}
	}

	// A later dispatch that defines the key again revives the row with its
	// history intact.
	if _, err := s.UpsertTaskGraph(graphRoot(root.CommitID, root.EnvironmentID), graphNodes(root.CommitID, root.EnvironmentID)); err != nil {
		t.Fatal(err)
	}
	got = reloadTask(t, s, unit.ID)
	if got.Retired || got.Status != StatusPending || got.Attempts != 2 {
		t.Errorf("a re-defined node comes back un-retired, on a new attempt: %+v", got)
	}
	runs, err := s.ListTaskRuns(unit.ID)
	if err != nil || len(runs) != 2 || runs[1].Status != StatusPassed {
		t.Errorf("the revived node keeps its history: %+v (err %v)", runs, err)
	}
}

// The (commit, environment) pair identifies one graph. The root row is the
// persistent identity of "this commit tested on this machine", so a concurrent
// second dispatch fails the insert (the runner retries it as a requeue) and a
// deliberate re-dispatch reuses the row under the partial unique index.
func TestOneRootTaskPerCommitEnvironment(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "uniqroot")

	// A second root row for the pair cannot be inserted: the index is what
	// makes a racing dispatch fail rather than create a twin graph.
	dup := graphRoot(root.CommitID, root.EnvironmentID)
	if err := s.DB.Create(dup).Error; err == nil {
		t.Error("a second root for the same (commit, environment) should be rejected")
	}

	// The intended path reuses the stored root row instead.
	again, err := s.UpsertTaskGraph(graphRoot(root.CommitID, root.EnvironmentID), graphNodes(root.CommitID, root.EnvironmentID))
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if again[0].ID != root.ID {
		t.Errorf("want the stored root %d, got %d", root.ID, again[0].ID)
	}
	var roots int64
	if err := s.DB.Model(&Task{}).
		Where("kind = ? AND commit_id = ? AND environment_id = ?", TaskKindRoot, root.CommitID, root.EnvironmentID).
		Count(&roots).Error; err != nil {
		t.Fatal(err)
	}
	if roots != 1 {
		t.Errorf("want exactly one root for the pair, got %d", roots)
	}

	// The sub-tasks share the pair and prove the index is partial.
	if len(subs) != 4 {
		t.Fatalf("expected 4 sub-tasks, got %d", len(subs))
	}

	// The same commit on another environment is a different graph.
	other, _ := newGraphFixture(t, s, "uniqroot2")
	second := &Task{
		Kind: TaskKindRoot, Name: "root elsewhere", CommitID: root.CommitID,
		EnvironmentID: other.ID, Tags: "cpu",
	}
	stored, err := s.UpsertTaskGraph(second, nil)
	if err != nil {
		t.Fatalf("another environment's graph should be allowed: %v", err)
	}
	if stored[0].ID == root.ID {
		t.Error("each environment gets its own graph for a commit")
	}
}

// A crashed run leaves rows behind: ResetStaleRunning returns them to pending
// so the next scheduler cycle retries the same attempt (the run follows its
// task, and the log continues where it stopped).
func TestResetStaleRunningTasks(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "stale")
	clone := subs[0]

	// The clone is claimed and running when the server dies; the container was
	// left running too (it died between a rollup and its commit).
	claimTask(t, s)
	if err := s.DB.Model(&Task{}).Where("id = ?", root.ID).
		Updates(map[string]any{"status": StatusRunning, "started_at": time.Now()}).Error; err != nil {
		t.Fatal(err)
	}

	n, err := s.ResetStaleRunning()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("want 2 stale rows reset, got %d", n)
	}
	got := reloadTask(t, s, clone.ID)
	if got.Status != StatusPending || got.StartedAt != nil {
		t.Errorf("a stale task goes back to pending without a start time: %+v", got)
	}
	if got.Error == "" {
		t.Error("the reset should say why the attempt restarted")
	}
	// The same attempt is retried: a crash is not a new attempt.
	if got.Attempts != 1 {
		t.Errorf("the attempt count should not change: %+v", got)
	}
	runs, err := s.ListTaskRuns(clone.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want the one attempt, got %d (err %v)", len(runs), err)
	}
	if runs[0].Status != StatusPending || !runs[0].StartedAt.IsZero() {
		t.Errorf("the interrupted run goes back to pending: %+v", runs[0])
	}
	// The containers follow the reset: nothing is running any more, so the
	// graph reads queued again instead of staying green-to-be.
	if got := reloadTask(t, s, root.ID); got.Status != StatusPending {
		t.Errorf("the container should roll back to pending: %+v", got)
	}
	// And the clone can be claimed again.
	if got := claimTask(t, s); got.ID != clone.ID {
		t.Errorf("the reset task should be claimable: %+v", got)
	}
}

func TestDeleteTasksForEnvironment(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "delenv")
	clone := subs[0]

	run := reportTask(t, s, clone.ID, StatusPassed, "ok")
	if ok, err := s.SetRunLogPrefix(run.ID, "runs/3/log/", 4); err != nil || !ok {
		t.Fatalf("record the attempt's log: ok=%t err=%v", ok, err)
	}
	if err := s.DeleteTasksForEnvironment(root.EnvironmentID); err != nil {
		t.Fatal(err)
	}

	// The environment's tasks, runs, logs and artifacts go together: the
	// dashboard shows site-wide history, so a dangling row would outlive the
	// machine it ran on.
	if _, err := s.GetTask(root.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("the root should be gone, got %v", err)
	}
	if _, err := s.GetTask(clone.ID); !errors.Is(err, ErrTaskNotFound) {
		t.Errorf("the nodes should be gone, got %v", err)
	}
	if _, err := s.GetTestRun(run.ID); !errors.Is(err, ErrTestRunNotFound) {
		t.Errorf("the runs should be gone, got %v", err)
	}
	// The log itself lives in object storage; what the database held is the
	// reference on the run row, gone with the run (asserted above).
	if artifacts, err := s.ListRunArtifacts(run.ID); err != nil || len(artifacts) != 0 {
		t.Errorf("the artifacts should be gone: %d (err %v)", len(artifacts), err)
	}
	if nodes, err := s.ListActiveNodes(root.ID); err != nil || len(nodes) != 0 {
		t.Errorf("the graph should be gone: %d (err %v)", len(nodes), err)
	}
}

func TestListRootTasksLimit(t *testing.T) {
	s := newTestTaskStore(t)
	for i := 0; i < 3; i++ {
		seedTaskGraph(t, s, fmt.Sprintf("limit%d", i))
	}
	tasks, err := s.ListRootTasks(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Errorf("want 2 roots, got %d", len(tasks))
	}
	// Newest first.
	if tasks[0].ID <= tasks[1].ID {
		t.Errorf("roots should be newest first: %d %d", tasks[0].ID, tasks[1].ID)
	}
}

// FindRootGraphsByCommits is the overlay the graph page draws: the root of a
// (commit, environment) pair plus the nodes the latest dispatch defined. A
// retired node belonged to an earlier yaml and is left out, so the overlay
// shows the current graph shape.
func TestFindRootGraphsByCommits(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "overlay")
	key := EnvCommit{Env: root.EnvironmentID, Commit: root.CommitID}

	graphs, err := s.FindRootGraphsByCommits([]int64{root.EnvironmentID}, []int64{root.CommitID})
	if err != nil {
		t.Fatal(err)
	}
	if len(graphs) != 1 {
		t.Fatalf("want one graph, got %d", len(graphs))
	}
	summary, ok := graphs[key]
	if !ok || summary.Root.ID != root.ID || len(summary.Subs) != 6 {
		t.Fatalf("the current graph should come back whole: %+v", graphs)
	}

	// Drop the unit stage: it is history from here on, and the overlay of the
	// pair must not draw it any more.
	nodes := graphNodes(root.CommitID, root.EnvironmentID)
	if _, err := s.UpsertTaskGraph(graphRoot(root.CommitID, root.EnvironmentID), append(nodes[:2:2], nodes[3:]...)); err != nil {
		t.Fatal(err)
	}
	graphs, err = s.FindRootGraphsByCommits([]int64{root.EnvironmentID}, []int64{root.CommitID})
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range graphs[key].Subs {
		if sub.ID == subs[2].ID {
			t.Errorf("a retired node should not be part of the overlay: %+v", sub)
		}
	}
	if len(graphs[key].Subs) != 5 {
		t.Errorf("want the five surviving nodes, got %d", len(graphs[key].Subs))
	}

	// Another commit on the same environment is a different cell of the
	// matrix, resolved in one call.
	_, otherCommit := newGraphFixture(t, s, "overlay2")
	if _, err := s.UpsertTaskGraph(graphRoot(otherCommit.ID, root.EnvironmentID), nil); err != nil {
		t.Fatal(err)
	}
	graphs, err = s.FindRootGraphsByCommits(
		[]int64{root.EnvironmentID}, []int64{root.CommitID, otherCommit.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(graphs) != 2 {
		t.Errorf("want both commits' graphs, got %d", len(graphs))
	}
	if got := graphs[EnvCommit{Env: root.EnvironmentID, Commit: otherCommit.ID}]; got.Root.CommitID != otherCommit.ID {
		t.Errorf("the second commit's graph is keyed wrong: %+v", got)
	}

	// Empty inputs short-circuit without error.
	if empty, err := s.FindRootGraphsByCommits(nil, []int64{root.CommitID}); err != nil || len(empty) != 0 {
		t.Errorf("no environments should return an empty map: %v (%v)", empty, err)
	}
	if empty, err := s.FindRootGraphsByCommits([]int64{root.EnvironmentID}, nil); err != nil || len(empty) != 0 {
		t.Errorf("no commits should return an empty map: %v (%v)", empty, err)
	}
}

// The small predicates the scheduler and the API classify nodes with.
func TestTaskKindAndStatusPredicates(t *testing.T) {
	for _, kind := range []string{TaskKindRoot, TaskKindRegressionStage} {
		if !TaskKindVirtual(kind) {
			t.Errorf("%q is a container", kind)
		}
		if !ValidateTaskKind(kind) {
			t.Errorf("%q is a known kind", kind)
		}
	}
	for _, kind := range []string{TaskKindClone, TaskKindBuild, TaskKindUnit, TaskKindRegressionCase} {
		if TaskKindVirtual(kind) {
			t.Errorf("%q executes and is not a container", kind)
		}
	}
	if ValidateTaskKind("perf") || ValidateTaskKind("") {
		t.Error("an unknown kind must not validate")
	}

	for _, status := range []string{StatusPassed, StatusFailed, StatusSkipped} {
		if !TaskStatusTerminal(status) {
			t.Errorf("%q ends an attempt", status)
		}
	}
	if TaskStatusTerminal(StatusPending) || TaskStatusTerminal(StatusRunning) {
		t.Error("pending and running are not terminal")
	}
	// The skip path shares the terminal vocabulary of a run.
	for _, status := range []string{StatusPending, StatusRunning, "bogus"} {
		if AttemptStatusValid(status) {
			t.Errorf("%q cannot end an attempt", status)
		}
	}

	// A case's run carries the stage's kind, so one matrix column covers all
	// the cases.
	kinds := []struct{ task, run string }{
		{TaskKindClone, RunKindClone},
		{TaskKindBuild, RunKindBuild},
		{TaskKindUnit, RunKindUnit},
		{TaskKindRegressionCase, RunKindRegression},
	}
	for _, k := range kinds {
		if got := RunKindForTask(k.task); got != k.run {
			t.Errorf("RunKindForTask(%q) = %q, want %q", k.task, got, k.run)
		}
		if !RunKindValid(k.run) {
			t.Errorf("%q should be a valid run kind", k.run)
		}
	}
	if RunKindValid(TaskKindRoot) {
		t.Error("the root is not a run kind: it never runs")
	}

	labels := map[string]string{
		TaskKindClone: "clone", TaskKindBuild: "build", TaskKindUnit: "unit test",
		TaskKindRegressionStage: "regression", TaskKindRegressionCase: "regression case",
	}
	for kind, want := range labels {
		if got := StageLabel(kind); got != want {
			t.Errorf("StageLabel(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestTaskDependsOnRoundTrip(t *testing.T) {
	task := &Task{}
	if deps := task.DependsOnIDs(); deps != nil {
		t.Errorf("a task without dependencies decodes to nil: %v", deps)
	}
	task.SetDependsOnIDs([]int64{3, 4})
	if task.DependsOn != "[3,4]" {
		t.Errorf("stored form: %q", task.DependsOn)
	}
	deps := task.DependsOnIDs()
	if len(deps) != 2 || deps[0] != 3 || deps[1] != 4 {
		t.Errorf("round trip: %v", deps)
	}
	// No dependencies clears the column rather than storing "[]": a root (or a
	// first stage) must read as unconstrained.
	task.SetDependsOnIDs(nil)
	if task.DependsOn != "" {
		t.Errorf("cleared: %q", task.DependsOn)
	}
	if deps := task.DependsOnIDs(); deps != nil {
		t.Errorf("cleared decode: %v", deps)
	}
	// A corrupted column is not fatal: the scheduler sees no dependency (the
	// alternative — refusing to schedule — would strand the graph).
	task.DependsOn = "not json"
	if deps := task.DependsOnIDs(); deps != nil {
		t.Errorf("malformed deps should decode to nil: %v", deps)
	}
}

// TestClaimReadyTaskSkipsANodeReArmedUnderIt drives the one window a claim
// cannot close on its own: a dispatch that lands between the candidate scan
// and the claim. It re-arms the node (pending again on a new attempt) and
// supersedes the attempt the scan saw, so the snapshot in hand describes work
// nobody will ever report for. Taking it would leave the node running for ever
// — its in-flight run was already closed, so the report lands on the old
// attempt and the node row is never written again, and no later claim can pick
// up a running task. The claim must leave the node to the next tick.
func TestClaimReadyTaskSkipsANodeReArmedUnderIt(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "rearm.db"), WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	env, commit := newGraphFixture(t, s, "rearm")
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID))
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	clone := stored[1] // no dependencies: the first node a claim can take

	// The query callback is the only seam into that window a test has: it
	// fires once, on the claim's candidate scan, and re-dispatches the graph
	// before the claim's own UPDATE runs.
	armed := true
	if err := s.DB.Callback().Query().After("gorm:query").Register("test:rearm-inside-claim", func(tx *gorm.DB) {
		if !armed || !strings.Contains(tx.Statement.SQL.String(), "retired") {
			return
		}
		armed = false
		if _, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID)); err != nil {
			t.Errorf("re-dispatch inside the claim window: %v", err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}

	got, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if armed {
		t.Fatal("the re-dispatch never landed inside the claim window; the test proves nothing")
	}
	if got != nil {
		t.Fatalf("the claim handed out node %d on attempt %d, which the dispatch had already replaced",
			got.ID, got.Attempts)
	}

	// The node is where the re-dispatch left it, and the next tick claims it
	// properly: attempt 2, with the run the claim opened for it.
	fresh := reloadTask(t, s, clone.ID)
	if fresh.Status != StatusPending || fresh.Attempts != 2 {
		t.Fatalf("after the lost claim: status=%s attempts=%d, want pending on attempt 2",
			fresh.Status, fresh.Attempts)
	}
	again, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again == nil || again.ID != clone.ID || again.Attempts != 2 {
		t.Fatalf("second claim = %+v, want node %d on attempt 2", again, clone.ID)
	}
	run, err := s.FindTaskRun(clone.ID, 2)
	if err != nil {
		t.Fatalf("attempt 2 run: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("attempt 2 run status = %s, want %s", run.Status, StatusRunning)
	}
	// And the report that follows takes the node to a terminal state — the
	// stuck-running outcome this guard exists to prevent.
	reportTask(t, s, clone.ID, StatusPassed, "cloned")
	if end := reloadTask(t, s, clone.ID); end.Status != StatusPassed {
		t.Errorf("node status after its report = %s, want %s", end.Status, StatusPassed)
	}
}

// graphNodesWithoutCase returns the fixture graph with one regression case
// left out — a yaml edit that drops a case while a graph is still queued.
func graphNodesWithoutCase(commitID, envID int64, preset string) []TaskNode {
	all := graphNodes(commitID, envID)
	out := make([]TaskNode, 0, len(all))
	for _, n := range all {
		if n.Task.NodeKey == caseNodeKey(preset) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// A node the new graph no longer defines is retired — and a retired node is
// never claimed again, so an attempt it left queued would stay pending for
// ever: a run on the run page nobody can close, in a graph the rollup does not
// count it in either. Retiring closes it instead, and says why.
func TestRetiringANodeClosesItsQueuedAttempt(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "retirequeued")
	stage := subs[3]
	cases := caseNodes(t, s, stage)
	heat := cases[0]

	// The same commit and environment are dispatched again, without the case.
	if _, err := s.UpsertTaskGraph(graphRoot(stage.CommitID, stage.EnvironmentID),
		graphNodesWithoutCase(stage.CommitID, stage.EnvironmentID, "heat")); err != nil {
		t.Fatalf("re-dispatch without the case: %v", err)
	}

	retired := reloadTask(t, s, heat.ID)
	if !retired.Retired {
		t.Fatal("the dropped case should be retired")
	}
	if retired.Status != StatusSkipped {
		t.Errorf("retired node status = %s, want %s", retired.Status, StatusSkipped)
	}
	if retired.Summary != RetiredSummary {
		t.Errorf("retired node summary = %q, want %q", retired.Summary, RetiredSummary)
	}
	runs, err := s.ListTaskRuns(heat.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want the one attempt, got %d (err %v)", len(runs), err)
	}
	if runs[0].Status != StatusSkipped {
		t.Errorf("the queued attempt of a retired node = %s, want %s", runs[0].Status, StatusSkipped)
	}
	if runs[0].Summary != RetiredSummary || runs[0].FinishedAt.IsZero() {
		t.Errorf("the closed attempt should carry the reason and an end time: %+v", runs[0])
	}
}

// A node retired while a worker is executing it is left alone: that worker is
// still there and reports into its own attempt. If the server dies first,
// nothing is left to report, so the restart closes the attempt instead of
// handing back a node that can never be claimed.
func TestRestartClosesARunningAttemptOfARetiredNode(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "retirerunning")
	stage := subs[3]
	heat := caseNodes(t, s, stage)[0]

	// Claim and finish the stages before it, so the case is what runs.
	var claimed *Task
	for i := 0; i < 6 && claimed == nil; i++ {
		got := claimTask(t, s)
		if got.ID == heat.ID {
			claimed = got
			break
		}
		reportTask(t, s, got.ID, StatusPassed, "passed")
	}
	if claimed == nil {
		t.Fatal("the heat case was never claimed")
	}
	// It is dropped by a new dispatch while it is running: the worker keeps
	// going and its attempt is not closed under it.
	if _, err := s.UpsertTaskGraph(graphRoot(stage.CommitID, stage.EnvironmentID),
		graphNodesWithoutCase(stage.CommitID, stage.EnvironmentID, "heat")); err != nil {
		t.Fatalf("re-dispatch without the case: %v", err)
	}
	if got := reloadTask(t, s, heat.ID); got.Status != StatusRunning {
		t.Errorf("a running attempt of a retired node = %s, want it left running", got.Status)
	}

	// The server dies before that worker reports.
	if _, err := s.ResetStaleRunning(); err != nil {
		t.Fatal(err)
	}
	got := reloadTask(t, s, heat.ID)
	if got.Status != StatusSkipped {
		t.Errorf("after the restart the retired node = %s, want %s", got.Status, StatusSkipped)
	}
	runs, err := s.ListTaskRuns(heat.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("want the one attempt, got %d (err %v)", len(runs), err)
	}
	if runs[0].Status != StatusSkipped || runs[0].Summary != RetiredSummary {
		t.Errorf("the interrupted attempt of a retired node = %s %q, want %s %q",
			runs[0].Status, runs[0].Summary, StatusSkipped, RetiredSummary)
	}
}
