package store

import (
	"errors"
	"testing"
)

// finishNode claims a node and reports the given status for its current
// attempt, the way the runner does, so a test can arrange a mix of finished
// and unfinished work.
func finishNode(t *testing.T, s *Store, taskID int64, status string) {
	t.Helper()
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatalf("get task %d: %v", taskID, err)
	}
	if _, err := s.ClaimReadyTask(); err != nil {
		t.Fatalf("claim: %v", err)
	}
	res := AttemptResult{Status: status, Attempt: task.Attempts, Total: 1}
	if status == StatusPassed {
		res.Passed = 1
	} else {
		res.Failed = 1
	}
	if _, err := s.FinishAttempt(taskID, res); err != nil {
		t.Fatalf("report %d as %s: %v", taskID, status, err)
	}
}

// TestCancelGraphCancelsUnfinishedWork is the policy's core promise: the nodes
// that have not reached an outcome — and the runs of their in-flight attempts
// — become cancelled, while a node that already finished keeps everything it
// produced. Containers follow their children up to the root.
func TestCancelGraphCancelsUnfinishedWork(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-unfinished")
	clone, build, unit, stage := kids[0], kids[1], kids[2], kids[3]
	cases := caseNodes(t, s, stage)

	// Clone passes; everything after it is still queued.
	finishNode(t, s, clone.ID, StatusPassed)

	cancelled, err := s.CancelGraph(root.ID, "")
	if err != nil {
		t.Fatalf("cancel graph: %v", err)
	}
	// The cancelled set is every unfinished node — the two stages behind the
	// one that passed, both regression cases, and the container they hang
	// under (a container is cancelled with its children; the rollup would
	// derive the same) — and never the node that already passed.
	want := map[int64]bool{build.ID: true, unit.ID: true, stage.ID: true,
		cases[0].ID: true, cases[1].ID: true}
	if len(cancelled) != len(want) {
		t.Fatalf("cancelled %v, want the unfinished nodes %v", cancelled, want)
	}
	for _, id := range cancelled {
		if !want[id] {
			t.Fatalf("cancelled node %d, which was not unfinished (clone=%d build=%d unit=%d)",
				id, clone.ID, build.ID, unit.ID)
		}
	}

	// The finished node is untouched: same status, same run, same counts.
	got, err := s.GetTask(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusPassed || got.Passed != 1 {
		t.Fatalf("the passed node was rewritten: %+v", got)
	}
	cloneRun, err := s.FindTaskRun(clone.ID, clone.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	if cloneRun.Status != StatusPassed {
		t.Fatalf("the passed node's run: want passed, got %q", cloneRun.Status)
	}

	// Every cancelled node carries the status and the summary on both its row
	// and the run of its attempt. A container keeps the rollup's wording
	// instead — it names the children that were cancelled — so only its
	// status is the store's to write.
	for _, id := range cancelled {
		task, err := s.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != StatusCancelled {
			t.Fatalf("node %q: want cancelled, got %q", task.NodeKey, task.Status)
		}
		if !task.Virtual && task.Summary != CancelledSummary {
			t.Fatalf("node %q: want the summary %q, got %q", task.NodeKey, CancelledSummary, task.Summary)
		}
		if task.Virtual {
			continue // no attempt, no run
		}
		run, err := s.FindTaskRun(id, task.Attempts)
		if err != nil {
			t.Fatalf("node %q: run: %v", task.NodeKey, err)
		}
		if run.Status != StatusCancelled || run.Summary != CancelledSummary {
			t.Fatalf("node %q attempt %d: want cancelled/%q, got %q/%q",
				task.NodeKey, run.Attempt, CancelledSummary, run.Status, run.Summary)
		}
		if run.FinishedAt.IsZero() {
			t.Fatalf("node %q: the cancelled run has no finish time", task.NodeKey)
		}
	}

	// Containers roll up to cancelled: the case container and the root.
	for _, id := range []int64{stage.ID, root.ID} {
		task, err := s.GetTask(id)
		if err != nil {
			t.Fatal(err)
		}
		if task.Status != StatusCancelled {
			t.Fatalf("container %q: want cancelled, got %q (summary %q)", task.NodeKey, task.Status, task.Summary)
		}
	}
	// The root counts the stages the way the rollup promises: the cancelled
	// ones count as not passed, so the tally still adds up.
	gotRoot, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot.Total != 5 || gotRoot.Passed != 1 || gotRoot.Skipped != 4 {
		t.Fatalf("root counts T/P/F/S = %d/%d/%d/%d, want 5/1/0/4",
			gotRoot.Total, gotRoot.Passed, gotRoot.Failed, gotRoot.Skipped)
	}

	// Cancelling again changes nothing and reports nothing.
	again, err := s.CancelGraph(root.ID, "")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the second cancel touched %v", again)
	}
}

// TestCancelGraphLeavesFinishedGraphsAlone: a policy that fires against a graph
// that already finished must be a no-op — cancelling history would rewrite
// results the dashboard has already shown.
func TestCancelGraphLeavesFinishedGraphsAlone(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-finished")
	stage := kids[3]
	for _, n := range append([]*Task{kids[0], kids[1], kids[2]}, caseNodes(t, s, stage)...) {
		finishNode(t, s, n.ID, StatusPassed)
	}
	before, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != StatusPassed {
		t.Fatalf("the graph should have finished: root is %q", before.Status)
	}

	cancelled, err := s.CancelGraph(root.ID, "")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if len(cancelled) != 0 {
		t.Fatalf("cancelled %v in a finished graph", cancelled)
	}
	after, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != StatusPassed || after.Passed != before.Passed || after.Total != before.Total {
		t.Fatalf("a finished graph was rewritten: %+v", after)
	}
	for _, n := range append([]*Task{kids[0], kids[1], kids[2]}, caseNodes(t, s, stage)...) {
		got, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusPassed {
			t.Fatalf("node %q: want passed, got %q", got.NodeKey, got.Status)
		}
	}
}

// TestCancelGraphAbandonsARunningAttempt: an attempt claimed but not reported
// is in flight. Cancelling closes it, and the late report the runner sends
// afterwards is refused rather than taken — the store must not revive a
// cancelled attempt, nor open a new one behind it.
func TestCancelGraphAbandonsARunningAttempt(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-running")
	clone, build := kids[0], kids[1]

	claimed, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != clone.ID {
		t.Fatalf("claimed %v, want the clone node %d", claimed, clone.ID)
	}
	if claimed.Status != StatusRunning {
		t.Fatalf("a claimed node should be running, got %q", claimed.Status)
	}
	// The node behind a cancelled one never runs: cancel while build is still
	// queued and only the clone's attempt is in flight.
	if _, err := s.CancelGraph(root.ID, "stopped by the policy"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	run, err := s.FindTaskRun(clone.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusCancelled || run.Summary != "stopped by the policy" {
		t.Fatalf("the running attempt: got %q/%q", run.Status, run.Summary)
	}
	if run.StartedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
		t.Fatalf("a cancelled running attempt should keep its window: %+v", run)
	}

	// The runner's report for that attempt arrives now. It carries the
	// attempt number, so the store knows exactly which attempt it describes.
	_, err = s.FinishAttempt(clone.ID, AttemptResult{
		Status: StatusFailed, Attempt: 1, Total: 1, Failed: 1, Error: "killed",
	})
	if !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("the late report: want ErrTaskCancelled, got %v", err)
	}
	task, err := s.GetTask(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != StatusCancelled {
		t.Fatalf("the late report took the node over: %q", task.Status)
	}
	runs, err := s.ListTaskRuns(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("the late report opened a new attempt: %d runs", len(runs))
	}
	if runs[0].Status != StatusCancelled || runs[0].Summary != "stopped by the policy" {
		t.Fatalf("the cancelled run was rewritten: %+v", runs[0])
	}

	// Nothing of the graph is claimable: the cancelled nodes are not pending,
	// and the nodes behind them never become ready.
	if next, err := s.ClaimReadyTask(); err != nil {
		t.Fatalf("claim after cancel: %v", err)
	} else if next != nil && next.ID == build.ID {
		t.Fatal("the queue still hands out a cancelled graph's nodes")
	}
}

// TestUpsertTaskGraphRevivesACancelledGraph: absorbing is what cancelled does
// to a *report*, not to a dispatch. Dispatching the row again — the site went
// back to the requeue policy, so a further push of the revision deduplicates
// onto the very row the policy dropped — runs the graph again: every node is
// re-armed on a fresh attempt, exactly as an ordinary re-dispatch re-arms a
// finished graph, and the cancelled attempt stays behind as history. Refusing
// would leave the row permanently undispatchable while it is still the row
// pushes land on.
func TestUpsertTaskGraphRevivesACancelledGraph(t *testing.T) {
	s := newTestTaskStore(t)
	env, commit := newGraphFixture(t, s, "cancel-revive")
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	root, clone, build := stored[0], stored[1], stored[2]

	// Clone passes, the policy drops the rest, and the run of the cancelled
	// attempt is what a later reader sees as the answer to that attempt.
	finishNode(t, s, clone.ID, StatusPassed)
	if _, err := s.CancelGraph(root.ID, CancelledSummary); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	before, err := s.GetTask(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Status != StatusCancelled {
		t.Fatalf("the dropped node is %q, want cancelled", before.Status)
	}

	again, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), graphNodes(commit.ID, env.ID))
	if err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if again[0].ID != root.ID {
		t.Fatalf("the re-dispatch made a new root %d, want the stored %d", again[0].ID, root.ID)
	}
	revived := again[2]
	if revived.ID != build.ID {
		t.Fatalf("the re-dispatch made a new build node %d, want %d", revived.ID, build.ID)
	}
	if revived.Status != StatusPending || revived.Attempts != before.Attempts+1 {
		t.Fatalf("the re-dispatched node is %q attempt %d, want pending attempt %d",
			revived.Status, revived.Attempts, before.Attempts+1)
	}
	if revived.Summary != "" || revived.Error != "" {
		t.Fatalf("the cancellation was left on the revived node: %q/%q", revived.Summary, revived.Error)
	}
	// The node that had passed is re-armed like any other: a dispatch runs the
	// whole graph, not only the part that was dropped.
	gotClone, err := s.GetTask(clone.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotClone.Status != StatusPending || gotClone.Attempts != clone.Attempts+1 || gotClone.Passed != 0 {
		t.Fatalf("the passed node was not re-armed: %+v", gotClone)
	}
	// The root follows: the rollup at the end of the dispatch reads a pending
	// graph, not the cancellation.
	gotRoot, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot.Status != StatusPending {
		t.Fatalf("the re-dispatched root is %q, want pending", gotRoot.Status)
	}

	// Two attempts, two runs: the cancelled one keeps its verdict, its summary
	// and its window, and the new one is open for the scheduler.
	runs, err := s.ListTaskRuns(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("the build node has %d runs, want the cancelled attempt and the new one", len(runs))
	}
	if runs[1].Attempt != before.Attempts || runs[1].Status != StatusCancelled || runs[1].Summary != CancelledSummary {
		t.Fatalf("the cancelled attempt was rewritten: %+v", runs[1])
	}
	if runs[1].FinishedAt.IsZero() {
		t.Fatal("the revived dispatch reopened the cancelled run")
	}
	if runs[0].Attempt != before.Attempts+1 || runs[0].Status != StatusPending {
		t.Fatalf("the new attempt's run: %+v", runs[0])
	}

	// And the graph is dispatchable again: the queue hands out the clone's new
	// attempt, which is what gates everything behind it.
	claimed, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ID != clone.ID || claimed.Attempts != clone.Attempts+1 {
		t.Fatalf("the queue handed out %+v, want the clone's attempt %d", claimed, clone.Attempts+1)
	}
}

// TestBeginAttemptRefusesCancelledTask: cancelled is absorbing. Opening an
// attempt on it would put the node back in the scheduler's queue underneath a
// cancelled graph.
func TestBeginAttemptRefusesCancelledTask(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-begin")
	if _, err := s.CancelGraph(root.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BeginAttempt(kids[1].ID); !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("BeginAttempt on a cancelled node: want ErrTaskCancelled, got %v", err)
	}
}
