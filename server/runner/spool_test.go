package runner

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogSpoolKeepsTheWholeLogUnderTheCap(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// The head region is smaller than the cap here (see newLogSpool): write
	// past it, but not past head+tail, so nothing is dropped.
	var want strings.Builder
	for i := 0; i < 8; i++ {
		line := strings.Repeat(string(rune('a'+i)), 4096) + "\n"
		want.WriteString(line)
		sp.Write([]byte(line))
	}

	if got := readAll(t, sp.Reader()); got != want.String() {
		t.Errorf("spool content = %q, want %q", got, want.String())
	}
	if got := sp.Size(); got != int64(want.Len()) {
		t.Errorf("spool size = %d, want %d", got, want.Len())
	}
	if got := sp.dropped(); got != 0 {
		t.Errorf("dropped = %d, want 0", got)
	}
	if tail := sp.Tail(10); tail != want.String()[want.Len()-10:] {
		t.Errorf("tail = %q", tail)
	}
}

func TestLogSpoolKeepsTheEndPastTheCap(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// 8 KiB cap, so head and tail are 4 KiB each (the head is clamped to
	// half). Write 8× that: the head must survive verbatim, the ring must
	// hold the newest 4 KiB, and the marker must say what went missing.
	head := strings.Repeat("H", 4096)
	sp.Write([]byte(head))
	for i := 0; i < 7; i++ {
		sp.Write([]byte(strings.Repeat("x", 4096)))
	}
	end := "the compiler failed here\n"
	sp.Write([]byte(end))

	content := readAll(t, sp.Reader())
	if !strings.HasPrefix(content, head) {
		t.Errorf("the beginning of the log was not kept: %q", content[:min(60, len(content))])
	}
	if !strings.HasSuffix(content, end) {
		t.Errorf("the end of the log was not kept: %q", content[max(0, len(content)-60):])
	}
	if !strings.Contains(content, "dropped") {
		t.Errorf("no marker naming the dropped bytes: %q", content)
	}
	total := int64(len(head)) + 7*4096 + int64(len(end))
	if want := total - 4096 /* the head kept */ - 4096; /* the ring kept */ sp.dropped() != want {
		t.Errorf("dropped = %d, want %d", sp.dropped(), want)
	}
	// The size is the head, the marker, and the ring: never the whole log
	// (that is the point of the cap), and never less than the two halves.
	if size := sp.Size(); size <= 4096*2 || size > 8*1024+200 {
		t.Errorf("size = %d, want a cap-sized log with both halves", size)
	}

	// The tail reader is what a summary is derived from: it must see the
	// final output, not the marker.
	if tail := sp.Tail(len(end)); tail != end {
		t.Errorf("tail = %q, want %q", tail, end)
	}
	long := sp.Tail(6000)
	if !strings.HasSuffix(long, end) || len(long) != 6000 {
		t.Errorf("tail across the ring boundary = %d bytes ending %q", len(long), long[max(0, len(long)-40):])
	}
}

func TestLogSpoolTailCrossesTheMarker(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	sp.Write([]byte(strings.Repeat("h", 4096)))
	sp.Write([]byte(strings.Repeat("t", 8192)))

	// The newest 4 KiB is the ring's; a longer tail reaches back through the
	// marker, so the pieces are stitched in order (ring first, then marker).
	if tail := sp.Tail(4096); tail != strings.Repeat("t", 4096) {
		t.Errorf("tail = %q, want the ring's newest bytes", tail)
	}
	tail := sp.Tail(4096 + 1024)
	if !strings.HasSuffix(tail, strings.Repeat("t", 4096)) {
		t.Errorf("tail does not end in the ring's newest bytes: %q", tail[len(tail)-40:])
	}
	if !strings.Contains(tail, "dropped") {
		t.Errorf("tail does not reach the marker: %q", tail[:min(80, len(tail))])
	}
}

func TestLogSpoolWriteSplitsAcrossTheRegions(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// One write larger than the head: it must fill the head and spill into
	// the ring, and a run of writes past the ring must keep only the newest.
	payload := make([]byte, 0, 12*1024)
	for i := 0; i < 12*1024; i++ {
		payload = append(payload, byte('0'+i%10))
	}
	sp.Write(payload)
	if got := sp.headN; got != 4096 {
		t.Errorf("head bytes = %d, want 4096", got)
	}
	if got := sp.ringHeld(); got != 4096 {
		t.Errorf("ring bytes = %d, want 4096 (a full ring)", got)
	}
	content := readAll(t, sp.Reader())
	if !strings.HasPrefix(content, string(payload[:4096])) {
		t.Errorf("head content wrong: %q", content[:40])
	}
	if !strings.HasSuffix(content, string(payload[len(payload)-4096:])) {
		t.Errorf("ring content wrong: %q", content[len(content)-40:])
	}
}

func TestLogSpoolAbandonedOnWriteError(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()
	sp.Write([]byte("before the failure\n"))

	// A closed file is what a full disk looks like from here: the write must
	// not report an error to the stage, and the spool must stop being read.
	sp.f.Close()
	if n, err := sp.Write([]byte("after\n")); n != len("after\n") || err != nil {
		t.Errorf("write after a failure = (%d, %v), want the bytes counted and no error", n, err)
	}
	if !sp.broken() {
		t.Error("spool should report itself broken")
	}
	if got := readAll(t, sp.Reader()); got != "" {
		t.Errorf("broken spool returned %q", got)
	}
	if got := sp.Tail(10); got != "" {
		t.Errorf("broken spool tail = %q, want empty", got)
	}
}

func TestLogSpoolUnwritableDirIsNotFatal(t *testing.T) {
	// A directory that does not exist stands in for an unwritable spool
	// directory: the writer must run without a spool.
	if _, err := newLogSpool(filepath.Join(t.TempDir(), "missing"), 8*1024); err == nil {
		t.Fatal("want an error for a missing spool directory")
	}
}

// TestLogSpoolSnapshotIsACopy: a reader that cannot wait for the stage — a
// download of a log still being written — gets every byte assembled so far,
// and closing it leaves nothing behind.
func TestLogSpoolSnapshotIsACopy(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer sp.Close()

	// Past the cap, so the snapshot covers the head, the marker and the ring.
	last := ""
	for i := 0; i < 20; i++ {
		last = strings.Repeat(string(rune('a'+i%26)), 512) + "\n"
		sp.Write([]byte(last))
	}
	assembled := readAll(t, sp.Reader())

	rc, size, err := sp.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	name := rc.Close()

	if got := string(body); got != assembled {
		t.Errorf("snapshot = %d bytes, want the %d the spool assembles", len(got), len(assembled))
	}
	if size != int64(len(assembled)) {
		t.Errorf("snapshot size = %d, want %d", size, len(assembled))
	}
	if !strings.Contains(string(body), "dropped") {
		t.Error("snapshot is missing the marker that names what the cap dropped")
	}
	if !strings.HasSuffix(string(body), last) {
		t.Error("snapshot is missing the output the stage wrote last")
	}
	// The copy is gone, and the spool it was taken from is untouched.
	if name != nil {
		t.Errorf("closing the snapshot: %v", name)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range files {
		if strings.Contains(e.Name(), "snapshot") {
			t.Errorf("snapshot file left behind: %s", e.Name())
		}
	}
	if got := readAll(t, sp.Reader()); got != assembled {
		t.Error("the spool changed while the snapshot was taken")
	}
}

func TestLogSpoolCloseRemovesTheFile(t *testing.T) {
	dir := t.TempDir()
	sp, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	sp.Write([]byte("hello\n"))
	path := sp.path
	if err := sp.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("spool file %s still there (err %v)", path, err)
	}
	if err := sp.Close(); err != nil {
		t.Errorf("second close: %v", err)
	}
}

func TestCleanStaleSpools(t *testing.T) {
	dir := t.TempDir()
	old, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	fresh, err := newLogSpool(dir, 8*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	// A file that is not ours must survive: the operator's temp directory is
	// shared.
	foreign := filepath.Join(dir, "someone-elses.tmp")
	if err := os.WriteFile(foreign, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old.path, stale, stale); err != nil {
		t.Fatal(err)
	}

	n, err := CleanStaleSpools(dir, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("removed %d file(s), want 1", n)
	}
	if _, err := os.Stat(old.path); !os.IsNotExist(err) {
		t.Error("the stale spool is still there")
	}
	if _, err := os.Stat(fresh.path); err != nil {
		t.Errorf("the running stage's spool was removed: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a foreign file was removed: %v", err)
	}
}

// readAll drains an assembled log.
func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read spool: %v", err)
	}
	return string(data)
}
