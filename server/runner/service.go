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
	"io"
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

	// LogLimits bounds the log copies a stage leaves behind (the stored
	// chunks and the complete log object). The zero value is the default for
	// every field; main sets it from the configuration's logs section.
	LogLimits LogLimits

	// live holds the writer of every log this process is still writing, so a
	// download can be served a stage's output before the stage ends — the
	// only time a complete log exists nowhere else. It is process-local: a
	// deployment that runs its workers on another host has nothing here, and
	// the download falls back to the stored chunks.
	liveMu sync.Mutex
	live   map[liveLogKey]*LogWriter
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
// would simply not be offered to a download while its stage runs.
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
// as live: what the caller writes to it is the stage's output, and a download
// can read it before the stage ends. The runner calls this once per stage; it
// is exported for the tests that need a stage which is still running.
func (s *Service) OpenLog(task *store.Task) *LogWriter {
	return s.logWriter(task)
}

// LiveLog returns the output of a stage this process is still running, as a
// reader the caller closes (which removes the copy behind it) and its length.
// ok is false when no writer here is producing that log — a stage that has
// ended, one another process is running, or a task that does not exist.
//
// A reader that wants the log of a stage which is still writing has nowhere
// else to get all of it: the stored chunks keep its beginning and its end, and
// the full-log object is only written when the stage ends.
func (s *Service) LiveLog(taskID int64, attempt int) (io.ReadCloser, int64, bool) {
	s.liveMu.Lock()
	lw := s.live[liveLogKey{taskID: taskID, attempt: attempt}]
	s.liveMu.Unlock()
	if lw == nil {
		return nil, 0, false
	}
	return lw.SnapshotLog()
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
