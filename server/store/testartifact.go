package store

import (
	"time"

	"gorm.io/gorm"
)

// Test artifacts: raw result/log/series files stored alongside a run, parsed
// on the client side. The runner does not interpret them beyond aggregate
// counts — the stored bytes are the source of truth for the per-case detail
// views. Unit tests store their googletest results file as a run-level
// artifact (CaseID 0); regression tests will store per-case logs and series
// data files (CaseID set) behind the same table.

// Artifact kinds.
const (
	ArtifactKindResults = "results" // unit/regression result file (gtest XML/JSON)
	ArtifactKindLog     = "log"     // per-case log (regression; reserved)
	ArtifactKindSeries  = "series"  // per-case series/plot data (regression; reserved)
)

// ArtifactKindValid returns whether kind is a supported artifact kind.
func ArtifactKindValid(kind string) bool {
	return kind == ArtifactKindResults || kind == ArtifactKindLog || kind == ArtifactKindSeries
}

// TestArtifact is one stored file. CaseID 0 means the artifact belongs to the
// run as a whole; otherwise it belongs to that test case result row.
type TestArtifact struct {
	ID        int64     `gorm:"primaryKey"`
	RunID     int64     `gorm:"index:idx_test_artifacts_run_kind;not null"`
	CaseID    int64     `gorm:"index;not null;default:0"`
	Kind      string    `gorm:"index:idx_test_artifacts_run_kind;not null"`
	Name      string    `gorm:"not null;default:''"` // source path / label
	Content   string    `gorm:"not null"`            // raw file content
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// ArtifactInput is an artifact as submitted with a run report (the runner's
// fetched results file, or a per-case log/series file). CaseID links the
// artifact to one case row (per-case results files); 0 attaches it to the
// run as a whole.
type ArtifactInput struct {
	Kind    string // ArtifactKindResults / ArtifactKindLog / ArtifactKindSeries
	CaseID  int64
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

// replaceRunArtifacts swaps a run's artifacts for the submitted list (the
// upsert-replace path: old artifacts die with the report they belonged to).
func replaceRunArtifacts(tx *gorm.DB, runID int64, artifacts []ArtifactInput) error {
	if err := tx.Where("run_id = ?", runID).Delete(&TestArtifact{}).Error; err != nil {
		return err
	}
	for i := range artifacts {
		a := TestArtifact{
			RunID:   runID,
			CaseID:  artifacts[i].CaseID,
			Kind:    artifacts[i].Kind,
			Name:    artifacts[i].Name,
			Content: artifacts[i].Content,
		}
		if err := tx.Create(&a).Error; err != nil {
			return err
		}
	}
	return nil
}
