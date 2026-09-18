package store

import (
	"context"
	"errors"
	"fmt"
	"log"

	"gorm.io/gorm"

	"md-builder/server/storage"
)

// migrateBatch bounds how many artifacts are loaded (and uploaded) at once:
// a database that predates the object store can hold a lot of inline content,
// and the migration must not hold it all in memory.
const migrateBatch = 100

// MigrateInlineArtifacts moves artifacts that are still stored in the
// database into object storage: each row's content is uploaded, the row is
// pointed at the object, and the inline content is cleared.
//
// It runs at startup and is idempotent — rows that already carry a key are
// skipped, so an interrupted run simply continues next time. A failure is
// reported but not fatal: the row stays readable through the inline path, and
// the next startup retries it.
func (s *Store) MigrateInlineArtifacts(ctx context.Context) (int, error) {
	objs, err := s.requireObjects()
	if err != nil {
		return 0, err
	}
	prefix := objs.KeyPrefix()

	migrated := 0
	var failures []error
	var rows []TestArtifact
	// Ordered by id so the duplicate-name suffixes the migration assigns
	// match the order the artifacts were created in.
	res := s.DB.Where("object_key = '' AND content <> ''").Order("id ASC").
		FindInBatches(&rows, migrateBatch, func(_ *gorm.DB, _ int) error {
			// Duplicate names within one run need distinct keys; the
			// counter is rebuilt per batch, so re-read what already
			// exists for those runs to stay consistent across batches.
			used := map[string]int{}
			for i := range rows {
				a := &rows[i]
				key := s.nextArtifactKey(prefix, used, a.RunID, a.Kind, a.Name)
				meta, err := objs.Put(ctx, key, []byte(a.Content))
				if err != nil {
					failures = append(failures, fmt.Errorf("artifact %d: %w", a.ID, err))
					continue
				}
				if err := s.DB.Model(&TestArtifact{}).Where("id = ?", a.ID).
					Updates(map[string]any{"object_key": meta.Key, "size": meta.Size, "content": ""}).Error; err != nil {
					failures = append(failures, fmt.Errorf("artifact %d: %w", a.ID, err))
					continue
				}
				migrated++
			}
			return nil
		})
	if res.Error != nil {
		failures = append(failures, res.Error)
	}
	if migrated > 0 {
		log.Printf("artifact migration: moved %d inline artifact(s) to %s", migrated, objs.Describe())
	}
	return migrated, errors.Join(failures...)
}

// nextArtifactKey returns the key for one migrated artifact, advancing the
// per-batch duplicate counter. Rows already in object storage occupy keys too,
// so the counter is primed from the database the first time a base key is
// seen.
func (s *Store) nextArtifactKey(prefix string, used map[string]int, runID int64, kind, name string) string {
	base := storage.ArtifactKey(prefix, runID, kind, name, 0)
	n, primed := used[base]
	if !primed {
		// How many rows of this run already reference this name? Those
		// keys (base, -2, -3, ...) are taken.
		var count int64
		if err := s.DB.Model(&TestArtifact{}).
			Where("run_id = ? AND kind = ? AND name = ? AND object_key <> ''", runID, kind, name).
			Count(&count).Error; err == nil {
			n = int(count)
		}
	}
	used[base] = n + 1
	if n == 0 {
		return base
	}
	return storage.ArtifactKey(prefix, runID, kind, name, n)
}
