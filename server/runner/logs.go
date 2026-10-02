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
//     read a page at a time through an indexed query. Past the cap it keeps
//     its beginning and a rolling window over its end (see chunkRef): a
//     reader that follows a stage must see the output it is writing now, and
//     a reader opening it later must still find where it ended.
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
	// defaultMaxLogBytes is the per-task cap on the stored chunks; past it the
	// beginning and the newest output are kept and the middle is given back as
	// new output arrives (it all stays in the full log).
	defaultMaxLogBytes = 8 * 1024 * 1024
	// storedHeadBytes is how much of the beginning the stored chunks hold onto
	// verbatim — the command line, the first error, the context a reader wants
	// before the noise. The rest of the cap rolls over the newest output. It
	// matches the spool's head region so both copies start the same way.
	storedHeadBytes = spoolHeadBytes
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
	// MaxStoredBytes caps the chunks kept in the database (0 = 8 MiB). Past
	// it the stored log keeps its beginning and its newest output and gives
	// the middle back: see LogWriter.
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

	// onClose runs once, after Close has finished with the writer: what the
	// Service uses to stop offering this writer's spool as a live log.
	onClose func()

	mu        sync.Mutex
	buf       []byte
	seq       int
	written   int64 // total bytes accepted from the session
	headBytes int64 // bytes of the stored copy held as the beginning
	stored    []chunkRef
	kept      int64 // bytes of the stored copy, the head region included
	truncat   bool
	timer     *time.Timer
	closed    bool
	lastFlush time.Time
}

// chunkRef is one stored chunk beyond the head region: what the writer has to
// know to give the cap back as new output arrives. The marker is never given
// back — it is the line that tells a reader the middle is missing, and it
// belongs right after the beginning, where the gap is.
type chunkRef struct {
	seq    int
	n      int64
	marker bool
}

// headBudget is how much of the stored copy is the protected beginning.
func (lw *LogWriter) headBudget() int64 {
	// Narrowed for a cap too small to hold a head region and a tail at once,
	// so that something always remains prunable and the copy stays bounded.
	if half := lw.maxStored / 2; half < storedHeadBytes {
		return half
	}
	return storedHeadBytes
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
	// the stored chunks keep their head and their end, and give up what is
	// between them once they run out of room.
	lw.spool.Write(p)

	n := len(p)
	lw.written += int64(n)
	if !lw.truncat && lw.written > lw.maxStored {
		// The stored copy has run out of room. What it holds between the
		// beginning and this point is the middle of a long log — the part a
		// reader can live without, and the part the marker is about to
		// announce — so it goes back now, and the marker takes its place:
		// the beginning, one line saying the middle is missing, then the
		// newest output from here on.
		lw.truncat = true
		lw.flushLocked()
		lw.dropStoredLocked(len(lw.stored))
		marker := fmt.Sprintf("\n… log truncated (exceeded %s); download the full log …\n", bytesLabel(lw.maxStored))
		lw.buf = append(lw.buf, marker...)
		if idx := lw.flushLocked(); idx >= 0 {
			lw.stored[idx].marker = true
		}
	}
	lw.buf = append(lw.buf, p...)
	if len(lw.buf) >= flushBytes {
		if lw.flushLocked() >= 0 {
			lw.pruneLocked()
		}
	}
	return n, nil
}

// setCloseHook registers a function to run once, after Close has finished.
// It is meant to be called right after construction, before the writer is
// shared.
func (lw *LogWriter) setCloseHook(fn func()) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.onClose = fn
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
	hook := lw.onClose
	lw.onClose = nil
	// Dropped under the lock, before the upload: a reader that asks for the
	// tail afterwards gets nothing rather than a file that is being removed.
	lw.spool = nil
	lw.mu.Unlock()

	// Outside the lock: the upload talks to object storage, and nothing will
	// write to the spool again anyway.
	lw.uploadFullLog(sp)
	if hook != nil {
		hook()
	}
}

// SnapshotLog copies the stage's output so far into a reader the caller closes
// (which removes the copy), with its length. It is what a download of a log
// that is still being written is served from: the complete output up to this
// moment, where the stored chunks hold only its beginning and its end.
//
// It reports false when there is nothing to serve — no spool (it never opened,
// or Close has taken it away), an empty log, or a failed copy.
func (lw *LogWriter) SnapshotLog() (io.ReadCloser, int64, bool) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	sp := lw.spool
	if sp == nil {
		return nil, 0, false
	}
	size := sp.Size()
	if size == 0 {
		return nil, 0, false
	}
	rc, n, err := sp.Snapshot()
	if err != nil {
		log.Printf("tasklog: task %d attempt %d: snapshot the live log: %v", lw.taskID, lw.attempt, err)
		return nil, 0, false
	}
	return rc, n, true
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
//
// It returns the chunk's index in the stored (prunable) list, or -1 when it
// stored nothing or the chunk belongs to the head region — which is never
// given back, so its caller has nothing to mark.
func (lw *LogWriter) flushLocked() int {
	if len(lw.buf) == 0 {
		lw.lastFlush = time.Now()
		return -1
	}
	content := string(lw.buf)
	n := int64(len(content))
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
		return -1
	}
	lw.lastFlush = time.Now()
	lw.kept += n
	if lw.headBytes < lw.headBudget() {
		lw.headBytes += n
		return -1
	}
	lw.stored = append(lw.stored, chunkRef{seq: lw.seq, n: n})
	return len(lw.stored) - 1
}

// pruneLocked gives the cap back: it drops the oldest stored chunks, never the
// head region and never the marker, until the stored copy fits again. What a
// reader loses is the middle of a log that ran past the cap — the newest
// output keeps arriving, which is the point. Callers hold mu.
//
// Removing a chunk is invisible to a reader: a poll asks for sequences past
// the last one it was given, and the sequences dropped here are always behind
// every cursor that has been served.
func (lw *LogWriter) pruneLocked() {
	var (
		victims []chunkRef
		freed   int64
	)
	// The loop measures what dropping the victims collected so far would
	// leave, not what is stored right now: kept only shrinks once the delete
	// has actually happened.
	for lw.kept-freed > lw.maxStored {
		idx := -1
		// The newest chunk always stays: it is where the writer's sequence
		// continues, and a reader must never be shown the log ending before
		// the output the stage has already written.
		for i := 0; i < len(lw.stored)-1; i++ {
			if !lw.stored[i].marker {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		victims = append(victims, lw.stored[idx])
		freed += lw.stored[idx].n
		lw.stored = append(lw.stored[:idx:idx], lw.stored[idx+1:]...)
	}
	lw.dropLocked(victims)
}

// dropStoredLocked gives back the first n stored chunks: the middle of a log
// that has just run out of room, which the marker takes the place of.
func (lw *LogWriter) dropStoredLocked(n int) {
	if n > len(lw.stored) {
		n = len(lw.stored)
	}
	victims := append([]chunkRef(nil), lw.stored[:n]...)
	lw.stored = append([]chunkRef(nil), lw.stored[n:]...)
	lw.dropLocked(victims)
}

// dropLocked deletes stored chunks from the database. The accounting follows
// the database, never the other way round: a failed delete puts the chunks
// back where they came from, so the next flush tries again and the cap is
// measured against what is really stored. Callers hold mu.
func (lw *LogWriter) dropLocked(victims []chunkRef) {
	if len(victims) == 0 {
		return
	}
	var freed int64
	seqs := make([]int, 0, len(victims))
	for _, v := range victims {
		seqs = append(seqs, v.seq)
		freed += v.n
	}
	if err := lw.store.DeleteTaskLogChunks(lw.taskID, lw.attempt, seqs); err != nil {
		lw.stored = append(append([]chunkRef(nil), victims...), lw.stored...)
		log.Printf("tasklog: give back %d chunk(s) of task %d attempt %d: %v", len(seqs), lw.taskID, lw.attempt, err)
		return
	}
	lw.kept -= freed
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
