package store

import (
	"context"
	"errors"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"md-builder/server/storage"
)

// artifactText reads an artifact's bytes through the store (object storage for
// rows that carry a key, the legacy inline column otherwise) — what the API
// serves.
func artifactText(t *testing.T, s *Store, a *TestArtifact) string {
	t.Helper()
	data, err := s.ArtifactContent(context.Background(), a)
	if err != nil {
		t.Fatalf("read artifact %d: %v", a.ID, err)
	}
	return string(data)
}

// unitRun dispatches a one-node graph (a unit stage) on the seeded (commit,
// environment) pair and returns its task together with the attempt's run. An
// artifact hangs off a run, and a run is what a real task's attempt opens at
// dispatch — so a test that wants to store bytes starts here.
func unitRun(t *testing.T, s *Store, env *TestEnvironment, commit *Commit) (*Task, *TestRun) {
	t.Helper()
	stored, err := s.UpsertTaskGraph(graphRoot(commit.ID, env.ID), []TaskNode{
		{Task: &Task{Kind: TaskKindUnit, NodeKey: TaskKindUnit, Name: "unit tests"}},
	})
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	task := stored[1]
	run, err := s.FindTaskRun(task.ID, task.Attempts)
	if err != nil {
		t.Fatalf("run of %s: %v", task.NodeKey, err)
	}
	return task, run
}

// reportArtifacts closes the task's current attempt with the given artifacts
// and returns that attempt's run — the artifacts' owner.
func reportArtifacts(t *testing.T, s *Store, task *Task, artifacts []ArtifactInput) *TestRun {
	t.Helper()
	run, err := s.FinishAttempt(task.ID, AttemptResult{Status: StatusPassed, Artifacts: artifacts})
	if err != nil {
		t.Fatalf("finish attempt of task %d: %v", task.ID, err)
	}
	return run
}

func TestArtifactsAreStoredInObjectStorage(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	run := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
	})

	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	a := artifacts[0]

	// The row is a reference: key and size, no inline bytes.
	wantKey := storage.ArtifactKey("", run.ID, ArtifactKindResults, "build/test_detail.xml", 0)
	if a.ObjectKey != wantKey {
		t.Fatalf("ObjectKey = %q, want %q", a.ObjectKey, wantKey)
	}
	if a.Size != int64(len("<testsuites/>")) {
		t.Fatalf("Size = %d", a.Size)
	}
	if a.Content != "" {
		t.Fatalf("inline content should be empty, got %q", a.Content)
	}
	// TaskID is denormalized onto the row so a container can list its
	// descendants' files without joining runs.
	if a.TaskID != task.ID {
		t.Fatalf("TaskID = %d, want the owning task %d", a.TaskID, task.ID)
	}

	// The bytes really are in the object store, under a key that names the
	// run and the artifact.
	if got := objs.Keys(); len(got) != 1 || got[0] != wantKey {
		t.Fatalf("objects = %v, want [%s]", got, wantKey)
	}
	if got := artifactText(t, s, &a); got != "<testsuites/>" {
		t.Fatalf("stored content = %q", got)
	}
}

// The retry path: a report that arrives after the attempt ended opens the next
// attempt, and that attempt's run row is created by the very transaction that
// uploads the artifacts. The rows must carry the owning task all the same —
// the owner is read from the task the report is about, never from the
// database, because a run created by the open transaction is invisible to a
// second connection: that read loses the owner or blocks on the transaction's
// own lock.
//
// A file-backed database (WAL, as the server opens it) rather than the shared
// in-memory one: it shows a lost owner as a wrong value instead of a hang.
func TestRetryReportKeepsArtifactOwnership(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "retry.db"), WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	first := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "first.xml", Content: "<a/>"},
	})
	second := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "second.xml", Content: "<b/>"},
	})
	if second.Attempt != 2 {
		t.Fatalf("the second report should open attempt 2: %+v", second)
	}
	for _, run := range []*TestRun{first, second} {
		artifacts, err := s.ListRunArtifacts(run.ID)
		if err != nil || len(artifacts) != 1 {
			t.Fatalf("artifacts of run %d: %+v (err %v)", run.ID, artifacts, err)
		}
		if artifacts[0].TaskID != task.ID {
			t.Errorf("the artifact of attempt %d lost its owner: TaskID=%d, want %d (%+v)",
				run.Attempt, artifacts[0].TaskID, task.ID, artifacts[0])
		}
	}
	// The task's bundle carries the current attempt (an earlier attempt's
	// files stay readable on that attempt's run page).
	refs, err := s.ListSubtreeArtifacts(task.ID)
	if err != nil {
		t.Fatalf("subtree artifacts: %v", err)
	}
	if len(refs) != 1 || refs[0].Artifact.RunID != second.ID || refs[0].TaskID != task.ID {
		t.Errorf("the bundle should carry the current attempt's artifact: %+v", refs)
	}
}

func TestArtifactKeyLayout(t *testing.T) {
	objs := storage.NewMemory()
	objs.Prefix = "artifacts/"
	s, err := Open("file::memory:?cache=shared", WithObjects(objs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	run := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "x"},
	})
	want := "artifacts/runs/" + strconv.FormatInt(run.ID, 10) + "/results/out.xml"
	if got := objs.Keys(); len(got) != 1 || got[0] != want {
		t.Fatalf("objects = %v, want [%s]", got, want)
	}
}

// Two fetched files can share a basename (different source directories); their
// keys must stay distinct or one would overwrite the other.
func TestArtifactDuplicateNamesGetDistinctKeys(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	run := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindFile, Name: "out/a.log", Content: "first"},
		{Kind: ArtifactKindFile, Name: "err/a.log", Content: "second"},
	})
	artifacts, _ := s.ListRunArtifacts(run.ID)
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(artifacts))
	}
	if artifacts[0].ObjectKey == artifacts[1].ObjectKey {
		t.Fatalf("duplicate names share a key: %q", artifacts[0].ObjectKey)
	}
	if got := artifactText(t, s, &artifacts[0]); got != "first" {
		t.Fatalf("artifact 0 = %q", got)
	}
	if got := artifactText(t, s, &artifacts[1]); got != "second" {
		t.Fatalf("artifact 1 = %q", got)
	}
	if objs.Len() != 2 {
		t.Fatalf("expected 2 objects, got %d", objs.Len())
	}
}

// A report replaces the attempt's artifacts rather than adding to them: the
// deterministic key means the new bytes overwrite the old object instead of
// piling up, and the run keeps the one artifact it now owns. The attempt is
// still in flight here (nothing terminal was reported yet), so the report
// lands on the same run — the retry path is a new attempt and a new key.
func TestReportReplacesAttemptArtifactsInPlace(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, run := unitRun(t, s, env, commit)

	// Files attached before the outcome lands (the runner's fetched-back
	// artifacts ride on the attempt the report will close).
	if err := s.AppendRunArtifactsByRun(run.ID, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "first"},
	}); err != nil {
		t.Fatalf("append artifacts: %v", err)
	}
	first, _ := s.ListRunArtifacts(run.ID)
	if len(first) != 1 {
		t.Fatalf("expected 1 artifact before the report, got %d", len(first))
	}

	done := reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "second"},
	})
	if done.ID != run.ID {
		t.Fatalf("the report closed run %d, want the in-flight attempt %d", done.ID, run.ID)
	}
	second, _ := s.ListRunArtifacts(done.ID)

	if len(second) != 1 {
		t.Fatalf("expected 1 artifact after replace, got %d", len(second))
	}
	if second[0].ObjectKey != first[0].ObjectKey {
		t.Fatalf("key changed on replace: %q -> %q", first[0].ObjectKey, second[0].ObjectKey)
	}
	if got := artifactText(t, s, &second[0]); got != "second" {
		t.Fatalf("content = %q, want the re-reported bytes", got)
	}
	if objs.Len() != 1 {
		t.Fatalf("expected 1 object, got %d", objs.Len())
	}
}

// The object store is mandatory: without one, recording an artifact fails and
// the transaction leaves nothing behind.
func TestArtifactWriteWithoutObjectStorageFails(t *testing.T) {
	s, err := Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	_, err = s.FinishAttempt(task.ID, AttemptResult{
		Status:    StatusPassed,
		Artifacts: []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "x"}},
	})
	if !errors.Is(err, storage.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	var artifacts int64
	if err := s.DB.Model(&TestArtifact{}).Count(&artifacts).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if artifacts != 0 {
		t.Fatalf("expected the failed report to leave no artifacts, got %d", artifacts)
	}
	// The rolled-back report left the node alone too: still on its pending
	// attempt, not on a half-written terminal one.
	if got := reloadTask(t, s, task.ID); got.Status != StatusPending {
		t.Fatalf("task status after the failed report = %q, want %q", got.Status, StatusPending)
	}

	// A run without artifacts is still fine: only artifacts need the
	// backend.
	if _, err := s.FinishAttempt(task.ID, AttemptResult{Status: StatusPassed}); err != nil {
		t.Fatalf("artifact-free report: %v", err)
	}
}

// Rows written before the object store existed stay readable through the
// inline column.
func TestLegacyInlineArtifactIsStillReadable(t *testing.T) {
	s, _ := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)
	run := reportArtifacts(t, s, task, nil)

	legacy := TestArtifact{RunID: run.ID, Kind: ArtifactKindResults, Name: "old.xml", Content: "<old/>"}
	if err := s.DB.Create(&legacy).Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if got := artifactText(t, s, &legacy); got != "<old/>" {
		t.Fatalf("legacy content = %q", got)
	}
}

func TestMigrateInlineArtifacts(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)
	run := reportArtifacts(t, s, task, nil)

	// Two legacy rows, one of them sharing a name with an artifact already
	// in object storage.
	fresh := TestArtifact{RunID: run.ID, Kind: ArtifactKindResults, Name: "new.xml", ObjectKey: "artifacts/runs/x/results/new.xml", Size: 3}
	legacy := TestArtifact{RunID: run.ID, Kind: ArtifactKindResults, Name: "old.xml", Content: "<old/>"}
	dup := TestArtifact{RunID: run.ID, Kind: ArtifactKindResults, Name: "new.xml", Content: "dup"}
	for _, row := range []TestArtifact{fresh, legacy, dup} {
		if err := s.DB.Create(&row).Error; err != nil {
			t.Fatalf("insert row: %v", err)
		}
	}
	before := objs.Len()

	n, err := s.MigrateInlineArtifacts(context.Background())
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if n != 2 {
		t.Fatalf("migrated %d artifacts, want 2", n)
	}
	if objs.Len() != before+2 {
		t.Fatalf("expected 2 new objects, got %d (before %d)", objs.Len(), before)
	}

	var rows []TestArtifact
	if err := s.DB.Where("run_id = ?", run.ID).Order("id ASC").Find(&rows).Error; err != nil {
		t.Fatalf("list rows: %v", err)
	}
	for i := range rows {
		if rows[i].ObjectKey == "" {
			t.Fatalf("row %d still has no object key: %+v", rows[i].ID, rows[i])
		}
		if rows[i].Content != "" {
			t.Fatalf("row %d still holds inline content", rows[i].ID)
		}
	}
	if got := artifactText(t, s, &rows[1]); got != "<old/>" {
		t.Fatalf("migrated content = %q", got)
	}
	if got := artifactText(t, s, &rows[2]); got != "dup" {
		t.Fatalf("migrated duplicate content = %q", got)
	}
	if rows[2].ObjectKey == rows[0].ObjectKey {
		t.Fatalf("migrated duplicate reused the key of the existing artifact: %q", rows[2].ObjectKey)
	}

	// Idempotent: nothing left to move.
	if n, err := s.MigrateInlineArtifacts(context.Background()); err != nil || n != 0 {
		t.Fatalf("second migration: n=%d err=%v", n, err)
	}
}

func TestSweepOrphanObjects(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	task, _ := unitRun(t, s, env, commit)

	reportArtifacts(t, s, task, []ArtifactInput{
		{Kind: ArtifactKindResults, Name: "out.xml", Content: "x"},
	})
	keys := objs.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 object, got %v", keys)
	}

	// A freshly written object is never swept: its row may still be
	// mid-commit.
	if n, err := s.SweepOrphanObjects(context.Background()); err != nil || n != 0 {
		t.Fatalf("fresh sweep: n=%d err=%v", n, err)
	}
	if objs.Len() != 1 {
		t.Fatal("a referenced object was swept")
	}

	// Delete the environment's tasks (and with them the run and its artifact
	// rows) and age the object past the grace period: now it is an orphan.
	if err := s.DeleteTasksForEnvironment(env.ID); err != nil {
		t.Fatalf("delete environment tasks: %v", err)
	}
	objs.SetLastModified(keys[0], time.Now().Add(-2*artifactGracePeriod))
	n, err := s.SweepOrphanObjects(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 || objs.Len() != 0 {
		t.Fatalf("sweep removed %d objects, %d left", n, objs.Len())
	}
}

// A run's log is referenced by the run row, not by an artifact row: the sweep
// must keep every part of it while the run is there and reclaim the whole
// directory with the run.
func TestSweepKeepsTheRunsLogParts(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	_, run := unitRun(t, s, env, commit)

	ctx := context.Background()
	prefix := storage.LogPrefix(objs.KeyPrefix(), run.ID)
	first, second := prefix+"part-000000000000", prefix+"part-000000000005"
	for _, part := range []struct{ key, body string }{{first, "hello"}, {second, " world"}} {
		if _, err := objs.Put(ctx, part.key, []byte(part.body)); err != nil {
			t.Fatalf("put %s: %v", part.key, err)
		}
	}
	ok, err := s.SetRunLogPrefix(run.ID, prefix, 11)
	if err != nil || !ok {
		t.Fatalf("set run log prefix: ok=%t err=%v", ok, err)
	}
	if got, err := s.GetTestRun(run.ID); err != nil || got.LogPrefix != prefix || got.LogBytes != 11 {
		t.Fatalf("run log fields = %+v, %v", got, err)
	}

	// Aged past the grace period but still referenced: every part kept.
	objs.SetLastModified(first, time.Now().Add(-2*artifactGracePeriod))
	objs.SetLastModified(second, time.Now().Add(-2*artifactGracePeriod))
	if n, err := s.SweepOrphanObjects(ctx); err != nil || n != 0 {
		t.Fatalf("sweep with a referenced log: n=%d err=%v", n, err)
	}
	if objs.Len() != 2 {
		t.Fatalf("a referenced log part was swept: %v", objs.Keys())
	}

	// Deleting the task's runs drops the reference; now they are orphans.
	if err := s.DeleteTasksForEnvironment(env.ID); err != nil {
		t.Fatalf("delete environment tasks: %v", err)
	}
	if _, err := s.GetTestRun(run.ID); !errors.Is(err, ErrTestRunNotFound) {
		t.Fatalf("run still there: %v", err)
	}
	n, err := s.SweepOrphanObjects(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 2 || objs.Len() != 0 {
		t.Fatalf("sweep removed %d objects, %d left", n, objs.Len())
	}
}

// An older server's log was one object named "full.log". That key is itself a
// valid prefix, so a run that never re-ran keeps its whole log alive too.
func TestSweepKeepsALegacyFullLog(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	_, run := unitRun(t, s, env, commit)

	ctx := context.Background()
	key := storage.ArtifactKey(objs.KeyPrefix(), run.ID, ArtifactKindLog, "full.log", 0)
	if _, err := objs.Put(ctx, key, []byte("the whole log\n")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if ok, err := s.SetRunLogPrefix(run.ID, key, int64(len("the whole log\n"))); err != nil || !ok {
		t.Fatalf("set run log prefix: ok=%t err=%v", ok, err)
	}
	objs.SetLastModified(key, time.Now().Add(-2*artifactGracePeriod))
	if n, err := s.SweepOrphanObjects(ctx); err != nil || n != 0 {
		t.Fatalf("sweep with a referenced full log: n=%d err=%v", n, err)
	}
	if objs.Len() != 1 {
		t.Fatal("a referenced full log was swept")
	}
}

// A run that re-runs writes its parts under a new prefix, leaving the old
// one unreferenced: the sweep reclaims the parts the run no longer points at.
func TestSweepReclaimsAReplacedLogPrefix(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	_, run := unitRun(t, s, env, commit)

	ctx := context.Background()
	old := storage.LogPrefix(objs.KeyPrefix(), run.ID)
	replacement := storage.LogPrefix("elsewhere", run.ID)
	for _, key := range []string{old + "part-000000000000", replacement + "part-000000000000"} {
		if _, err := objs.Put(ctx, key, []byte("bytes")); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
		objs.SetLastModified(key, time.Now().Add(-2*artifactGracePeriod))
	}
	if ok, err := s.SetRunLogPrefix(run.ID, replacement, 5); err != nil || !ok {
		t.Fatalf("set run log prefix: ok=%t err=%v", ok, err)
	}
	n, err := s.SweepOrphanObjects(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("sweep removed %d objects, want 1", n)
	}
	if keys := objs.Keys(); len(keys) != 1 || keys[0] != replacement+"part-000000000000" {
		t.Fatalf("left behind %v", keys)
	}
}

// SetRunLogPrefix on a run that is gone reports "not stored" rather than
// failing, so the writer can tell its upload has no owner.
func TestSetRunLogPrefixWithoutARun(t *testing.T) {
	s, _ := newTestStoreWithObjects(t)
	ok, err := s.SetRunLogPrefix(9999, "runs/9999/log/", 12)
	if err != nil || ok {
		t.Fatalf("unknown run: ok=%t err=%v", ok, err)
	}
	if ok, err := s.SetRunLogPrefix(0, "", 0); err != nil || ok {
		t.Fatalf("empty reference: ok=%t err=%v", ok, err)
	}
	if ok, err := s.SetRunLogPrefix(0, "runs/0/log/", 0); err != nil || ok {
		t.Fatalf("run zero: ok=%t err=%v", ok, err)
	}
}

// ListRunLogParts returns the parts in stream order with the offset each
// starts at, and reads a legacy single object as a one-part log.
func TestListRunLogParts(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	_, run := unitRun(t, s, env, commit)

	ctx := context.Background()
	if _, err := s.ListRunLogParts(ctx, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty prefix: %v", err)
	}
	prefix := storage.LogPrefix(objs.KeyPrefix(), run.ID)
	if parts, err := s.ListRunLogParts(ctx, prefix); err != nil || len(parts) != 0 {
		t.Fatalf("run without a log: parts=%v err=%v", parts, err)
	}

	// Written out of order, and one part holding binary bytes: the listing
	// is ordered by the offset in the name, not by the backend's order.
	for _, part := range []struct {
		off  int64
		body string
	}{{8 << 20, "third"}, {0, "first"}, {2 << 20, "second"}} {
		if _, err := objs.Put(ctx, storage.LogKey(objs.KeyPrefix(), run.ID, part.off), []byte(part.body)); err != nil {
			t.Fatalf("put: %v", err)
		}
	}
	parts, err := s.ListRunLogParts(ctx, prefix)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	want := []LogPart{
		{Key: storage.LogKey(objs.KeyPrefix(), run.ID, 0), Start: 0, Size: 5},
		{Key: storage.LogKey(objs.KeyPrefix(), run.ID, 2<<20), Start: 2 << 20, Size: 6},
		{Key: storage.LogKey(objs.KeyPrefix(), run.ID, 8<<20), Start: 8 << 20, Size: 5},
	}
	if !slices.Equal(parts, want) {
		t.Fatalf("parts = %+v, want %+v", parts, want)
	}
	if got := parts[1].End(); got != (2<<20)+6 {
		t.Fatalf("End = %d, want %d", got, (2<<20)+6)
	}

	// A legacy run's "full.log" has no offset in its name: it reads as the
	// single part starting at zero.
	legacy := storage.ArtifactKey(objs.KeyPrefix(), run.ID, ArtifactKindLog, "full.log", 0)
	if _, err := objs.Put(ctx, legacy, []byte("done\n")); err != nil {
		t.Fatalf("put legacy: %v", err)
	}
	parts, err = s.ListRunLogParts(ctx, legacy)
	if err != nil {
		t.Fatalf("list legacy: %v", err)
	}
	if len(parts) != 1 || parts[0].Start != 0 || parts[0].Size != 5 {
		t.Fatalf("legacy parts = %+v", parts)
	}
}

// Objects outside the runs namespace belong to someone else and are left
// alone.
func TestSweepIgnoresForeignObjects(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	if _, err := objs.Put(context.Background(), "other/tool/data.bin", []byte("keep")); err != nil {
		t.Fatalf("put: %v", err)
	}
	objs.SetLastModified("other/tool/data.bin", time.Now().Add(-100*time.Hour))

	n, err := s.SweepOrphanObjects(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if n != 0 || objs.Len() != 1 {
		t.Fatalf("sweep removed %d objects (want 0), %d left", n, objs.Len())
	}
}

// The key layout of a run's log is built in two packages that cannot import
// each other: storage owns the path, the store owns the "log" kind. They must
// agree, or a part written by the runner lands outside the directory the
// sweep keeps an eye on.
func TestLogKeyLayoutMatchesTheStoreKind(t *testing.T) {
	prefix := storage.LogPrefix("artifacts", 7)
	if want := prefix + storage.LogPartName(8<<20); prefix+storage.LogPartName(8<<20) != want {
		t.Fatal("LogPartName is not the part's name")
	}
	key := storage.LogKey("artifacts", 7, 8<<20)
	if want := storage.ArtifactKey("artifacts", 7, ArtifactKindLog, storage.LogPartName(8<<20), 0); key != want {
		t.Fatalf("LogKey = %q, want %q", key, want)
	}
	if dir := path.Dir(key) + "/"; dir != prefix {
		t.Fatalf("a part sits in %q, but the run keeps %q alive", dir, prefix)
	}
}
