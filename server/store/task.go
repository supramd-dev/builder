package store

import (
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
)

// Task kinds. The root task represents the whole dispatched graph; the other
// kinds are the sub-tasks it consists of. The list is open: future kinds
// (e.g. performance tests) only need a kind constant plus an executor.
const (
	TaskKindRoot       = "root"
	TaskKindClone      = "clone"
	TaskKindBuild      = "build"
	TaskKindUnit       = "unit"
	TaskKindRegression = "regression"
)

// Task statuses.
const (
	TaskPending = "pending"
	TaskRunning = "running"
	TaskDone    = "done"
	TaskFailed  = "failed"
	TaskSkipped = "skipped"
)

// Task trigger sources: what dispatched the graph. Webhook (0) is the
// default — a GitLab push; manual (1) is a user-submitted test from the UI.
const (
	TaskTriggerWebhook int = 0
	TaskTriggerManual  int = 1
)

// Task is one node of a dispatched task graph: either the root (the whole
// test of one commit on one environment, replacing the former Job row) or a
// sub-task (clone / build / unit / regression, ...). Sub-tasks of a graph
// share RootID; the root task's RootID equals its own ID.
//
// Dependencies are stored as a JSON array of task IDs (DependsOn) and never
// change after creation: re-dispatching rebuilds the graph.
type Task struct {
	ID            int64  `gorm:"primaryKey"`
	RootID        int64  `gorm:"index;not null"`
	Kind          string `gorm:"index;not null"`
	Name          string `gorm:"not null"`
	CommitID      int64  `gorm:"index;not null"`
	EnvironmentID int64  `gorm:"index;not null"`
	Tags          string `gorm:"not null;default:''"`
	Trigger       int    `gorm:"not null;default:0"` // 0 = webhook, 1 = manual (TaskTrigger*)
	Config        string `gorm:"type:text"`          // root: entry snapshot; sub-task: stage snapshot (JSON)
	DependsOn     string `gorm:"type:text"`          // JSON array of task IDs, e.g. "[3,4]"
	Status        string `gorm:"index;not null;default:'pending'"`
	Error         string `gorm:"not null;default:''"`
	Attempts      int    `gorm:"not null;default:0"`
	StartedAt     *time.Time
	FinishedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// DependsOnIDs decodes the stored dependency list. A root (or any task
// without dependencies) yields nil.
func (t *Task) DependsOnIDs() []int64 {
	if t.DependsOn == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(t.DependsOn), &ids); err != nil {
		return nil
	}
	return ids
}

// SetDependsOnIDs encodes ids into the stored dependency list.
func (t *Task) SetDependsOnIDs(ids []int64) {
	if len(ids) == 0 {
		t.DependsOn = ""
		return
	}
	b, err := json.Marshal(ids)
	if err != nil {
		t.DependsOn = ""
		return
	}
	t.DependsOn = string(b)
}

// CommitSHA is the commit SHA the graph tests. It is resolved from the
// commits table (tasks store CommitID foreign keys only).
func (s *Store) CommitSHA(commitID int64) string {
	var c Commit
	if err := s.DB.Select("sha").First(&c, commitID).Error; err != nil {
		return ""
	}
	return c.SHA
}

// ErrTaskNotFound is returned when no task matches a query.
var ErrTaskNotFound = errors.New("store: task not found")

// CreateTaskGraph inserts a root task and its sub-tasks in one transaction:
// sub-task DependsOn entries are resolved to real IDs inside the transaction
// (they may reference the root by the TaskRootPlaceholder ID and each other
// by index into subtasks). Returns the stored rows with IDs filled in.
//
// Each subtask entry in deps may use:
//   - TaskRootPlaceholder: the root task
//   - 1000+i (TaskSubPlaceholderBase + i): subtasks[i]
func CreateTaskGraph(s *Store, root *Task, subtasks []*Task, deps [][]int64) ([]*Task, error) {
	if root.Kind != TaskKindRoot {
		return nil, errors.New("store: task graph requires a root task")
	}
	if len(subtasks) != len(deps) {
		return nil, errors.New("store: subtasks and deps length mismatch")
	}
	root.Status = TaskPending
	root.RootID = 0 // set to the root's own ID after insert

	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if root.ID == 0 {
			if err := tx.Create(root).Error; err != nil {
				return err
			}
		}
		// RootID is self-referential; the row may already exist (requeue).
		if root.RootID != root.ID {
			if err := tx.Model(root).Update("root_id", root.ID).Error; err != nil {
				return err
			}
		}
		for i, st := range subtasks {
			st.RootID = root.ID
			st.Status = TaskPending
			// Resolve placeholders to real IDs.
			resolved := make([]int64, 0, len(deps[i]))
			for _, d := range deps[i] {
				switch {
				case d == TaskRootPlaceholder:
					resolved = append(resolved, root.ID)
				case d >= TaskSubPlaceholderBase:
					idx := int(d - TaskSubPlaceholderBase)
					if idx >= i {
						return errors.New("store: task dependency references a later sub-task")
					}
					resolved = append(resolved, subtasks[idx].ID)
				default:
					resolved = append(resolved, d)
				}
			}
			st.SetDependsOnIDs(resolved)
			if err := tx.Create(st).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	root.RootID = root.ID
	return append([]*Task{root}, subtasks...), nil
}

// Placeholder IDs usable in CreateTaskGraph deps.
const (
	TaskRootPlaceholder    int64 = -1
	TaskSubPlaceholderBase int64 = 1000
)

// FindRootTaskByCommitEnv returns the root task for a (commit, environment)
// pair, or ErrTaskNotFound.
func (s *Store) FindRootTaskByCommitEnv(commitID, envID int64) (*Task, error) {
	var t Task
	err := s.DB.Where("kind = ? AND commit_id = ? AND environment_id = ?",
		TaskKindRoot, commitID, envID).First(&t).Error
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return &t, nil
}

// ListSubTasks returns the sub-tasks of a graph in creation (topological)
// order.
func (s *Store) ListSubTasks(rootID int64) ([]Task, error) {
	var tasks []Task
	if err := s.DB.Where("root_id = ? AND kind <> ?", rootID, TaskKindRoot).
		Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// GetTask loads one task by ID.
func (s *Store) GetTask(id int64) (*Task, error) {
	var t Task
	if err := s.DB.First(&t, id).Error; err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return &t, nil
}

// CreateTask inserts a single task row (used for the root before its
// sub-tasks are created).
func (s *Store) CreateTask(t *Task) error {
	if t.Status == "" {
		t.Status = TaskPending
	}
	return s.DB.Create(t).Error
}

// UpdateTaskConfig refreshes a root task's config snapshot and tags
// (requeue path; the graph is rebuilt from the fresh snapshot).
func (s *Store) UpdateTaskConfig(id int64, config, tags string) error {
	return s.DB.Model(&Task{}).Where("id = ?", id).
		Updates(map[string]any{"config": config, "tags": tags}).Error
}

// DeleteTaskGraph removes a root task, its sub-tasks and their logs. Used on
// re-dispatch (the graph is rebuilt from the fresh config snapshot).
func (s *Store) DeleteTaskGraph(rootID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var ids []int64
		if err := tx.Model(&Task{}).Where("root_id = ?", rootID).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := tx.Where("task_id IN ?", ids).Delete(&TaskLog{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("root_id = ?", rootID).Delete(&Task{}).Error
	})
}

// RedeployRootTask resets a root task for a fresh run: the old sub-tasks and
// logs are deleted (the graph may have changed), attempts are bumped, state
// cleared. Returns the reset root.
func (s *Store) RedeployRootTask(root *Task) error {
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var ids []int64
		if err := tx.Model(&Task{}).Where("root_id = ?", root.ID).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) > 0 {
			if err := tx.Where("task_id IN ?", ids).Delete(&TaskLog{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("root_id = ? AND kind <> ?", root.ID, TaskKindRoot).
			Delete(&Task{}).Error; err != nil {
			return err
		}
		return tx.Model(&Task{}).Where("id = ?", root.ID).Updates(map[string]any{
			"status":      TaskPending,
			"error":       "",
			"attempts":    root.Attempts + 1,
			"started_at":  nil,
			"finished_at": nil,
		}).Error
	})
	if err != nil {
		return err
	}
	root.Status = TaskPending
	root.Error = ""
	root.Attempts++
	root.StartedAt = nil
	root.FinishedAt = nil
	return nil
}

// ClaimReadyTask atomically claims the oldest pending sub-task whose
// dependencies are all done: readiness is checked in Go (DependsOn is JSON,
// not portable SQL), the claim itself is an optimistic UPDATE guarded on
// status='pending', so concurrent schedulers never double-claim. Returns nil
// when no ready task exists.
func (s *Store) ClaimReadyTask() (*Task, error) {
	var candidates []Task
	if err := s.DB.Where("kind <> ? AND status = ?", TaskKindRoot, TaskPending).
		Order("id ASC").Limit(64).Find(&candidates).Error; err != nil {
		return nil, err
	}
	for i := range candidates {
		t := &candidates[i]
		ready, err := s.taskDepsDone(t)
		if err != nil {
			return nil, err
		}
		if !ready {
			continue
		}
		now := time.Now()
		res := s.DB.Model(&Task{}).Where("id = ? AND status = ?", t.ID, TaskPending).
			Updates(map[string]any{
				"status":     TaskRunning,
				"started_at": now,
			})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			continue // lost the race; try the next candidate
		}
		t.Status = TaskRunning
		t.StartedAt = &now
		return t, nil
	}
	return nil, nil
}

// taskDepsDone reports whether all dependencies of t are in status done.
func (s *Store) taskDepsDone(t *Task) (bool, error) {
	deps := t.DependsOnIDs()
	if len(deps) == 0 {
		return true, nil
	}
	var count int64
	if err := s.DB.Model(&Task{}).Where("id IN ? AND status = ?", deps, TaskDone).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count == int64(len(deps)), nil
}

// FinishTask sets the terminal state of a task and stamps FinishedAt.
func (s *Store) FinishTask(id int64, status, errMsg string) error {
	now := time.Now()
	return s.DB.Model(&Task{}).Where("id = ?", id).
		Updates(map[string]any{
			"status":      status,
			"error":       errMsg,
			"finished_at": now,
		}).Error
}

// SkipDependents marks every pending sub-task of rootID that (transitively)
// depends on failedID as skipped, with the given reason. Sub-tasks already
// running or terminal are left alone.
func (s *Store) SkipDependents(rootID, failedID int64, reason string) error {
	tasks, err := s.ListSubTasks(rootID)
	if err != nil {
		return err
	}
	byID := make(map[int64]*Task, len(tasks))
	for i := range tasks {
		byID[tasks[i].ID] = &tasks[i]
	}
	// BFS from the failed task over reverse dependency edges.
	blocked := map[int64]bool{failedID: true}
	changed := true
	for changed {
		changed = false
		for i := range tasks {
			t := &tasks[i]
			if blocked[t.ID] || t.Status != TaskPending {
				continue
			}
			for _, d := range t.DependsOnIDs() {
				if blocked[d] {
					blocked[t.ID] = true
					changed = true
					break
				}
			}
		}
	}
	for id := range blocked {
		if id == failedID {
			continue
		}
		if err := s.FinishTask(id, TaskSkipped, reason); err != nil {
			return err
		}
		if t := byID[id]; t != nil {
			t.Status = TaskSkipped
		}
	}
	return nil
}

// RefreshRootStatus re-evaluates the root task's status from its sub-tasks:
// when every sub-task reached a terminal state, the root is done unless any
// of them failed or was skipped (then failed). Returns the new root status
// and whether it changed.
func (s *Store) RefreshRootStatus(rootID int64) (string, bool, error) {
	root, err := s.GetTask(rootID)
	if err != nil {
		return "", false, err
	}
	var subs []Task
	if err := s.DB.Where("root_id = ? AND id <> ?", rootID, rootID).Find(&subs).Error; err != nil {
		return "", false, err
	}
	terminal, anyBad := 0, false
	for i := range subs {
		switch subs[i].Status {
		case TaskDone, TaskFailed, TaskSkipped:
			terminal++
			if subs[i].Status != TaskDone {
				anyBad = true
			}
		}
	}
	if len(subs) == 0 || terminal != len(subs) {
		return root.Status, false, nil
	}
	newStatus := TaskDone
	if anyBad {
		newStatus = TaskFailed
	}
	if root.Status == newStatus {
		return newStatus, false, nil
	}
	now := time.Now()
	updates := map[string]any{"status": newStatus, "finished_at": now}
	if newStatus == TaskFailed && root.StartedAt == nil {
		updates["started_at"] = now
	}
	if err := s.DB.Model(&Task{}).Where("id = ?", rootID).Updates(updates).Error; err != nil {
		return "", false, err
	}
	return newStatus, true, nil
}

// ListRootTasks returns the most recent root tasks, newest first, capped at
// limit.
func (s *Store) ListRootTasks(limit int) ([]Task, error) {
	if limit <= 0 {
		limit = 20
	}
	var tasks []Task
	if err := s.DB.Where("kind = ?", TaskKindRoot).Order("id DESC").Limit(limit).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// RootTaskSummary is a root task plus its sub-tasks, as returned by
// FindRootTasksByCommits.
type RootTaskSummary struct {
	Root *Task
	Subs []Task
}

// FindRootGraphsByCommits returns ALL root tasks (done included) for the
// given environments and commits, with their sub-tasks, keyed by
// (environment, commit). The full dashboard uses it to show every graph's
// stages regardless of whether its runs were already reported.
func (s *Store) FindRootGraphsByCommits(envIDs, commitIDs []int64) (map[EnvCommit]RootTaskSummary, error) {
	out := map[EnvCommit]RootTaskSummary{}
	if len(envIDs) == 0 || len(commitIDs) == 0 {
		return out, nil
	}
	var roots []Task
	if err := s.DB.Where("kind = ? AND environment_id IN ? AND commit_id IN ?",
		TaskKindRoot, envIDs, commitIDs).Find(&roots).Error; err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return out, nil
	}
	ids := make([]int64, len(roots))
	for i := range roots {
		ids[i] = roots[i].ID
		key := EnvCommit{Env: roots[i].EnvironmentID, Commit: roots[i].CommitID}
		out[key] = RootTaskSummary{Root: &roots[i]}
	}
	var subs []Task
	if err := s.DB.Where("root_id IN ? AND kind <> ?", ids, TaskKindRoot).Order("id ASC").Find(&subs).Error; err != nil {
		return nil, err
	}
	for i := range subs {
		for j := range roots {
			if subs[i].RootID == roots[j].ID {
				key := EnvCommit{Env: roots[j].EnvironmentID, Commit: roots[j].CommitID}
				if s, ok := out[key]; ok {
					s.Subs = append(s.Subs, subs[i])
					out[key] = s
				}
			}
		}
	}
	return out, nil
}

// FindRootTasksByCommits returns root tasks for the given environments and
// commits, keyed by (environment, commit), so the dashboard can overlay
// live state on cells that have no test run yet. Only "live" roots are
// returned: pending/running, or failed without any reported run (a done root
// means the runs were reported; the run cell takes over).
func (s *Store) FindRootTasksByCommits(envIDs, commitIDs []int64) (map[EnvCommit]RootTaskSummary, error) {
	all, err := s.FindRootGraphsByCommits(envIDs, commitIDs)
	if err != nil {
		return nil, err
	}
	for key, summary := range all {
		if summary.Root.Status == TaskDone {
			delete(all, key) // the reported run takes over the cell
		}
	}
	return all, nil
}

// ResetStaleRunning marks any tasks left in "running" after a crash back to
// "pending" (sub-tasks are re-executed; the root is re-derived), so they are
// retried on the next scheduler cycle. Called at startup. Returns the number
// of reset rows.
func (s *Store) ResetStaleRunning() (int64, error) {
	res := s.DB.Model(&Task{}).Where("status = ?", TaskRunning).
		Updates(map[string]any{
			"status":     TaskPending,
			"started_at": nil,
			"error":      "reset after restart",
		})
	return res.RowsAffected, res.Error
}

// DeleteTasksForEnvironment removes all tasks (and logs) of an environment,
// in a transaction. Called when an environment is deleted.
func (s *Store) DeleteTasksForEnvironment(envID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var ids []int64
		if err := tx.Model(&Task{}).Where("environment_id = ?", envID).Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		if err := tx.Where("task_id IN ?", ids).Delete(&TaskLog{}).Error; err != nil {
			return err
		}
		return tx.Where("id IN ?", ids).Delete(&Task{}).Error
	})
}

// ValidateTaskKind reports whether kind is a known task kind.
func ValidateTaskKind(kind string) bool {
	switch kind {
	case TaskKindRoot, TaskKindClone, TaskKindBuild, TaskKindUnit, TaskKindRegression:
		return true
	}
	return false
}
