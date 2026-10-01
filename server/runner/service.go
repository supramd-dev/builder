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
	"time"

	"md-builder/server/store"
)

// defaults for the scheduling pool.
const (
	defaultWorkers = 2
	pollInterval   = 2 * time.Second
)

// defaultFetchTimeout bounds the md-builder.yaml read when Service.FetchTimeout
// is unset: long enough for a slow repository server, short enough that a hung
// one fails instead of holding the dispatching goroutine.
const defaultFetchTimeout = 60 * time.Second

// Service is the runner component: task dispatch (creation), scheduling
// (execution pool) and sub-task execution. Construct with NewService; call
// Start once from main to launch the scheduling pool.
type Service struct {
	Store      *store.Store
	SSH        Execer                                                                                // transport to the test environments
	Clone      RepoCloner                                                                            // server-side source acquisition
	FetchYAML  YAMLFetcher                                                                           // reads md-builder.yaml at a commit (tests inject)
	ResolveRef func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) // ref → SHA for manual dispatch (tests inject)

	// Workers is the scheduling pool size (0 = defaultWorkers). main sets
	// it from the configuration's worker section.
	Workers int

	// FetchTimeout caps the md-builder.yaml read (0 = defaultFetchTimeout).
	FetchTimeout time.Duration
}

// fetchTimeout is the effective deadline for reading the test matrix.
func (s *Service) fetchTimeout() time.Duration {
	if s.FetchTimeout > 0 {
		return s.FetchTimeout
	}
	return defaultFetchTimeout
}

// resolveRef resolves the Service's ref resolver, defaulting to the real
// git ls-remote implementation.
func (s *Service) resolveRef(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
	if s.ResolveRef != nil {
		return s.ResolveRef(ctx, repoURL, ref, creds)
	}
	return ResolveRef(ctx, repoURL, ref, creds)
}

// NewService returns a Service wired to the real SSH transport, git cloner and
// yaml fetcher. Workers starts at 0, the default pool size; main sets it from
// the configuration.
func NewService(s *store.Store) *Service {
	return &Service{
		Store:     s,
		SSH:       SSHExecer{},
		Clone:     SSHExecer{},
		FetchYAML: NewYAMLFetcher(),
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

// runClaimed executes one claimed sub-task. The node's outcome and everything
// it implies are the store's business: the report (store.FinishAttempt) writes
// the attempt's run, the task's cache and the containers' rollup, and skips
// the nodes behind a task that did not pass — a dependent can never run once
// its dependency failed or was skipped. Every stage of a graph is a node here,
// so no stage can be left queued with no way forward.
func (s *Service) runClaimed(ctx context.Context, task *store.Task) {
	if err := s.ExecuteTask(ctx, task); err != nil {
		log.Printf("runner: task %d: execute: %v", task.ID, err)
	}
}
