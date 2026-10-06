package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Task kinds. A task is one node of a dispatched graph, and it is the stable
// identity of one "test": re-dispatching the same commit on the same
// environment reuses the same rows and adds an attempt (a TestRun) to them.
//
// The virtual kinds never execute and never record a run — they are
// containers whose status and counts are rolled up from their children:
//
//	root (virtual)
//	 ├── clone
//	 ├── build
//	 ├── unit
//	 └── regression (virtual)
//	      ├── regression_case "a"
//	      └── regression_case "b"
const (
	TaskKindRoot            = "root"            // virtual: the whole test of one commit on one environment
	TaskKindClone           = "clone"           //
	TaskKindBuild           = "build"           // optional
	TaskKindUnit            = "unit"            // optional
	TaskKindRegressionStage = "regression"      // virtual: the container of the regression cases
	TaskKindRegressionCase  = "regression_case" // one md-builder.yaml regression preset
)

// Task and run statuses. Both use the same vocabulary: a real task's status
// is a cache of its latest attempt's run (FinishAttempt writes both), and a
// virtual task's is derived from its children (RollupTaskTree).
const (
	StatusPending = "pending"
	StatusRunning = "running"
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	// StatusTimeout is a failure with a cause of its own: the stage's command
	// outlived the timeout md-builder.yaml gave it and was killed. It is a
	// status rather than a wording of "failed" so the dashboard, the run page
	// and the log all name the cause, and it gates the stages behind it
	// exactly like a failure does.
	StatusTimeout = "timeout"
	// StatusSkipped is a real status, not a summary convention: the node never
	// ran because an upstream task failed. Its reason is free text in Summary.
	StatusSkipped = "skipped"
	// StatusCancelled is the status of work a site policy dropped: when a
	// repeated commit is configured to cancel the older dispatch
	// (CommitOverlapForkCancel), the earlier commit's unfinished nodes and
	// their in-flight runs become cancelled. It is terminal, and unlike a
	// failure it is not the stage's verdict — the work was stopped from the
	// outside. Finished work is never rewritten: only nodes that had not
	// reached a terminal state are cancelled.
	StatusCancelled = "cancelled"
)

// Task trigger sources: what dispatched the graph. Webhook (0) is the
// default — a GitLab push; manual (1) is a user-submitted test from the UI;
// manual-yaml (2) is a user-requested dispatch of the yaml matrix (a webhook
// run on demand).
const (
	TaskTriggerWebhook    int = 0
	TaskTriggerManual     int = 1
	TaskTriggerManualYAML int = 2
)

// Task is one node of a dispatched task graph — and, since every dispatch of
// a commit reuses the (commit, environment) root, the long-lived identity of
// the test that node stands for.
//
// Two independent structures live on it: DependsOn is the scheduling DAG
// (what must finish before this node may run; a JSON array of task ids) and
// ParentID is the tree (what this node rolls up into, and where it nests on
// the graph page). A parent-child edge does NOT imply ordering: a virtual
// container is never a gate, so its children depend on build/clone directly.
//
// Retired marks a node the current md-builder.yaml no longer defines: it is
// never scheduled again and takes no part in its parent's rollup, but its
// runs, logs and artifacts are kept as history.
type Task struct {
	ID int64 `gorm:"primaryKey"`
	// RootID is the graph this node belongs to; the root's equals its own ID.
	// At most one node per (RootID, NodeKey) — the identity a re-dispatch
	// matches on. The index is partial because a root row is inserted before
	// its own id is known (RootID 0), and roots are already unique by
	// (commit_id, environment_id).
	RootID   int64  `gorm:"index;not null;uniqueIndex:idx_tasks_node,where:kind <> 'root'"`
	NodeKey  string `gorm:"not null;default:'';uniqueIndex:idx_tasks_node,where:kind <> 'root'"`
	ParentID int64  `gorm:"index;not null;default:0"` // the tree: rollup + nesting (the root has 0)
	Kind     string `gorm:"index;not null"`
	Virtual  bool   `gorm:"not null;default:false"`
	Retired  bool   `gorm:"index;not null;default:false"`
	Name     string `gorm:"not null"`
	// Description is the human label from md-builder.yaml (stage or preset
	// description), re-stored on every dispatch: it may change between them.
	Description string `gorm:"not null;default:''"`
	// At most one root per (commit, environment): a partial unique index, so
	// sub-tasks (which share the pair) stay unconstrained. The requeue path
	// relies on the insert failing when a concurrent dispatch got there first.
	CommitID      int64  `gorm:"index;not null;uniqueIndex:idx_tasks_root,where:kind = 'root'"`
	EnvironmentID int64  `gorm:"index;not null;uniqueIndex:idx_tasks_root,where:kind = 'root'"`
	Tags          string `gorm:"not null;default:''"`
	Trigger       int    `gorm:"not null;default:0"` // 0 = webhook, 1 = manual, 2 = manual-yaml (TaskTrigger*)
	// Config is the node's config snapshot: the root carries the entry
	// snapshot (RootConfig), a stage node its stage config, a case node its
	// CaseStageConfig. Virtual nodes store "{}".
	Config    string `gorm:"type:text"`
	DependsOn string `gorm:"type:text"` // JSON array of task IDs, e.g. "[3,4]"
	Status    string `gorm:"index;not null;default:'pending'"`
	Summary   string `gorm:"not null;default:''"`
	Error     string `gorm:"not null;default:''"`
	Total     int    `gorm:"not null;default:0"`
	Passed    int    `gorm:"not null;default:0"`
	Failed    int    `gorm:"not null;default:0"`
	Skipped   int    `gorm:"not null;default:0"`
	// Attempts counts the dispatches of this node — and, for a real node, its
	// attempts (each dispatch begins one, and each attempt is one TestRun).
	Attempts   int `gorm:"not null;default:0"`
	StartedAt  *time.Time
	FinishedAt *time.Time
	CreatedAt  time.Time
	UpdatedAt  time.Time
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

// TaskKindVirtual returns whether kind is a container that never executes:
// the graph root and the regression stage. Virtual nodes have no runs, their
// status and counts come from their children, and nothing may depend on them
// (a container can never become ready, so such an edge would deadlock).
func TaskKindVirtual(kind string) bool {
	return kind == TaskKindRoot || kind == TaskKindRegressionStage
}

// TaskStatusTerminal returns whether status is a final state of a node.
func TaskStatusTerminal(status string) bool {
	return status == StatusPassed || status == StatusFailed ||
		status == StatusTimeout || status == StatusSkipped ||
		status == StatusCancelled
}

// ValidateTaskKind reports whether kind is a known task kind.
func ValidateTaskKind(kind string) bool {
	switch kind {
	case TaskKindRoot, TaskKindClone, TaskKindBuild, TaskKindUnit,
		TaskKindRegressionStage, TaskKindRegressionCase:
		return true
	}
	return false
}

// StageLabel names the kind in user-facing messages ("the clone stage").
func StageLabel(kind string) string {
	switch kind {
	case TaskKindClone:
		return "clone"
	case TaskKindBuild:
		return "build"
	case TaskKindUnit:
		return "unit test"
	case TaskKindRegressionStage:
		return "regression"
	case TaskKindRegressionCase:
		return "regression case"
	}
	return kind
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

// ErrVirtualTask is returned by the attempt functions when they are handed a
// container: virtual nodes have no attempts (and no runs).
var ErrVirtualTask = errors.New("store: virtual tasks do not execute")

// TaskSubPlaceholderBase is the placeholder base for UpsertTaskGraph deps:
// deps entries of 1000+i reference nodes[i] (which must be an earlier entry).
// Dependencies on a virtual node are rejected — see UpsertTaskGraph.
const TaskSubPlaceholderBase int64 = 1000

// TaskNode is one node of a graph about to be persisted: its row plus the
// two links that need resolving inside the transaction. Deps entries at or
// above TaskSubPlaceholderBase address the call's own nodes by index;
// ParentKey names the parent's NodeKey ("" = the root).
type TaskNode struct {
	Task      *Task
	Deps      []int64
	ParentKey string
}

// UpsertTaskGraph persists one dispatch's graph: the root row (created when
// the (commit, environment) pair is new) plus its nodes, matched by NodeKey.
//
// Matching by NodeKey is what makes a task outlive a dispatch. A node the new
// graph defines is refreshed and re-armed (status pending, fresh config,
// un-retired, Attempts++); a node it does not define is marked Retired and
// keeps its runs, logs and artifacts as history. Nodes the graph no longer
// defines are never deleted — that is the whole point of the NodeKey.
//
// Every real node gets the attempt's TestRun here, in status pending: the
// matrix cell and the run page exist from dispatch time and follow the stage
// live (the scheduler flips the run to running when it claims the node).
//
// Returns the stored nodes in the order given, with IDs filled in.
func (s *Store) UpsertTaskGraph(root *Task, nodes []TaskNode) ([]*Task, error) {
	if root.Kind != TaskKindRoot {
		return nil, errors.New("store: task graph requires a root task")
	}
	root.Status = StatusPending
	root.Virtual = true
	root.RootID = 0 // set to the root's own ID after insert

	stored := make([]*Task, len(nodes))
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// The root: insert, or adopt the row a concurrent dispatch of the same
		// (commit, environment) pair created. The partial unique index makes
		// that insert a no-op instead of a failure, so two dispatches racing on
		// one pair always end up sharing one root.
		root.NodeKey = TaskKindRoot
		root.Virtual = true
		root.Attempts = 0
		root.RootID = 0 // not known until the row exists
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(root).Error; err != nil {
			return err
		}
		if root.ID == 0 {
			existing, err := findRootTaskTx(tx, root.CommitID, root.EnvironmentID)
			if err != nil {
				return err
			}
			// A re-dispatch: keep the identity (id, creation time) and add an
			// attempt to it.
			root.ID = existing.ID
			root.Attempts = existing.Attempts
			root.CreatedAt = existing.CreatedAt
		}
		root.RootID = root.ID
		root.Attempts++
		if err := tx.Model(&Task{}).Where("id = ?", root.ID).Updates(map[string]any{
			"root_id":     root.ID,
			"node_key":    TaskKindRoot,
			"parent_id":   0,
			"virtual":     true,
			"retired":     false,
			"name":        root.Name,
			"description": root.Description,
			"tags":        root.Tags,
			"trigger":     root.Trigger,
			"config":      root.Config,
			"depends_on":  "",
			"status":      StatusPending,
			"summary":     "",
			"error":       "",
			"total":       0,
			"passed":      0,
			"failed":      0,
			"skipped":     0,
			"attempts":    root.Attempts,
			"started_at":  nil,
			"finished_at": nil,
		}).Error; err != nil {
			return err
		}
		root.Status = StatusPending

		// Pass 1: upsert every node so each has an ID, then resolve the links
		// (a parent or a placeholder may address a node later in the list).
		prior, err := listNodesTx(tx, root.ID)
		if err != nil {
			return err
		}
		byKey := make(map[string]*Task, len(prior))
		for i := range prior {
			byKey[prior[i].NodeKey] = &prior[i]
		}
		for i := range nodes {
			in := nodes[i].Task
			if in.NodeKey == "" {
				return errors.New("store: task graph node without a node key")
			}
			stored[i] = in
			if old := byKey[in.NodeKey]; old != nil {
				// A node the graph already defines: keep its identity (and its
				// attempt counter, which the new attempt extends) and refresh
				// everything else from the graph.
				in.ID = old.ID
				in.Attempts = old.Attempts
				in.CreatedAt = old.CreatedAt
				// The dispatch supersedes the attempt in flight, if the
				// scheduler had already claimed the node: nothing else would
				// ever close that run, and a run left running for ever is a
				// ghost attempt on the run page and in every list that reads
				// runs. A report from the goroutine still executing it is
				// accepted afterwards by its own attempt number
				// (AttemptResult.Attempt) and replaces this verdict.
				if err := supersedeAttemptsTx(tx, in.ID, in.Attempts); err != nil {
					return err
				}
			} else {
				in.ID = 0
				in.Attempts = 0
				in.CreatedAt = time.Time{}
			}
			in.RootID = root.ID
			in.CommitID = root.CommitID
			in.EnvironmentID = root.EnvironmentID
			in.Virtual = TaskKindVirtual(in.Kind)
			in.Retired = false
			in.Status = StatusPending
			in.Summary, in.Error = "", ""
			in.Total, in.Passed, in.Failed, in.Skipped = 0, 0, 0, 0
			in.StartedAt, in.FinishedAt = nil, nil
			if in.Virtual {
				in.Config = "{}"
			}
			if err := tx.Save(in).Error; err != nil {
				return err
			}
		}

		// Pass 2: resolve the tree and the scheduling edges.
		ids := make(map[string]int64, len(nodes))
		ids[TaskKindRoot] = root.ID
		for i := range stored {
			ids[stored[i].NodeKey] = stored[i].ID
		}
		for i := range nodes {
			n := stored[i]
			parentID := root.ID
			if nodes[i].ParentKey != "" {
				id, ok := ids[nodes[i].ParentKey]
				if !ok {
					return fmt.Errorf("store: task %q names an unknown parent %q", n.NodeKey, nodes[i].ParentKey)
				}
				parentID = id
			}
			resolved := make([]int64, 0, len(nodes[i].Deps))
			for _, d := range nodes[i].Deps {
				switch {
				case d >= TaskSubPlaceholderBase:
					idx := int(d - TaskSubPlaceholderBase)
					if idx >= len(nodes) {
						return errors.New("store: task dependency references an unknown node")
					}
					target := stored[idx]
					if target.Virtual {
						return fmt.Errorf("store: task %q depends on the virtual node %q (a container can never gate its children)", n.NodeKey, target.NodeKey)
					}
					resolved = append(resolved, target.ID)
				case d > 0:
					resolved = append(resolved, d)
				default:
					return errors.New("store: task dependency on the root task is not allowed (the root is derived from its sub-tasks and can never gate them)")
				}
			}
			n.ParentID = parentID
			n.SetDependsOnIDs(resolved)
			if err := tx.Model(&Task{}).Where("id = ?", n.ID).Updates(map[string]any{
				"parent_id":  parentID,
				"depends_on": n.DependsOn,
			}).Error; err != nil {
				return err
			}
		}

		// Retire what this graph no longer defines. Nothing is deleted: the
		// node keeps its runs, logs and artifacts, and its status (it is
		// terminal already, or an in-flight attempt is left to finish —
		// retireGhostAttemptsTx closes the queued ones nothing will ever run).
		keys := make([]string, 0, len(nodes))
		for i := range nodes {
			keys = append(keys, nodes[i].Task.NodeKey)
		}
		retire := tx.Model(&Task{}).Where("root_id = ? AND kind <> ?", root.ID, TaskKindRoot)
		if len(keys) > 0 {
			retire = retire.Where("node_key NOT IN ?", keys)
		}
		if err := retire.Update("retired", true).Error; err != nil {
			return err
		}
		if err := retireGhostAttemptsTx(tx, root.ID, false); err != nil {
			return err
		}

		// Every real node opens its attempt here (see BeginAttempt): the run
		// exists before the stage starts so the cell and the run page are live
		// from dispatch time on.
		if _, err := beginAttemptsTx(tx, stored); err != nil {
			return err
		}
		return rollupTx(tx, root.ID)
	})
	if err != nil {
		return nil, err
	}
	return append([]*Task{root}, stored...), nil
}

// findRootTaskTx loads a (commit, environment) root inside a transaction.
func findRootTaskTx(tx *gorm.DB, commitID, envID int64) (*Task, error) {
	var t Task
	err := tx.Where("kind = ? AND commit_id = ? AND environment_id = ?",
		TaskKindRoot, commitID, envID).First(&t).Error
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return &t, nil
}

// BeginAttempt opens the next attempt of one real task: it bumps Attempts,
// re-arms the task (pending, no counts, no summary — the run carries the
// attempt's own outcome) and creates the attempt's run in status pending.
// Called at dispatch, and by FinishAttempt when a report arrives for a task
// whose attempt already ended.
func (s *Store) BeginAttempt(taskID int64) (*TestRun, error) {
	var run TestRun
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		task, err := getTaskTx(tx, taskID)
		if err != nil {
			return err
		}
		if task.Virtual {
			return ErrVirtualTask
		}
		if task.Status == StatusCancelled {
			// Cancelled is absorbing: opening an attempt on it would put the
			// node back in the scheduler's queue under a cancelled graph.
			return ErrTaskCancelled
		}
		runs, err := beginAttemptsTx(tx, []*Task{task})
		if err != nil {
			return err
		}
		run = runs[0]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &run, nil
}

// SupersededSummary is the summary a run carries when a new dispatch of its
// task took over while it was still queued or running.
const SupersededSummary = "superseded by a new dispatch of this task"

// supersedeAttemptsTx closes the attempts a new dispatch takes over: it marks
// every non-terminal run up to (and including) attempt as skipped, so no run
// of a re-armed node is left non-terminal. A later report for one of them
// still lands on it (FinishAttempt with the attempt number) and replaces the
// verdict with what the goroutine really produced.
func supersedeAttemptsTx(tx *gorm.DB, taskID int64, attempt int) error {
	if attempt < 1 {
		return nil
	}
	return tx.Model(&TestRun{}).
		Where("task_id = ? AND attempt <= ? AND status IN ?", taskID, attempt,
			[]string{StatusPending, StatusRunning}).
		Updates(map[string]any{
			"status":      StatusSkipped,
			"summary":     SupersededSummary,
			"finished_at": time.Now(),
		}).Error
}

// RetiredSummary is the summary a task or run carries when the node it
// belongs to is no longer part of the graph.
const RetiredSummary = "dropped by a new dispatch of this graph"

// retireGhostAttemptsTx closes the attempts of a graph's retired nodes that
// could never be closed otherwise. A retired node is history: the scheduler
// never scans it again (ClaimReadyTask filters it out) and the rollup never
// counts it, so an attempt left pending or running would sit there for ever —
// a ghost attempt on the run page and in every list that reads runs, which is
// the state retiring must not leave behind. Nothing is deleted: the run keeps
// its place in the history, with the reason it was closed.
//
// closeRunning also closes attempts a worker may still be executing. Only a
// restart may do that (ResetStaleRunning), when no worker is left to report;
// during a dispatch a running attempt is left alone, because its worker is
// still there and reports the outcome into that same attempt.
func retireGhostAttemptsTx(tx *gorm.DB, rootID int64, closeRunning bool) error {
	statuses := []string{StatusPending}
	if closeRunning {
		statuses = append(statuses, StatusRunning)
	}
	var nodes []Task
	if err := tx.Where("root_id = ? AND retired = ? AND status IN ?", rootID, true, statuses).
		Find(&nodes).Error; err != nil {
		return err
	}
	for i := range nodes {
		now := time.Now()
		if err := tx.Model(&Task{}).Where("id = ?", nodes[i].ID).Updates(map[string]any{
			"status":      StatusSkipped,
			"summary":     RetiredSummary,
			"finished_at": now,
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&TestRun{}).
			Where("task_id = ? AND attempt = ? AND status IN ?", nodes[i].ID, nodes[i].Attempts, statuses).
			Updates(map[string]any{
				"status":      StatusSkipped,
				"summary":     RetiredSummary,
				"finished_at": now,
			}).Error; err != nil {
			return err
		}
	}
	return nil
}

// beginAttemptsTx opens one attempt per real node (see BeginAttempt) and
// returns the created runs, aligned with nodes.
func beginAttemptsTx(tx *gorm.DB, nodes []*Task) ([]TestRun, error) {
	runs := make([]TestRun, len(nodes))
	for i := range nodes {
		n := nodes[i]
		attempt, err := nextAttemptTx(tx, n)
		if err != nil {
			return nil, err
		}
		n.Attempts = attempt
		if n.Virtual {
			// A container's "attempt" is the dispatch itself: no run, and the
			// status stays derived until the rollup at the end of the
			// transaction.
			if err := tx.Model(&Task{}).Where("id = ?", n.ID).
				Update("attempts", n.Attempts).Error; err != nil {
				return nil, err
			}
			continue
		}
		run := TestRun{
			TaskID:        n.ID,
			Attempt:       n.Attempts,
			Kind:          RunKindForTask(n.Kind),
			EnvironmentID: n.EnvironmentID,
			CommitID:      n.CommitID,
			Status:        StatusPending,
		}
		// Another report may have opened this very attempt between the read
		// above and this insert. The loser adopts the row instead of failing:
		// both reports describe the same attempt, and the unique index on
		// (task_id, attempt) must not turn a retry into a database error.
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&run)
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 0 {
			if err := tx.Where("task_id = ? AND attempt = ?", run.TaskID, run.Attempt).
				First(&run).Error; err != nil {
				return nil, err
			}
		}
		if err := tx.Model(&Task{}).Where("id = ?", n.ID).Updates(map[string]any{
			"attempts": n.Attempts,
			"status":   StatusPending,
			"summary":  "",
			"error":    "",
		}).Error; err != nil {
			return nil, err
		}
		n.Status = StatusPending
		n.Summary = ""
		n.Error = ""
		// The attempt's run replaces the task's cached counts.
		if err := tx.Model(&Task{}).Where("id = ?", n.ID).
			Updates(map[string]any{"total": 0, "passed": 0, "failed": 0, "skipped": 0}).Error; err != nil {
			return nil, err
		}
		n.Total, n.Passed, n.Failed, n.Skipped = 0, 0, 0, 0
		runs[i] = run
	}
	return runs, nil
}

// nextAttemptTx returns the attempt number to open for n: the number after the
// highest run on record, never at or below the node's own counter. The counter
// the caller holds may be stale (two reports for one task, a re-dispatch the
// caller has not seen), and an attempt number that is already taken would hit
// the unique index on (task_id, attempt).
func nextAttemptTx(tx *gorm.DB, n *Task) (int, error) {
	var highest int
	if err := tx.Model(&TestRun{}).Where("task_id = ?", n.ID).
		Select("COALESCE(MAX(attempt), 0)").Scan(&highest).Error; err != nil {
		return 0, err
	}
	if n.Attempts > highest {
		highest = n.Attempts
	}
	return highest + 1, nil
}

// getTaskTx loads one task inside a transaction.
func getTaskTx(tx *gorm.DB, id int64) (*Task, error) {
	var t Task
	if err := tx.First(&t, id).Error; err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTaskNotFound
		}
		return nil, err
	}
	return &t, nil
}

// listNodesTx returns every node of a graph except the root, retired ones
// included (the caller filters).
func listNodesTx(tx *gorm.DB, rootID int64) ([]Task, error) {
	var tasks []Task
	if err := tx.Where("root_id = ? AND id <> ?", rootID, rootID).Order("id ASC").
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// FindRootTaskByCommitEnv returns the root task for a (commit, environment)
// pair, or ErrTaskNotFound.
func (s *Store) FindRootTaskByCommitEnv(commitID, envID int64) (*Task, error) {
	return findRootTaskTx(s.DB, commitID, envID)
}

// GetTask loads one task by ID.
func (s *Store) GetTask(id int64) (*Task, error) {
	return getTaskTx(s.DB, id)
}

// ListActiveNodes returns the graph's current nodes (the ones the latest
// dispatch defined), in creation order, the root excluded.
func (s *Store) ListActiveNodes(rootID int64) ([]Task, error) {
	var tasks []Task
	if err := s.DB.Where("root_id = ? AND id <> ? AND retired = ?", rootID, rootID, false).
		Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListRetiredNodes returns the nodes a later dispatch dropped from the graph:
// history only — never scheduled, not rolled up into their parent.
func (s *Store) ListRetiredNodes(rootID int64) ([]Task, error) {
	var tasks []Task
	if err := s.DB.Where("root_id = ? AND id <> ? AND retired = ?", rootID, rootID, true).
		Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// ListChildren returns a node's direct children (the root's sub-tasks, a
// regression stage's cases), retired ones included, in creation order.
func (s *Store) ListChildren(parentID int64) ([]Task, error) {
	var tasks []Task
	if err := s.DB.Where("parent_id = ?", parentID).Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

// claimBatch is how many pending nodes the scheduler looks at in one round.
// The scan walks the whole pending set in batches of this size: a single fixed
// window would hide every ready node that sits behind enough queued ones —
// across the whole site, since the query is not scoped to one graph — and a
// node whose dependency never passes stays pending for ever, so the pending
// set only ever grows.
const claimBatch = 64

// ClaimReadyTask atomically claims the oldest pending real task whose
// dependencies all passed: readiness is checked in Go (DependsOn is JSON, not
// portable SQL), the claim itself is an optimistic UPDATE guarded on
// status='pending', so concurrent schedulers never double-claim. The claimed
// node's in-flight run flips to running in the same transaction. Returns nil
// when no ready task exists anywhere in the queued set.
func (s *Store) ClaimReadyTask() (*Task, error) {
	for after := int64(0); ; {
		var candidates []Task
		if err := s.DB.Where("virtual = ? AND retired = ? AND status = ? AND id > ?",
			false, false, StatusPending, after).
			Order("id ASC").Limit(claimBatch).Find(&candidates).Error; err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			return nil, nil
		}
		// Keyset paging: the next round continues after the last candidate,
		// so the walk ends and no row is examined twice.
		after = candidates[len(candidates)-1].ID
		ready, err := s.readyDeps(candidates)
		if err != nil {
			return nil, err
		}
		for i := range candidates {
			t := &candidates[i]
			if !ready[t.ID] {
				continue
			}
			now := time.Now()
			claimed := false
			err = s.DB.Transaction(func(tx *gorm.DB) error {
				// The attempt counter is part of the guard: a dispatch that
				// landed after the scan re-arms the node (pending again) and
				// supersedes the attempt this snapshot describes. Claiming it
				// would hand the worker a node it cannot report for — the run it
				// flips is already gone, and the report would land on the old
				// attempt while the node stays running for ever, with nothing
				// left able to claim it.
				res := tx.Model(&Task{}).Where("id = ? AND status = ? AND attempts = ?",
					t.ID, StatusPending, t.Attempts).
					Updates(map[string]any{
						"status":     StatusRunning,
						"started_at": now,
					})
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected == 0 {
					// Lost the race, or the node is not the one this snapshot
					// describes: either way the next tick reads it fresh.
					return nil
				}
				claimed = true
				// The attempt's run follows its task: the run page and the matrix
				// cell show the stage running from here on.
				if err := tx.Model(&TestRun{}).
					Where("task_id = ? AND attempt = ? AND status = ?", t.ID, t.Attempts, StatusPending).
					Updates(map[string]any{"status": StatusRunning, "started_at": now, "finished_at": nil}).Error; err != nil {
					return err
				}
				// Starting a node starts its containers too: every status
				// change of a real node leaves the virtual ones rolled up, so
				// a graph whose stage runs reads running rather than queued.
				return rollupTx(tx, t.RootID)
			})
			if err != nil {
				return nil, err
			}
			if !claimed {
				continue
			}
			t.Status = StatusRunning
			t.StartedAt = &now
			return t, nil
		}
	}
}

// readyDeps answers, for a batch of candidates, whether every dependency of
// each passed: a dependency that was skipped does not satisfy it (skipped
// means an upstream failure stopped it, so running this node would test a
// broken tree). One query covers the batch — the readiness check reads the
// same handful of dependency rows over and over as the scan walks the queued
// set, and its result is a property of those rows alone.
func (s *Store) readyDeps(candidates []Task) (map[int64]bool, error) {
	passed := map[int64]bool{}
	var ids []int64
	for i := range candidates {
		for _, d := range candidates[i].DependsOnIDs() {
			if _, seen := passed[d]; !seen {
				passed[d] = false
				ids = append(ids, d)
			}
		}
	}
	if len(ids) > 0 {
		var rows []Task
		if err := s.DB.Select("id", "status").Where("id IN ?", ids).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			passed[rows[i].ID] = rows[i].Status == StatusPassed
		}
	}
	out := make(map[int64]bool, len(candidates))
	for i := range candidates {
		ok := true
		for _, d := range candidates[i].DependsOnIDs() {
			if !passed[d] {
				ok = false
				break
			}
		}
		out[candidates[i].ID] = ok
	}
	return out, nil
}

// SkipTask marks one node skipped for the given reason and closes its
// in-flight attempt the same way (a skipped node never ran, so its run reads
// skipped, not failed). The task must be pending — a running or terminal task
// is left alone.
//
// A skipped node is not an outcome the nodes behind it can wait on: they are
// skipped in the same transaction, exactly as they are after a failed report
// (FinishAttempt), so nothing is left in the queue that can never be claimed.
func (s *Store) SkipTask(taskID int64, reason string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		task, err := getTaskTx(tx, taskID)
		if err != nil {
			return err
		}
		if task.Status != StatusPending {
			return nil
		}
		if err := skipTasksTx(tx, []skipEntry{{task: task, reason: reason}}); err != nil {
			return err
		}
		if err := skipDependentsTx(tx, task.RootID, task.ID, reason); err != nil {
			return err
		}
		// The containers above it follow, exactly as they do after a report
		// (FinishAttempt) or a cascade (SkipDependents): every status change of
		// a real node leaves the virtual ones rolled up.
		return rollupTx(tx, task.RootID)
	})
}

// skipTasksTx marks nodes skipped and closes the attempt each is on. The
// callers hold a snapshot of the graph (the cascade walks it before opening
// its transaction), but nothing here trusts it: the status and the attempt
// number are re-read inside the transaction, and the write itself is guarded
// on the row still being pending, so a node the scheduler claimed — or a
// re-dispatch that opened a new attempt — since the snapshot keeps its own
// work and its own run. Skipping a running node would close the attempt that
// is executing and leave the report of the goroutine that ran it landing on
// an attempt it never touched.
func skipTasksTx(tx *gorm.DB, entries []skipEntry) error {
	now := time.Now()
	for i := range entries {
		t, reason := entries[i].task, entries[i].reason
		cur, err := getTaskTx(tx, t.ID)
		if err != nil {
			if errors.Is(err, ErrTaskNotFound) {
				continue
			}
			return err
		}
		if cur.Status != StatusPending {
			continue
		}
		res := tx.Model(&Task{}).Where("id = ? AND status = ?", cur.ID, StatusPending).Updates(map[string]any{
			"status":      StatusSkipped,
			"summary":     reason,
			"error":       "",
			"finished_at": now,
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			continue // claimed between the read and the update
		}
		t.Status = StatusSkipped
		t.Summary = reason
		t.FinishedAt = &now
		t.Attempts = cur.Attempts
		if cur.Virtual {
			continue
		}
		var run TestRun
		if err := tx.Where("task_id = ? AND attempt = ?", cur.ID, cur.Attempts).First(&run).Error; err != nil {
			if errors.Is(err, ErrNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
				continue // no attempt was ever opened for this node
			}
			return err
		}
		if err := tx.Model(&TestRun{}).Where("id = ? AND status IN ?", run.ID,
			[]string{StatusPending, StatusRunning}).
			Updates(map[string]any{
				"status":      StatusSkipped,
				"summary":     reason,
				"started_at":  now,
				"finished_at": now,
			}).Error; err != nil {
			return err
		}
		// The reason is the node's and the run's summary and nothing else: a
		// stage that never ran produced no output, so it has no log (the run's
		// log is what the runner writes, and no runner ever saw this attempt).
		// The task page shows the summary beside the empty log.
	}
	return nil
}

// SkipDependents marks every pending node of rootID that (transitively)
// depends on failedID as skipped, with the given reason. Nodes already
// running or terminal are left alone, as are retired nodes (they are history)
// and the virtual containers (the rollup derives those).
func (s *Store) SkipDependents(rootID, failedID int64, reason string) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return skipDependentsTx(tx, rootID, failedID, reason)
	})
}

// skipEntry is one node to mark skipped, with the reason it carries: the same
// cascade can have several origins (a cancellation drops a whole container, and
// every node of it blocks what stands behind it), and a node names the origin
// that reached it rather than "something upstream".
type skipEntry struct {
	task   *Task
	reason string
}

// skipDependentsTx is SkipDependents' body, callable inside a transaction the
// caller already owns: a report that ends a node (FinishAttempt) skips what
// stands behind it in the very transaction that records the outcome, so no
// reader can see a failed node whose dependents are still queued.
func skipDependentsTx(tx *gorm.DB, rootID, failedID int64, reason string) error {
	skipped, err := blockedTx(tx, rootID, map[int64]string{failedID: reason})
	if err != nil {
		return err
	}
	if len(skipped) == 0 {
		return nil
	}
	if err := skipTasksTx(tx, skipped); err != nil {
		return err
	}
	return rollupTx(tx, rootID)
}

// blockedTx walks the graph's reverse dependency edges from the given origins —
// the nodes that just ended without passing, or the ones a cancellation just
// dropped — and returns the pending real nodes that can no longer run, each with
// the reason of the origin that reached it first. Nodes already running or
// terminal are left alone, as are the retired ones (history) and the virtual
// containers (the rollup derives those). The walk is transitive: what stands
// behind a blocked node is blocked too, and carries the same origin's reason.
func blockedTx(tx *gorm.DB, rootID int64, origins map[int64]string) ([]skipEntry, error) {
	var tasks []Task
	if err := tx.Where("root_id = ? AND id <> ? AND virtual = ? AND retired = ?",
		rootID, rootID, false, false).Order("id ASC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	// BFS from the origins over the reverse dependency edges.
	blocked := map[int64]string{}
	for id, reason := range origins {
		blocked[id] = reason
	}
	changed := true
	for changed {
		changed = false
		for i := range tasks {
			t := &tasks[i]
			if _, done := blocked[t.ID]; done || t.Status != StatusPending {
				continue
			}
			for _, d := range t.DependsOnIDs() {
				if reason, ok := blocked[d]; ok {
					blocked[t.ID] = reason
					changed = true
					break
				}
			}
		}
	}
	var skipped []skipEntry
	for i := range tasks {
		// An origin is where the walk started, not one of its dependents:
		// ending it is the caller's business.
		if _, isOrigin := origins[tasks[i].ID]; isOrigin {
			continue
		}
		if reason, ok := blocked[tasks[i].ID]; ok && tasks[i].Status == StatusPending {
			skipped = append(skipped, skipEntry{task: &tasks[i], reason: reason})
		}
	}
	return skipped, nil
}

// maxReasonLen caps the reason a skipped node carries, matching the runner's
// cap on the summaries it writes (runner.maxSummaryLen): both end up in the
// same summary column and the same log line.
const maxReasonLen = 500

// FailureReason is the reason a node's dependents carry once the node ended
// without passing: which task stopped them and why. A timeout says so: the
// stages behind it were stopped by the clock, not by a failing assertion, and a
// cancellation says so too: nobody's assertion failed, the work was dropped.
func FailureReason(task *Task, res AttemptResult) string {
	reason := "upstream task " + task.Name
	switch res.Status {
	case StatusSkipped:
		reason += " was skipped"
	case StatusTimeout:
		reason += " timed out"
	case StatusCancelled:
		reason += " was cancelled"
	default:
		reason += " failed"
	}
	if res.Error != "" {
		reason += ": " + res.Error
	}
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}
	return reason
}

// RollupTaskTree recomputes every virtual node of the tree from its
// non-retired children, bottom-up (the root last). Real nodes are left
// alone: their status and counts are their latest attempt's.
func (s *Store) RollupTaskTree(rootID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		return rollupTx(tx, rootID)
	})
}

// rollupTx is RollupTaskTree's body: it walks the tree depth-first and
// rewrites each container from its children.
func rollupTx(tx *gorm.DB, rootID int64) error {
	nodes, err := listNodesTx(tx, rootID)
	if err != nil {
		return err
	}
	root, err := getTaskTx(tx, rootID)
	if err != nil {
		return err
	}
	children := map[int64][]*Task{}
	for i := range nodes {
		if nodes[i].Retired {
			continue // history: not part of the current graph's state
		}
		children[nodes[i].ParentID] = append(children[nodes[i].ParentID], &nodes[i])
	}
	var walk func(node *Task) error
	walk = func(node *Task) error {
		kids := children[node.ID]
		for _, kid := range kids {
			if kid.Virtual {
				if err := walk(kid); err != nil {
					return err
				}
			}
		}
		if !node.Virtual {
			return nil
		}
		r := rollupChildren(node.Kind, kids)
		changed := node.Status != r.Status || node.Summary != r.Summary ||
			node.Total != r.Total || node.Passed != r.Passed ||
			node.Failed != r.Failed || node.Skipped != r.Skipped ||
			!sameTime(node.StartedAt, r.StartedAt) || !sameTime(node.FinishedAt, r.FinishedAt)
		node.Status, node.Summary = r.Status, r.Summary
		node.Total, node.Passed, node.Failed, node.Skipped = r.Total, r.Passed, r.Failed, r.Skipped
		node.StartedAt, node.FinishedAt = r.StartedAt, r.FinishedAt
		if !changed {
			return nil
		}
		return tx.Model(&Task{}).Where("id = ?", node.ID).Updates(map[string]any{
			"status":      r.Status,
			"summary":     r.Summary,
			"total":       r.Total,
			"passed":      r.Passed,
			"failed":      r.Failed,
			"skipped":     r.Skipped,
			"started_at":  r.StartedAt,
			"finished_at": r.FinishedAt,
		}).Error
	}
	return walk(root)
}

// virtualRollup is a container's derived state.
type virtualRollup struct {
	Status     string
	Summary    string
	Total      int
	Passed     int
	Failed     int
	Skipped    int
	StartedAt  *time.Time
	FinishedAt *time.Time
}

// rollupChildren derives a container's state from its children. The rules:
// any failed child fails the container; a container whose failures are all
// timeouts takes the timeout status instead (the cause is the point); otherwise
// a child already running makes it running; otherwise a queued child keeps it
// pending; otherwise a cancelled child (a policy stopped the graph) makes the
// container cancelled; otherwise a skipped child (an upstream failure stopped
// it) makes the container skipped; everything else passed.
//
// A cancelled child counts in the skipped tally rather than one of its own: it
// did not run and it did not pass, so the counts keep adding up, and the
// status word is what carries "cancelled" to the dashboard. A timed-out child counts in the failed tally: it is not a pass either, and
// the counts column has to add up. A real child counts as one unit; a container
// child (the root's regression stage) contributes its own children's tally
// instead of itself, so a container counts the leaves under it and never a
// wrapper. Timestamps aggregate, so a container answers "how did this stage
// do?" without a join.
func rollupChildren(kind string, kids []*Task) virtualRollup {
	noun := "stages"
	if kind == TaskKindRegressionStage {
		noun = "cases"
	}
	out := virtualRollup{Status: StatusPending}
	if len(kids) == 0 {
		// A container with nothing under it has nothing to report; leave it
		// pending rather than inventing a pass.
		out.Summary = "no " + noun
		return out
	}
	var failedNames, timeoutNames, cancelledNames []string
	runningKids, pendingKids, skippedKids := 0, 0, 0
	failedKids, timeoutKids, cancelledKids := 0, 0, 0
	for _, kid := range kids {
		if kid.Virtual {
			out.Total += kid.Total
			out.Passed += kid.Passed
			out.Failed += kid.Failed
			out.Skipped += kid.Skipped
		} else {
			out.Total++
			switch kid.Status {
			case StatusPassed:
				out.Passed++
			case StatusFailed, StatusTimeout:
				out.Failed++
			case StatusSkipped, StatusCancelled:
				// Both ended without running to a verdict; cancelled has no
				// tally of its own (the status word carries it).
				out.Skipped++
			}
		}
		switch kid.Status {
		case StatusRunning:
			runningKids++
		case StatusPending:
			pendingKids++
		case StatusFailed:
			failedKids++
			failedNames = append(failedNames, kid.Name)
		case StatusTimeout:
			timeoutKids++
			timeoutNames = append(timeoutNames, kid.Name)
		case StatusCancelled:
			cancelledKids++
			cancelledNames = append(cancelledNames, kid.Name)
		case StatusSkipped:
			skippedKids++
		}
		out.StartedAt = earlierTime(out.StartedAt, kid.StartedAt)
		out.FinishedAt = laterTime(out.FinishedAt, kid.FinishedAt)
	}
	switch {
	case timeoutKids > 0 && failedKids == 0:
		out.Status = StatusTimeout
		out.Summary = fmt.Sprintf("%d/%d %s passed; timed out: %s",
			out.Passed, out.Total, noun, nameList(timeoutNames))
	case out.Failed > 0:
		out.Status = StatusFailed
		summary := fmt.Sprintf("%d/%d %s passed; failed: %s",
			out.Passed, out.Total, noun, nameList(failedNames))
		if timeoutKids > 0 {
			// A mix of both causes: the container reads "failed", and the
			// timeouts are still named rather than swallowed by the word.
			summary += "; timed out: " + nameList(timeoutNames)
		}
		out.Summary = summary
	case runningKids > 0:
		out.Status = StatusRunning
		out.Summary = fmt.Sprintf("%d/%d %s passed; %d in progress",
			out.Passed, out.Total, noun, runningKids)
	case pendingKids > 0:
		// Nothing has been claimed yet: a container is queued, not running,
		// until one of its children actually starts (the dashboard cell for a
		// freshly dispatched commit says "pending").
		out.Status = StatusPending
		out.Summary = fmt.Sprintf("%d/%d %s passed; %d queued",
			out.Passed, out.Total, noun, pendingKids)
	case cancelledKids > 0:
		out.Status = StatusCancelled
		out.Summary = fmt.Sprintf("%d/%d %s passed; cancelled: %s",
			out.Passed, out.Total, noun, nameList(cancelledNames))
	case skippedKids > 0:
		out.Status = StatusSkipped
		out.Summary = fmt.Sprintf("%d/%d %s skipped (upstream failure)",
			out.Skipped, out.Total, noun)
	default:
		out.Status = StatusPassed
		out.Summary = fmt.Sprintf("%d/%d %s passed", out.Passed, out.Total, noun)
	}
	return out
}

// nameList renders the child names of a rollup summary, capped at three with
// an ellipsis so a wide graph does not turn one cell into a paragraph.
func nameList(names []string) string {
	if len(names) > 3 {
		return strings.Join(names[:3], ", ") + ", …"
	}
	return strings.Join(names, ", ")
}

// earlierTime/laterTime fold a child's timestamps into a container's window.
// A nil child bound does not constrain it.
func earlierTime(cur, v *time.Time) *time.Time {
	if v == nil {
		return cur
	}
	if cur == nil || v.Before(*cur) {
		return v
	}
	return cur
}

func laterTime(cur, v *time.Time) *time.Time {
	if v == nil {
		return cur
	}
	if cur == nil || v.After(*cur) {
		return v
	}
	return cur
}

func sameTime(a, b *time.Time) bool {
	switch {
	case a == nil && b == nil:
		return true
	case a == nil || b == nil:
		return false
	default:
		return a.Equal(*b)
	}
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

// RootTaskSummary is a root task plus its current nodes, as returned by
// FindRootGraphsByCommits.
type RootTaskSummary struct {
	Root *Task
	Subs []Task
}

// FindRootGraphsByCommits returns the root tasks for the given environments
// and commits, with their ACTIVE nodes (retired ones belong to history, not
// to the current graph), keyed by (environment, commit).
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
	byRoot := map[int64]EnvCommit{}
	for i := range roots {
		ids[i] = roots[i].ID
		key := EnvCommit{Env: roots[i].EnvironmentID, Commit: roots[i].CommitID}
		byRoot[roots[i].ID] = key
		out[key] = RootTaskSummary{Root: &roots[i]}
	}
	var subs []Task
	if err := s.DB.Where("root_id IN ? AND id <> root_id AND retired = ?", ids, false).
		Order("id ASC").Find(&subs).Error; err != nil {
		return nil, err
	}
	for i := range subs {
		key, ok := byRoot[subs[i].RootID]
		if !ok {
			continue
		}
		s := out[key]
		s.Subs = append(s.Subs, subs[i])
		out[key] = s
	}
	return out, nil
}

// ResetStaleRunning marks any node left in "running" after a crash back to
// "pending" and its in-flight run with it, so the attempt is retried (as the
// same attempt) on the next scheduler cycle. Called at startup. Returns the
// number of reset rows.
func (s *Store) ResetStaleRunning() (int64, error) {
	var n int64
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		// The graphs that are reset: their containers were rolled up from the
		// running nodes and have to follow them back to queued.
		var roots []int64
		if err := tx.Model(&Task{}).Where("status = ? AND kind <> ?", StatusRunning, TaskKindRoot).
			Distinct().Pluck("root_id", &roots).Error; err != nil {
			return err
		}
		res := tx.Model(&Task{}).Where("status = ?", StatusRunning).
			Updates(map[string]any{
				"status":     StatusPending,
				"started_at": nil,
				"error":      "reset after restart",
			})
		if res.Error != nil {
			return res.Error
		}
		n = res.RowsAffected
		// The runs follow their tasks: a run whose attempt was interrupted
		// goes back to pending and is picked up by the next claim. (Its log
		// continues from the last stored sequence, so nothing is lost.)
		if err := tx.Model(&TestRun{}).Where("status = ?", StatusRunning).
			Updates(map[string]any{"status": StatusPending, "started_at": nil, "finished_at": nil}).Error; err != nil {
			return err
		}
		for _, rootID := range roots {
			if rootID == 0 {
				continue
			}
			// A node retired while it was running has no worker left behind
			// after the restart, and nothing will ever claim a retired node:
			// its attempt is closed rather than handed back as queued.
			if err := retireGhostAttemptsTx(tx, rootID, true); err != nil {
				return err
			}
			if err := rollupTx(tx, rootID); err != nil {
				return err
			}
		}
		return nil
	})
	return n, err
}

// DeleteTasksForEnvironment removes all tasks of an environment with their
// runs, artifacts and logs, in a transaction. Called when an environment is
// deleted.
func (s *Store) DeleteTasksForEnvironment(envID int64) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var taskIDs []int64
		if err := tx.Model(&Task{}).Where("environment_id = ?", envID).
			Pluck("id", &taskIDs).Error; err != nil {
			return err
		}
		var runIDs []int64
		if err := tx.Model(&TestRun{}).Where("environment_id = ?", envID).
			Pluck("id", &runIDs).Error; err != nil {
			return err
		}
		for _, chunk := range chunkIDs(runIDs, 500) {
			if err := tx.Where("run_id IN ?", chunk).Delete(&TestArtifact{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("environment_id = ?", envID).Delete(&TestRun{}).Error; err != nil {
			return err
		}
		return tx.Where("environment_id = ?", envID).Delete(&Task{}).Error
	})
}

// chunkIDs splits ids into batches (SQLite and PostgreSQL both cap how many
// bind parameters one statement may carry).
func chunkIDs(ids []int64, size int) [][]int64 {
	var out [][]int64
	for len(ids) > size {
		out = append(out, ids[:size])
		ids = ids[size:]
	}
	if len(ids) > 0 {
		out = append(out, ids)
	}
	return out
}
