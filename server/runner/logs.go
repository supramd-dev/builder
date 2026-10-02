package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// Task logs. SSH output streams into a LogWriter, and a stage's log lives in
// two places, neither of them the database:
//
//   - the output written since the last upload, in memory, bounded by
//     partBytes. A reader following a running stage is served from here (see
//     LogSource), so following a live log costs no object-storage round trips;
//   - one object per full buffer, under the run's log prefix
//     (<prefix>/runs/<runID>/log/part-<offset>): the parts. Each part carries
//     the byte offset it starts at, which is what lets a reader address any
//     byte of a very long log without reading the parts before it.
//
// The database keeps only the prefix and the byte count (store.TestRun.LogPrefix
// and .LogBytes) — what a reader and the orphan sweep need — so no row ever
// holds log text, and no log is truncated, capped or rewritten. The complete
// log is the parts in order, whenever it is read.
//
// A process that dies mid-stage loses only what its buffer held (less than one
// part). The retry keeps the attempt and its run, so the next writer continues
// after the parts already stored instead of overwriting them, and writes one
// line at the seam saying so (see restartNote).

const (
	// defaultPartBytes is how much output a running stage buffers in memory
	// before it uploads a part. It bounds the memory one stage costs, the
	// size of one object, and how far a reader in another process can lag
	// behind a running stage.
	defaultPartBytes = 8 * 1024 * 1024
	// maxBufferParts is how many parts' worth of output may pile up in memory
	// while the uploads keep failing: past it the writer refuses output (see
	// LogWriter.Write), because the alternative is a runaway stage taking the
	// server's memory with it.
	maxBufferParts = 4
	// logPutTimeout bounds one part upload, and one read of a part. Parts are
	// partBytes each, so they get more room than an artifact upload (putTimeout
	// in the store, tuned for 8 MiB files).
	logPutTimeout = 5 * time.Minute
)

// LogLimits bounds a stage's log, as configured under the `logs` section. The
// zero value is the documented default.
type LogLimits struct {
	// PartBytes is how much output a running stage holds in memory before it
	// is uploaded as one part (0 = 8 MiB): the memory a stage costs, the size
	// of one object, and how far a viewer in another process lags. A smaller
	// part is a fresher remote view at the price of more objects.
	PartBytes int64
}

// partBytes resolves the default.
func (l LogLimits) partBytes() int64 {
	if l.PartBytes > 0 {
		return l.PartBytes
	}
	return defaultPartBytes
}

// LogWriter is an io.Writer that stores a stage's output in parts. It is safe
// for concurrent use (SSH multiplexes stdout and stderr into separate writers).
// Close stores the remainder. Output passes through RedactSecrets first:
// anything a command prints that contains the site's secrets (the access
// token, the MD_SECRET_TOKEN value) is replaced with REDACTED before it is
// buffered, so no copy — memory, part, or the API's response — ever holds one.
type LogWriter struct {
	store   *store.Store
	taskID  int64
	attempt int
	runID   int64
	// prefix is where this writer's parts go. It is derived from the run id
	// rather than read from the run row: a run written by an older server
	// pointed at the single object it held then, and the first part stored
	// here re-points the row at the directory instead (see storedPrefix).
	prefix    string
	partBytes int64
	// objs is the backend the parts go to (nil when there is none — the unit
	// suite's store, or a run that no longer exists: the stage still runs and
	// its log stays in memory).
	objs   storage.Store
	redact func(string) string // built once at construction; nil = nothing to scrub

	// onClose runs once, after Close has finished with the writer: what the
	// Service uses to stop offering this writer as its task's live log.
	onClose func()

	mu    sync.Mutex
	buf   []byte          // output written since the last stored part
	parts []store.LogPart // the parts already stored, in stream order
	base  int64           // the end of the last stored part: where the next one starts
	// orphaned reports that the run this log belongs to is gone (a re-dispatch
	// replaced it while the stage was finishing): the bytes already stored are
	// left to the orphan sweep, and the rest of the stage's output stays in
	// memory rather than going into parts nothing references.
	orphaned bool
	// overflow reports that the buffer hit its ceiling because the parts are
	// not being stored. It is what keeps the complaint to one line per
	// episode rather than one per write.
	overflow bool
	closed   bool
	// storedPrefix/storedBytes are what the run row said its log was, when it
	// already had one: what this writer resumes after, and the fallback for
	// where the log ends when its parts cannot be listed.
	storedPrefix string
	storedBytes  int64
}

// NewLogWriter returns a LogWriter for a task's CURRENT attempt, so a stage
// that is retried after a crash (which keeps its attempt — see
// store.ResetStaleRunning) continues its log instead of overwriting it. A run
// that already has parts is resumed after them, with one line announcing the
// seam.
func NewLogWriter(s *store.Store, task *store.Task, limits LogLimits) *LogWriter {
	lw := &LogWriter{
		store:     s,
		taskID:    task.ID,
		attempt:   task.Attempts,
		partBytes: limits.partBytes(),
		objs:      s.Objects(),
	}
	if run, err := s.FindTaskRun(task.ID, task.Attempts); err == nil {
		lw.runID = run.ID
		lw.storedPrefix = run.LogPrefix
		lw.storedBytes = run.LogBytes
	}
	if lw.objs != nil {
		lw.prefix = storage.LogPrefix(lw.objs.KeyPrefix(), lw.runID)
	}
	lw.resume()
	return lw
}

// resume places the writer after the parts its attempt already has. A stage
// whose process died keeps its attempt (and its run) when it is retried, so
// this writer must continue the stream: the parts already stored are the log's
// beginning, and the buffer starts with a line that marks the join.
func (lw *LogWriter) resume() {
	if lw.objs == nil || lw.runID == 0 || lw.storedPrefix == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), logPutTimeout)
	defer cancel()
	parts, err := lw.store.ListRunLogParts(ctx, lw.storedPrefix)
	if err != nil {
		log.Printf("tasklog: task %d attempt %d: read the stored parts under %s: %v",
			lw.taskID, lw.attempt, lw.storedPrefix, err)
		// The parts cannot be listed, so where the log ends cannot be read
		// back. Continuing from the byte count the run row last recorded is
		// the one answer that cannot overwrite output already stored.
		lw.base = lw.storedBytes
		lw.buf = append(lw.buf, restartNote(lw.partBytes)...)
		return
	}
	if len(parts) == 0 {
		return
	}
	lw.parts = parts
	lw.base = parts[len(parts)-1].End()
	lw.buf = append(lw.buf, restartNote(lw.partBytes)...)
}

// restartNote is what a writer puts at the seam when it takes over a log that
// already has output: a reader has to know the halves are separate runs of the
// stage, and that whatever the dead process held in memory — less than one
// part — is gone for good.
func restartNote(partBytes int64) string {
	return fmt.Sprintf("\n… md-builder restarted while this stage was running; up to %s of its output was lost …\n",
		bytesLabel(partBytes))
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

// Write buffers p and stores a part once the buffer holds one. It never returns
// an error: log failures are logged but must not fail the SSH session.
//
// The buffer is what a live reader is served from until the part is stored, so
// storing it is not a tidiness: it is what bounds the memory a stage costs and
// what a reader in another process sees.
func (lw *LogWriter) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return len(p), nil // drop output after close (session teardown)
	}
	if lw.redact != nil {
		p = []byte(lw.redact(string(p)))
	}
	// Give the buffer its chance to drain before deciding whether there is
	// room: a backend that has come back is used again by this very write, so
	// a buffer that once reached its ceiling is not stuck holding it forever.
	lw.drainLocked()
	// The parts are not being stored (a backend that is down, or a store
	// without one) and the buffer is at its ceiling. Refusing the output is
	// the only thing left that keeps a runaway stage from taking the server's
	// memory with it — and it is never silent.
	if int64(len(lw.buf)) >= maxBufferParts*lw.partBytes {
		if !lw.overflow {
			lw.overflow = true
			log.Printf("tasklog: task %d attempt %d: refusing output past %s in memory: the log parts are not being stored",
				lw.taskID, lw.attempt, bytesLabel(maxBufferParts*lw.partBytes))
		}
		return len(p), nil
	}
	lw.buf = append(lw.buf, p...)
	lw.drainLocked()
	return len(p), nil
}

// drainLocked stores as many full parts as the buffer holds, and stops at the
// first upload that stores nothing: the bytes stay in the buffer (and the
// offset in the part's name is still where the stream is), so retrying the
// same bytes would spin. Callers hold mu.
func (lw *LogWriter) drainLocked() {
	for int64(len(lw.buf)) >= lw.partBytes {
		if !lw.uploadLocked(int(lw.partBytes)) {
			return
		}
	}
}

// setCloseHook registers a function to run once, after Close has finished.
// It is meant to be called right after construction, before the writer is
// shared.
func (lw *LogWriter) setCloseHook(fn func()) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.onClose = fn
}

// Close stores whatever is still buffered and stops the writer: once it
// returns, the parts hold the stage's whole log.
func (lw *LogWriter) Close() {
	lw.mu.Lock()
	if lw.closed {
		lw.mu.Unlock()
		return
	}
	lw.closed = true
	lw.uploadLocked(len(lw.buf))
	hook := lw.onClose
	lw.onClose = nil
	lw.mu.Unlock()
	if hook != nil {
		hook()
	}
}

// Flush stores what is buffered now, without closing the writer (called before
// the stage's outcome is derived, so the run that reports it describes a log
// that is already complete).
func (lw *LogWriter) Flush() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if lw.closed {
		return
	}
	lw.uploadLocked(len(lw.buf))
}

// Tail returns the last n bytes of the stage's output — what a reader that
// wants its outcome asks for (the summary line, the last error of a failed
// command). The newest output is in memory, so this answers without object
// storage whenever the buffer covers it; otherwise the last stored part
// supplies the rest (one part is as far back as it reads: the tail is bounded
// work, not a read of the whole log). It returns "" for a writer that has
// produced nothing.
func (lw *LogWriter) Tail(n int) string {
	lw.mu.Lock()
	if n <= 0 {
		lw.mu.Unlock()
		return ""
	}
	if len(lw.buf) >= n {
		out := string(lw.buf[len(lw.buf)-n:])
		lw.mu.Unlock()
		return out
	}
	buffered := string(lw.buf)
	var last store.LogPart
	havePart := len(lw.parts) > 0
	if havePart {
		last = lw.parts[len(lw.parts)-1]
	}
	objs := lw.objs
	lw.mu.Unlock()

	if !havePart || objs == nil {
		// Nothing but the buffer: either the log is shorter than one part, or
		// the parts are not being stored at all.
		return buffered
	}
	ctx, cancel := context.WithTimeout(context.Background(), logPutTimeout)
	defer cancel()
	data, err := objs.Get(ctx, last.Key)
	if err != nil {
		log.Printf("tasklog: task %d attempt %d: read the last log part %s: %v", lw.taskID, lw.attempt, last.Key, err)
		return buffered
	}
	need := n - len(buffered)
	if need >= len(data) {
		return string(data) + buffered
	}
	return string(data[len(data)-need:]) + buffered
}

// uploadLocked stores the first n bytes of the buffer as one part, and reports
// whether they left the buffer. Callers hold mu.
//
// A failed upload leaves the bytes where they are: the next attempt stores them
// together with whatever arrived since, and the offset in the part's name is
// still where the stream is. The run row follows the store — it is updated only
// once the object is in — so a reader never goes looking for a part that is not
// there.
func (lw *LogWriter) uploadLocked(n int) bool {
	if n <= 0 {
		return false
	}
	if lw.objs == nil || lw.runID == 0 || lw.orphaned {
		return false // no backend to store into, or nothing to store for: the log stays in memory
	}
	if n > len(lw.buf) {
		n = len(lw.buf)
	}
	key := storage.LogKey(lw.objs.KeyPrefix(), lw.runID, lw.base)
	ctx, cancel := context.WithTimeout(context.Background(), logPutTimeout)
	defer cancel()
	if _, err := lw.objs.Put(ctx, key, lw.buf[:n]); err != nil {
		log.Printf("tasklog: task %d attempt %d: store the log part at %d: %v", lw.taskID, lw.attempt, lw.base, err)
		return false
	}
	lw.parts = append(lw.parts, store.LogPart{Key: key, Start: lw.base, Size: int64(n)})
	lw.base += int64(n)
	lw.overflow = false
	// The buffer gives those bytes back: object storage keeps the log now, and
	// the writer's memory only has to hold what is not stored yet.
	copy(lw.buf, lw.buf[n:])
	lw.buf = lw.buf[:len(lw.buf)-n]

	ok, err := lw.store.SetRunLogPrefix(lw.runID, lw.prefix, lw.base)
	if err != nil {
		log.Printf("tasklog: task %d attempt %d: record the log prefix on run %d: %v",
			lw.taskID, lw.attempt, lw.runID, err)
		return true // the bytes are stored; only the run's pointer lagged
	}
	if !ok {
		log.Printf("tasklog: task %d attempt %d: run %d is gone; its log stays in memory",
			lw.taskID, lw.attempt, lw.runID)
		lw.orphaned = true
	}
	return true
}

// bytesLabel renders a byte count for a human reader (one decimal).
func bytesLabel(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(n)
	for _, u := range units {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", value)
}

// Compile-time interface checks.
var (
	_ io.Writer = (*LogWriter)(nil)
)
