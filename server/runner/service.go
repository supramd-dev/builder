// Package runner is the test-execution component of md-builder: it owns the
// task model (a root task with a small DAG of sub-tasks: clone → build →
// unit/regression), the scheduler that runs ready sub-tasks on remote
// environments over SSH, the server-side repository clone + upload, and the
// incremental task log store. The former sshcheck package (SSH transport)
// and worker package (scheduling) are absorbed here.
//
// The component's surface is Service: main wires it into the API server;
// handlers call DispatchForCommit (task creation), the store for queries,
// and Start for the scheduling pool.
package runner

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"md-builder/server/store"
)

// defaults for the scheduling pool.
const (
	defaultWorkers = 2
	pollInterval   = 2 * time.Second
)

// Service is the runner component: task dispatch (creation), scheduling
// (execution pool) and sub-task execution. Construct with NewService; call
// Start once from main to launch the scheduling pool.
type Service struct {
	Store     *store.Store
	SSH       Execer      // transport to the test environments
	Clone     RepoCloner  // server-side source acquisition
	FetchYAML YAMLFetcher // reads md-builder.yaml at a commit (tests inject)

	// Workers is the scheduling pool size (0 = defaultWorkers; the
	// MD_BUILDER_WORKERS env var overrides at NewService time).
	Workers int
}

// NewService returns a Service wired to the real SSH transport and git
// cloner, honoring MD_BUILDER_WORKERS.
func NewService(s *store.Store) *Service {
	workers := 0
	if v := os.Getenv("MD_BUILDER_WORKERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			workers = n
		} else {
			log.Printf("runner: invalid MD_BUILDER_WORKERS %q; using default", v)
		}
	}
	return &Service{
		Store:     s,
		SSH:       SSHExecer{},
		Clone:     SSHExecer{},
		FetchYAML: GitYAMLFetcher,
		Workers:   workers,
	}
}

// Start launches the scheduling pool: it resets stale running tasks (crash
// recovery) and polls for ready sub-tasks until ctx is cancelled.
func (s *Service) Start(ctx context.Context) {
	if n, err := s.Store.ResetStaleRunning(); err != nil {
		log.Printf("runner: reset stale running tasks: %v", err)
	} else if n > 0 {
		log.Printf("runner: reset %d stale running task(s) to pending", n)
	}

	n := s.Workers
	if n <= 0 {
		n = defaultWorkers
	}
	for i := 0; i < n; i++ {
		go s.loop(ctx)
	}
	log.Printf("runner: started %d worker(s)", n)
}

// loop is one scheduling goroutine: poll for a ready task, execute it,
// repeat (drain) until the queue is empty, then wait for the next tick.
func (s *Service) loop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				task, err := s.Store.ClaimReadyTask()
				if err != nil {
					log.Printf("runner: claim: %v", err)
					break
				}
				if task == nil {
					break // queue empty
				}
				s.runClaimed(ctx, task)
			}
		}
	}
}

// runClaimed executes one claimed sub-task and then updates the graph
// state: skipping blocked dependents on failure and refreshing the root.
func (s *Service) runClaimed(ctx context.Context, task *store.Task) {
	if err := s.ExecuteTask(ctx, task); err != nil {
		log.Printf("runner: task %d: execute: %v", task.ID, err)
	}

	// Re-read the terminal state the executor wrote.
	final, err := s.Store.GetTask(task.ID)
	if err != nil {
		log.Printf("runner: task %d: reread: %v", task.ID, err)
		return
	}
	if final.Status == store.TaskFailed {
		if err := s.Store.SkipDependents(final.RootID, final.ID,
			"skipped: upstream task "+final.Name+" failed"); err != nil {
			log.Printf("runner: task %d: skip dependents: %v", final.ID, err)
		}
		// A test stage that never ran still needs its dashboard cell.
		s.recordSkippedRuns(final)
	}
	if _, _, err := s.Store.RefreshRootStatus(final.RootID); err != nil {
		log.Printf("runner: root %d: refresh: %v", final.RootID, err)
	}
}

// recordSkippedRuns writes failed TestRun rows for skipped unit/regression
// sub-tasks so the dashboard shows ✗ instead of a blank cell.
func (s *Service) recordSkippedRuns(failed *store.Task) {
	subs, err := s.Store.ListSubTasks(failed.RootID)
	if err != nil {
		return
	}
	for i := range subs {
		sub := &subs[i]
		if sub.Kind != store.TaskKindUnit && sub.Kind != store.TaskKindRegression {
			continue
		}
		if sub.Status != store.TaskSkipped {
			continue
		}
		input := &store.RunInput{
			EnvironmentID: sub.EnvironmentID,
			CommitID:      sub.CommitID,
			Kind:          sub.Kind,
			Status:        store.StatusFailed,
			Summary:       "skipped: " + failed.Name + " failed: " + failed.Error,
		}
		if _, err := s.Store.UpsertTestRun(input); err != nil {
			log.Printf("runner: task %d: record skipped %s run: %v", sub.ID, sub.Kind, err)
		}
	}
}
