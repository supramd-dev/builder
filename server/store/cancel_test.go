package store

import (
	"errors"
	"strings"
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

// TestCancelSubtreeOfAContainer: cancelling a container drops what is under it
// and nothing else — the way a whole group of tests is stopped at once.
// Cancelling the regression container leaves the build and unit stages beside
// it alone, cases that had already finished keep their results, and the
// container's summary names the children it dropped.
func TestCancelSubtreeOfAContainer(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-subtree")
	clone, build, unit, stage := kids[0], kids[1], kids[2], kids[3]
	cases := caseNodes(t, s, stage)

	// Clone, build and unit finish; the first case finishes too, so the
	// cancellation has something finished to leave alone inside the subtree it
	// targets.
	for _, n := range []*Task{clone, build, unit, cases[0]} {
		finishNode(t, s, n.ID, StatusPassed)
	}

	cancelled, err := s.CancelSubtree(stage.ID, "cancelled by hand by tester")
	if err != nil {
		t.Fatalf("cancel the regression container: %v", err)
	}
	// The container and the one case that had not finished — and nothing else.
	if len(cancelled) != 2 {
		t.Fatalf("cancelled %v, want the container %d and the unfinished case %d",
			cancelled, stage.ID, cases[1].ID)
	}
	for _, id := range cancelled {
		if id != stage.ID && id != cases[1].ID {
			t.Fatalf("cancelled node %d, which is not under the regression container", id)
		}
	}

	// The case that was still queued: cancelled, with the caller's summary, and
	// the run of its attempt closed the same way.
	got, err := s.GetTask(cases[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled || got.Summary != "cancelled by hand by tester" {
		t.Fatalf("the dropped case is %q/%q", got.Status, got.Summary)
	}
	caseRun, err := s.FindTaskRun(cases[1].ID, got.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	if caseRun.Status != StatusCancelled || caseRun.Summary != "cancelled by hand by tester" {
		t.Fatalf("the dropped case's run: %q/%q", caseRun.Status, caseRun.Summary)
	}

	// The case that had finished inside the subtree keeps everything.
	kept, err := s.GetTask(cases[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if kept.Status != StatusPassed || kept.Passed != 1 || kept.Summary == "cancelled by hand by tester" {
		t.Fatalf("the finished case inside the subtree was rewritten: %+v", kept)
	}

	// The stages beside it are untouched: cancelling a group is not cancelling
	// the pipeline around it.
	for _, n := range []*Task{build, unit} {
		got, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusPassed {
			t.Fatalf("stage %q was cancelled with the container: %q", got.NodeKey, got.Status)
		}
	}

	// The container reads the rollup of its children: the dropped case is named
	// the way the rollup names it, and the graph above follows.
	container, err := s.GetTask(stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if container.Status != StatusCancelled ||
		!strings.Contains(container.Summary, "cancelled: regression: poisson") {
		t.Fatalf("the container is %q/%q, want the rollup to name the dropped case",
			container.Status, container.Summary)
	}
	if strings.Contains(container.Summary, "heat") {
		t.Fatalf("the container's summary names a case that passed: %q", container.Summary)
	}
	gotRoot, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoot.Status != StatusCancelled {
		t.Fatalf("the graph above the container is %q, want cancelled", gotRoot.Status)
	}
	if gotRoot.Total != 5 || gotRoot.Passed != 4 || gotRoot.Skipped != 1 {
		t.Fatalf("root counts T/P/F/S = %d/%d/%d/%d, want 5/4/0/1",
			gotRoot.Total, gotRoot.Passed, gotRoot.Failed, gotRoot.Skipped)
	}

	// Cancelling again changes nothing.
	again, err := s.CancelSubtree(stage.ID, "cancelled by hand by tester")
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("the second cancel touched %v", again)
	}
}

// TestCancelSubtreeOfALeaf: the narrowest cancellation — one stage, its own
// run, and nothing beside it. The stages beside it keep running, and the graph
// is not painted cancelled while they still are.
func TestCancelSubtreeOfALeaf(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-leaf")
	clone, build, unit, stage := kids[0], kids[1], kids[2], kids[3]
	cases := caseNodes(t, s, stage)

	finishNode(t, s, clone.ID, StatusPassed)
	finishNode(t, s, build.ID, StatusPassed)

	cancelled, err := s.CancelSubtree(unit.ID, "")
	if err != nil {
		t.Fatalf("cancel the unit stage: %v", err)
	}
	if len(cancelled) != 1 || cancelled[0] != unit.ID {
		t.Fatalf("cancelled %v, want just the unit stage %d", cancelled, unit.ID)
	}
	got, err := s.GetTask(unit.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An empty summary is the store's own wording: this is what a caller that
	// does not say why gets.
	if got.Status != StatusCancelled || got.Summary != CancelledSummary {
		t.Fatalf("the cancelled stage is %q/%q, want cancelled/%q",
			got.Status, got.Summary, CancelledSummary)
	}
	run, err := s.FindTaskRun(unit.ID, got.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusCancelled || run.Summary != CancelledSummary {
		t.Fatalf("the cancelled stage's run: %q/%q", run.Status, run.Summary)
	}
	for _, n := range []*Task{clone, build} {
		other, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if other.Status != StatusPassed {
			t.Fatalf("sibling %q became %q", other.NodeKey, other.Status)
		}
	}
	// The work queued behind the cancelled stage is still queued, not dropped
	// with it, and the graph above reads queued rather than cancelled: stopping
	// one stage is not stopping the pipeline around it.
	for _, n := range cases {
		other, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if other.Status != StatusPending {
			t.Fatalf("queued case %q became %q", other.NodeKey, other.Status)
		}
	}
	queued, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != StatusPending {
		t.Fatalf("the graph with work still queued reads %q, want pending", queued.Status)
	}

	// Once the rest of the graph finishes, the cancelled stage is what the graph
	// reads: the rollup carries it up unchanged.
	for _, n := range cases {
		finishNode(t, s, n.ID, StatusPassed)
	}
	settled, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.Status != StatusCancelled {
		t.Fatalf("the finished graph reads %q, want cancelled", settled.Status)
	}
	if settled.Total != 5 || settled.Passed != 4 || settled.Skipped != 1 {
		t.Fatalf("root counts T/P/F/S = %d/%d/%d/%d, want 5/4/0/1",
			settled.Total, settled.Passed, settled.Failed, settled.Skipped)
	}
	// And the cancelled stage keeps the wording the cancellation gave it.
	again, err := s.GetTask(unit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Summary != CancelledSummary {
		t.Fatalf("the rollup rewrote the cancelled stage's summary: %q", again.Summary)
	}

	// An unknown id is not a cancellation.
	if _, err := s.CancelSubtree(unit.ID+1000, ""); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("cancelling an unknown task: want ErrTaskNotFound, got %v", err)
	}
}

// TestCancelSubtreeOfARunningCase: the same group cancellation while the work
// is in flight — the case claimed by a worker is cancelled and the run of its
// attempt is closed, so the late report it sends afterwards is refused.
func TestCancelSubtreeOfARunningCase(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-subtree-running")
	clone, build, unit, stage := kids[0], kids[1], kids[2], kids[3]
	cases := caseNodes(t, s, stage)

	for _, n := range []*Task{clone, build, unit} {
		finishNode(t, s, n.ID, StatusPassed)
	}
	// The queue hands out a case of the regression container (both depend on
	// build, which passed).
	claimed, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.ParentID != stage.ID {
		t.Fatalf("claimed %+v, want a case of the regression container %d", claimed, stage.ID)
	}

	if _, err := s.CancelSubtree(root.ID, "cancelled by hand by tester"); err != nil {
		t.Fatalf("cancel the graph: %v", err)
	}
	run, err := s.FindTaskRun(claimed.ID, claimed.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != StatusCancelled || run.Summary != "cancelled by hand by tester" {
		t.Fatalf("the running case's attempt: %q/%q", run.Status, run.Summary)
	}
	if run.StartedAt.IsZero() || run.FinishedAt.Before(run.StartedAt) {
		t.Fatalf("a cancelled running attempt should keep its window: %+v", run)
	}
	if _, err := s.FinishAttempt(claimed.ID, AttemptResult{
		Status: StatusPassed, Attempt: claimed.Attempts, Total: 1, Passed: 1,
	}); !errors.Is(err, ErrTaskCancelled) {
		t.Fatalf("the late report of a hand-cancelled case: want ErrTaskCancelled, got %v", err)
	}

	// The case that had not been handed out is cancelled with it, so a group is
	// never half-stopped, and nothing of the graph is left in the queue.
	for _, n := range cases {
		got, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusCancelled {
			t.Fatalf("case %q is %q after the cancellation", got.NodeKey, got.Status)
		}
	}
	if next, err := s.ClaimReadyTask(); err != nil {
		t.Fatalf("claim after cancel: %v", err)
	} else if next != nil {
		t.Fatalf("the queue still hands out %q", next.NodeKey)
	}
}

// TestCancelSubtreeSkipsWhatStandsBehindIt: cancelling a stage that other work
// is waiting on cannot leave that work queued — the queue only hands out nodes
// whose dependencies all passed, so it would sit at pending for ever and the
// graph would never settle. It is skipped with the reason, exactly as it is
// behind a failed node.
func TestCancelSubtreeSkipsWhatStandsBehindIt(t *testing.T) {
	s := newTestTaskStore(t)
	root, kids := seedTaskGraph(t, s, "cancel-skips-behind")
	clone, build, unit, stage := kids[0], kids[1], kids[2], kids[3]
	cases := caseNodes(t, s, stage)

	finishNode(t, s, clone.ID, StatusPassed)

	// Build is what everything after it waits on.
	cancelled, err := s.CancelSubtree(build.ID, "cancelled by hand by tester")
	if err != nil {
		t.Fatalf("cancel the build stage: %v", err)
	}
	if len(cancelled) != 1 || cancelled[0] != build.ID {
		t.Fatalf("cancelled %v, want just the build stage %d", cancelled, build.ID)
	}

	// Every node waiting on it is skipped, with the reason naming what stopped
	// it — the cancellation, not a failure of its own.
	const reason = "upstream task build was cancelled"
	for _, n := range append([]*Task{unit}, cases...) {
		got, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != StatusSkipped || got.Summary != reason {
			t.Fatalf("node %q behind the cancelled build is %q/%q, want skipped/%q",
				got.NodeKey, got.Status, got.Summary, reason)
		}
		run, err := s.FindTaskRun(n.ID, got.Attempts)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != StatusSkipped || run.Summary != reason {
			t.Fatalf("node %q's attempt is %q/%q, want skipped/%q",
				got.NodeKey, run.Status, run.Summary, reason)
		}
	}
	// The container over the skipped cases follows them, and the graph reads
	// what became of its stages.
	container, err := s.GetTask(stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if container.Status != StatusSkipped {
		t.Fatalf("the container over the skipped cases reads %q", container.Status)
	}
	got, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled {
		t.Fatalf("the graph reads %q, want cancelled", got.Status)
	}
	// The cancelled build counts in the skipped tally like every node that
	// ended without a verdict (cancelled has no column of its own), so the
	// count adds up: one pass, four that never got one.
	if got.Total != 5 || got.Passed != 1 || got.Skipped != 4 {
		t.Fatalf("root counts T/P/F/S = %d/%d/%d/%d, want 5/1/0/4",
			got.Total, got.Passed, got.Failed, got.Skipped)
	}

	// Nothing is left in the queue: the graph has settled, and a scheduler
	// asking for work is told there is none rather than handed a node that can
	// never run.
	if next, err := s.ClaimReadyTask(); err != nil {
		t.Fatalf("claim after cancel: %v", err)
	} else if next != nil {
		t.Fatalf("the queue still hands out %q", next.NodeKey)
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
