package store

import (
	"time"
)

// Test artifacts: raw result/log/series files stored alongside a run, parsed
// on the client side. The runner does not interpret them beyond aggregate
// counts — the stored bytes are the source of truth for the per-case detail
// views. A unit run stores its googletest results file as a run-level
// artifact; a regression case's fetched files ride on that case's own run.
// The future per-case log/series artifacts attach the same way.

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

// TestArtifact is one stored file of one attempt's run. RunID is the exact
// owner (an artifact belongs to the attempt that produced it); TaskID is
// denormalized so a task's files can be listed — including a container's,
// whose artifacts live on the runs of its descendants — without joining runs.
//
// The bytes live in object storage; the row holds the reference. ObjectKey is
// the object's key in the configured bucket and Size its length, so listing
// artifacts never has to talk to the backend. Content is the pre-object-store
// storage: empty for everything written since, and kept only so rows created
// before the migration stay readable.
type TestArtifact struct {
	ID        int64     `gorm:"primaryKey"`
	RunID     int64     `gorm:"index:idx_test_artifacts_run_kind;not null"`
	TaskID    int64     `gorm:"index;not null;default:0"`
	Kind      string    `gorm:"index:idx_test_artifacts_run_kind;not null"`
	Name      string    `gorm:"not null;default:''"` // source path / label
	ObjectKey string    `gorm:"not null;default:''"` // object storage key
	Size      int64     `gorm:"not null;default:0"`  // object size in bytes
	Content   string    `gorm:"not null;default:''"` // legacy inline content
	CreatedAt time.Time `gorm:"not null;default:CURRENT_TIMESTAMP"`
}

// ArtifactInput is an artifact as submitted with a run report (the runner's
// fetched results file, or a future per-case log/series file). It attaches
// to the attempt the report carries.
type ArtifactInput struct {
	Kind    string // ArtifactKindResults / ArtifactKindLog / ArtifactKindSeries
	Name    string
	Content string
}

// ListRunArtifacts returns one run's artifacts in submission order.
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

// TaskArtifactRef is one artifact together with the task that produced it, so
// a container's download bundle can name its entries after the tree.
type TaskArtifactRef struct {
	TaskID   int64
	TaskName string
	TaskKey  string
	Artifact TestArtifact
}

// ListSubtreeArtifacts returns the artifacts of the latest attempts of
// taskID and every task under it, deepest order (children after their parent,
// siblings by creation). Retired descendants are excluded: their files belong
// to an earlier graph shape, not to the current bundle.
func (s *Store) ListSubtreeArtifacts(taskID int64) ([]TaskArtifactRef, error) {
	tasks, err := s.subtreeTasks(taskID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(tasks))
	byID := make(map[int64]*Task, len(tasks))
	for i := range tasks {
		ids[i] = tasks[i].ID
		byID[tasks[i].ID] = &tasks[i]
	}
	runs, err := s.LatestRunsByTaskIDs(ids)
	if err != nil {
		return nil, err
	}
	runIDs := make([]int64, 0, len(runs))
	runTask := make(map[int64]int64, len(runs))
	for taskID, r := range runs {
		runIDs = append(runIDs, r.ID)
		runTask[r.ID] = taskID
	}
	if len(runIDs) == 0 {
		return nil, nil
	}
	var artifacts []TestArtifact
	if err := s.DB.Where("run_id IN ?", runIDs).Order("id ASC").Find(&artifacts).Error; err != nil {
		return nil, err
	}
	out := make([]TaskArtifactRef, 0, len(artifacts))
	for i := range artifacts {
		t := byID[runTask[artifacts[i].RunID]]
		if t == nil {
			continue
		}
		out = append(out, TaskArtifactRef{
			TaskID: t.ID, TaskName: t.Name, TaskKey: t.NodeKey, Artifact: artifacts[i],
		})
	}
	return out, nil
}

// subtreeTasks returns taskID and its non-retired descendants, breadth-first
// (so a parent precedes its children), each group in creation order.
func (s *Store) subtreeTasks(taskID int64) ([]Task, error) {
	root, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	out := []Task{*root}
	frontier := []int64{root.ID}
	for len(frontier) > 0 {
		var kids []Task
		if err := s.DB.Where("parent_id IN ? AND retired = ?", frontier, false).
			Order("id ASC").Find(&kids).Error; err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for i := range kids {
			out = append(out, kids[i])
			frontier = append(frontier, kids[i].ID)
		}
	}
	return out, nil
}
