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
// It returns the ids of the nodes it cancelled, in graph order, so the caller
// can abort the ones it is executing right now (the store cannot interrupt a
// stage; the runner can — see Service.CancelTask).
func (s *Store) CancelGraph(rootID int64, summary string) ([]int64, error) {
	if summary == "" {
		summary = CancelledSummary
	}
	now := time.Now()
	var cancelled []int64
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		nodes, err := listNodesTx(tx, rootID)
		if err != nil {
			return err
		}
		for i := range nodes {
			n := &nodes[i]
			// Retired nodes are history the current graph no longer defines,
			// and a terminal node has an outcome of its own: neither is ours
			// to overwrite.
			if n.Retired || TaskStatusTerminal(n.Status) {
				continue
			}
			if err := cancelAttemptTx(tx, n, summary, now); err != nil {
				return err
			}
			if err := tx.Model(&Task{}).Where("id = ?", n.ID).Updates(map[string]any{
				"status":      StatusCancelled,
				"summary":     summary,
				"finished_at": now,
			}).Error; err != nil {
				return err
			}
			cancelled = append(cancelled, n.ID)
		}
		return rollupTx(tx, rootID)
	})
	if err != nil {
		return nil, err
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
