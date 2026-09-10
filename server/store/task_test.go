package store

import (
	"fmt"
	"testing"
	"time"
)

func newTestTaskStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedTaskGraph(t *testing.T, s *Store, name string) (*Task, []*Task) {
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

	root := &Task{
		Kind: TaskKindRoot, Name: "root", CommitID: commit.ID, EnvironmentID: env.ID,
		Tags: "cpu", Config: `{"tags":["cpu"]}`,
	}
	subs := []*Task{
		{Kind: TaskKindClone, Name: "clone", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: TaskKindBuild, Name: "build", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: TaskKindUnit, Name: "unit", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: TaskKindRegression, Name: "regression", CommitID: commit.ID, EnvironmentID: env.ID},
	}
	deps := [][]int64{
		{TaskRootPlaceholder},
		{TaskSubPlaceholderBase + 0}, // build <- clone
		{TaskSubPlaceholderBase + 1}, // unit <- build
		{TaskSubPlaceholderBase + 1}, // regression <- build
	}
	stored, err := CreateTaskGraph(s, root, subs, deps)
	if err != nil {
		t.Fatal(err)
	}
	return stored[0], stored[1:]
}

func TestCreateTaskGraphResolvesDeps(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "graph")

	if root.RootID != root.ID {
		t.Errorf("root RootID: want %d, got %d", root.ID, root.RootID)
	}
	// clone depends on root only.
	if deps := subs[0].DependsOnIDs(); len(deps) != 1 || deps[0] != root.ID {
		t.Errorf("clone deps: want [%d], got %v", root.ID, deps)
	}
	// build depends on clone.
	if deps := subs[1].DependsOnIDs(); len(deps) != 1 || deps[0] != subs[0].ID {
		t.Errorf("build deps: want [%d], got %v", subs[0].ID, deps)
	}
	// unit and regression depend on build.
	for _, i := range []int{2, 3} {
		if deps := subs[i].DependsOnIDs(); len(deps) != 1 || deps[0] != subs[1].ID {
			t.Errorf("sub %d deps: want [%d], got %v", i, subs[1].ID, deps)
		}
	}
	if got, err := s.ListSubTasks(root.ID); err != nil || len(got) != 4 {
		t.Errorf("ListSubTasks: %d subs, err %v", len(got), err)
	}
	// RootID links every node to the root.
	for _, sub := range subs {
		if sub.RootID != root.ID {
			t.Errorf("sub %s RootID: want %d, got %d", sub.Kind, root.ID, sub.RootID)
		}
	}
}

func TestCreateTaskGraphForwardDepRejected(t *testing.T) {
	s := newTestTaskStore(t)
	root := &Task{Kind: TaskKindRoot, Name: "root", CommitID: 1, EnvironmentID: 1}
	subs := []*Task{
		{Kind: TaskKindClone, Name: "clone"},
		{Kind: TaskKindBuild, Name: "build"},
	}
	// clone referencing build (a later sub-task) is invalid.
	deps := [][]int64{{TaskSubPlaceholderBase + 1}, {}}
	if _, err := CreateTaskGraph(s, root, subs, deps); err == nil {
		t.Error("forward dependency should be rejected")
	}
}

func TestClaimReadyTaskRespectsDependencies(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "claim")
	_ = root

	// Initially only clone (deps all done trivially — root counts as done? No:
	// the root is pending, so clone is NOT ready).
	got, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("clone should not be ready while root is pending; got %+v", got)
	}

	// Mark the root done: clone becomes ready.
	if err := s.FinishTask(root.ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Kind != TaskKindClone {
		t.Fatalf("want clone claimed, got %+v", got)
	}
	if got.Status != TaskRunning || got.StartedAt == nil {
		t.Errorf("claimed task should be running with StartedAt: %+v", got)
	}

	// A second claim must not return the same task.
	again, err := s.ClaimReadyTask()
	if err != nil || again != nil {
		t.Errorf("no other task should be ready; got %+v err %v", again, err)
	}

	// Finish clone: build becomes ready.
	if err := s.FinishTask(subs[0].ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Kind != TaskKindBuild {
		t.Fatalf("want build claimed, got %+v", got)
	}

	// build fails: unit/regression never become ready.
	if err := s.FinishTask(subs[1].ID, TaskFailed, "boom"); err != nil {
		t.Fatal(err)
	}
	got, err = s.ClaimReadyTask()
	if err != nil || got != nil {
		t.Errorf("nothing should be ready after build failed; got %+v err %v", got, err)
	}
}

func TestClaimReadyTaskOldestFirst(t *testing.T) {
	s := newTestTaskStore(t)
	// Two independent graphs with ready clone tasks.
	_, subs1 := seedTaskGraph(t, s, "old")
	if err := s.FinishTask(subs1[0].RootID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	// A second graph.
	root2, subs2 := seedTaskGraph(t, s, "new")
	if err := s.FinishTask(root2.ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	_ = subs2

	first, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil {
		t.Fatalf("both clones should be claimable: %+v %+v", first, second)
	}
	if first.ID >= second.ID {
		t.Errorf("claims should follow id ASC order: %d then %d", first.ID, second.ID)
	}
}

func TestSkipDependents(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skip")

	// clone fails → everything downstream is skipped.
	if err := s.FinishTask(subs[0].ID, TaskFailed, "clone failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SkipDependents(root.ID, subs[0].ID, "skipped: upstream clone failed"); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{1, 2, 3} {
		got, err := s.GetTask(subs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != TaskSkipped {
			t.Errorf("sub %d status: want skipped, got %s", i, got.Status)
		}
		if got.Error == "" {
			t.Errorf("sub %d should carry the skip reason", i)
		}
	}
}

func TestSkipDependentsKeepsRunningAndDone(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "skiplive")
	_ = root
	// build is done, unit is running; clone fails.
	if err := s.FinishTask(subs[1].ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.DB.Model(&Task{}).Where("id = ?", subs[2].ID).
		Updates(map[string]any{"status": TaskRunning, "started_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(subs[0].ID, TaskFailed, "clone failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.SkipDependents(root.ID, subs[0].ID, "skipped"); err != nil {
		t.Fatal(err)
	}
	// unit was running: untouched. regression (pending, deps on build) untouched.
	got, _ := s.GetTask(subs[2].ID)
	if got.Status != TaskRunning {
		t.Errorf("running unit should not be skipped: %s", got.Status)
	}
	got, _ = s.GetTask(subs[3].ID)
	if got.Status != TaskPending {
		t.Errorf("regression does not depend on clone; should stay pending: %s", got.Status)
	}
}

func TestRefreshRootStatus(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "rootstatus")

	// Not all terminal yet: root stays pending.
	if err := s.FinishTask(subs[0].ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := s.RefreshRootStatus(root.ID); err != nil || changed {
		t.Errorf("root should not change while sub-tasks are open: changed=%v err=%v", changed, err)
	}

	// All done → root done.
	for _, i := range []int{1, 2, 3} {
		if err := s.FinishTask(subs[i].ID, TaskDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	status, changed, err := s.RefreshRootStatus(root.ID)
	if err != nil || !changed || status != TaskDone {
		t.Errorf("want root done (changed): status=%s changed=%v err=%v", status, changed, err)
	}

	// A failed graph: build the second fixture and fail one leg.
	root2, subs2 := seedTaskGraph(t, s, "rootfail")
	for _, i := range []int{0, 1} {
		if err := s.FinishTask(subs2[i].ID, TaskDone, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishTask(subs2[2].ID, TaskFailed, "unit failed"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(subs2[3].ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	status, changed, err = s.RefreshRootStatus(root2.ID)
	if err != nil || !changed || status != TaskFailed {
		t.Errorf("want root failed (changed): status=%s changed=%v err=%v", status, changed, err)
	}
}

func TestRedeployRootTaskRebuilds(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "redeploy")

	// Run the graph to completion and write some logs.
	for _, sub := range subs {
		if err := s.FinishTask(sub.ID, TaskDone, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendTaskLog(sub.ID, 1, "some output\n"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendTaskLog(root.ID, 1, "root log\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(root.ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}

	if err := s.RedeployRootTask(root); err != nil {
		t.Fatal(err)
	}
	if root.Status != TaskPending || root.Attempts != 1 || root.FinishedAt != nil {
		t.Errorf("root should be reset: %+v", root)
	}
	subsAfter, err := s.ListSubTasks(root.ID)
	if err != nil || len(subsAfter) != 0 {
		t.Errorf("sub-tasks should be gone after redeploy: %d err=%v", len(subsAfter), err)
	}
	if logs, err := s.ReadTaskLogs(root.ID, 0); err != nil || len(logs) != 0 {
		t.Errorf("root logs should be gone: %d err=%v", len(logs), err)
	}
}

func TestFindRootTasksByCommitsSkipsDone(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "overlay")

	// Live root is returned.
	runs, err := s.FindRootTasksByCommits([]int64{root.EnvironmentID}, []int64{root.CommitID})
	if err != nil {
		t.Fatal(err)
	}
	key := EnvCommit{Env: root.EnvironmentID, Commit: root.CommitID}
	summary, ok := runs[key]
	if !ok || summary.Root.ID != root.ID || len(summary.Subs) != 4 {
		t.Fatalf("live root missing: %+v", runs)
	}

	// A done root is not returned.
	if err := s.FinishTask(subs[0].ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishTask(root.ID, TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	runs, err = s.FindRootTasksByCommits([]int64{root.EnvironmentID}, []int64{root.CommitID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := runs[key]; ok {
		t.Error("done root should not be returned for overlay")
	}
}

func TestResetStaleRunningTasks(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "stale")

	if _, err := s.ClaimReadyTask(); err != nil {
		t.Fatal(err)
	}
	// Fake a crash: force the root and clone to running.
	now := time.Now()
	for _, id := range []int64{root.ID, subs[0].ID} {
		if err := s.DB.Model(&Task{}).Where("id = ?", id).
			Updates(map[string]any{"status": TaskRunning, "started_at": now}).Error; err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ResetStaleRunning()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("want 2 stale tasks reset, got %d", n)
	}
	got, _ := s.GetTask(subs[0].ID)
	if got.Status != TaskPending {
		t.Errorf("stale task should be pending again: %s", got.Status)
	}
}

func TestDeleteTasksForEnvironment(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "delenv")

	if err := s.AppendTaskLog(subs[0].ID, 1, "log\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteTasksForEnvironment(root.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTask(root.ID); err != ErrTaskNotFound {
		t.Errorf("tasks should be gone, got err %v", err)
	}
	if logs, err := s.ReadTaskLogs(subs[0].ID, 0); err != nil || len(logs) != 0 {
		t.Errorf("logs should be gone: %d err=%v", len(logs), err)
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
