package store

import (
	"context"
	"errors"
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

func TestArtifactsAreStoredInObjectStorage(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
		Artifacts: []ArtifactInput{
			{Kind: ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

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

	// The bytes really are in the object store, under a key that names the
	// run and the artifact.
	if got := objs.Keys(); len(got) != 1 || got[0] != wantKey {
		t.Fatalf("objects = %v, want [%s]", got, wantKey)
	}
	if got := artifactText(t, s, &a); got != "<testsuites/>" {
		t.Fatalf("stored content = %q", got)
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

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
		Artifacts: []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "x"}},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
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

	run, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
		Artifacts: []ArtifactInput{
			{Kind: ArtifactKindFile, Name: "out/a.log", Content: "first"},
			{Kind: ArtifactKindFile, Name: "err/a.log", Content: "second"},
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
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

// A re-report of the same run replaces its artifacts; the deterministic key
// means the new bytes overwrite the old object instead of piling up.
func TestReplaceRunArtifactsOverwritesInPlace(t *testing.T) {
	s, objs := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)

	in := &RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
		Artifacts: []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "first"}},
	}
	run, err := s.UpsertTestRun(in)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	first, _ := s.ListRunArtifacts(run.ID)

	in.Artifacts = []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "second"}}
	run, err = s.UpsertTestRun(in)
	if err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	second, _ := s.ListRunArtifacts(run.ID)

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

	_, err = s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
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

	// A run without artifacts is still fine: only artifacts need the
	// backend.
	if _, err := s.UpsertTestRun(&RunInput{EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit}); err != nil {
		t.Fatalf("artifact-free upsert: %v", err)
	}
}

// Rows written before the object store existed stay readable through the
// inline column.
func TestLegacyInlineArtifactIsStillReadable(t *testing.T) {
	s, _ := newTestStoreWithObjects(t)
	env, commit := seedEnvAndCommit(t, s)
	run, err := s.UpsertTestRun(&RunInput{EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
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
	run, err := s.UpsertTestRun(&RunInput{EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
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

	if _, err := s.UpsertTestRun(&RunInput{
		EnvironmentID: env.ID, CommitID: commit.ID, Kind: RunKindUnit,
		Artifacts: []ArtifactInput{{Kind: ArtifactKindResults, Name: "out.xml", Content: "x"}},
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
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

	// Delete the run and age the object past the grace period: now it is an
	// orphan.
	if err := s.DeleteTestRun(env.ID, commit.ID, RunKindUnit); err != nil {
		t.Fatalf("delete run: %v", err)
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
