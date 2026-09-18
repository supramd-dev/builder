package store

import (
	"time"

	"gorm.io/gorm"
)

// Test artifacts: raw result/log/series files stored alongside a run, parsed
// on the client side. The runner does not interpret them beyond aggregate
// counts — the stored bytes are the source of truth for the per-case detail
// views. A unit run stores its googletest results file as a run-level
// artifact; a regression case's fetched files ride on the case's own (child)
// run. The future per-case log/series artifacts attach the same way.

// Artifact kinds.
const (
	ArtifactKindResults = "results" // unit/regression result file (gtest XML/JSON)
	ArtifactKindLog     = "log"     // per-case log (regression; reserved)
	ArtifactKindSeries  = "series"  // per-case series/plot data (regression; reserved)
	ArtifactKindFile    = "file"    // raw file fetched back as-is (build artifacts); never parsed
)

// ArtifactKindValid returns whether kind is a supported artifact kind.
func ArtifactKindValid(kind string) bool {
	return kind == ArtifactKindResults || kind == ArtifactKindLog ||
		kind == ArtifactKindSeries || kind == ArtifactKindFile
}

// TestArtifact is one stored file of one run (top-level or child — a child
// run's artifacts are fetched by that run's id).
//
// The bytes live in object storage; the row holds the reference. ObjectKey is
// the object's key in the configured bucket and Size its length, so listing
// artifacts never has to talk to the backend. Content is the pre-object-store
// storage: empty for everything written since, and kept only so rows created
// before the migration stay readable.
type TestArtifact struct {
	ID        int64     `gorm:"primaryKey"`
	RunID     int64     `gorm:"index:idx_test_artifacts_run_kind;not null"`
	Kind      string    `gorm:"index:idx_test_artifacts_run_kind;not null"`
	Name      string    `gorm:"not null;default:''"` // source path / label
	ObjectKey string    `gorm:"not null;default:''"` // object storage key
	Size      int64     `gorm:"not null;default:0"`  // object size in bytes
	Content   string    `gorm:"not null;default:''"` // legacy inline content
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// ArtifactInput is an artifact as submitted with a run report (the runner's
// fetched results file, or a future per-case log/series file). It attaches
// to the run the report carries.
type ArtifactInput struct {
	Kind    string // ArtifactKindResults / ArtifactKindLog / ArtifactKindSeries
	Name    string
	Content string
}

// ListRunArtifacts returns a run's artifacts in submission order.
func (s *Store) ListRunArtifacts(runID int64) ([]TestArtifact, error) {
	var list []TestArtifact
	if err := s.DB.Where("run_id = ?", runID).Order("id ASC").Find(&list).Error; err != nil {
		return nil, err
	}
	return list, nil
}

// GetArtifact loads one artifact by id.
func (s *Store) GetArtifact(id int64) (*TestArtifact, error) {
	var a TestArtifact
	if err := s.DB.First(&a, id).Error; err != nil {
		return nil, err
	}
	return &a, nil
}

// ListRunArtifactsDeep returns a run's artifacts plus every child run's
// (regression cases carry their own). The map keys are the owning run ids;
// the parent's entry is present even when empty so callers can distinguish
// "no artifacts anywhere" from "run not found".
func (s *Store) ListRunArtifactsDeep(runID int64) (map[int64][]TestArtifact, error) {
	runIDs := []int64{runID}
	var children []TestRun
	if err := s.DB.Where("parent_id = ?", runID).Order("id ASC").Find(&children).Error; err != nil {
		return nil, err
	}
	for _, c := range children {
		runIDs = append(runIDs, c.ID)
	}
	var all []TestArtifact
	if err := s.DB.Where("run_id IN ?", runIDs).Order("id ASC").Find(&all).Error; err != nil {
		return nil, err
	}
	out := make(map[int64][]TestArtifact, len(runIDs))
	for i := range all {
		out[all[i].RunID] = append(out[all[i].RunID], all[i])
	}
	return out, nil
}

// replaceRunArtifacts swaps a run's artifacts for the submitted list (the
// upsert-replace path: old artifacts die with the report they belonged to).
// The submitted bytes are uploaded first, so the transaction can only fail
// before the rows exist. Objects the replaced rows referenced are reclaimed
// by the orphan sweep.
func (s *Store) replaceRunArtifacts(tx *gorm.DB, runID int64, artifacts []ArtifactInput) error {
	rows, err := s.putArtifacts(runID, artifacts)
	if err != nil {
		return err
	}
	if err := tx.Where("run_id = ?", runID).Delete(&TestArtifact{}).Error; err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	return tx.Create(&rows).Error
}
