package store

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Test run kinds. "build" records the outcome of the build stage on an
// environment (the dashboard shows it as a third matrix kind); a build run
// has no per-case results — its summary is the compiler/command output tail.
const (
	RunKindRegression = "regression"
	RunKindUnit       = "unit"
	RunKindBuild      = "build"
)

// Test case / run statuses. StatusSkipped is a case-level status only (a
// child run whose sub-task was skipped because an upstream task failed);
// top-level runs themselves stay passed/failed.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
)

// TestRun is the result of one test kind on one environment at one commit —
// a cell of the dashboard matrix. Regression cases are nested runs: a child
// row (ParentID set, Name = preset name) under the parent regression run, so
// the detail API's `cases` list and the child's own detail page share one
// representation. Top-level runs have ParentID 0 and an empty Name.
//
// The (environment, commit, kind, parent, name) tuple is unique: reporting
// again for it replaces the stored result. TaskID links the stage sub-task
// that produced the run (its task_logs hold the stage's stdout; 0 = external
// report with no task). Unit runs carry aggregate counts only — the per-case
// detail comes from the results-file artifact parsed in the browser.
type TestRun struct {
	ID             int64   `gorm:"primaryKey"`
	EnvironmentID  int64   `gorm:"uniqueIndex:idx_test_runs_env_commit_kind_name;not null"`
	CommitID       int64   `gorm:"uniqueIndex:idx_test_runs_env_commit_kind_name;not null"`
	Kind           string  `gorm:"uniqueIndex:idx_test_runs_env_commit_kind_name;not null"` // "regression", "unit" or "build"
	ParentID       int64   `gorm:"uniqueIndex:idx_test_runs_env_commit_kind_name;not null;default:0"`
	Name           string  `gorm:"uniqueIndex:idx_test_runs_env_commit_kind_name;not null;default:''"`
	Status         string  `gorm:"not null"` // derived from the child runs (or reported directly)
	Summary        string  `gorm:"not null;default:''"`
	Message        string  `gorm:"not null;default:''"` // child runs: the case's note (summary line / upstream error)
	DurationMillis float64 `gorm:"column:duration_ms;not null;default:0"`
	Position       int     `gorm:"not null;default:0"` // order within the parent
	Total          int     `gorm:"not null;default:0"`
	Passed         int     `gorm:"not null;default:0"`
	Failed         int     `gorm:"not null;default:0"`
	Skipped        int     `gorm:"not null;default:0"`
	TaskID         int64   `gorm:"not null;default:0"`
	StartedAt      time.Time
	FinishedAt     time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// CaseInput describes one case of a full-run report (POST /api/test-runs
// with a cases array): the case becomes a child TestRun under the reported
// run.
type CaseInput struct {
	Name           string
	Status         string // passed | failed | skipped
	Message        string
	DurationMillis float64
	TaskID         int64 // the case's own sub-task (0 = unknown)
}

// RunInput carries a test-run report as submitted by the reporter: case
// results (regression reports — each becomes a child run), aggregate counts
// (the unit path: parsed from the results file's root attributes, per-case
// data stays in Artifacts), optional raw artifacts and a simplified summary.
// When Cases are present the counts are derived from them (Skipped stays 0);
// without cases the explicit counts and Status are used directly. Overrides
// refines the no-cases path: a failed override turns the run failed even when
// the counts alone would pass (e.g. the test binary crashed after reporting).
type RunInput struct {
	EnvironmentID int64
	CommitID      int64
	Kind          string
	TaskID        int64
	Cases         []CaseInput
	Total         int
	Passed        int
	Failed        int
	Skipped       int
	Status        string // used only when Cases is empty; defaults to passed
	StatusFailed  bool   // no-cases path: force failed regardless of counts
	Summary       string
	Artifacts     []ArtifactInput
	StartedAt     time.Time
	FinishedAt    time.Time
}

// CaseRunInput carries ONE regression case outcome to UpsertCaseRun (the
// per-case sub-task path): the child run is replaced by name under the
// (environment, commit) regression run and the parent's aggregates recomputed.
type CaseRunInput struct {
	EnvironmentID  int64
	CommitID       int64
	TaskID         int64
	Name           string
	Status         string // passed | failed | skipped
	Message        string
	DurationMillis float64
	StartedAt      time.Time
	FinishedAt     time.Time
	Artifacts      []ArtifactInput // attached to the child run
}

// ErrInvalidRunKind is returned when a run kind is not one of the supported
// kinds (regression / unit / build).
var ErrInvalidRunKind = errors.New("store: run kind must be regression, unit or build")

// ErrInvalidCaseStatus is returned when a case status is not one of
// passed/failed/skipped.
var ErrInvalidCaseStatus = errors.New("store: case status must be passed, failed or skipped")

// UpsertTestRun stores a run report. If a top-level run already exists for
// the (environment, commit, kind) triple, its child runs and artifacts are
// replaced in a transaction. With cases, each case becomes a child run and
// the counts/status are derived from them (a run passes when every case
// passes); without cases the explicit counts and status are stored (the
// aggregate unit path — optionally forced failed through StatusFailed when
// the command exited non-zero).
func (s *Store) UpsertTestRun(in *RunInput) (*TestRun, error) {
	if !RunKindValid(in.Kind) {
		return nil, ErrInvalidRunKind
	}
	for i := range in.Cases {
		if !CaseStatusValid(in.Cases[i].Status) {
			return nil, ErrInvalidCaseStatus
		}
		if in.Cases[i].Name == "" {
			return nil, errors.New("store: case name is required")
		}
	}
	for i := range in.Artifacts {
		if !ArtifactKindValid(in.Artifacts[i].Kind) {
			return nil, fmt.Errorf("store: invalid artifact kind %q", in.Artifacts[i].Kind)
		}
	}

	var run TestRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("environment_id = ? AND commit_id = ? AND kind = ? AND parent_id = 0 AND name = ''",
			in.EnvironmentID, in.CommitID, in.Kind).First(&run).Error
		if err != nil && err != ErrNotFound {
			return err
		}

		if err == nil {
			// Replace: drop the old child runs (and their artifacts) and the
			// run-level artifacts, keep the row.
			if err := deleteRunChildren(tx, run.ID); err != nil {
				return err
			}
			if err := tx.Where("run_id = ?", run.ID).Delete(&TestArtifact{}).Error; err != nil {
				return err
			}
		}
		// else: not found — create a fresh row below.

		run.EnvironmentID = in.EnvironmentID
		run.CommitID = in.CommitID
		run.Kind = in.Kind
		run.TaskID = in.TaskID
		run.StartedAt = in.StartedAt
		run.FinishedAt = in.FinishedAt
		run.Summary = in.Summary
		if len(in.Cases) > 0 {
			run.Total = len(in.Cases)
			run.Passed = 0
			for i := range in.Cases {
				if in.Cases[i].Status == StatusPassed {
					run.Passed++
				}
			}
			run.Failed = run.Total - run.Passed
			run.Skipped = 0
			run.Status = StatusFailed
			if run.Failed == 0 {
				run.Status = StatusPassed
			}
		} else {
			// No case-level results: the explicit counts and status are
			// authoritative (the aggregate unit path reports counts parsed
			// from the results file root, per-case data stays in artifacts).
			run.Total = in.Total
			run.Passed = in.Passed
			run.Failed = in.Failed
			run.Skipped = in.Skipped
			run.Status = in.Status
			if run.Status == "" {
				run.Status = StatusPassed
			}
			if in.StatusFailed {
				run.Status = StatusFailed
			}
			if run.Status != StatusPassed && run.Status != StatusFailed {
				return ErrInvalidCaseStatus
			}
		}

		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		for i := range in.Cases {
			child := childFromCase(in, &in.Cases[i], run.ID, i)
			if err := tx.Create(&child).Error; err != nil {
				return err
			}
		}
		return replaceRunArtifacts(tx, run.ID, in.Artifacts)
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// childFromCase builds the child TestRun row for one reported case.
func childFromCase(in *RunInput, c *CaseInput, parentID int64, position int) TestRun {
	return TestRun{
		EnvironmentID:  in.EnvironmentID,
		CommitID:       in.CommitID,
		Kind:           in.Kind,
		ParentID:       parentID,
		Name:           c.Name,
		Status:         c.Status,
		Message:        c.Message,
		DurationMillis: c.DurationMillis,
		Total:          1,
		Passed:         boolInt(c.Status == StatusPassed),
		Failed:         boolInt(c.Status == StatusFailed),
		Skipped:        boolInt(c.Status == StatusSkipped),
		TaskID:         c.TaskID,
		Position:       position,
		StartedAt:      in.StartedAt,
		FinishedAt:     in.FinishedAt,
	}
}

// GetTestRun loads a run by id.
func (s *Store) GetTestRun(id int64) (*TestRun, error) {
	var run TestRun
	if err := s.DB.First(&run, id).Error; err != nil {
		return nil, err
	}
	return &run, nil
}

// RunKindValid returns whether kind is a supported test kind.
func RunKindValid(kind string) bool {
	return kind == RunKindRegression || kind == RunKindUnit || kind == RunKindBuild
}

// CaseStatusValid returns whether status is a supported case status.
func CaseStatusValid(status string) bool {
	return status == StatusPassed || status == StatusFailed || status == StatusSkipped
}

// ListChildRuns returns the child runs of a parent in submission order.
func (s *Store) ListChildRuns(parentID int64) ([]TestRun, error) {
	var runs []TestRun
	if err := s.DB.Where("parent_id = ?", parentID).Order("position ASC, id ASC").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// UpsertCaseRun stores the outcome of ONE regression case sub-task as a
// child run under the regression run of its (environment, commit): the child
// is replaced by name and the parent's counts/status/summary are recomputed
// across every child seen so far. Creates the parent when the first case
// lands. The regression stages run as one sub-task per case, so the parent
// aggregates incrementally.
func (s *Store) UpsertCaseRun(in *CaseRunInput) (*TestRun, *TestRun, error) {
	if in.Name == "" {
		return nil, nil, errors.New("store: case name is required")
	}
	if !CaseStatusValid(in.Status) {
		return nil, nil, ErrInvalidCaseStatus
	}
	for i := range in.Artifacts {
		if !ArtifactKindValid(in.Artifacts[i].Kind) {
			return nil, nil, fmt.Errorf("store: invalid artifact kind %q", in.Artifacts[i].Kind)
		}
	}
	var parent, child TestRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// The parent run keyed by (env, commit, kind, 0, "") — create on
		// first sight, keep the row afterwards.
		err := tx.Where("environment_id = ? AND commit_id = ? AND kind = ? AND parent_id = 0 AND name = ''",
			in.EnvironmentID, in.CommitID, RunKindRegression).First(&parent).Error
		if err != nil && err != ErrNotFound {
			return err
		}
		if err != nil {
			parent = TestRun{EnvironmentID: in.EnvironmentID, CommitID: in.CommitID, Kind: RunKindRegression}
		}

		// Replace the child with the same name (a re-run of the case) and
		// its artifacts.
		var previous TestRun
		err = tx.Where("parent_id = ? AND name = ?", parent.ID, in.Name).First(&previous).Error
		if err != nil && err != ErrNotFound {
			return err
		}
		position := -1 // a replaced case keeps its original slot
		if err == nil {
			position = previous.Position
			if err := tx.Where("run_id = ?", previous.ID).Delete(&TestArtifact{}).Error; err != nil {
				return err
			}
			if err := tx.Delete(&previous).Error; err != nil {
				return err
			}
		}

		// The parent row must exist (with its ID) before the child links it.
		if err := tx.Save(&parent).Error; err != nil {
			return err
		}

		// A new case appends after the existing children.
		if position < 0 {
			var count int64
			if err := tx.Model(&TestRun{}).Where("parent_id = ?", parent.ID).Count(&count).Error; err != nil {
				return err
			}
			position = int(count)
		}
		child = TestRun{
			EnvironmentID:  in.EnvironmentID,
			CommitID:       in.CommitID,
			Kind:           RunKindRegression,
			ParentID:       parent.ID,
			Name:           in.Name,
			Status:         in.Status,
			Message:        in.Message,
			DurationMillis: in.DurationMillis,
			Total:          1,
			Passed:         boolInt(in.Status == StatusPassed),
			Failed:         boolInt(in.Status == StatusFailed),
			Skipped:        boolInt(in.Status == StatusSkipped),
			TaskID:         in.TaskID,
			Position:       position,
			StartedAt:      in.StartedAt,
			FinishedAt:     in.FinishedAt,
		}
		if err := tx.Create(&child).Error; err != nil {
			return err
		}
		if err := replaceRunArtifacts(tx, child.ID, in.Artifacts); err != nil {
			return err
		}

		// Recompute the parent aggregate over every child run.
		var children []TestRun
		if err := tx.Where("parent_id = ?", parent.ID).Order("position ASC, id ASC").Find(&children).Error; err != nil {
			return err
		}
		recomputeParent(&parent, children)
		return tx.Save(&parent).Error
	})
	if err != nil {
		return nil, nil, err
	}
	return &parent, &child, nil
}

// recomputeParent derives the parent's counts/status/summary from its child
// runs. A skipped child never fails the parent by itself; the all-skipped
// placeholder (upstream failure before any case could run) is surfaced by
// the summary's "skipped:" prefix, which the dashboard translates.
func recomputeParent(parent *TestRun, children []TestRun) {
	parent.Total = len(children)
	parent.Passed, parent.Failed, parent.Skipped = 0, 0, 0
	var failed, skipped []string
	for i := range children {
		switch {
		case children[i].Status == StatusPassed:
			parent.Passed++
		case children[i].Status == StatusSkipped:
			parent.Skipped++
			skipped = append(skipped, children[i].Name)
		default:
			parent.Failed++
			failed = append(failed, children[i].Name)
		}
	}
	parent.Status = StatusFailed
	if parent.Failed == 0 {
		parent.Status = StatusPassed
	}
	switch {
	case len(failed) > 0:
		if len(failed) > 3 {
			failed = append(failed[:3], "...")
		}
		parent.Summary = fmt.Sprintf("%d/%d cases passed; failed: %s",
			parent.Passed, parent.Total, strings.Join(failed, ", "))
	case parent.Total > 0 && parent.Passed == 0 && parent.Skipped > 0:
		// Every recorded case is a skip placeholder: mirror the runner's
		// "skipped:" display translation.
		parent.Summary = "skipped: " + children[0].Message
	default:
		parent.Summary = fmt.Sprintf("%d/%d cases passed", parent.Passed, parent.Total)
	}
}

// AppendRunArtifactsByRun adds artifacts to one run by ID without touching
// child runs. Used by the runner to attach fetched files to a unit run.
func (s *Store) AppendRunArtifactsByRun(runID int64, artifacts []ArtifactInput) error {
	if len(artifacts) == 0 {
		return nil
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		for i := range artifacts {
			if !ArtifactKindValid(artifacts[i].Kind) {
				return fmt.Errorf("store: invalid artifact kind %q", artifacts[i].Kind)
			}
			a := TestArtifact{
				RunID:   runID,
				Kind:    artifacts[i].Kind,
				Name:    artifacts[i].Name,
				Content: artifacts[i].Content,
			}
			if err := tx.Create(&a).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// ResetRegressionRun clears the child runs and artifacts of the regression
// run for (environment, commit) — a re-dispatch rebuilds the case set, so
// stale runs of presets no longer in the matrix must go. Keeps the parent
// row itself (the unique index slot) with reset counts.
func (s *Store) ResetRegressionRun(envID, commitID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var run TestRun
		if err := tx.Where("environment_id = ? AND commit_id = ? AND kind = ? AND parent_id = 0 AND name = ''",
			envID, commitID, RunKindRegression).First(&run).Error; err != nil {
			if err == ErrNotFound {
				return nil // nothing recorded yet
			}
			return err
		}
		if err := deleteRunChildren(tx, run.ID); err != nil {
			return err
		}
		if err := tx.Where("run_id = ?", run.ID).Delete(&TestArtifact{}).Error; err != nil {
			return err
		}
		updates := map[string]any{
			"total": 0, "passed": 0, "failed": 0, "skipped": 0,
			"status": StatusFailed, "summary": "",
		}
		return tx.Model(&TestRun{}).Where("id = ?", run.ID).Updates(updates).Error
	})
}

// FindRunsByCommits returns TOP-LEVEL runs of the given kind for the given
// environment set, keyed by (environment, commit). Only commits in the given
// list are considered, so the caller can align cells with dashboard columns.
// Child runs (cases) are excluded — they surface through their parent's
// detail, never as matrix cells.
func (s *Store) FindRunsByCommits(kind string, envIDs, commitIDs []int64) (map[EnvCommit]TestRun, error) {
	runs := map[EnvCommit]TestRun{}
	if len(envIDs) == 0 || len(commitIDs) == 0 {
		return runs, nil
	}
	var list []TestRun
	if err := s.DB.Where("kind = ? AND parent_id = 0 AND environment_id IN ? AND commit_id IN ?",
		kind, envIDs, commitIDs).Find(&list).Error; err != nil {
		return nil, err
	}
	for _, r := range list {
		runs[EnvCommit{Env: r.EnvironmentID, Commit: r.CommitID}] = r
	}
	return runs, nil
}

// EnvCommit keys a run by its environment and commit.
type EnvCommit struct {
	Env    int64
	Commit int64
}

// DeleteTestRun removes one run, its child runs and their artifacts (requeue
// cleanup: a stage dropped from a rebuilt graph must not leave its stale run
// behind).
func (s *Store) DeleteTestRun(envID, commitID int64, kind string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var run TestRun
		err := tx.Where("environment_id = ? AND commit_id = ? AND kind = ? AND parent_id = 0 AND name = ''",
			envID, commitID, kind).First(&run).Error
		if err != nil {
			if err == ErrNotFound {
				return nil // nothing to clean
			}
			return err
		}
		if err := deleteRunChildren(tx, run.ID); err != nil {
			return err
		}
		if err := tx.Where("run_id = ?", run.ID).Delete(&TestArtifact{}).Error; err != nil {
			return err
		}
		return tx.Delete(&TestRun{}, run.ID).Error
	})
}

// DeleteRunsForEnvironment removes all runs (top-level and children, with
// their artifacts) of an environment, in a transaction. Called when an
// environment is deleted so no dangling dashboard rows remain.
func (s *Store) DeleteRunsForEnvironment(envID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var runIDs []int64
		if err := tx.Model(&TestRun{}).Where("environment_id = ?", envID).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := tx.Where("run_id IN ?", runIDs).Delete(&TestArtifact{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("environment_id = ?", envID).Delete(&TestRun{}).Error
	})
}

// deleteRunChildren removes a run's child runs and their artifacts (the
// caller handles the run's own artifacts).
func deleteRunChildren(tx *gorm.DB, parentID int64) error {
	var childIDs []int64
	if err := tx.Model(&TestRun{}).Where("parent_id = ?", parentID).
		Pluck("id", &childIDs).Error; err != nil {
		return err
	}
	if len(childIDs) > 0 {
		if err := tx.Where("run_id IN ?", childIDs).Delete(&TestArtifact{}).Error; err != nil {
			return err
		}
	}
	return tx.Where("parent_id = ?", parentID).Delete(&TestRun{}).Error
}

// boolInt maps a bool to 0/1 for the child-run count columns.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
