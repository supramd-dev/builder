package store

import "testing"

func TestTaskLogAppendRead(t *testing.T) {
	s := newTestTaskStore(t)
	root, subs := seedTaskGraph(t, s, "logs")

	// Root has no logs yet.
	if got, err := s.MaxTaskLogSeq(root.ID); err != nil || got != 0 {
		t.Errorf("empty max seq: %d err %v", got, err)
	}

	for i, chunk := range []string{"first\n", "second\n", "third\n"} {
		if err := s.AppendTaskLog(subs[0].ID, i+1, chunk); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.MaxTaskLogSeq(subs[0].ID); err != nil || got != 3 {
		t.Errorf("max seq: want 3, got %d err %v", got, err)
	}

	// Read all.
	logs, err := s.ReadTaskLogs(subs[0].ID, 0)
	if err != nil || len(logs) != 3 {
		t.Fatalf("read all: %d logs err %v", len(logs), err)
	}
	if logs[0].Content != "first\n" || logs[2].Seq != 3 {
		t.Errorf("log order wrong: %+v", logs)
	}

	// Incremental read after seq 2 returns only the third chunk.
	inc, err := s.ReadTaskLogs(subs[0].ID, 2)
	if err != nil || len(inc) != 1 || inc[0].Content != "third\n" {
		t.Errorf("incremental read wrong: %+v err %v", inc, err)
	}

	// Other tasks are isolated.
	if logs, err := s.ReadTaskLogs(subs[1].ID, 0); err != nil || len(logs) != 0 {
		t.Errorf("logs should be task-scoped: %d err %v", len(logs), err)
	}

	// Deletion clears them.
	if err := s.DeleteTaskLogs(subs[0].ID); err != nil {
		t.Fatal(err)
	}
	if logs, err := s.ReadTaskLogs(subs[0].ID, 0); err != nil || len(logs) != 0 {
		t.Errorf("logs should be deleted: %d err %v", len(logs), err)
	}
}
