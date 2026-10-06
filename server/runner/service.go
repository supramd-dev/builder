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
	"sync"
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

	// LogLimits bounds a stage's log: the part size, which is the memory a
	// running stage holds and what one object holds. The zero value is the
	// default; main sets it from the configuration's logs section.
	LogLimits LogLimits

	// live holds the writer of every log this process is still writing: its
	// buffer is the newest output of those stages, so a reader following one
	// sees it immediately and a download gets the whole log as it stands. It
	// is process-local: a deployment that runs its workers on another host has
	// nothing here, and readers there see the stored parts instead.
	liveMu sync.Mutex
	live   map[liveLogKey]*LogWriter

	// cancels holds the cancel func of every task this process is executing
	// right now, so the runner can abort a stage it is running when a site
	// policy drops its node (a newer dispatch of the same commit, see
	// CancelTask). It is process-local: a deployment whose workers run on
	// another process leaves the cancellation to the store's status, and the
	// stage stops at its next report instead.
	cancelMu sync.Mutex
	cancels  map[int64]context.CancelFunc
}

// liveLogKey identifies one stage's log: a task's attempt, for which only one
// writer runs at a time.
type liveLogKey struct {
	taskID  int64
	attempt int
}

// logWriter returns a log writer for a task's current attempt, with the
// Service's log limits. Every writer the Service builds is registered here,
// which is the one place they are built — a writer that is not registered
// would simply not be readable while its stage runs (see OpenLogSource).
func (s *Service) logWriter(task *store.Task) *LogWriter {
	lw := NewLogWriter(s.Store, task, s.LogLimits)
	s.liveMu.Lock()
	defer s.liveMu.Unlock()
	if s.live == nil {
		s.live = make(map[liveLogKey]*LogWriter)
	}
	key := liveLogKey{taskID: lw.taskID, attempt: lw.attempt}
	s.live[key] = lw
	lw.setCloseHook(func() {
		// Only if it is still ours: a re-dispatch may have started the next
		// attempt's writer for the same key while this one was closing.
		s.liveMu.Lock()
		defer s.liveMu.Unlock()
		if s.live[key] == lw {
			delete(s.live, key)
		}
	})
	return lw
}

// OpenLog starts the log writer for a task's current attempt and registers it
// as live: what the caller writes to it is the stage's output, and a reader can
// follow it (or download it) before the stage ends. The runner calls this once
// per stage; it is exported for the tests that need a stage which is still
// running.
func (s *Service) OpenLog(task *store.Task) *LogWriter {
	return s.logWriter(task)
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
	// A child context so this stage can be aborted on its own: cancelling it
	// closes the attempt's SSH session under whatever it is running (see
	// CancelTask). The parent's cancellation still propagates — a shutdown
	// stops every stage.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.trackCancel(task.ID, cancel)
	defer s.untrackCancel(task.ID)
	if err := s.ExecuteTask(ctx, task); err != nil {
		log.Printf("runner: task %d: execute: %v", task.ID, err)
	}
}

// trackCancel registers the cancel func of a task this process is executing, so
// CancelTask can reach it.
func (s *Service) trackCancel(taskID int64, cancel context.CancelFunc) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	if s.cancels == nil {
		s.cancels = make(map[int64]context.CancelFunc)
	}
	s.cancels[taskID] = cancel
}

// untrackCancel drops a task's registration once its stage is over. One attempt
// of a node runs at a time (only a pending node is claimed), so the entry being
// dropped is always this execution's own.
func (s *Service) untrackCancel(taskID int64) {
	s.cancelMu.Lock()
	defer s.cancelMu.Unlock()
	delete(s.cancels, taskID)
}

// CancelTask aborts the stage this process is running for a task, closing its
// SSH session (the attempt is logged as such and its node's status is the
// store's business, not the runner's). It reports whether a stage of this task
// was running here at all: a task that is queued, finished, or being executed
// by another process has nothing for this process to abort, and the store's
// cancellation is what ends it.
func (s *Service) CancelTask(taskID int64) bool {
	s.cancelMu.Lock()
	cancel := s.cancels[taskID]
	s.cancelMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}
