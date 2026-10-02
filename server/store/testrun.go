package store

import (
	"errors"
	"time"

	"gorm.io/gorm"
)

// Test run kinds: the stage kind of the task the run belongs to. A regression
// case's run is a "regression" run — the case is the task, and the kind groups
// the runs the matrix and the dashboards filter by. The virtual nodes (the
// root, the regression stage) have no runs.
const (
	RunKindClone      = "clone"
	RunKindBuild      = "build"
	RunKindUnit       = "unit"
	RunKindRegression = "regression"
)

// ErrInvalidRunKind is returned when a run kind is not one of the supported
// kinds (clone / build / unit / regression).
var ErrInvalidRunKind = errors.New("store: run kind must be clone, build, unit or regression")

// ErrInvalidRunStatus is returned when a reported status is not one of
// passed/failed/skipped.
var ErrInvalidRunStatus = errors.New("store: run status must be passed, failed or skipped")

// RunKindForTask maps a task kind to its run kind. The regression cases carry
// the stage's kind, so one matrix column covers them all.
func RunKindForTask(taskKind string) string {
	switch taskKind {
	case TaskKindClone:
		return RunKindClone
	case TaskKindBuild:
		return RunKindBuild
	case TaskKindUnit:
		return RunKindUnit
	case TaskKindRegressionCase:
		return RunKindRegression
	}
	return taskKind
}

// RunKindValid reports whether kind is a supported run kind.
func RunKindValid(kind string) bool {
	switch kind {
	case RunKindClone, RunKindBuild, RunKindUnit, RunKindRegression:
		return true
	}
	return false
}

// AttemptStatusValid reports whether status may end an attempt.
func AttemptStatusValid(status string) bool {
	return status == StatusPassed || status == StatusFailed || status == StatusSkipped
}

// TestRun is one attempt of one real task: when it ran, how long it took and
// how it ended. It is also the matrix cell — the dashboard looks up the task
// node of a stage and shows its latest run.
//
// One run is one task's attempt, not one test case: an attempt may hold
// several cases (a unit stage's gtest cases, a case's inner checks), so a run
// reports counts beside its status — see the count fields below. The task
// carries the same numbers as its latest attempt's outcome, because that is
// what a parent's rollup adds up; a virtual node (the root, the regression
// stage) runs nothing and has no run at all — its counts are its children's.
//
// The run row is created with the attempt (at dispatch, in status pending), so
// a stage has a cell and a detail page from the moment it is queued and both
// follow it live: ClaimReadyTask flips the run to running, FinishAttempt
// closes it. (task_id, attempt) is the identity — a retry of a task is a new
// attempt, a new run, and never overwrites the previous attempt's record.
//
// Kind/EnvironmentID/CommitID are denormalized from the task so the matrix and
// the run lists filter without joining tasks.
type TestRun struct {
	ID      int64  `gorm:"primaryKey"`
	TaskID  int64  `gorm:"uniqueIndex:idx_test_runs_task_attempt;not null"`
	Attempt int    `gorm:"uniqueIndex:idx_test_runs_task_attempt;not null"`
	Kind    string `gorm:"index;not null"`
	// EnvironmentID and CommitID carry the task's, as a plain index: a
	// dashboard column reads all runs of a commit without joining tasks.
	EnvironmentID  int64   `gorm:"index;not null"`
	CommitID       int64   `gorm:"index;not null"`
	Status         string  `gorm:"index;not null;default:'pending'"`
	Summary        string  `gorm:"not null;default:''"`
	DurationMillis float64 `gorm:"column:duration_ms;not null;default:0"`
	// The attempt's test counts. Kept on the run (and not only on the task)
	// because one attempt can report more than one case: a unit stage runs
	// several gtest cases and reports how many of them passed, a regression
	// case may report its inner checks. FinishAttempt writes the same numbers
	// onto the task in the same transaction, so the node's cache and its run
	// never disagree; which of the two a reader uses is a matter of what it
	// already has in hand (the matrix and the task page read the node, the run
	// page reads the run).
	Total   int `gorm:"not null;default:0"`
	Passed  int `gorm:"not null;default:0"`
	Failed  int `gorm:"not null;default:0"`
	Skipped int `gorm:"not null;default:0"`
	// LogPrefix is where the run's log lives in object storage — the
	// directory its parts are written under, "<prefix>/runs/<id>/log/" — and
	// "" when it has none (a stage that never ran, or one whose process
	// died before its first part was uploaded). The runner writes the parts
	// and the API reads them back by byte offset; LogBytes is how many bytes
	// of the log are stored there, and LogPrefix is what the orphan sweep
	// keeps alive.
	//
	// The column keeps its old name: before the log became parts it held one
	// object's key, which is itself a valid (single-object) prefix, so the
	// logs of runs written by an older server are still found by listing it.
	//
	// It is on the run (not the task) because a run is one attempt: a retry
	// is a new attempt with a new log, and the previous attempt keeps its own.
	LogPrefix  string `gorm:"column:log_object_key;not null;default:''"`
	LogBytes   int64  `gorm:"not null;default:0"`
	StartedAt  time.Time
	FinishedAt time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// SetRunLogPrefix records where a run's log parts live and how many bytes of
// them are stored. The writer calls it after each part, so the run always
// describes what a reader will find; the parts of a run whose row has been
// replaced by a re-dispatch are unreferenced and the sweep reclaims them.
// ok is false when no such run exists. A missing run is not an error.
func (s *Store) SetRunLogPrefix(runID int64, prefix string, size int64) (bool, error) {
	if runID == 0 || prefix == "" {
		return false, nil
	}
	res := s.DB.Model(&TestRun{}).Where("id = ?", runID).
		Updates(map[string]any{"log_object_key": prefix, "log_bytes": size})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// EnvCommit keys a run by its environment and commit.
type EnvCommit struct {
	Env    int64
	Commit int64
}

// GetTestRun loads one run by ID.
func (s *Store) GetTestRun(id int64) (*TestRun, error) {
	var r TestRun
	if err := s.DB.First(&r, id).Error; err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTestRunNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ErrTestRunNotFound is returned when no run matches a query.
var ErrTestRunNotFound = errors.New("store: test run not found")

// GetTestRunWithTask loads a run together with the task it belongs to: the run
// page shows the task's name, kind and description.
func (s *Store) GetTestRunWithTask(id int64) (*TestRun, *Task, error) {
	run, err := s.GetTestRun(id)
	if err != nil {
		return nil, nil, err
	}
	task, err := s.GetTask(run.TaskID)
	if err != nil {
		return nil, nil, err
	}
	return run, task, nil
}

// FindTaskRun returns the run of one attempt of one task.
func (s *Store) FindTaskRun(taskID int64, attempt int) (*TestRun, error) {
	var r TestRun
	if err := s.DB.Where("task_id = ? AND attempt = ?", taskID, attempt).First(&r).Error; err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTestRunNotFound
		}
		return nil, err
	}
	return &r, nil
}

// ListTaskRuns returns a task's attempts, newest first.
func (s *Store) ListTaskRuns(taskID int64) ([]TestRun, error) {
	var runs []TestRun
	if err := s.DB.Where("task_id = ?", taskID).Order("attempt DESC").Find(&runs).Error; err != nil {
		return nil, err
	}
	return runs, nil
}

// LatestRunsByTaskIDs returns the newest attempt of each given task, keyed by
// task id. Tasks without a run (virtual nodes, or a node whose attempt was
// never opened) are absent.
func (s *Store) LatestRunsByTaskIDs(taskIDs []int64) (map[int64]TestRun, error) {
	out := map[int64]TestRun{}
	if len(taskIDs) == 0 {
		return out, nil
	}
	sub := s.DB.Model(&TestRun{}).Select("MAX(id)").Where("task_id IN ?", taskIDs).
		Group("task_id")
	var runs []TestRun
	if err := s.DB.Where("id IN (?)", sub).Find(&runs).Error; err != nil {
		return nil, err
	}
	for _, r := range runs {
		out[r.TaskID] = r
	}
	return out, nil
}

// AttemptResult is one attempt's outcome as its reporter observed it: the
// runner when a stage finishes, or an external tester through
// POST /api/test-runs. Counts belong to the attempt; FinishAttempt writes the
// same values onto the task so the node cache and the run never disagree.
//
// Status may be left empty to derive it from the counts (failed when any case
// failed, else passed) — a reporter that knows better (the test binary crashed
// after reporting, the command's exit code failed) passes it explicitly.
type AttemptResult struct {
	Status  string
	Summary string
	Error   string
	// Attempt is the attempt the reporter ran; 0 means "the task's current
	// attempt" (what a report that only knows the task sends). The runner
	// passes the attempt it claimed, so its outcome cannot land on a later
	// attempt that a re-dispatch opened in the meantime.
	Attempt        int
	Total          int
	Passed         int
	Failed         int
	Skipped        int
	DurationMillis float64
	StartedAt      time.Time
	FinishedAt     time.Time
	Artifacts      []ArtifactInput
}

// FinishAttempt records the outcome of a task's current attempt: it writes the
// attempt's run, refreshes the task's cache from the same values, and rolls
// the virtual nodes back up so the containers always agree with their
// children. A node reported failed or skipped also leaves its dependents
// skipped, in the same transaction: a dependency that did not pass can never
// be waited on, so a graph whose node ends either way has no node left that
// could still be claimed.
//
// A report for a task whose attempt already ended starts the next attempt
// instead of overwriting the previous one: that is the retry path, and both
// the old and the new attempt stay readable.
//
// A reporter that names the attempt it ran (AttemptResult.Attempt) writes only
// that attempt's run when the task has moved on since — a re-dispatch may have
// re-armed the node while the reporter was working, and its outcome belongs to
// the attempt it actually ran, not to the fresh one queued behind it.
func (s *Store) FinishAttempt(taskID int64, res AttemptResult) (*TestRun, error) {
	if res.Status == "" {
		res.Status = deriveStatus(res)
	}
	if !AttemptStatusValid(res.Status) {
		return nil, ErrInvalidRunStatus
	}
	now := time.Now()
	if res.FinishedAt.IsZero() {
		res.FinishedAt = now
	}
	if res.StartedAt.IsZero() {
		res.StartedAt = res.FinishedAt
		if res.DurationMillis > 0 {
			res.StartedAt = res.FinishedAt.Add(-time.Duration(res.DurationMillis) * time.Millisecond)
		}
	}
	if res.DurationMillis == 0 {
		res.DurationMillis = float64(res.FinishedAt.Sub(res.StartedAt).Milliseconds())
	}
	var run TestRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		task, err := getTaskTx(tx, taskID)
		if err != nil {
			return err
		}
		if task.Virtual {
			return ErrVirtualTask
		}
		stale := res.Attempt > 0 && res.Attempt != task.Attempts
		if stale {
			// The reporter ran an attempt the task has since left behind (a
			// re-dispatch re-armed the node): its outcome is that attempt's,
			// and the node's own state — pending on the new attempt — must
			// not be overwritten with work nobody did for it.
			if err := tx.Where("task_id = ? AND attempt = ?", task.ID, res.Attempt).
				First(&run).Error; err != nil {
				if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrTestRunNotFound
				}
				return err
			}
		} else {
			run, err = currentAttemptTx(tx, task)
			if err != nil {
				return err
			}
		}
		// The report replaces the attempt's artifacts (a re-report of the same
		// attempt swaps them); the bytes are uploaded inside the transaction,
		// so the database never references an object that was not stored.
		rows, err := s.putArtifacts(run.ID, task.ID, res.Artifacts)
		if err != nil {
			return err
		}
		if err := deleteArtifactsByRunIDs(tx, []int64{run.ID}); err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.Create(&rows).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&TestRun{}).Where("id = ?", run.ID).Updates(map[string]any{
			"status":      res.Status,
			"summary":     res.Summary,
			"duration_ms": res.DurationMillis,
			"total":       res.Total,
			"passed":      res.Passed,
			"failed":      res.Failed,
			"skipped":     res.Skipped,
			"started_at":  res.StartedAt,
			"finished_at": res.FinishedAt,
		}).Error; err != nil {
			return err
		}
		run.Status, run.Summary = res.Status, res.Summary
		run.DurationMillis = res.DurationMillis
		run.Total, run.Passed, run.Failed, run.Skipped = res.Total, res.Passed, res.Failed, res.Skipped
		run.StartedAt, run.FinishedAt = res.StartedAt, res.FinishedAt
		if stale {
			// The node's state belongs to the attempt that is current now, so
			// nothing above it changes either: the rollup already reflects it.
			return nil
		}
		if err := tx.Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
			"status":      res.Status,
			"summary":     res.Summary,
			"error":       res.Error,
			"total":       res.Total,
			"passed":      res.Passed,
			"failed":      res.Failed,
			"skipped":     res.Skipped,
			"started_at":  res.StartedAt,
			"finished_at": res.FinishedAt,
		}).Error; err != nil {
			return err
		}
		if res.Status != StatusFailed && res.Status != StatusSkipped {
			return rollupTx(tx, task.RootID)
		}
		// A node that did not pass gates everything behind it: its dependents
		// can never be claimed (taskDepsPassed waits for passed), so they are
		// skipped here — with their own attempts' runs — instead of staying
		// pending for ever. The reason reads the way the runner used to write
		// it, now for both outcomes.
		if err := skipDependentsTx(tx, task.RootID, task.ID, FailureReason(task, res)); err != nil {
			return err
		}
		return rollupTx(tx, task.RootID)
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// currentAttemptTx returns the task's in-flight attempt, opening the next one
// when the latest attempt has already ended (or none exists yet — a report for
// a task that was never claimed).
func currentAttemptTx(tx *gorm.DB, task *Task) (TestRun, error) {
	var run TestRun
	err := tx.Where("task_id = ? AND attempt = ?", task.ID, task.Attempts).First(&run).Error
	switch {
	case err == nil:
		if !TaskStatusTerminal(run.Status) {
			return run, nil
		}
		// The attempt already ended: a new report is a retry.
		runs, err := beginAttemptsTx(tx, []*Task{task})
		if err != nil {
			return run, err
		}
		return runs[0], nil
	case errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound):
		runs, err := beginAttemptsTx(tx, []*Task{task})
		if err != nil {
			return run, err
		}
		return runs[0], nil
	}
	return run, err
}

// deriveStatus derives a run's status from its counts: a failed case fails the
// run; a run whose cases were all skipped is skipped.
func deriveStatus(res AttemptResult) string {
	switch {
	case res.Failed > 0:
		return StatusFailed
	case res.Total > 0 && res.Skipped == res.Total:
		return StatusSkipped
	default:
		return StatusPassed
	}
}

// AppendRunArtifactsByRun adds artifacts to one run by ID without touching the
// run's result. Used by the runner to attach fetched files to a unit run after
// the report landed.
func (s *Store) AppendRunArtifactsByRun(runID int64, artifacts []ArtifactInput) error {
	if len(artifacts) == 0 {
		return nil
	}
	run, err := s.GetTestRun(runID)
	if err != nil {
		return err
	}
	rows, err := s.putArtifacts(runID, run.TaskID, artifacts)
	if err != nil {
		return err
	}
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return tx.Create(&rows).Error
	})
}
