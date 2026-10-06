package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// CancelledSummary is what the store writes on a node and a run it cancels:
// the work was stopped from the outside by a repeated-commit policy (see
// CancelGraph), not by anything the stage itself did. It names the cause the
// way the superseded summary does — the run row, its log and its artifacts
// stay readable, so the dashboard can say what happened to that attempt.
const CancelledSummary = "cancelled by a newer dispatch of this commit"

// ErrTaskCancelled is what a report for cancelled work gets. It is an error
// rather than a silent success because the runner did run something: the log
// line is how the abandoned attempt is explained, and the store must not take
// the outcome — the run is already cancelled and its node is not coming back.
var ErrTaskCancelled = errors.New("store: the task was cancelled")

// CancelGraph cancels the unfinished work of one task graph: every node of it
// whose status is not terminal becomes cancelled, and the run of its in-flight
// attempt — pending or running — is closed as cancelled on the way, with
// CancelledSummary. A virtual container's own status follows from the rollup at
// the end, so the root ends up cancelled too.
//
// Only unfinished work is touched. A node that already reached a terminal state
// keeps its status, its counts, its log and its artifacts: cancelling a graph
// stops what is still running, it never rewrites what already ran.
//
// It is CancelSubtree of the graph's root: the whole graph is that subtree.
//
// It returns the ids of the nodes it cancelled, in graph order, so the caller
// can abort the ones it is executing right now (the store cannot interrupt a
// stage; the runner can — see Service.CancelTask).
func (s *Store) CancelGraph(rootID int64, summary string) ([]int64, error) {
	return s.CancelSubtree(rootID, summary)
}

// CancelSubtree cancels the unfinished work of one task and of everything below
// it — the way a run is stopped by hand, or a whole container's worth of work
// at once (cancelling the regression container drops every case in it that has
// not finished, running ones included). The target is part of its own subtree,
// so cancelling a leaf cancels that one stage; cancelling a root cancels the
// entire graph, which is CancelGraph.
//
// The nodes become cancelled together with the run of the attempt that was in
// flight, in one transaction, and the containers above them roll up to
// cancelled in the same one: a reader never sees a cancelled stage under a
// container that still reads running. Only unfinished work is touched — a node
// with a status of its own keeps it, its result and its log.
//
// The cancelled status is written with the summary the caller gives (empty
// means CancelledSummary, the repeated-commit policy's wording), so a hand
// cancellation can say who stopped it instead of naming a dispatch that did
// not happen.
//
// It returns the ids of the nodes it cancelled, in graph order — the
// containers it stopped with their children among them — so the caller can
// abort the ones it is executing right now (see Service.CancelSubtree, and
// Service.CancelTask for the abort itself).
func (s *Store) CancelSubtree(taskID int64, summary string) ([]int64, error) {
	if summary == "" {
		summary = CancelledSummary
	}
	now := time.Now()
	var cancelled []int64
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		target, err := getTaskTx(tx, taskID)
		if err != nil {
			return err
		}
		nodes, err := subtreeTx(tx, target)
		if err != nil {
			return err
		}
		dropped, err := cancelNodesTx(tx, nodes, summary, now)
		if err != nil {
			return err
		}
		// What stands behind a cancelled node is skipped in the same
		// transaction, and the containers roll up last: a reader never sees a
		// cancelled stage whose dependents are still queued, nor a graph left
		// waiting for a node that can never be claimed.
		if err := skipAfterCancelTx(tx, target.RootID, dropped); err != nil {
			return err
		}
		cancelled = make([]int64, len(dropped))
		for i := range dropped {
			cancelled[i] = dropped[i].ID
		}
		return rollupTx(tx, target.RootID)
	})
	if err != nil {
		return nil, err
	}
	return cancelled, nil
}

// skipAfterCancelTx marks the work behind the nodes a cancellation just dropped
// as skipped, each with the reason of the cancelled node that blocked it. It is
// the same cascade store.FinishAttempt runs for a node that ended without
// passing, and it is needed for the same reason: the queue only ever hands out a
// node whose dependencies all passed, so a node waiting on a cancelled one would
// never be claimed — it would sit at pending for ever, with the dashboard and
// the graph page following it.
//
// Nothing is skipped when the whole graph was cancelled: no pending node is left
// to strand. The container the cancelled nodes hang under rolls up afterwards
// (CancelSubtree), so a stage skipped here reads as its container reads it.
func skipAfterCancelTx(tx *gorm.DB, rootID int64, cancelled []Task) error {
	if len(cancelled) == 0 {
		return nil
	}
	origins := make(map[int64]string, len(cancelled))
	for i := range cancelled {
		origins[cancelled[i].ID] = FailureReason(&cancelled[i], AttemptResult{Status: StatusCancelled})
	}
	skipped, err := blockedTx(tx, rootID, origins)
	if err != nil {
		return err
	}
	return skipTasksTx(tx, skipped)
}

// subtreeTx returns the nodes a cancellation of target covers, in graph order:
// every node of its graph for a root (the root itself is derived by the rollup,
// as it always is), the target and its descendants for a container, and only
// the target for a leaf.
func subtreeTx(tx *gorm.DB, target *Task) ([]Task, error) {
	nodes, err := listNodesTx(tx, target.RootID)
	if err != nil {
		return nil, err
	}
	if target.Kind == TaskKindRoot {
		return nodes, nil
	}
	// The descendants of the target, walked through the tree it is the root of.
	kids := map[int64][]int{}
	for i := range nodes {
		kids[nodes[i].ParentID] = append(kids[nodes[i].ParentID], i)
	}
	out := []Task{*target}
	queue := []int64{target.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, i := range kids[id] {
			out = append(out, nodes[i])
			queue = append(queue, nodes[i].ID)
		}
	}
	return out, nil
}

// cancelNodesTx cancels the unfinished nodes of one list and returns them in the
// order given: a retired node is history the current graph no longer defines,
// and a terminal node has an outcome of its own, so neither is the caller's to
// overwrite.
func cancelNodesTx(tx *gorm.DB, nodes []Task, summary string, now time.Time) ([]Task, error) {
	var cancelled []Task
	for i := range nodes {
		n := &nodes[i]
		if n.Retired || TaskStatusTerminal(n.Status) {
			continue
		}
		if err := cancelAttemptTx(tx, n, summary, now); err != nil {
			return nil, err
		}
		if err := tx.Model(&Task{}).Where("id = ?", n.ID).Updates(map[string]any{
			"status":      StatusCancelled,
			"summary":     summary,
			"finished_at": now,
		}).Error; err != nil {
			return nil, err
		}
		n.Status = StatusCancelled
		n.Summary = summary
		cancelled = append(cancelled, *n)
	}
	return cancelled, nil
}

// cancelAttemptTx closes a node's in-flight attempt as cancelled. A node whose
// attempt is already terminal (or which never opened one — a virtual node) has
// nothing to close: the node's own status is still the caller's to set.
func cancelAttemptTx(tx *gorm.DB, task *Task, summary string, now time.Time) error {
	var run TestRun
	err := tx.Where("task_id = ? AND attempt = ?", task.ID, task.Attempts).First(&run).Error
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	if TaskStatusTerminal(run.Status) {
		return nil
	}
	duration := run.DurationMillis
	if !run.StartedAt.IsZero() {
		duration = float64(now.Sub(run.StartedAt).Milliseconds())
	}
	return tx.Model(&TestRun{}).Where("id = ?", run.ID).Updates(map[string]any{
		"status":      StatusCancelled,
		"summary":     summary,
		"duration_ms": duration,
		"finished_at": now,
	}).Error
}

// RootsByCommitIDs returns the root tasks of the given commit rows, oldest
// commit first. Commits that were never dispatched have no root and are simply
// absent from the result.
func (s *Store) RootsByCommitIDs(commitIDs []int64) ([]Task, error) {
	if len(commitIDs) == 0 {
		return nil, nil
	}
	var roots []Task
	err := s.DB.Where("kind = ? AND commit_id IN ?", TaskKindRoot, commitIDs).
		Order("commit_id ASC").Find(&roots).Error
	return roots, err
}
