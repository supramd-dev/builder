package store

import (
	"errors"
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

// Test case / run statuses.
const (
	StatusPassed = "passed"
	StatusFailed = "failed"
)

// TestCaseResult is the outcome of a single test case within a run: the
// error value of a regression case, or simply pass/fail for unit tests.
type TestCaseResult struct {
	ID         int64   `gorm:"primaryKey"`
	TestRunID  int64   `gorm:"index;not null"`
	Name       string  `gorm:"not null"`
	Status     string  `gorm:"not null"` // "passed" or "failed"
	ErrorValue float64 `gorm:"not null;default:0"`
	Message    string  `gorm:"not null;default:''"`
	Position   int     `gorm:"not null;default:0"` // order within the run
}

// TestRun is the result of one test kind on one environment at one commit —
// a cell of the dashboard matrix. The (environment, commit, kind) triple is
// unique: reporting again for it replaces the stored result.
type TestRun struct {
	ID            int64  `gorm:"primaryKey"`
	EnvironmentID int64  `gorm:"uniqueIndex:idx_test_runs_env_commit_kind;not null"`
	CommitID      int64  `gorm:"uniqueIndex:idx_test_runs_env_commit_kind;not null"`
	Kind          string `gorm:"uniqueIndex:idx_test_runs_env_commit_kind;not null"` // "regression" or "unit"
	Status        string `gorm:"not null"`                                           // derived from the cases
	Summary       string `gorm:"not null;default:''"`                                // one-paragraph conclusion (simplified report)
	Total         int    `gorm:"not null;default:0"`
	Passed        int    `gorm:"not null;default:0"`
	Failed        int    `gorm:"not null;default:0"`
	StartedAt     time.Time
	FinishedAt    time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// RunInput carries a test-run report as submitted by the reporter: the case
// list plus optional timestamps and a simplified summary. Counts are derived
// from the cases when any are present; when there are no cases the explicit
// Status/Summary on RunInput is used directly (the worker's simplified path).
type RunInput struct {
	EnvironmentID int64
	CommitID      int64
	Kind          string
	Cases         []TestCaseResult
	Status        string // used only when Cases is empty; defaults to passed
	Summary       string
	StartedAt     time.Time
	FinishedAt    time.Time
}

// ErrInvalidRunKind is returned when a run kind is not one of the supported
// kinds (regression / unit / build).
var ErrInvalidRunKind = errors.New("store: run kind must be regression, unit or build")

// ErrInvalidCaseStatus is returned when a case status is not passed/failed.
var ErrInvalidCaseStatus = errors.New("store: case status must be passed or failed")

// UpsertTestRun stores a run report. If a run already exists for the
// (environment, commit, kind) triple, its case results are replaced in a
// transaction. The counts and status are derived from the cases: a run
// passes when every case passes.
func (s *Store) UpsertTestRun(in *RunInput) (*TestRun, error) {
	if !RunKindValid(in.Kind) {
		return nil, ErrInvalidRunKind
	}
	for i := range in.Cases {
		if in.Cases[i].Status != StatusPassed && in.Cases[i].Status != StatusFailed {
			return nil, ErrInvalidCaseStatus
		}
		if in.Cases[i].Name == "" {
			return nil, errors.New("store: case name is required")
		}
		in.Cases[i].TestRunID = 0 // set below for the run being written
		in.Cases[i].ID = 0
	}

	var run TestRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("environment_id = ? AND commit_id = ? AND kind = ?",
			in.EnvironmentID, in.CommitID, in.Kind).First(&run).Error
		if err != nil && err != ErrNotFound {
			return err
		}

		if err == nil {
			// Replace: drop the old case results, keep the same row.
			if err := tx.Where("test_run_id = ?", run.ID).Delete(&TestCaseResult{}).Error; err != nil {
				return err
			}
		}
		// else: not found — create a fresh row below.

		run.EnvironmentID = in.EnvironmentID
		run.CommitID = in.CommitID
		run.Kind = in.Kind
		run.StartedAt = in.StartedAt
		run.FinishedAt = in.FinishedAt
		run.Summary = in.Summary
		run.Total = len(in.Cases)
		run.Passed = 0
		for i := range in.Cases {
			if in.Cases[i].Status == StatusPassed {
				run.Passed++
			}
		}
		run.Failed = run.Total - run.Passed
		if run.Total == 0 {
			// No case-level results: the explicit status is authoritative
			// (the simplified worker path reports a one-paragraph conclusion
			// without per-case detail).
			run.Status = in.Status
			if run.Status == "" {
				run.Status = StatusPassed
			}
			if run.Status != StatusPassed && run.Status != StatusFailed {
				return ErrInvalidCaseStatus
			}
		} else {
			run.Status = StatusFailed
			if run.Failed == 0 {
				run.Status = StatusPassed
			}
		}

		if err := tx.Save(&run).Error; err != nil {
			return err
		}
		for i := range in.Cases {
			in.Cases[i].TestRunID = run.ID
			in.Cases[i].Position = i
			if err := tx.Create(&in.Cases[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// GetTestRun loads a run with the associated environment and commit.
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

// ListCaseResults returns the cases of a run in submission order.
func (s *Store) ListCaseResults(runID int64) ([]TestCaseResult, error) {
	var cases []TestCaseResult
	if err := s.DB.Where("test_run_id = ?", runID).Order("position ASC, id ASC").Find(&cases).Error; err != nil {
		return nil, err
	}
	return cases, nil
}

// FindRunsByCommits returns runs of the given kind for the given environment
// set, keyed by (environment, commit). Only commits in the given list are
// considered, so the caller can align cells with dashboard columns.
func (s *Store) FindRunsByCommits(kind string, envIDs, commitIDs []int64) (map[EnvCommit]TestRun, error) {
	runs := map[EnvCommit]TestRun{}
	if len(envIDs) == 0 || len(commitIDs) == 0 {
		return runs, nil
	}
	var list []TestRun
	if err := s.DB.Where("kind = ? AND environment_id IN ? AND commit_id IN ?",
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

// DeleteRunsForEnvironment removes all runs (and their case results) of an
// environment, in a transaction. Called when an environment is deleted so no
// dangling dashboard rows remain.
func (s *Store) DeleteRunsForEnvironment(envID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var runIDs []int64
		if err := tx.Model(&TestRun{}).Where("environment_id = ?", envID).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		if len(runIDs) > 0 {
			if err := tx.Where("test_run_id IN ?", runIDs).Delete(&TestCaseResult{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("environment_id = ?", envID).Delete(&TestRun{}).Error
	})
}
