package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Job statuses.
const (
	JobPending = "pending"
	JobRunning = "running"
	JobDone    = "done"
	JobFailed  = "failed"
)

// Job is one scheduled test execution: a commit to test against an
// environment, following a specific matrix entry's config snapshot. The
// (commit, environment) pair is unique so re-dispatching (repeat push or
// manual re-trigger) requeues the existing job instead of creating a
// duplicate.
type Job struct {
	ID            int64  `gorm:"primaryKey"`
	CommitID      int64  `gorm:"uniqueIndex:idx_jobs_commit_env;index;not null"`
	EnvironmentID int64  `gorm:"uniqueIndex:idx_jobs_commit_env;index;not null"`
	Tags          string `gorm:"not null;default:''"` // entry tags, comma-joined (display)
	Config        string `gorm:"type:text"`           // merged entry config snapshot (JSON)
	TestInputRef  string `gorm:"not null;default:''"` // TestRepoRef snapshot at dispatch
	Status        string `gorm:"not null;default:'pending'"`
	Error         string `gorm:"not null;default:''"`
	Attempts      int    `gorm:"not null;default:0"`
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// ErrJobNotFound is returned when no job matches a query.
var ErrJobNotFound = errors.New("store: job not found")

// CreateJobs inserts new pending jobs. When a (commit, environment) pair
// already exists it is requeued (status reset to pending, config refreshed,
// attempts bumped) — this is how repeat pushes and manual re-triggers work.
// Returns the created/requeued jobs.
func (s *Store) CreateJobs(jobs []*Job) ([]*Job, error) {
	for _, j := range jobs {
		if j.CommitID == 0 || j.EnvironmentID == 0 {
			return nil, errors.New("store: job requires commit and environment")
		}
	}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for _, j := range jobs {
			var existing Job
			err := tx.Where("commit_id = ? AND environment_id = ?", j.CommitID, j.EnvironmentID).
				First(&existing).Error
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err == nil {
				// Requeue: refresh snapshot, reset state.
				existing.Tags = j.Tags
				existing.Config = j.Config
				existing.TestInputRef = j.TestInputRef
				existing.Status = JobPending
				existing.Error = ""
				existing.Attempts++
				existing.StartedAt = nil
				existing.FinishedAt = nil
				if err := tx.Save(&existing).Error; err != nil {
					return err
				}
				*j = existing
				continue
			}
			j.Status = JobPending
			j.Attempts = 0
			if err := tx.Create(j).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return jobs, nil
}

// ClaimNextJob atomically claims a pending job for execution: it sets status
// to running and stamps StartedAt. Multiple workers may call this
// concurrently; the optimistic UPDATE ... WHERE status='pending' guarantees
// each job is claimed by at most one worker.
func (s *Store) ClaimNextJob(workerID int64) (*Job, error) {
	_ = workerID
	var job Job
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// Pick the oldest pending job.
		if err := tx.Where("status = ?", JobPending).Order("id ASC").First(&job).Error; err != nil {
			return err
		}
		now := time.Now()
		res := tx.Model(&Job{}).Where("id = ? AND status = ?", job.ID, JobPending).
			Updates(map[string]any{
				"status":     JobRunning,
				"started_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrJobNotFound // lost the race
		}
		job.Status = JobRunning
		job.StartedAt = &now
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrJobNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &job, nil
}

// FinishJob sets the terminal state of a job and stamps FinishedAt.
func (s *Store) FinishJob(id int64, status, errMsg string) error {
	now := time.Now()
	return s.DB.Model(&Job{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":      status,
			"error":       errMsg,
			"finished_at": now,
		}).Error
}

// ResetStaleRunning marks any jobs left in "running" after a crash back to
// "pending", so they are retried on the next worker cycle. Called at worker
// startup.
func (s *Store) ResetStaleRunning() (int64, error) {
	res := s.DB.Model(&Job{}).Where("status = ?", JobRunning).
		Updates(map[string]any{
			"status":     JobPending,
			"started_at": nil,
			"error":      "reset after restart",
		})
	return res.RowsAffected, res.Error
}

// ListJobs returns the most recent jobs, newest first, capped at limit.
func (s *Store) ListJobs(limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 20
	}
	var jobs []Job
	if err := s.DB.Order("id DESC").Limit(limit).Find(&jobs).Error; err != nil {
		return nil, err
	}
	return jobs, nil
}

// FindJobsByCommits returns jobs keyed by (environment, commit), so the
// dashboard can overlay job state on cells that have no test_run yet.
func (s *Store) FindJobsByCommits(envIDs, commitIDs []int64) (map[EnvCommit]Job, error) {
	jobs := map[EnvCommit]Job{}
	if len(envIDs) == 0 || len(commitIDs) == 0 {
		return jobs, nil
	}
	var list []Job
	if err := s.DB.Where("environment_id IN ? AND commit_id IN ?", envIDs, commitIDs).
		Find(&list).Error; err != nil {
		return nil, err
	}
	for _, j := range list {
		// Keep only the "live" states for overlay: pending/running/failed.
		// A done job means the run was reported; the run cell takes over.
		if j.Status == JobDone {
			continue
		}
		jobs[EnvCommit{Env: j.EnvironmentID, Commit: j.CommitID}] = j
	}
	return jobs, nil
}
