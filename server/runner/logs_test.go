package runner

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
)

func TestLogWriterChunksBySize(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(42), LogLimits{})

	// One big write (≥ flushBytes) flushes immediately.
	big := strings.Repeat("a", flushBytes+10)
	if n, err := lw.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("write: %d %v", n, err)
	}
	logs, err := s.ReadTaskLogs(42, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 chunk after big write, got %d", len(logs))
	}
	if logs[0].Content != big {
		t.Errorf("chunk content truncated: %d bytes", len(logs[0].Content))
	}

	// A small write stays buffered until Close.
	small := "hello\n"
	lw.Write([]byte(small))
	logs, _ = s.ReadTaskLogs(42, 1, 0)
	if len(logs) != 1 {
		t.Fatalf("small write should stay buffered, got %d chunks", len(logs))
	}
	lw.Close()
	logs, _ = s.ReadTaskLogs(42, 1, 0)
	if len(logs) != 2 {
		t.Fatalf("Close should flush the remainder, got %d chunks", len(logs))
	}
	if logs[1].Content != small {
		t.Errorf("second chunk wrong: %q", logs[1].Content)
	}
	// Sequence numbers are contiguous from 1.
	if logs[0].Seq != 1 || logs[1].Seq != 2 {
		t.Errorf("seq wrong: %d, %d", logs[0].Seq, logs[1].Seq)
	}
}

func TestLogWriterCapsHugeOutput(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(7), LogLimits{})

	// Far more than defaultMaxLogBytes: the beginning and the newest output
	// are kept, the middle is given back as new output arrives. Every 1 MiB
	// write ends in a tag naming it, so the test can say which parts survived.
	// The extra 9 MiB past the cap refill the window after the crossing and
	// then make it roll, which is the steady state of a stage that keeps
	// writing.
	const writes = defaultMaxLogBytes/(1<<20) + 9
	for i := 0; i < writes; i++ {
		payload := strings.Repeat("x", 1<<20-len(fmt.Sprintf("END-%03d\n", i))) + fmt.Sprintf("END-%03d\n", i)
		if _, err := lw.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	lw.Close()

	logs, _ := s.ReadTaskLogs(7, 1, 0)
	var (
		total int
		all   strings.Builder
	)
	for _, l := range logs {
		total += len(l.Content)
		all.WriteString(l.Content)
	}
	stored := all.String()

	// The copy stays bounded — and by the whole cap, not by some fraction of
	// it: the head region and the window over the end share it.
	if total > defaultMaxLogBytes {
		t.Errorf("stored log not capped: %d bytes", total)
	}
	if total < defaultMaxLogBytes/2 {
		t.Errorf("stored log gave back too much: %d bytes", total)
	}
	// The stage's first output and its last are both there...
	if !strings.HasPrefix(stored, "xxxx") || !strings.Contains(stored, "END-000\n") {
		t.Error("the stored log lost its beginning")
	}
	if !strings.Contains(stored, fmt.Sprintf("END-%03d\n", writes-1)) {
		t.Error("the stored log lost the newest output")
	}
	// ...the middle is not, and one marker says so...
	if strings.Contains(stored, "END-003\n") {
		t.Error("the stored log kept output the cap should have given back")
	}
	if n := strings.Count(stored, "log truncated"); n != 1 {
		t.Errorf("stored log carries the truncation marker %d times, want 1", n)
	}
	// ...and the marker sits where the gap is, right after the beginning.
	if head := strings.Index(stored, "log truncated"); head > storedHeadBytes+flushBytes {
		t.Errorf("truncation marker at %d, want it at the end of the head region (%d)", head, storedHeadBytes)
	}

	// Sequence numbers stay monotonic and keep rising as the window rolls, so
	// a reader following the log is never handed a sequence twice.
	for i := 1; i < len(logs); i++ {
		if logs[i].Seq <= logs[i-1].Seq {
			t.Fatalf("sequence went backwards at %d: %d after %d", i, logs[i].Seq, logs[i-1].Seq)
		}
	}
	if logs[0].Seq != 1 {
		t.Errorf("first stored chunk is seq %d, want the log's first chunk (1)", logs[0].Seq)
	}
}

// TestLogWriterFollowsPastTheCap is the property a live view depends on: a
// reader that follows a stage keeps being handed its new output after the log
// runs past the stored cap. Before the window rolled, the chunk stream stopped
// at the cap, so the viewer's cursor never advanced again and the follow died
// on the truncation marker — with the stage's last words, the ones that say
// how it failed, never arriving.
func TestLogWriterFollowsPastTheCap(t *testing.T) {
	s := openTestStore(t)
	limits := LogLimits{MaxStoredBytes: 8 * 1024, SpoolDir: t.TempDir()}
	lw := NewLogWriter(s, logTask(followTaskID), limits)

	// A reader polls the way the viewer does: after=<last seq it was given>.
	cursor := 0
	read := func() string {
		t.Helper()
		logs, err := s.ReadTaskLogs(followTaskID, 1, cursor)
		if err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		for _, l := range logs {
			out.WriteString(l.Content)
			cursor = l.Seq
		}
		return out.String()
	}

	// Fill the cap and then some, reading every so often.
	for i := 0; i < 40; i++ {
		lw.Write([]byte(strings.Repeat("chatter\n", 200)))
		read()
	}

	// The reader is caught up: the next output must reach it.
	lw.Write([]byte("line after the cap\n"))
	lw.Flush()
	got := read()
	if !strings.Contains(got, "line after the cap") {
		t.Fatalf("a reader past the cap was not handed the newest output: %q", got)
	}

	// And it keeps arriving, one line at a time, with no repeats.
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		line := fmt.Sprintf("tick %d\n", i)
		lw.Write([]byte(line))
		lw.Flush()
		got := read()
		if got != line {
			t.Fatalf("read after tick %d = %q, want exactly %q", i, got, line)
		}
		if seen[got] {
			t.Fatalf("line %q delivered twice", got)
		}
		seen[got] = true
	}
	lw.Close()

	// What the reader rendered is what a page opened now would show: the
	// beginning, the marker, and the newest output.
	full, err := s.ReadTaskLogs(followTaskID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored strings.Builder
	for _, l := range full {
		stored.WriteString(l.Content)
	}
	if !strings.Contains(stored.String(), "tick 19") {
		t.Error("stored log lost the newest output")
	}
	if !strings.Contains(stored.String(), "log truncated") {
		t.Error("stored log lost the marker")
	}
}

// TestLogWriterHandsOutTheLiveLog: a download of a stage that is still running
// reads the writer's spool — the only complete copy at that moment — and stops
// finding one once the stage has closed its writer.
func TestLogWriterHandsOutTheLiveLog(t *testing.T) {
	s := openTestStore(t)
	svc := NewService(s)
	svc.LogLimits = LogLimits{MaxStoredBytes: 8 * 1024, SpoolDir: t.TempDir()}
	lw := svc.OpenLog(logTask(liveLogTaskID))

	lw.Write([]byte("step 1\nstep 2\n"))
	lw.Flush()

	rc, size, ok := svc.LiveLog(liveLogTaskID, 1)
	if !ok {
		t.Fatal("no live log for a stage this process is running")
	}
	body, err := io.ReadAll(rc)
	name := rc.Close() // closing removes the snapshot behind it
	if err != nil {
		t.Fatalf("read the live log: %v", err)
	}
	if name != nil {
		t.Errorf("closing the live log: %v", name)
	}
	if string(body) != "step 1\nstep 2\n" {
		t.Errorf("live log = %q", body)
	}
	if int64(len(body)) != size {
		t.Errorf("live log size = %d, want the %d bytes it served", size, len(body))
	}

	// A task this process is not running has no live log.
	if _, _, ok := svc.LiveLog(liveLogTaskID, 2); ok {
		t.Error("a live log was offered for an attempt nobody is running")
	}
	if _, _, ok := svc.LiveLog(followTaskID, 1); ok {
		t.Error("a live log was offered for a task nobody is running")
	}

	// Once the stage ends, the object takes over: the live log is gone.
	lw.Close()
	if _, _, ok := svc.LiveLog(liveLogTaskID, 1); ok {
		t.Error("a closed writer is still offered as a live log")
	}
}

func TestLogWriterAfterCloseDrops(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(9), LogLimits{})
	lw.Write([]byte("before close\n"))
	lw.Close()

	// Output after close (session teardown races) is dropped, not persisted.
	lw.Write([]byte("after close\n"))
	lw.Close() // double close is a no-op

	logs, _ := s.ReadTaskLogs(9, 1, 0)
	var all string
	for _, l := range logs {
		all += l.Content
	}
	if strings.Contains(all, "after close") {
		t.Error("post-close output should be dropped")
	}
	if !strings.Contains(all, "before close") {
		t.Errorf("pre-close output missing: %q", all)
	}
}

// logTask is the minimal task a LogWriter needs: its ID (the log's owner) and
// its current attempt (the sequence it appends to). The log tests need no task
// rows — task_logs is keyed by task ID, not by a foreign key.
func logTask(id int64) *store.Task {
	return &store.Task{ID: id, Attempts: 1}
}

// The task ids the full-log tests use. They are far above the ids the other
// fixtures dispatch (the package's tests share one in-memory database), so a
// writer here never finds a run or a log it did not create.
const (
	fullLogTaskID   = 90001
	verbatimTaskID  = 90002
	resumedTaskID   = 90003
	noRunTaskID     = 90004
	tailTaskID      = 90005
	fallbackTaskID  = 90006
	noObjectsTaskID = 90007
	followTaskID    = 90008
	liveLogTaskID   = 90009
)

// createRun inserts the run row a full log hangs off (its object key lives on
// the run), for tests that do not need a whole dispatched graph. Attempt is
// the run's attempt number; the task row itself is not needed.
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
// tests that count or read objects.
func memoryObjects(t *testing.T, s *store.Store) *storage.Memory {
	t.Helper()
	objs, ok := s.Objects().(*storage.Memory)
	if !ok {
		t.Fatalf("test store has no in-memory object backend: %T", s.Objects())
	}
	return objs
}

// TestLogWriterUploadsTheCompleteLog: when a stage ends, its whole output is
// one object per run, recorded on the run row, and the spool file is gone.
func TestLogWriterUploadsTheCompleteLog(t *testing.T) {
	s := openTestStore(t)
	dir := t.TempDir()
	run := createRun(t, s, fullLogTaskID, 1)

	// More than the spool cap (8 KiB: 4 KiB of head + a 4 KiB ring), so the
	// object is the head, a marker, and the end — the shape a capped log
	// takes.
	lw := NewLogWriter(s, logTask(fullLogTaskID), LogLimits{MaxFileBytes: 8 * 1024, SpoolDir: dir})
	for i := 0; i < 4; i++ {
		lw.Write([]byte(strings.Repeat("chatter\n", 512))) // 4 KiB per write
	}
	lastLine := "error: the thing that mattered\n"
	lw.Write([]byte(lastLine))
	lw.Close()

	got, err := s.FindTaskRun(fullLogTaskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	wantKey := storage.ArtifactKey("", run.ID, store.ArtifactKindLog, fullLogName, 0)
	if got.LogObjectKey != wantKey {
		t.Fatalf("run log key = %q, want %q", got.LogObjectKey, wantKey)
	}
	data, err := memoryObjects(t, s).Get(context.Background(), wantKey)
	if err != nil {
		t.Fatalf("read the full log: %v", err)
	}
	if got.LogBytes != int64(len(data)) {
		t.Errorf("run log bytes = %d, object size = %d", got.LogBytes, len(data))
	}
	if !strings.HasSuffix(string(data), lastLine) {
		t.Errorf("the full log does not end in the stage's last output: %q", data[max(0, len(data)-60):])
	}
	if !strings.Contains(string(data), "dropped") {
		t.Errorf("a capped full log must say what it dropped: %q", data[:min(80, len(data))])
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("the spool file was not removed: %v (err %v)", entries, err)
	}
}

// TestLogWriterKeepsAShortLogVerbatim: under the cap the object is the log
// itself — no marker, nothing reordered.
func TestLogWriterKeepsAShortLogVerbatim(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, verbatimTaskID, 1)
	lw := NewLogWriter(s, logTask(verbatimTaskID), LogLimits{SpoolDir: t.TempDir()})
	var want strings.Builder
	for i := 0; i < 100; i++ {
		line := fmt.Sprintf("line %d\n", i)
		want.WriteString(line)
		lw.Write([]byte(line))
	}
	lw.Close()

	got, err := s.FindTaskRun(verbatimTaskID, 1)
	if err != nil {
		t.Fatal(err)
	}
	data, err := memoryObjects(t, s).Get(context.Background(), got.LogObjectKey)
	if err != nil {
		t.Fatalf("read the full log: %v", err)
	}
	if string(data) != want.String() {
		t.Errorf("full log = %q, want the output verbatim", data)
	}
}

// TestLogWriterResumedAttemptKeepsTheChunkPath: a task re-executed after a
// restart keeps its attempt, and the writer then sees only part of that
// attempt's output. Uploading that part as "the full log" would be a file that
// looks complete and is not, so those attempts stay on the stored chunks.
func TestLogWriterResumedAttemptKeepsTheChunkPath(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, resumedTaskID, 1)
	if err := s.AppendTaskLog(&store.TaskLog{
		TaskID: resumedTaskID, Attempt: 1, RunID: run.ID, Seq: 1,
		Content: "the first run's output\n",
	}); err != nil {
		t.Fatal(err)
	}

	lw := NewLogWriter(s, logTask(resumedTaskID), LogLimits{SpoolDir: t.TempDir()})
	lw.Write([]byte("the second run's output\n"))
	lw.Close()

	if n := memoryObjects(t, s).Len(); n != 0 {
		t.Errorf("%d object(s) uploaded for a partially seen attempt", n)
	}
	got, _ := s.FindTaskRun(resumedTaskID, 1)
	if got.LogObjectKey != "" {
		t.Errorf("run log key = %q, want none", got.LogObjectKey)
	}
	// Both runs' output is in the chunks, which is what the download then
	// serves: nothing is lost, it is only not the "complete log" object.
	logs, err := s.ReadTaskLogs(resumedTaskID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, l := range logs {
		all.WriteString(l.Content)
	}
	if !strings.Contains(all.String(), "first run's output") || !strings.Contains(all.String(), "second run's output") {
		t.Errorf("stored log = %q", all.String())
	}
}

// TestLogWriterWithoutARunSkipsTheFullLog: the object needs a run to hang off.
// A stage whose run row is missing keeps its chunks and nothing else.
func TestLogWriterWithoutARunSkipsTheFullLog(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(noRunTaskID), LogLimits{SpoolDir: t.TempDir()})
	lw.Write([]byte("output with no run\n"))
	lw.Close() // must not panic or error

	if n := memoryObjects(t, s).Len(); n != 0 {
		t.Errorf("%d object(s) uploaded without a run", n)
	}
	logs, err := s.ReadTaskLogs(noRunTaskID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Content != "output with no run\n" {
		t.Errorf("stored chunks = %+v", logs)
	}
}

// TestLogWriterWithoutObjectStorageStillStoresChunks: object storage is
// mandatory in a deployment, but a store opened without one (the adduser path,
// most tests) still runs stages — with the chunks as the only copy.
func TestLogWriterWithoutObjectStorageStillStoresChunks(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	createRun(t, s, noObjectsTaskID, 1)

	lw := NewLogWriter(s, logTask(noObjectsTaskID), LogLimits{SpoolDir: t.TempDir()})
	lw.Write([]byte("hello\n"))
	lw.Close()

	logs, err := s.ReadTaskLogs(noObjectsTaskID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Content != "hello\n" {
		t.Errorf("stored chunks = %+v", logs)
	}
}

// TestLogWriterTailSurvivesTheStoredCap pins why the summary is derived from
// the writer's spool: past the stored cap the stored log ends in a truncation
// marker, so the line a stage's outcome is read from is in no chunk at all.
func TestLogWriterTailSurvivesTheStoredCap(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, tailTaskID, 1)
	limits := LogLimits{MaxStoredBytes: 8 * 1024, SpoolDir: t.TempDir()}
	lw := NewLogWriter(s, logTask(tailTaskID), limits)

	for i := 0; i < 8; i++ {
		lw.Write([]byte(strings.Repeat("chatter\n", 600)))
	}
	lw.Write([]byte("MD-BUILDER-SUMMARY: 3 failed\n"))
	lw.Flush()

	if out := lw.Tail(logSummaryTailBytes); !strings.Contains(out, "MD-BUILDER-SUMMARY") {
		t.Errorf("spool tail = %q, want the stage's last output", out)
	}
	logs, err := s.ReadTaskLogs(tailTaskID, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored strings.Builder
	for _, l := range logs {
		stored.WriteString(l.Content)
	}
	// Both copies keep the end: the viewer and the download read the stored
	// chunks, so a reader that opens a capped log must find what the stage
	// ended with there too — not only in the spool.
	if !strings.Contains(stored.String(), "MD-BUILDER-SUMMARY") {
		t.Error("the stored chunks lost the stage's last output")
	}
	if !strings.Contains(stored.String(), "log truncated") {
		t.Errorf("stored log = %q, want the truncation marker", stored.String())
	}
	lw.Close()
}

// TestStageOutputFallsBackToTheStoredChunks: with no spool there is nothing to
// read but the chunks ("stageOutput" is what the stage paths call).
func TestStageOutputFallsBackToTheStoredChunks(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, fallbackTaskID, 1)
	if err := s.AppendTaskLog(&store.TaskLog{
		TaskID: fallbackTaskID, Attempt: 1, Seq: 1, Content: "MD-BUILDER-SUMMARY: 1 passed\n",
	}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: s}
	task := logTask(fallbackTaskID)
	// A writer whose spool could not be created (an unwritable directory).
	lw := NewLogWriter(s, task, LogLimits{SpoolDir: filepath.Join(t.TempDir(), "missing")})
	defer lw.Close()

	if out := svc.stageOutput(task, lw); !strings.Contains(out, "MD-BUILDER-SUMMARY") {
		t.Errorf("stageOutput = %q, want the stored chunks", out)
	}
}

// openTestStore is a plain store fixture for the log tests (no task rows
// needed: task_logs is keyed by task ID only).
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestLogWriterRedactsSecrets: after SetSecrets, everything written is
// scrubbed before it reaches the task log — a command echoing its
// environment must not persist the site's secrets.
func TestLogWriterRedactsSecrets(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, logTask(43), LogLimits{})
	lw.SetSecrets("glpat-tok", "s3cr't-value", "  ") // last one is blank: ignored

	out := "env: MD_SECRET_TOKEN=s3cr't-value\nauth: glpat-tok\nplain: untouched\n"
	if _, err := lw.Write([]byte(out)); err != nil {
		t.Fatal(err)
	}
	lw.Close()

	logs, err := s.ReadTaskLogs(43, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	all := ""
	for _, l := range logs {
		all += l.Content
	}
	if strings.Contains(all, "glpat-tok") || strings.Contains(all, "s3cr't-value") {
		t.Fatalf("secret leaked into log: %q", all)
	}
	if !strings.Contains(all, "REDACTED") || !strings.Contains(all, "plain: untouched") {
		t.Fatalf("redaction misplaced: %q", all)
	}

	// Before SetSecrets, output passes through untouched (failEarly's
	// pre-redacted messages, plain clone output).
	lw2 := NewLogWriter(s, logTask(44), LogLimits{})
	lw2.Write([]byte("raw token glpat-tok here\n"))
	lw2.Close()
	logs, _ = s.ReadTaskLogs(44, 1, 0)
	all = ""
	for _, l := range logs {
		all += l.Content
	}
	if !strings.Contains(all, "glpat-tok") {
		t.Fatalf("unarmed writer should pass through: %q", all)
	}
}
