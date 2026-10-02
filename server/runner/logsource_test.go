package runner

import (
	"context"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// The task ids the source tests use (see the log tests' note on the range).
const (
	sourceTaskID     = 91001
	followTaskID     = 91002
	gapTaskID        = 91003
	emptyTaskID      = 91004
	legacyTaskID     = 91005
	emptyPlainTaskID = 91006
)

// newSource returns a source for a task's attempt, from a service configured
// the way a small deployment would be (an 8-byte part, so the fixtures' logs
// have several parts each).
func newSource(t *testing.T, s *store.Store, taskID int64) (*Service, *LogSource) {
	t.Helper()
	svc := NewService(s)
	svc.LogLimits = LogLimits{PartBytes: 8}
	return svc, svc.OpenLogSource(logTask(taskID), 1)
}

// TestLogSourceServesOnePagePerPart: a reader catching up on a finished log
// pages through it, and each page is one part — the bytes a viewer is handed
// are addressed by offset, and no part is fetched twice.
func TestLogSourceServesOnePagePerPart(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, sourceTaskID, 1)
	lw := NewLogWriter(s, logTask(sourceTaskID), LogLimits{PartBytes: 8})
	lw.Write([]byte("0123456789abcdefXYZ")) // 19 bytes: parts of 8, 8, 3
	lw.Close()

	_, src := newSource(t, s, sourceTaskID)
	ctx := context.Background()
	if got := src.PageBytes(); got != 8 {
		t.Errorf("page = %d, want the part size", got)
	}
	if end, err := src.End(ctx); err != nil || end != 19 {
		t.Fatalf("end = %d (err %v), want 19", end, err)
	}

	pages := []struct {
		off  int64
		max  int64
		want string
	}{
		{0, src.PageBytes(), "01234567"},
		{8, src.PageBytes(), "89abcdef"}, // the second part: not a re-read of the first
		{16, src.PageBytes(), "XYZ"},     // what is left: a short page means the end
		{19, src.PageBytes(), ""},        // past the end
		{8, 4, "89ab"},                   // a smaller page stays inside one part
		{20, src.PageBytes(), ""},        // past the end by more than a page
		{0, 0, ""},                       // a page of nothing
		{-1, 8, ""},                      // a negative offset
	}
	for _, p := range pages {
		got, err := src.ReadFrom(ctx, p.off, p.max)
		if err != nil {
			t.Fatalf("read at %d: %v", p.off, err)
		}
		if string(got) != p.want {
			t.Errorf("read at %d (%d bytes) = %q, want %q", p.off, p.max, got, p.want)
		}
	}

	// Reading through a source that is not live twice gives the same log: the
	// listing it caches is the run's, not a fixed snapshot of the bucket.
	if got := string(readAll(t, src)); got != "0123456789abcdefXYZ" {
		t.Errorf("whole log = %q", got)
	}
}

// TestLogSourceFollowsALivingStage: a viewer following a stage that is running
// here is handed its new output as it arrives, from the buffer at the end and
// then from the parts that buffer becomes — in order, once each, with the
// cursor the API reports as the log's end.
func TestLogSourceFollowsALivingStage(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, followTaskID, 1)
	svc := NewService(s)
	svc.LogLimits = LogLimits{PartBytes: 8}
	lw := svc.OpenLog(logTask(followTaskID))
	src := svc.OpenLogSource(logTask(followTaskID), 1)

	ctx := context.Background()
	var (
		seen   strings.Builder
		cursor int64
	)
	read := func() {
		t.Helper()
		for {
			end, err := src.End(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if end <= cursor {
				return // caught up
			}
			page, err := src.ReadFrom(ctx, cursor, src.PageBytes())
			if err != nil {
				t.Fatalf("read at %d: %v", cursor, err)
			}
			if len(page) == 0 {
				t.Fatalf("the source is stuck at %d, with %d bytes to come", cursor, end-cursor)
			}
			seen.Write(page)
			cursor += int64(len(page))
		}
	}

	for i := 0; i < 20; i++ {
		lw.Write([]byte("line one\nline two\n")) // 19 bytes: two parts and a remainder
		read()
	}
	lw.Write([]byte("the end\n"))
	read()
	lw.Close()
	read()

	want := strings.Repeat("line one\nline two\n", 20) + "the end\n"
	if seen.String() != want {
		t.Errorf("followed %d bytes, want %d:\n%q", seen.Len(), len(want), seen.String())
	}
	// The parts outlive the writer: what the log is now reads the same.
	if got := string(readAll(t, svc.OpenLogSource(logTask(followTaskID), 1))); got != want {
		t.Errorf("stored log = %q", got)
	}
}

// TestLogSourceTailSnapsToAPartBoundary: a tail read places itself in the part
// holding the byte it wants. A stage that restarted between two parts leaves a
// gap in the stream, and a tail that lands in that gap must step back to the
// part rather than answer with nothing.
func TestLogSourceTailSnapsToAPartBoundary(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, gapTaskID, 1)
	ctx := context.Background()

	// Two parts, the second written by a writer that resumed at 100 (the
	// byte count the run row recorded) — 92 bytes were never stored.
	if _, err := s.Objects().Put(ctx, storage.LogKey("", run.ID, 0), []byte("aaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Objects().Put(ctx, storage.LogKey("", run.ID, 100), []byte("rest\n")); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetRunLogPrefix(run.ID, storage.LogPrefix("", run.ID), 105); err != nil || !ok {
		t.Fatalf("record the run's log: ok=%t err=%v", ok, err)
	}

	_, src := newSource(t, s, gapTaskID)
	if end, err := src.End(ctx); err != nil || end != 105 {
		t.Fatalf("end = %d (err %v), want the last part's end (105)", end, err)
	}
	// A tail reaching back into the gap (8..100) reads the part after it, not
	// nothing: 20 bytes back is 85, which nothing covers.
	data, start, err := src.Tail(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "rest\n" || start != 100 {
		t.Errorf("tail across the gap = %q at %d, want the part at 100", data, start)
	}
	// A tail inside the part is served from there, exactly.
	if data, start, err := src.Tail(ctx, 3); err != nil || string(data) != "st\n" || start != 102 {
		t.Errorf("tail inside a part = %q at %d (err %v)", data, start, err)
	}
	// And a tail of nothing asks for nothing.
	if data, start, err := src.Tail(ctx, 0); err != nil || data != nil || start != 0 {
		t.Errorf("empty tail = %q at %d (err %v)", data, start, err)
	}
}

// TestLogSourceWithoutALog: a run that has not logged anything (or a store
// without object storage) reads as an empty log — a 200 with nothing in it,
// not an error.
func TestLogSourceWithoutALog(t *testing.T) {
	s := openTestStore(t)
	createRun(t, s, emptyTaskID, 1)
	svc := NewService(s)

	ctx := context.Background()
	for _, attempt := range []int{1, 2} { // the attempt that has a run, and one that has not
		src := svc.OpenLogSource(logTask(emptyTaskID), attempt)
		if end, err := src.End(ctx); err != nil || end != 0 {
			t.Errorf("attempt %d: end = %d (err %v), want an empty log", attempt, end, err)
		}
		if data, start, err := src.Tail(ctx, 1024); err != nil || data != nil || start != 0 {
			t.Errorf("attempt %d: tail = %q at %d (err %v)", attempt, data, start, err)
		}
		if data, err := src.ReadFrom(ctx, 0, 1024); err != nil || data != nil {
			t.Errorf("attempt %d: read = %q (err %v)", attempt, data, err)
		}
	}

	// A store with no object backend at all: nothing to read, no error.
	plain, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = plain.Close() })
	createRun(t, plain, emptyPlainTaskID, 1)
	src := NewService(plain).OpenLogSource(logTask(emptyPlainTaskID), 1)
	if end, err := src.End(ctx); err != nil || end != 0 {
		t.Errorf("without object storage: end = %d (err %v)", end, err)
	}
}

// TestLogSourceReadsALegacyFullLog: a run whose log was written before it
// became parts points at one object. It reads as a one-part log, which is what
// keeps the runs that were never re-dispatched readable after an upgrade.
func TestLogSourceReadsALegacyFullLog(t *testing.T) {
	s := openTestStore(t)
	run := createRun(t, s, legacyTaskID, 1)
	ctx := context.Background()

	body := "everything the stage printed\n"
	key := storage.ArtifactKey("", run.ID, store.ArtifactKindLog, "full.log", 0)
	if _, err := s.Objects().Put(ctx, key, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.SetRunLogPrefix(run.ID, key, int64(len(body))); err != nil || !ok {
		t.Fatalf("record the run's log: ok=%t err=%v", ok, err)
	}

	_, src := newSource(t, s, legacyTaskID)
	if end, err := src.End(ctx); err != nil || end != int64(len(body)) {
		t.Fatalf("end = %d (err %v), want %d", end, err, len(body))
	}
	if got := string(readAll(t, src)); got != body {
		t.Errorf("log = %q, want the whole object", got)
	}
}
