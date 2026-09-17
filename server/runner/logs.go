package runner

import (
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"md-builder/server/store"
)

// Incremental task log persistence. SSH output streams into a LogWriter,
// which buffers and appends chunks to the task_logs table: at least one
// row per flushInterval, or earlier when the buffer reaches flushBytes.
// The frontend polls GET /api/tasks/{id}/log?after=<seq> for new chunks.

const (
	// flushBytes is the buffer size that triggers an early flush.
	flushBytes = 32 * 1024
	// flushInterval is the maximum time output waits before being stored.
	flushInterval = 2 * time.Second
	// maxLogBytes is the per-task log cap; output beyond it is dropped.
	maxLogBytes = 8 * 1024 * 1024
)

// LogWriter is an io.Writer that persists task output incrementally. It is
// safe for concurrent use (SSH multiplexes stdout and stderr into separate
// writers). Close flushes the remainder and stops the background timer.
// Output passes through RedactSecrets first: anything a command prints
// that contains the site's secrets (the access token, the MD_SECRET_TOKEN
// value) is replaced with REDACTED before it is stored.
type LogWriter struct {
	store  *store.Store
	taskID int64
	redact func(string) string // built once at construction; nil = nothing to scrub

	mu        sync.Mutex
	buf       []byte
	seq       int
	written   int64 // total bytes accepted (for the cap)
	truncat   bool
	timer     *time.Timer
	closed    bool
	lastFlush time.Time
}

// NewLogWriter returns a LogWriter appending to the task's log. A background
// timer flushes partial buffers every flushInterval until Close.
func NewLogWriter(s *store.Store, taskID int64) *LogWriter {
	lw := &LogWriter{store: s, taskID: taskID, lastFlush: time.Now()}
	lw.timer = time.AfterFunc(flushInterval, lw.tick)
	return lw
}

// SetSecrets scrubs the given secrets from everything written from now on.
// The site's access token and secret token belong here: a command echoing
// its environment (or a curl -v printing an Authorization header) would
// otherwise persist them in the task log. Empty strings are ignored.
// Call before the session's output starts streaming.
func (lw *LogWriter) SetSecrets(secrets ...string) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.redact = redactorFor(secrets)
}

// redactorFor builds the redaction function for the given secrets.
func redactorFor(secrets []string) func(string) string {
	vals := make([]string, 0, len(secrets))
	for _, s := range secrets {
		if s = strings.TrimSpace(s); s != "" {
			vals = append(vals, s)
		}
	}
	if len(vals) == 0 {
		return nil
	}
	return func(s string) string {
		for _, v := range vals {
			s = Redact(s, v)
		}
		return s
	}
}

// Write buffers p and flushes when the buffer is full. It never returns an
// error: log failures are logged but must not fail the SSH session.
func (lw *LogWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return len(p), nil // drop output after close (session teardown)
	}
	if lw.redact != nil {
		p = []byte(lw.redact(string(p)))
	}
	n := len(p)
	if lw.written+int64(n) > maxLogBytes {
		if !lw.truncat {
			lw.truncat = true
			lw.buf = append(lw.buf, []byte("\n… log truncated (exceeded 8MB) …\n")...)
		}
		lw.written += int64(n)
		if len(lw.buf) >= flushBytes {
			lw.flushLocked()
		}
		return n, nil
	}
	lw.written += int64(n)
	lw.buf = append(lw.buf, p...)
	if len(lw.buf) >= flushBytes {
		lw.flushLocked()
	}
	return n, nil
}

// Close flushes the remaining buffer and stops the background timer.
func (lw *LogWriter) Close() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	lw.closed = true
	if lw.timer != nil {
		lw.timer.Stop()
	}
	lw.flushLocked()
}

// Flush stores any buffered output immediately, without closing the writer
// (called before the task outcome is derived from the persisted log).
func (lw *LogWriter) Flush() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.flushLocked()
}

// flushLocked stores the buffered bytes as the next chunk. Callers hold mu.
func (lw *LogWriter) flushLocked() {
	if len(lw.buf) == 0 {
		lw.lastFlush = time.Now()
		return
	}
	content := string(lw.buf)
	lw.buf = lw.buf[:0]
	lw.seq++
	if err := lw.store.AppendTaskLog(lw.taskID, lw.seq, content); err != nil {
		// A failed chunk is dropped (with its sequence number) — the next
		// chunk still lands; log persistence must never kill a task.
		lw.seq--
		log.Printf("tasklog: append task %d seq %d: %v", lw.taskID, lw.seq+1, err)
		return
	}
	lw.lastFlush = time.Now()
}

// tick is the background flush; it re-arms until closed.
func (lw *LogWriter) tick() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	if time.Since(lw.lastFlush) >= flushInterval {
		lw.flushLocked()
	}
	lw.timer = time.AfterFunc(flushInterval, lw.tick)
}

// Compile-time interface checks.
var (
	_ io.Writer = (*LogWriter)(nil)
)
