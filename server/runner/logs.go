package runner

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// Incremental task log persistence. SSH output streams into a LogWriter,
// which keeps two copies of it:
//
//   - stored chunks in the task_logs table, appended at least once per
//     flushInterval (or earlier when the buffer reaches flushBytes). The
//     frontend polls GET /api/tasks/{id}/log?after=<seq> for new chunks, so
//     this copy stays small and bounded (MaxStoredBytes) — a live log is
//     read a page at a time through an indexed query.
//   - the complete log, spooled to a local file while the stage runs and
//     uploaded once, when it ends, as one object per run. That object is what
//     the download serves and what the run's summary is derived from, so a
//     stage whose output runs past the stored cap still reports, and hands
//     over, the output it actually ended with — including the failure at the
//     end of a very long log.

const (
	// flushBytes is the buffer size that triggers an early flush.
	flushBytes = 32 * 1024
	// flushInterval is the maximum time output waits before being stored.
	flushInterval = 2 * time.Second
	// defaultMaxLogBytes is the per-task cap on the stored chunks; output
	// beyond it is dropped from the database (it stays in the full log).
	defaultMaxLogBytes = 8 * 1024 * 1024
	// defaultMaxFileBytes is the per-run cap on the full log object.
	defaultMaxFileBytes = 64 * 1024 * 1024
	// logPutTimeout bounds the full-log upload. The payload is up to
	// MaxFileBytes, so it gets more room than an artifact upload (putTimeout
	// in the store, tuned for 8 MiB files).
	logPutTimeout = 5 * time.Minute
)

// fullLogName is the object name of a run's complete log, under the reserved
// ArtifactKindLog kind: <prefix>/runs/<runID>/log/full.log.
const fullLogName = "full.log"

// LogLimits bounds a stage's two log copies, as configured under the `logs`
// section. The zero value is the documented default for each field.
type LogLimits struct {
	// MaxStoredBytes caps the chunks kept in the database (0 = 8 MiB).
	MaxStoredBytes int64
	// MaxFileBytes caps the complete log kept in object storage (0 = 64
	// MiB). Its beginning and its end both survive a cap: see logSpool.
	MaxFileBytes int64
	// SpoolDir is where a stage's log is spooled while it runs ("" = the
	// OS temp directory).
	SpoolDir string
}

// SpoolDirectory is the directory spool files are written to, defaults
// resolved. The startup sweep (CleanStaleSpools) asks the same question, so
// it cleans up after the same directory.
func (l LogLimits) SpoolDirectory() string {
	if dir := strings.TrimSpace(l.SpoolDir); dir != "" {
		return dir
	}
	return os.TempDir()
}

// storedBytes / fileBytes resolve the caps' defaults.
func (l LogLimits) storedBytes() int64 {
	if l.MaxStoredBytes > 0 {
		return l.MaxStoredBytes
	}
	return defaultMaxLogBytes
}

func (l LogLimits) fileBytes() int64 {
	if l.MaxFileBytes > 0 {
		return l.MaxFileBytes
	}
	return defaultMaxFileBytes
}

// LogWriter is an io.Writer that persists task output incrementally and spools
// the complete log for object storage. It is safe for concurrent use (SSH
// multiplexes stdout and stderr into separate writers). Close flushes the
// remainder, stops the background timer and uploads the full log. Output
// passes through RedactSecrets first: anything a command prints that contains
// the site's secrets (the access token, the MD_SECRET_TOKEN value) is replaced
// with REDACTED before it is stored — in either copy.
type LogWriter struct {
	store   *store.Store
	taskID  int64
	attempt int
	runID   int64
	redact  func(string) string // built once at construction; nil = nothing to scrub

	// maxStored caps the stored chunks; spool holds the complete log.
	maxStored int64
	spool     *logSpool
	// ownLog reports whether this writer produced the attempt's log from
	// its first byte. It is what makes the full log trustworthy: a writer
	// that started on an attempt which already had stored chunks (a stage
	// resumed after a restart keeps its attempt and its output) holds only
	// part of the log, and uploading that part as "the full log" would be a
	// file that looks complete and is not. Such an attempt keeps the chunk
	// path, which is cumulative.
	ownLog bool

	mu        sync.Mutex
	buf       []byte
	seq       int
	written   int64 // total bytes accepted (for the stored-chunk cap)
	truncat   bool
	timer     *time.Timer
	closed    bool
	lastFlush time.Time
}

// NewLogWriter returns a LogWriter appending to a task's CURRENT attempt, so a
// retried task (or one re-executed after a restart, which keeps its attempt —
// see store.ResetStaleRunning) writes after the output already stored and
// never duplicates a sequence number. A background timer flushes partial
// buffers every flushInterval until Close.
//
// It also opens the spool for the run's complete log. A spool that cannot be
// created (an unwritable directory) is not an error: the stage runs, the
// stored chunks carry its output, and the run ends up with no full log.
func NewLogWriter(s *store.Store, task *store.Task, limits LogLimits) *LogWriter {
	lw := &LogWriter{
		store:     s,
		taskID:    task.ID,
		attempt:   task.Attempts,
		maxStored: limits.storedBytes(),
		lastFlush: time.Now(),
	}
	if run, err := s.FindTaskRun(task.ID, task.Attempts); err == nil {
		lw.runID = run.ID
	}
	if max, err := s.MaxTaskLogSeq(task.ID, task.Attempts); err == nil {
		lw.seq = max
		lw.ownLog = max == 0
	}
	if sp, err := newLogSpool(limits.SpoolDirectory(), limits.fileBytes()); err != nil {
		log.Printf("tasklog: task %d attempt %d: full log will not be spooled: %v", task.ID, task.Attempts, err)
	} else {
		lw.spool = sp
	}
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
	// The spool takes every byte (within its own cap, which keeps the end);
	// the stored chunks stop at theirs.
	lw.spool.Write(p)

	n := len(p)
	if lw.written+int64(n) > lw.maxStored {
		if !lw.truncat {
			lw.truncat = true
			marker := fmt.Sprintf("\n… log truncated (exceeded %s); download the full log …\n", bytesLabel(lw.maxStored))
			lw.buf = append(lw.buf, marker...)
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

// Close flushes the remaining buffer, stops the background timer and uploads
// the run's complete log.
func (lw *LogWriter) Close() {
	lw.mu.Lock()
	if lw.closed {
		lw.mu.Unlock()
		return
	}
	lw.closed = true
	if lw.timer != nil {
		lw.timer.Stop()
	}
	lw.flushLocked()
	sp := lw.spool
	// Dropped under the lock, before the upload: a reader that asks for the
	// tail afterwards gets nothing rather than a file that is being removed.
	lw.spool = nil
	lw.mu.Unlock()

	// Outside the lock: the upload talks to object storage, and nothing will
	// write to the spool again anyway.
	lw.uploadFullLog(sp)
}

// Flush stores any buffered output immediately, without closing the writer
// (called before the task outcome is derived from the log).
func (lw *LogWriter) Flush() {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.flushLocked()
}

// Tail returns the last n bytes of the stage's output — what a reader that
// wants its outcome asks for. It reads the spool, not the stored chunks, so a
// stage that ran past the stored cap still yields the output it ended with
// (the stored tail is then a truncation marker). It returns "" when no spool
// is available (it never was, or Close has already taken it away), and the
// caller falls back to the stored chunks.
func (lw *LogWriter) Tail(n int) string {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	return lw.spool.Tail(n)
}

// uploadFullLog stores the stage's complete output as its run's log object and
// records the key on the run. Every failure is logged and swallowed: the
// stored chunks are the log of record whenever this does not work, and a
// stage's outcome never depends on the network.
//
// It is always called with the spool the writer stopped using, and owns
// closing it (which removes the file).
func (lw *LogWriter) uploadFullLog(sp *logSpool) {
	if sp == nil {
		return
	}
	defer func() {
		if err := sp.Close(); err != nil {
			log.Printf("tasklog: task %d attempt %d: remove the log spool: %v", lw.taskID, lw.attempt, err)
		}
	}()
	if sp.broken() || sp.Size() == 0 || !lw.ownLog {
		return
	}
	objs := lw.store.Objects()
	if objs == nil {
		// Object storage is mandatory in a deployment, but a store opened
		// without one (the adduser path, most unit tests) still runs
		// stages — with the chunks as the only copy.
		return
	}
	runID := lw.runID
	if runID == 0 {
		// The run row is created with the attempt, which may have happened
		// after this writer was constructed.
		if run, err := lw.store.FindTaskRun(lw.taskID, lw.attempt); err == nil {
			runID = run.ID
		}
	}
	if runID == 0 {
		return
	}

	key := storage.ArtifactKey(objs.KeyPrefix(), runID, store.ArtifactKindLog, fullLogName, 0)
	size := sp.Size()
	ctx, cancel := context.WithTimeout(context.Background(), logPutTimeout)
	defer cancel()
	if _, err := objs.PutStream(ctx, key, sp.Reader(), size); err != nil {
		log.Printf("tasklog: task %d attempt %d: upload the full log: %v", lw.taskID, lw.attempt, err)
		return
	}
	ok, err := lw.store.SetRunLogObject(runID, key, size)
	if err != nil {
		log.Printf("tasklog: task %d attempt %d: record the full log on run %d: %v", lw.taskID, lw.attempt, runID, err)
		return
	}
	if !ok {
		// The run is gone (a re-dispatch replaced it while this stage was
		// finishing): nothing will ever reference the object, so drop it
		// now rather than leaving it to the sweep.
		if err := objs.Delete(ctx, key); err != nil {
			log.Printf("tasklog: task %d attempt %d: remove the unreferenced full log %s: %v", lw.taskID, lw.attempt, key, err)
		}
	}
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
	if err := lw.store.AppendTaskLog(&store.TaskLog{
		TaskID: lw.taskID, Attempt: lw.attempt, RunID: lw.runID,
		Seq: lw.seq, Content: content,
	}); err != nil {
		// A failed chunk is dropped (with its sequence number) — the next
		// chunk still lands; log persistence must never kill a task.
		lw.seq--
		log.Printf("tasklog: append task %d attempt %d seq %d: %v", lw.taskID, lw.attempt, lw.seq+1, err)
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
