package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"gorm.io/gorm"

	"md-builder/server/storage"
)

// Artifact bytes live in object storage; the database keeps a reference (the
// object key plus the size) in test_artifacts. This file owns that split: the
// key layout, the upload that precedes every artifact insert, and the
// accessor the API layer reads through.

// putTimeout bounds one artifact upload. The runner caps each fetched file
// (maxArtifactBytes), so an upload that takes this long is a broken backend,
// not a large file.
const putTimeout = 60 * time.Second

// Option configures Store at Open time.
type Option func(*Store)

// WithObjects wires the object-storage backend that holds artifact bytes.
// Without it, any attempt to record an artifact fails: the backend is
// mandatory, not optional.
func WithObjects(objs storage.Store) Option {
	return func(s *Store) { s.objects = objs }
}

// Objects returns the configured artifact store, or nil when the Store was
// opened without one (the adduser path, which never touches artifacts).
func (s *Store) Objects() storage.Store { return s.objects }

// requireObjects returns the artifact store or a clear error.
func (s *Store) requireObjects() (storage.Store, error) {
	if s.objects == nil {
		return nil, storage.ErrNotConfigured
	}
	return s.objects, nil
}

// objectCtx bounds one object-storage round trip.
func objectCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), putTimeout)
}

// putArtifacts uploads each artifact's bytes and returns the rows to insert.
//
// Uploading happens before the insert, inside the caller's transaction: a
// failed upload aborts the transaction, so the database never references an
// object that was not stored. The reverse (an object with no row) is possible
// when a transaction rolls back after the upload; that leaves an unreferenced
// object, which the orphan sweep reclaims.
func (s *Store) putArtifacts(runID int64, artifacts []ArtifactInput) ([]TestArtifact, error) {
	if len(artifacts) == 0 {
		// A report without artifacts needs no backend: most runs (and
		// every store opened by adduser) have nothing to store.
		return nil, nil
	}
	objs, err := s.requireObjects()
	if err != nil {
		return nil, err
	}
	// Validate every kind before uploading anything, so a bad batch is
	// rejected without leaving objects behind.
	for i := range artifacts {
		if !ArtifactKindValid(artifacts[i].Kind) {
			return nil, fmt.Errorf("store: invalid artifact kind %q", artifacts[i].Kind)
		}
	}
	ctx, cancel := objectCtx()
	defer cancel()

	prefix := objs.KeyPrefix()
	// Artifacts of one run may share a name (two source paths with the same
	// basename); the duplicate index keeps their keys distinct while
	// staying deterministic.
	used := make(map[string]int, len(artifacts))
	rows := make([]TestArtifact, 0, len(artifacts))
	for i := range artifacts {
		a := artifacts[i]
		base := storage.ArtifactKey(prefix, runID, a.Kind, a.Name, 0)
		key := base
		if n := used[base]; n > 0 {
			key = storage.ArtifactKey(prefix, runID, a.Kind, a.Name, n)
		}
		used[base]++

		meta, err := objs.Put(ctx, key, []byte(a.Content))
		if err != nil {
			return nil, fmt.Errorf("store artifact %s (%s): %w", a.Name, a.Kind, err)
		}
		rows = append(rows, TestArtifact{
			RunID:     runID,
			Kind:      a.Kind,
			Name:      a.Name,
			ObjectKey: meta.Key,
			Size:      meta.Size,
		})
	}
	return rows, nil
}

// ArtifactContent returns an artifact's bytes: from object storage when the
// row carries a key, from the inline column for rows written before the
// object store existed (a migration converts those at startup; this keeps any
// the migration could not convert readable).
func (s *Store) ArtifactContent(ctx context.Context, a *TestArtifact) ([]byte, error) {
	if a.ObjectKey == "" {
		return []byte(a.Content), nil
	}
	objs, err := s.requireObjects()
	if err != nil {
		return nil, err
	}
	return objs.Get(ctx, a.ObjectKey)
}

// OpenArtifact streams an artifact's bytes, for the download and zip paths
// where buffering the whole file is unnecessary. Legacy inline rows are
// wrapped in a reader.
func (s *Store) OpenArtifact(ctx context.Context, a *TestArtifact) (io.ReadCloser, int64, error) {
	if a.ObjectKey == "" {
		data := []byte(a.Content)
		return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
	}
	objs, err := s.requireObjects()
	if err != nil {
		return nil, 0, err
	}
	return objs.Open(ctx, a.ObjectKey)
}

// deleteArtifactsByRunIDs removes the artifact rows of the given runs. The
// objects they referenced are reclaimed by SweepOrphanObjects rather than
// here: these deletes run inside transactions, and removing an object before
// the commit would lose data if the transaction rolled back.
func deleteArtifactsByRunIDs(tx *gorm.DB, runIDs []int64) error {
	if len(runIDs) == 0 {
		return nil
	}
	return tx.Where("run_id IN ?", runIDs).Delete(&TestArtifact{}).Error
}
