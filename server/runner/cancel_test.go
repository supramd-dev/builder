package runner

import (
	"testing"
	"time"

	"md-builder/server/store"
)

// manualSummary is the wording a hand cancellation carries: whoever pressed the
// button, named in the summary the node and the run end up with.
const manualSummary = "cancelled by hand by tester"

// TestCancelSubtreeStopsTheContainerItNames covers the manual half of
// cancellation: the store drops the whole group the task names (the regression
// container and every case under it that has not finished), the runner aborts
// the stage it is executing for the running one, and the report that aborted
// stage sends afterwards is discarded.
//
// What the cancellation does NOT touch is the point of the first half: the unit
// stage beside the container keeps running to its own outcome.
func TestCancelSubtreeStopsTheContainerItNames(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	ctx := testContext()

	// Clone runs on the fixture's service; build too, since the unit stage and
	// the regression cases are only ready behind it. The unit stage and one case
	// are then claimed and left running.
	unitTask, caseTask := claimUnitAndCase(t, svc, s, cloneTask)

	// One case runs a script that only ends when its context is cancelled.
	blk := newBlockingExecer()
	blocking := &Service{Store: s, SSH: blk, Clone: cloner}
	done := make(chan struct{})
	go func() {
		defer close(done)
		blocking.runClaimed(ctx, caseTask)
	}()
	select {
	case <-blk.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the regression case never started")
	}

	// The unit stage is a leaf, and nothing under the cancellation is running:
	// a leaf cancellation drops the unit stage alone and has nothing to abort —
	// the case, which is running, is not the store's to stop here and is not the
	// runner's to kill either.
	leaf, err := blocking.CancelSubtree(unitTask.ID, manualSummary)
	if err != nil {
		t.Fatalf("cancel the unit stage: %v", err)
	}
	if len(leaf.Nodes) != 1 || leaf.Nodes[0] != unitTask.ID || !leaf.Cancelled() {
		t.Fatalf("cancelling the unit stage dropped %v, want just %d", leaf.Nodes, unitTask.ID)
	}
	if leaf.Aborted != 0 {
		t.Fatalf("cancelling the unit stage aborted %d stage(s) of another subtree", leaf.Aborted)
	}
	running, err := s.GetTask(caseTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if running.Status != store.StatusRunning {
		t.Fatalf("the running case is %q after a sibling was cancelled", running.Status)
	}

	// Now the container: every case under it goes, the one running included.
	container, err := s.GetTask(caseTask.ParentID)
	if err != nil {
		t.Fatal(err)
	}
	if container.Kind != store.TaskKindRegressionStage {
		t.Fatalf("the running case's parent is %s, want the regression container", container.Kind)
	}
	kids, err := s.ListChildren(container.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(kids) != 2 {
		t.Fatalf("the container has %d cases, want the two of the fixture", len(kids))
	}

	group, err := blocking.CancelSubtree(container.ID, manualSummary)
	if err != nil {
		t.Fatalf("cancel the regression container: %v", err)
	}
	// The container, the case that was running and the case still queued — and
	// the unit stage that was already cancelled is not part of it.
	want := map[int64]bool{container.ID: true, kids[0].ID: true, kids[1].ID: true}
	if len(group.Nodes) != 3 || group.Nodes[0] != container.ID {
		t.Fatalf("cancelling the container dropped %v, want %v (target first)", group.Nodes, want)
	}
	for _, id := range group.Nodes {
		if !want[id] {
			t.Fatalf("cancelling the container dropped node %d, which is not under it", id)
		}
	}
	if group.Aborted != 1 {
		t.Fatalf("aborted %d stage(s), want the one case this process was running", group.Aborted)
	}

	// The aborted stage's session is closed, so its execution returns.
	select {
	case <-blk.aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled case was not aborted")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the aborted case's execution did not return")
	}

	// The store kept the cancellation: the aborted case reported an outcome (a
	// killed session looks like a failed command), and it was discarded — one
	// run, the cancelled one, with the summary the caller gave.
	node, err := s.GetTask(caseTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Status != store.StatusCancelled || node.Summary != manualSummary {
		t.Fatalf("the aborted case is %q/%q, want cancelled/%q",
			node.Status, node.Summary, manualSummary)
	}
	runs, err := s.ListTaskRuns(caseTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("the discarded report opened %d run(s), want the one cancelled attempt", len(runs))
	}
	if runs[0].Status != store.StatusCancelled || runs[0].Summary != manualSummary {
		t.Fatalf("the aborted case's run is %q/%q, want cancelled/%q",
			runs[0].Status, runs[0].Summary, manualSummary)
	}
	// The case that never started is cancelled too: a group is stopped whole.
	queued := kids[0]
	if kids[0].ID == caseTask.ID {
		queued = kids[1]
	}
	other, err := s.GetTask(queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if other.Status != store.StatusCancelled || other.Summary != manualSummary {
		t.Fatalf("the queued case is %q/%q, want cancelled/%q",
			other.Status, other.Summary, manualSummary)
	}
	// And the graph above it rolls up cancelled rather than left running.
	root, err := s.GetTask(container.RootID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status != store.StatusCancelled {
		t.Fatalf("the graph reads %q, want cancelled", root.Status)
	}

	// Cancelling the same container again finds nothing: the second press of
	// the button is not reported as having stopped anything.
	again, err := blocking.CancelSubtree(container.ID, manualSummary)
	if err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	if again.Cancelled() || again.Aborted != 0 {
		t.Fatalf("the second cancel reports %v (%d aborted)", again.Nodes, again.Aborted)
	}

	// An unknown task is an error, not a silent success.
	if _, err := blocking.CancelSubtree(container.ID+10000, manualSummary); err == nil {
		t.Fatal("cancelling an unknown task reported success")
	}
}

// claimUnitAndCase drives the fixture's graph far enough that the unit stage and
// one regression case are both ready, claims them (so both are running) and
// returns them. Everything on the way — clone, then build behind it — is
// executed on svc with its fake execer.
func claimUnitAndCase(t *testing.T, svc *Service, s *store.Store, cloneTask *store.Task) (unit, c *store.Task) {
	t.Helper()
	ctx := testContext()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatalf("clone: %v", err)
	}
	for unit == nil || c == nil {
		task, err := s.ClaimReadyTask()
		if err != nil || task == nil {
			t.Fatalf("claim: %v %v (unit=%v case=%v)", err, task, unit, c)
		}
		switch task.Kind {
		case store.TaskKindUnit:
			if unit == nil {
				unit = task
				continue
			}
		case store.TaskKindRegressionCase:
			if c == nil {
				c = task
				continue
			}
		}
		// Only build can come up before the two of them, and it has to finish
		// for either to be ready.
		if task.Kind != store.TaskKindBuild {
			t.Fatalf("unexpected claimable %s", task.Kind)
		}
		if err := svc.ExecuteTask(ctx, task); err != nil {
			t.Fatalf("execute %s: %v", task.Kind, err)
		}
	}
	return unit, c
}
