package store

import (
	"context"
	"log"
	"time"

	"md-builder/server/storage"
)

// Artifact objects are reclaimed by sweeping, not by the delete paths. Runs
// and artifacts are deleted from inside transactions (a re-report replaces a
// run's artifacts, a re-dispatch resets a regression run, an environment is
// removed), and removing an object before that transaction commits would lose
// data if it rolled back. Instead the database stays the index: every sweep
// compares the bucket against the artifact rows and deletes what nothing
// references any more.

// artifactGracePeriod protects a freshly written object. Uploads happen just
// before the row that references them is committed, so an object younger than
// this may simply be mid-insert.
const artifactGracePeriod = time.Hour

// SweepOrphanObjects deletes every object under the runs prefix that no
// artifact row references and that is older than artifactGracePeriod. It
// returns the number of objects removed.
func (s *Store) SweepOrphanObjects(ctx context.Context) (int, error) {
	objs, err := s.requireObjects()
	if err != nil {
		return 0, err
	}

	listed, err := objs.List(ctx, storage.RunsPrefix(objs.KeyPrefix()))
	if err != nil {
		return 0, err
	}
	if len(listed) == 0 {
		return 0, nil
	}

	// Every key the database still references. Reading them in one query
	// keeps the sweep exact: it also reclaims the object of an artifact
	// whose row was replaced by a newer report of the same run.
	var referenced []string
	if err := s.DB.Model(&TestArtifact{}).Where("object_key <> ''").Pluck("object_key", &referenced).Error; err != nil {
		return 0, err
	}
	live := make(map[string]struct{}, len(referenced))
	for _, key := range referenced {
		live[key] = struct{}{}
	}

	cutoff := time.Now().Add(-artifactGracePeriod)
	removed := 0
	for i := range listed {
		obj := listed[i]
		if _, ok := live[obj.Key]; ok {
			continue
		}
		if !obj.LastModified.IsZero() && obj.LastModified.After(cutoff) {
			continue
		}
		if err := objs.Delete(ctx, obj.Key); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// StartArtifactGC sweeps now and then every interval until ctx is done. It is
// a no-op when no object store is configured.
func (s *Store) StartArtifactGC(ctx context.Context, interval time.Duration) {
	if s.objects == nil {
		return
	}
	if interval <= 0 {
		interval = storage.DefaultGCIntervalHours * time.Hour
	}
	go func() {
		sweep := func() {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			removed, err := s.SweepOrphanObjects(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				log.Printf("artifact gc: %v", err)
			case removed > 0:
				log.Printf("artifact gc: reclaimed %d unreferenced object(s)", removed)
			}
		}
		sweep()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
