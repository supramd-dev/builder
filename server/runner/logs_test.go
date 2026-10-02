package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// logTask is the minimal task a LogWriter needs: its ID (the log's owner) and
// its current attempt (the run whose log this is). The log tests need no task
// rows — the log hangs off the run, not the task.
func logTask(id int64) *store.Task {
	return &store.Task{ID: id, Attempts: 1}
}

// The task ids the log tests use. They are far above the ids the other
// fixtures dispatch (the package's tests share one in-memory database), so a
// writer here never finds a run or a log it did not create.
const (
	partsTaskID     = 90001
	verbatimTaskID  = 90002
	resumedTaskID   = 90003
	noRunTaskID     = 90004
	seamTaskID      = 90005
	blindTaskID     = 90006
	noObjectsTaskID = 90007
	refuseTaskID    = 90008
	liveLogTaskID   = 90009
)

// createRun inserts the run row a log hangs off (the run is what holds the
// pointer to the log, and what the orphan sweep keeps it alive by), for tests
// that do not need a whole dispatched graph. The task row itself is not
// needed.
func createRun(t *testing.T, s *store.Store, taskID int64, attempt int) *store.TestRun {
	t.Helper()
	run := &store.TestRun{
		TaskID: taskID, Attempt: attempt,
		Kind: store.RunKindClone, Status: store.StatusRunning,
	}
	if err := s.DB.Create(run).Error; err != nil {
		t.Fatalf("create run for task %d: %v", taskID, err)
	}
	return run
}

// memoryObjects returns the fixture's in-memory artifact backend, for the
// tests that read the stored parts.
func memoryObjects(t *testing.T, s *store.Store) *storage.Memory {
	t.Helper()
	objs, ok := s.Objects().(*storage.Memory)
	if !ok {
		t.Fatalf("test store has no in-memory object backend: %T", s.Objects())
	}
	return objs
}

// getPart reads one stored part, failing the test when it is not there.
func getPart(t *testing.T, s *store.Store, run *store.TestRun, off int64) string {
	t.Helper()
	data, err := s.Objects().Get(context.Background(), storage.LogKey("", run.ID, off))
	if err != nil {
		t.Fatalf("read the part at %d: %v", off, err)
	}
	return string(data)
}

// TestLogWriterStoresPartsAsTheyFill: output goes to object storage one part
// per partBytes written, each named after the offset it starts at, and the run
// row follows — it points at the directory and counts the bytes stored so far.
// What is not yet a full part stays in memory, where a live reader finds it.
func TestLogWriterStoresPartsAsTheyFill(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, partsTaskID, 1)
	// A part smaller than one Write, so the boundaries are the writer's
	// arithmetic rather than the shape of the test's writes.
	lw := NewLogWriter(s, logTask(partsTaskID), LogLimits{PartBytes: 8})
	defer lw.Close()

	if _, err := lw.Write([]byte("aaaaaaaaaaaa")); err != nil { // 12 bytes: one part + 4 held
		t.Fatal(err)
	}
	objs := memoryObjects(t, s)
	if objs.Len() != 1 {
		t.Fatalf("%d objects after 12 bytes, want 1 part", objs.Len())
	}
	if got, want := getPart(t, s, run, 0), "aaaaaaaa"; got != want {
		t.Errorf("part at 0 = %q, want %q", got, want)
	}
	got, err := s.GetTestRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := storage.LogPrefix("", run.ID); got.LogPrefix != want {
		t.Errorf("run log prefix = %q, want %q", got.LogPrefix, want)
	}
	if got.LogBytes != 8 {
		t.Errorf("run log bytes = %d, want the 8 stored", got.LogBytes)
	}

	// The writer's own tail is the buffered part of the stream: the newest
	// bytes, which no reader has to reach object storage for.
	if got := lw.Tail(4); got != "aaaa" {
		t.Errorf("buffered tail = %q, want the 4 unwritten bytes", got)
	}

	// Four more bytes fill the buffer — the two it was still holding plus the
	// new ones — so a second part goes up at the offset the first ended at: no
	// gap, no overlap, nothing dropped.
	lw.Write([]byte("bbbb"))
	if objs.Len() != 2 {
		t.Fatalf("%d objects after the second fill, want 2", objs.Len())
	}
	if got, want := getPart(t, s, run, 8), "aaaabbbb"; got != want {
		t.Errorf("part at 8 = %q, want %q", got, want)
	}
	if got, _ := s.GetTestRun(run.ID); got.LogBytes != 16 {
		t.Errorf("run log bytes = %d, want 16", got.LogBytes)
	}

	lw.Close()
	if n := objs.Len(); n != 2 {
		t.Errorf("Close with an empty buffer stored %d objects, want the 2 parts", n)
	}
}

// TestLogWriterKeepsAShortLogVerbatim: under one part there is nothing to
// upload until the stage ends, and then the single part is the output itself —
// no marker, nothing reordered, nothing dropped.
func TestLogWriterKeepsAShortLogVerbatim(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, verbatimTaskID, 1)
	lw := NewLogWriter(s, logTask(verbatimTaskID), LogLimits{})

	var want strings.Builder
	for i := 0; i < 100; i++ {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		lw.Write([]byte(line))
	}
	lw.Close()

	objs := memoryObjects(t, s)
	if objs.Len() != 1 {
		t.Fatalf("%d objects for a log under one part, want 1", objs.Len())
	}
	if got := getPart(t, s, run, 0); got != want.String() {
		t.Errorf("stored log = %q, want the output verbatim", got)
	}
}

// TestLogWriterTailReadsAcrossTheSeam: the newest output is in memory, but a
// tail that reaches further back than the buffer holds is completed from the
// last stored part — the line a stage's outcome is read from may be in either.
func TestLogWriterTailReadsAcrossTheSeam(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, seamTaskID, 1)
	lw := NewLogWriter(s, logTask(seamTaskID), LogLimits{PartBytes: 8})
	defer lw.Close()

	lw.Write([]byte("0123456789abcdef")) // two parts, buffer empty
	lw.Write([]byte("XYZ"))              // 3 bytes buffered

	if got := lw.Tail(6); got != "defXYZ" {
		t.Errorf("tail = %q, want %q (the part's end and the buffer)", got, "defXYZ")
	}
	if got := lw.Tail(2); got != "YZ" {
		t.Errorf("short tail = %q, want the buffer alone", got)
	}
	// More than the log holds: the last part and the buffer are all there is to
	// read (the tail is bounded work, not a read of the whole log).
	if got := lw.Tail(1000); got != "89abcdefXYZ" {
		t.Errorf("whole tail = %q", got)
	}
	if got := lw.Tail(0); got != "" {
		t.Errorf("tail of nothing = %q", got)
	}
}

// TestLogWriterResumesAfterStoredParts: a stage whose process died keeps its
// attempt, so the next writer must continue the stream the dead one left —
// after the parts already stored, never over them — and say at the seam what
// happened to the output the dead process was holding.
func TestLogWriterResumesAfterStoredParts(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, resumedTaskID, 1)

	// What the dead process left: one part, and a run row pointing at it.
	ctx := context.Background()
	first := "the first run's output\n"
	if _, err := s.Objects().Put(ctx, storage.LogKey("", run.ID, 0), []byte(first)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetRunLogPrefix(run.ID, storage.LogPrefix("", run.ID), int64(len(first))); err != nil || !ok {
		t.Fatalf("record the run's log: ok=%t err=%v", ok, err)
	}

	lw := NewLogWriter(s, logTask(resumedTaskID), LogLimits{PartBytes: 4096})
	if !strings.Contains(lw.Tail(1<<10), "md-builder restarted") {
		t.Errorf("the resumed log must say where the seam is: %q", lw.Tail(1<<10))
	}
	lw.Write([]byte("the second run's output\n"))
	lw.Close()

	// The stored part is untouched...
	if got := getPart(t, s, run, 0); got != first {
		t.Errorf("part at 0 = %q, want the first run's output untouched", got)
	}
	// ...and the new output sits after it, at the offset the first ended at.
	second := getPart(t, s, run, int64(len(first)))
	if !strings.Contains(second, "md-builder restarted") || !strings.HasSuffix(second, "the second run's output\n") {
		t.Errorf("part at %d = %q", len(first), second)
	}
	if got, _ := s.GetTestRun(run.ID); got.LogBytes != int64(len(first)+len(second)) {
		t.Errorf("run log bytes = %d, want the two parts' %d", got.LogBytes, len(first)+len(second))
	}
}

// TestLogWriterWithoutARunStaysInMemory: a stage whose run row is gone has
// nothing to hang a log off. It still runs, and its output is not lost while
// the writer holds it.
func TestLogWriterWithoutARunStaysInMemory(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(noRunTaskID), LogLimits{PartBytes: 8})
	lw.Write([]byte("output with no run\n"))
	lw.Close() // must not panic or error

	if n := memoryObjects(t, s).Len(); n != 0 {
		t.Errorf("%d object(s) uploaded without a run", n)
	}
	if got := lw.Tail(1 << 10); got != "output with no run\n" {
		t.Errorf("the writer lost its own output: %q", got)
	}
}

// TestLogWriterWithoutObjectStorageStaysInMemory: object storage is mandatory
// in a deployment, but a store opened without one (the adduser path, most
// tests) still runs stages — and then memory is the whole copy.
func TestLogWriterWithoutObjectStorageStaysInMemory(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	createRun(t, s, noObjectsTaskID, 1)

	lw := NewLogWriter(s, logTask(noObjectsTaskID), LogLimits{PartBytes: 8})
	lw.Write([]byte("hello\n"))
	lw.Close()

	src := (&Service{Store: s}).OpenLogSource(logTask(noObjectsTaskID), 1)
	if end, err := src.End(context.Background()); err != nil || end != 0 {
		t.Errorf("a log with no backend reads as %d bytes (err %v), want none stored", end, err)
	}
	if got := lw.Tail(1 << 10); got != "hello\n" {
		t.Errorf("the writer lost its own output: %q", got)
	}
}

// failingObjects is an object backend whose uploads fail: what a stage faces
// while the object store is unreachable.
type failingObjects struct {
	*storage.Memory
	down bool
}

func (f *failingObjects) Put(ctx context.Context, key string, data []byte) (storage.ObjectMeta, error) {
	if f.down {
		return storage.ObjectMeta{}, errors.New("object storage is down")
	}
	return f.Memory.Put(ctx, key, data)
}

// TestLogWriterRefusesOutputPastTheBufferCeiling: while the parts cannot be
// stored, the buffer is the only copy, and a stage that logs without end must
// not take the server's memory with it. Past the ceiling new output is refused
// — and loudly — but nothing already accepted is dropped: the bytes that were
// kept go up in their offsets once the backend comes back, so a reader's
// cursor is still true.
func TestLogWriterRefusesOutputPastTheBufferCeiling(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(&failingObjects{Memory: storage.NewMemory(), down: true}))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	run := createRun(t, s, refuseTaskID, 1)

	lw := NewLogWriter(s, logTask(refuseTaskID), LogLimits{PartBytes: 8})
	defer lw.Close()

	// Ten 4-byte writes: the ceiling is maxBufferParts (4) × partBytes (8) = 32
	// bytes, so the last writes are refused rather than buffered.
	for i := 0; i < 10; i++ {
		if _, err := lw.Write([]byte("abcd")); err != nil {
			t.Fatal(err)
		}
	}
	kept := lw.Tail(1 << 10)
	if len(kept) != maxBufferParts*8 {
		t.Errorf("kept %d bytes, want the ceiling of %d", len(kept), maxBufferParts*8)
	}
	if strings.Trim(kept, "abcd") != "" {
		t.Errorf("the kept output is not the accepted bytes: %q", kept)
	}

	// The backend comes back: the held bytes are stored at the offsets they
	// belong at, and writing continues where it left off.
	objs := s.Objects().(*failingObjects)
	objs.down = false
	if _, err := lw.Write([]byte("efgh")); err != nil {
		t.Fatal(err)
	}
	for off := int64(0); off < int64(len(kept)); off += 8 {
		if got := getPartFrom(t, objs, run, off); got != kept[off:off+8] {
			t.Errorf("part at %d = %q, want the held bytes %q", off, got, kept[off:off+8])
		}
	}
	if got, _ := s.GetTestRun(run.ID); got.LogPrefix != storage.LogPrefix("", run.ID) || got.LogBytes != int64(len(kept)) {
		t.Errorf("run log after recovery = %q (%d bytes), want the held %d", got.LogPrefix, got.LogBytes, len(kept))
	}
}

// getPartFrom reads a part from a backend directly (the fast path above uses
// the store's own, which is not the failing double once uploads resume).
func getPartFrom(t *testing.T, objs storage.Store, run *store.TestRun, off int64) string {
	t.Helper()
	data, err := objs.Get(context.Background(), storage.LogKey("", run.ID, off))
	if err != nil {
		t.Fatalf("read the part at %d: %v", off, err)
	}
	return string(data)
}

// TestLogWriterResumesBlindWhenThePartsCannotBeListed: a listing that fails
// leaves the writer unable to read where the log ends. It must not assume
// zero — that would overwrite the beginning of a log that is already stored —
// so it continues from the byte count the run row recorded.
func TestLogWriterResumesBlindWhenThePartsCannotBeListed(t *testing.T) {
	objs := &blindObjects{Memory: storage.NewMemory()}
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(objs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	run := createRun(t, s, blindTaskID, 1)

	// A run with a stored log the listing cannot see: 100 bytes, says the row.
	if ok, err := s.SetRunLogPrefix(run.ID, storage.LogPrefix("", run.ID), 100); err != nil || !ok {
		t.Fatalf("record the run's log: ok=%t err=%v", ok, err)
	}
	objs.listFails = true

	lw := NewLogWriter(s, logTask(blindTaskID), LogLimits{PartBytes: 4096})
	if !strings.Contains(lw.Tail(1<<10), "md-builder restarted") {
		t.Error("the resumed log must say where the seam is")
	}
	lw.Write([]byte("after the blind resume\n"))
	lw.Close()

	// Everything lands after the byte the row recorded: nothing below it is
	// overwritten, which is the whole point of not assuming zero.
	if got := getPartFrom(t, objs, run, 100); !strings.HasSuffix(got, "after the blind resume\n") {
		t.Errorf("part at 100 = %q, want the output continued, not restarted", got)
	}
	if n := objs.Len(); n != 1 {
		t.Errorf("%d objects, want only the one written after the recorded end", n)
	}
	for _, off := range []int64{0, 50, 99} {
		if _, err := objs.Get(context.Background(), storage.LogKey("", run.ID, off)); !errors.Is(err, storage.ErrNotFound) {
			t.Errorf("a part was written at %d, below the recorded end", off)
		}
	}
}

// blindObjects is an object backend whose listings fail (a store that can
// write and read objects, but not enumerate them).
type blindObjects struct {
	*storage.Memory
	listFails bool
}

func (b *blindObjects) List(ctx context.Context, prefix string) ([]storage.ObjectMeta, error) {
	if b.listFails {
		return nil, errors.New("listing is down")
	}
	return b.Memory.List(ctx, prefix)
}

// TestLogWriterAfterCloseDrops: output that arrives while the session is being
// torn down must not be stored (the stage is over; its log is closed).
func TestLogWriterAfterCloseDrops(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, liveLogTaskID, 1)
	lw := NewLogWriter(s, logTask(liveLogTaskID), LogLimits{})
	lw.Write([]byte("before close\n"))
	lw.Close()

	lw.Write([]byte("after close\n"))
	lw.Close() // double close is a no-op

	src := (&Service{Store: s}).OpenLogSource(logTask(liveLogTaskID), 1)
	all := string(readAll(t, src))
	if strings.Contains(all, "after close") {
		t.Error("post-close output should be dropped")
	}
	if all != "before close\n" {
		t.Errorf("stored log = %q, want only the pre-close output", all)
	}
}

// TestLogWriterHandsOutTheLiveLog: while this process runs a stage, a reader
// reads its writer (the oldest bytes from the stored parts, the newest from the
// buffer), and the writer stops being offered once the stage has closed.
func TestLogWriterHandsOutTheLiveLog(t *testing.T) {
	s := openTestStore(t)
	svc := NewService(s)
	svc.LogLimits = LogLimits{PartBytes: 8}
	createRun(t, s, liveLogTaskID, 1)

	lw := svc.OpenLog(logTask(liveLogTaskID))
	lw.Write([]byte("step 1\nst"))
	lw.Flush()

	src := svc.OpenLogSource(logTask(liveLogTaskID), 1)
	if got := string(readAll(t, src)); got != "step 1\nst" {
		t.Errorf("live log = %q, want the parts and the buffer", got)
	}
	if end, err := src.End(context.Background()); err != nil || end != 9 {
		t.Errorf("live log end = %d (err %v), want 9", end, err)
	}

	// A task this process is not running has no writer to read: the source
	// falls back to what the run row points at.
	if other := svc.OpenLogSource(logTask(liveLogTaskID), 2); other.live != nil {
		t.Error("a live writer was offered for an attempt nobody is running")
	}

	// The stage ends: the writer is withdrawn, and the stored parts are all
	// that is left (the buffer was stored by Close).
	lw.Write([]byte("ep 2\n"))
	lw.Close()
	if after := svc.OpenLogSource(logTask(liveLogTaskID), 1); after.live != nil {
		t.Error("a closed writer is still offered as a live log")
	}
	if got := string(readAll(t, svc.OpenLogSource(logTask(liveLogTaskID), 1))); got != "step 1\nstep 2\n" {
		t.Errorf("log after the stage ended = %q", got)
	}
}

// storedLog reads a task's attempt's log back the way the API does: through a
// LogSource, out of the run's stored parts. It is what a test asserts on when
// it wants to know what a reader would be shown.
func storedLog(t *testing.T, s *store.Store, task *store.Task, attempt int) string {
	t.Helper()
	src := (&Service{Store: s}).OpenLogSource(task, attempt)
	return string(readAll(t, src))
}

// storeParts stores body as one part of the task's current attempt's log and
// points the run at it: what a previous process, or an older server, would have
// left behind.
func storeParts(t *testing.T, s *store.Store, task *store.Task, body string) *store.TestRun {
	t.Helper()
	run, err := s.FindTaskRun(task.ID, task.Attempts)
	if err != nil {
		t.Fatalf("find the run of task %d attempt %d: %v", task.ID, task.Attempts, err)
	}
	if s.Objects() == nil {
		t.Fatalf("store has no object backend to store the log of task %d in", task.ID)
	}
	if _, err := s.Objects().Put(context.Background(), storage.LogKey("", run.ID, 0), []byte(body)); err != nil {
		t.Fatalf("store the log of task %d: %v", task.ID, err)
	}
	if ok, err := s.SetRunLogPrefix(run.ID, storage.LogPrefix("", run.ID), int64(len(body))); err != nil || !ok {
		t.Fatalf("record the log of task %d: ok=%t err=%v", task.ID, ok, err)
	}
	return run
}

// openTestStore is a plain store fixture for the log tests (no task rows
// needed: the log hangs off the run, not the task).
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// readAll reads a source from the beginning to the end of its log, paging the
// way the API does.
func readAll(t *testing.T, src *LogSource) []byte {
	t.Helper()
	ctx := context.Background()
	end, err := src.End(ctx)
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	var out []byte
	for off := int64(0); off < end; {
		page, err := src.ReadFrom(ctx, off, src.PageBytes())
		if err != nil {
			t.Fatalf("read the log at %d: %v", off, err)
		}
		if len(page) == 0 {
			break
		}
		out = append(out, page...)
		off += int64(len(page))
	}
	return out
}

// TestLogWriterRedactsSecrets: after SetSecrets, everything written is
// scrubbed before it is buffered — a command echoing its environment must not
// persist the site's secrets in memory, in a part, or in an API response.
func TestLogWriterRedactsSecrets(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, 43, 1)
	lw := NewLogWriter(s, logTask(43), LogLimits{})
	lw.SetSecrets("glpat-tok", "s3cr't-value", "  ") // last one is blank: ignored

	out := "env: MD_SECRET_TOKEN=s3cr't-value\nauth: glpat-tok\nplain: untouched\n"
	if _, err := lw.Write([]byte(out)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(lw.Tail(1<<10), "glpat-tok") {
		t.Fatalf("secret leaked into the buffer: %q", lw.Tail(1<<10))
	}
	lw.Close()

	all := getPart(t, s, run, 0)
	if strings.Contains(all, "glpat-tok") || strings.Contains(all, "s3cr't-value") {
		t.Fatalf("secret leaked into a part: %q", all)
	}
	if !strings.Contains(all, "REDACTED") || !strings.Contains(all, "plain: untouched") {
		t.Fatalf("redaction misplaced: %q", all)
	}

	// Before SetSecrets, output passes through untouched (failEarly's
	// pre-redacted messages, plain clone output).
	createRun(t, s, 44, 1)
	lw2 := NewLogWriter(s, logTask(44), LogLimits{})
	lw2.Write([]byte("raw token glpat-tok here\n"))
	lw2.Close()
	if got := getPart(t, s, createRunRow(t, s, 44), 0); !strings.Contains(got, "glpat-tok") {
		t.Fatalf("unarmed writer should pass through: %q", got)
	}
}

// createRunRow reloads the run a task's attempt ran on.
func createRunRow(t *testing.T, s *store.Store, taskID int64) *store.TestRun {
	t.Helper()
	run, err := s.FindTaskRun(taskID, 1)
	if err != nil {
		t.Fatalf("find the run for task %d: %v", taskID, err)
	}
	return run
}

// TestStageOutputFallsBackToTheStoredParts: stageOutput is what a stage's
// outcome is read from, and it reads the writer's tail — which reaches into the
// stored parts when the buffer does not cover it.
func TestStageOutputFallsBackToTheStoredParts(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, seamTaskID, 1)
	svc := &Service{Store: s}
	task := logTask(seamTaskID)
	lw := svc.OpenLog(task)
	defer lw.Close()

	lw.Write([]byte("chatter\nMD-BUILDER-SUMMARY: 1 passed\n"))
	lw.Flush()
	// The buffer is emptied by the flush, so the outcome can only come from
	// the stored part.
	lw.Write([]byte("x"))

	if out := svc.stageOutput(task, lw); !strings.Contains(out, "MD-BUILDER-SUMMARY") {
		t.Errorf("stageOutput = %q, want the stored output", out)
	}
	if out := svc.stageOutput(task, nil); out != "" {
		t.Errorf("stageOutput without a writer = %q, want nothing", out)
	}
}

// TestLogWriterSurvivesAStoreThatLosesTheRun: the run row can go while a stage
// is finishing (a re-dispatch). Storing must then stop rather than write parts
// nothing references, and the stage must still finish.
func TestLogWriterSurvivesAStoreThatLosesTheRun(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, noRunTaskID, 1)
	lw := NewLogWriter(s, logTask(noRunTaskID), LogLimits{PartBytes: 8})

	// The first part stores; then the run is deleted under the writer.
	lw.Write([]byte("aaaaaaaa"))
	if n := memoryObjects(t, s).Len(); n != 1 {
		t.Fatalf("%d objects after the first fill, want 1", n)
	}
	if err := s.DB.Delete(&store.TestRun{}, run.ID).Error; err != nil {
		t.Fatal(err)
	}
	lw.Write([]byte("bbbbbbbb")) // stored before the writer learns the run is gone
	lw.Write([]byte("cccccccc")) // and after that the writer keeps it in memory
	lw.Close()                   // must not panic

	if n := memoryObjects(t, s).Len(); n != 2 {
		t.Errorf("%d objects, want the two parts it stored before it stopped", n)
	}
	// The bytes the writer kept are still readable in this process.
	if got := lw.Tail(1 << 10); got != "bbbbbbbbcccccccc" {
		t.Errorf("tail after the run vanished = %q", got)
	}
}
