package runner

import (
	"log"

	"md-builder/server/store"
)

// Repeated-commit policy, dispatch side. A revision that reaches the matrix
// twice (a push and then an MR for the same SHA, a tag push of a pushed commit,
// a manual re-trigger) can either keep one graph — the default, which requeues
// what is there — or get a graph of its own per recording (the fork policies,
// see store.RecordCommit). What is left here is what the fork policies do to
// the work the earlier recordings left behind: nothing for
// store.CommitOverlapFork, and cancellation for store.CommitOverlapForkCancel.

// cancelPriorWork cancels the unfinished work of the earlier commit rows of the
// same revision, and returns how many task graphs it dropped (0 unless the
// site's policy is store.CommitOverlapForkCancel). It runs after the new
// dispatch created its graphs, never before: a dispatch that failed — an
// unreadable yaml, no matching environment — must not be the reason a running
// test disappears.
//
// A graph counts as dropped only when it had unfinished work to give up: one
// that had already finished, or that an earlier recording already cancelled, is
// left exactly as it is and is not counted — the number is reported to the
// dispatcher as "what this dispatch dropped".
//
// Only unfinished work goes: a node that already finished keeps its status and
// its runs, and the graphs roll up with the cancelled nodes counted as dropped
// (store.CancelGraph). The returned count is what the caller reports back to
// the dispatcher; it counts graphs, not nodes, because "how many runs did this
// push drop" is a question about the columns on the dashboard.
func (s *Service) cancelPriorWork(cfg *store.SiteConfig, commit *store.Commit) int {
	if cfg == nil || cfg.OverlapPolicy() != store.CommitOverlapForkCancel {
		return 0
	}
	if commit == nil || commit.ID == 0 {
		return 0
	}
	priors, err := s.Store.PriorCommits(commit.Repo, commit.SHA, commit.ID)
	if err != nil {
		log.Printf("runner: commit %d: prior commits of %s@%s: %v",
			commit.ID, commit.Repo, commit.SHA, err)
		return 0
	}
	if len(priors) == 0 {
		return 0
	}
	ids := make([]int64, 0, len(priors))
	for i := range priors {
		ids = append(ids, priors[i].ID)
	}
	roots, err := s.Store.RootsByCommitIDs(ids)
	if err != nil {
		log.Printf("runner: commit %d: task graphs of %d earlier recording(s): %v",
			commit.ID, len(priors), err)
		return 0
	}

	cancelled := 0
	for i := range roots {
		root := roots[i]
		nodeIDs, err := s.Store.CancelGraph(root.ID, store.CancelledSummary)
		if err != nil {
			// One graph failing to cancel must not stop the others: the next
			// recording of this revision retries it, and the nodes it did not
			// reach are the ones still running.
			log.Printf("runner: task %d: cancel graph of the earlier commit %d: %v",
				root.ID, root.CommitID, err)
			continue
		}
		// Abort the stages this process is running for it: the store has
		// already written the cancelled status, so the runner is only closing
		// the sessions that would otherwise keep going until they report an
		// outcome the store then refuses (store.ErrTaskCancelled).
		for _, nodeID := range nodeIDs {
			s.CancelTask(nodeID)
		}
		// Counted only when the graph had something left to drop: an earlier
		// recording that finished, or one a recording before that already
		// cancelled (its nodes are terminal, so CancelGraph returns none), was
		// not dropped by this dispatch and must not be reported as if it were.
		if len(nodeIDs) > 0 {
			cancelled++
		}
	}
	return cancelled
}
