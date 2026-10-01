package store

import (
	"fmt"
	"testing"
)

// appendChunk stores one log chunk of a task's current attempt, the way the
// runner's LogWriter does after seeding its sequence from MaxTaskLogSeq.
func appendChunk(t *testing.T, s *Store, taskID int64, attempt, seq int, content string) {
	t.Helper()
	if err := s.AppendTaskLog(&TaskLog{
		TaskID: taskID, Attempt: attempt, Seq: seq, Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestTaskLogAppendRead(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "logs")

	// The root is virtual and never logs, but what this pins is that a task
	// with no output reads back as empty rather than failing.
	if got, err := s.MaxTaskLogSeq(root.ID, 1); err != nil || got != 0 {
		t.Errorf("empty max seq: %d err %v", got, err)
	}

	for i, chunk := range []string{"first\n", "second\n", "third\n"} {
		appendChunk(t, s, subs[0].ID, 1, i+1, chunk)
	}
	if got, err := s.MaxTaskLogSeq(subs[0].ID, 1); err != nil || got != 3 {
		t.Errorf("max seq: want 3, got %d err %v", got, err)
	}

	// Read all of the attempt.
	logs, err := s.ReadTaskLogs(subs[0].ID, 1, 0)
	if err != nil || len(logs) != 3 {
		t.Fatalf("read all: %d logs err %v", len(logs), err)
	}
	if logs[0].Content != "first\n" || logs[2].Seq != 3 {
		t.Errorf("log order wrong: %+v", logs)
	}

	// Incremental read after seq 2 returns only the third chunk.
	inc, err := s.ReadTaskLogs(subs[0].ID, 1, 2)
	if err != nil || len(inc) != 1 || inc[0].Content != "third\n" {
		t.Errorf("incremental read wrong: %+v err %v", inc, err)
	}

	// Other tasks are isolated.
	if logs, err := s.ReadTaskLogs(subs[1].ID, 1, 0); err != nil || len(logs) != 0 {
		t.Errorf("logs should be task-scoped: %d err %v", len(logs), err)
	}

	// Deletion clears them.
	if err := s.DeleteTaskLogs(subs[0].ID); err != nil {
		t.Fatal(err)
	}
	if logs, err := s.ReadTaskLogs(subs[0].ID, 1, 0); err != nil || len(logs) != 0 {
		t.Errorf("logs should be deleted: %d err %v", len(logs), err)
	}
}

// A retried task keeps every attempt's output: (task_id, attempt, seq) is the
// identity, so attempt 2's chunks neither overwrite nor read back with attempt
// 1's — which is what lets the run page show each attempt's log.
func TestTaskLogIsAttemptScoped(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "logattempt")
	clone := subs[0]

	appendChunk(t, s, clone.ID, 1, 1, "attempt one\n")
	if _, err := s.FinishAttempt(clone.ID, AttemptResult{Status: StatusPassed}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	// The retry opens attempt 2 (see FinishAttempt); its writer starts from a
	// fresh sequence.
	run, err := s.BeginAttempt(clone.ID)
	if err != nil {
		t.Fatalf("begin attempt: %v", err)
	}
	if run.Attempt != 2 {
		t.Fatalf("retry opened attempt %d, want 2", run.Attempt)
	}
	if got, err := s.MaxTaskLogSeq(clone.ID, run.Attempt); err != nil || got != 0 {
		t.Errorf("new attempt max seq = %d err %v, want 0", got, err)
	}
	appendChunk(t, s, clone.ID, run.Attempt, 1, "attempt two\n")

	first, err := s.ReadTaskLogs(clone.ID, 1, 0)
	if err != nil || len(first) != 1 || first[0].Content != "attempt one\n" {
		t.Errorf("attempt 1 logs = %+v err %v", first, err)
	}
	second, err := s.ReadTaskLogs(clone.ID, 2, 0)
	if err != nil || len(second) != 1 || second[0].Content != "attempt two\n" {
		t.Errorf("attempt 2 logs = %+v err %v", second, err)
	}
}

// A long log is read page by page: one read returns at most logReadLimit
// chunks, and continuing with after=lastSeq reaches the end. The web log
// viewer drains a finished attempt this way (it reads a page at a time until a
// short one comes back), and the download handler streams batches the same
// way, so the cap is part of the contract both depend on — change it here and
// the viewer's page size has to change with it.
func TestReadTaskLogsPagesALongLog(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "logpages")
	clone := subs[0]

	// The viewer's page size is a client constant (LOG_PAGE in
	// frontend/src/TaskLogView.tsx): it stops draining when a page comes back
	// shorter than this. Raising the limit without raising that constant would
	// make every long log look complete after one page, so the value is pinned
	// here rather than only used.
	if logReadLimit != 1000 {
		t.Fatalf("logReadLimit is %d, but the web log viewer reads pages of 1000: update LOG_PAGE in TaskLogView.tsx too", logReadLimit)
	}

	const chunks = logReadLimit + 25
	for i := 1; i <= chunks; i++ {
		appendChunk(t, s, clone.ID, 1, i, fmt.Sprintf("line %d\n", i))
	}

	first, err := s.ReadTaskLogs(clone.ID, 1, 0)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first) != logReadLimit {
		t.Fatalf("one read returned %d chunks, want the %d-chunk cap", len(first), logReadLimit)
	}
	if first[0].Seq != 1 || first[len(first)-1].Seq != logReadLimit {
		t.Errorf("first page spans seq %d..%d, want 1..%d",
			first[0].Seq, first[len(first)-1].Seq, logReadLimit)
	}

	// Draining with after=lastSeq — what the viewer and the download do —
	// yields every chunk exactly once and in order.
	seen := 0
	after := 0
	for {
		page, err := s.ReadTaskLogs(clone.ID, 1, after)
		if err != nil {
			t.Fatalf("read after %d: %v", after, err)
		}
		if len(page) == 0 {
			break
		}
		for _, l := range page {
			seen++
			if l.Seq != seen {
				t.Fatalf("chunk %d arrived as seq %d", seen, l.Seq)
			}
			if want := fmt.Sprintf("line %d\n", seen); l.Content != want {
				t.Fatalf("chunk %d content %q, want %q", seen, l.Content, want)
			}
		}
		after = page[len(page)-1].Seq
	}
	if seen != chunks {
		t.Errorf("drained %d chunks, want %d", seen, chunks)
	}
}

// TestReadTaskLogTailReturnsTheEnd pins what the runner's summary extraction
// reads: the END of a stage's log, not its beginning. A stage's summary line
// is the last thing it prints, and a log longer than one read page would
// otherwise be summarized from output that ran minutes earlier (see
// runner.readLogTail).
func TestReadTaskLogTailReturnsTheEnd(t *testing.T) {
	s := newTestTaskStore(t)
	_, subs := seedTaskGraph(t, s, "logtail")
	clone := subs[0]

	// Nothing stored: no chunks, no error.
	if logs, err := s.ReadTaskLogTail(clone.ID, 1); err != nil || len(logs) != 0 {
		t.Fatalf("empty tail: %d logs err %v", len(logs), err)
	}

	// A short log reads back whole, in order.
	for i, chunk := range []string{"a\n", "b\n", "c\n"} {
		appendChunk(t, s, clone.ID, 1, i+1, chunk)
	}
	logs, err := s.ReadTaskLogTail(clone.ID, 1)
	if err != nil || len(logs) != 3 {
		t.Fatalf("short tail: %d logs err %v", len(logs), err)
	}
	if logs[0].Content != "a\n" || logs[2].Content != "c\n" {
		t.Errorf("short tail wrong: %+v", logs)
	}

	// A long log reads back as its last page: the cap in size, the highest
	// sequences in content — the head is what a caller must NOT be handed.
	const lines = logReadLimit + 25
	for i := 4; i <= lines; i++ {
		appendChunk(t, s, clone.ID, 1, i, fmt.Sprintf("line %d\n", i))
	}
	tail, err := s.ReadTaskLogTail(clone.ID, 1)
	if err != nil {
		t.Fatalf("long tail: %v", err)
	}
	if len(tail) != logReadLimit {
		t.Fatalf("tail page size = %d, want %d", len(tail), logReadLimit)
	}
	if tail[0].Seq != lines-logReadLimit+1 || tail[len(tail)-1].Seq != lines {
		t.Errorf("tail spans seq %d..%d, want %d..%d",
			tail[0].Seq, tail[len(tail)-1].Seq, lines-logReadLimit+1, lines)
	}
	if want := fmt.Sprintf("line %d\n", lines); tail[len(tail)-1].Content != want {
		t.Errorf("last chunk %q, want %q (the end of the log)", tail[len(tail)-1].Content, want)
	}

	// Another attempt's tail is its own.
	if logs, err := s.ReadTaskLogTail(clone.ID, 2); err != nil || len(logs) != 0 {
		t.Errorf("attempt 2 tail = %d logs err %v, want empty", len(logs), err)
	}
}
