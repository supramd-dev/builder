package runner

// Manual cancellation, runner side. The store decides what "cancelled" means
// for a node and a run (store.CancelSubtree); the runner is the only one who
// can interrupt what it is executing right now, and that is all this adds to
// what the store did.

// CancelOutcome is what a cancellation did: the nodes it dropped, in graph
// order, and how many of them this process was executing when it asked — that
// is, how many stages were actually interrupted. A stage another process is
// running, or one nobody had claimed yet, is cancelled in the store without
// being counted here; the count is "what this process had to kill".
type CancelOutcome struct {
	Nodes   []int64
	Aborted int
}

// Cancelled reports whether the cancellation had unfinished work to stop. A
// task that had already finished, or one that was cancelled before, drops
// nothing and says so: there is no such thing as cancelling history.
func (o CancelOutcome) Cancelled() bool { return len(o.Nodes) > 0 }

// CancelSubtree cancels the unfinished work of one task and of everything below
// it — a stage on its own, a whole container's worth of tests (every case of
// the regression container, running ones included), or the entire graph when
// the task is the graph's root. Only unfinished work goes; a node that already
// reached an outcome keeps it (store.CancelSubtree).
//
// The store's cancellation is what ends the work: it closes the run of the
// attempt that was in flight and writes the cancelled status, so a report that
// arrives afterwards is refused (store.ErrTaskCancelled). Aborting here is the
// other half: the stages this process is running for the nodes just dropped get
// their SSH session closed, instead of being left to finish something whose
// outcome the store would then refuse.
//
// The summary is what the cancelled nodes and runs carry. Empty means
// store.CancelledSummary, the repeated-commit policy's wording.
func (s *Service) CancelSubtree(taskID int64, summary string) (CancelOutcome, error) {
	nodeIDs, err := s.Store.CancelSubtree(taskID, summary)
	if err != nil {
		return CancelOutcome{}, err
	}
	out := CancelOutcome{Nodes: nodeIDs}
	for _, nodeID := range nodeIDs {
		if s.CancelTask(nodeID) {
			out.Aborted++
		}
	}
	return out, nil
}
