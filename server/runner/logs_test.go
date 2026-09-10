package runner

import (
	"strings"
	"testing"

	"md-builder/server/store"
)

func TestLogWriterChunksBySize(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, 42)

	// One big write (≥ flushBytes) flushes immediately.
	big := strings.Repeat("a", flushBytes+10)
	if n, err := lw.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("write: %d %v", n, err)
	}
	logs, err := s.ReadTaskLogs(42, 0)
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
	logs, _ = s.ReadTaskLogs(42, 0)
	if len(logs) != 1 {
		t.Fatalf("small write should stay buffered, got %d chunks", len(logs))
	}
	lw.Close()
	logs, _ = s.ReadTaskLogs(42, 0)
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
	lw := NewLogWriter(s, 7)

	// Far more than maxLogBytes: everything past the cap is dropped.
	payload := strings.Repeat("x", 1<<20) // 1 MiB
	for i := 0; i < maxLogBytes/(1<<20)+2; i++ {
		if _, err := lw.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	lw.Close()

	logs, _ := s.ReadTaskLogs(7, 0)
	var total int
	var last string
	for _, l := range logs {
		total += len(l.Content)
		last = l.Content
	}
	if total > maxLogBytes+200 {
		t.Errorf("log not capped: %d bytes", total)
	}
	if !strings.Contains(last, "log truncated") {
		t.Errorf("truncation marker missing in last chunk %q", last[:min(80, len(last))])
	}
}

func TestLogWriterAfterCloseDrops(t *testing.T) {
	s := openTestStore(t)
	lw := NewLogWriter(s, 9)
	lw.Write([]byte("before close\n"))
	lw.Close()

	// Output after close (session teardown races) is dropped, not persisted.
	lw.Write([]byte("after close\n"))
	lw.Close() // double close is a no-op

	logs, _ := s.ReadTaskLogs(9, 0)
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

// openTestStore is a plain store fixture for the log tests (no task rows
// needed: task_logs is keyed by task ID only).
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared")
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
