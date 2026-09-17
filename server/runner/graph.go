package runner

import (
	"fmt"

	"md-builder/server/store"
)

// Task graph construction: a merged matrix entry becomes a small DAG of
// sub-tasks under a root task. The shape is data-driven (build is optional;
// test stages depend on build when present, else on clone) so future kinds
// (performance tests) only extend the node list.

// GraphTask is one node of the constructed graph before persistence: its
// kind, display name, per-stage config snapshot and dependencies expressed
// as graph placeholders (see store.CreateTaskGraph).
type GraphTask struct {
	Kind    string
	Name    string
	Config  string // JSON snapshot of the stage config
	Deps    []int64
	Timeout int // resolved stage timeout in seconds (0 = entry default)
}

// BuildTaskGraph converts a merged matrix entry into the task list to
// persist under a root: index 0 is the clone task (always present), the
// remaining tasks follow in dependency order. Dependencies reference earlier
// entries by store.TaskSubPlaceholderBase + index (never the root: the root
// is a container whose status is derived from its sub-tasks, and
// store.CreateTaskGraph rejects root edges).
func BuildTaskGraph(entry *MergedEntry) ([]GraphTask, error) {
	if entry == nil {
		return nil, fmt.Errorf("no entry config")
	}

	var tasks []GraphTask

	// 0: clone — the server fetches and uploads both repositories. No
	// dependencies: the root is a container, not a gate (it only reaches a
	// terminal state once every sub-task has), so clone must not depend on
	// it. No per-stage config today; the snapshot stays a hook for future
	// options.
	tasks = append(tasks, GraphTask{
		Kind:   store.TaskKindClone,
		Name:   "clone repositories",
		Config: "{}",
	})

	// 1: build — optional (a code tree that builds itself, or a manual
	// dispatch without a build stage, skips it). The full entry snapshot is
	// stored so the executor can export env vars. Test stages depend on
	// build when present, else on clone.
	testDep := store.TaskSubPlaceholderBase + 0 // clone
	if !entry.Build.Command.IsEmpty() {
		buildJSON, err := marshalJSON(BuildStageConfig{
			Command:   entry.Build.Command.Clean(),
			Workdir:   entry.Build.Workdir,
			Artifacts: entry.Build.Artifacts.Clean(),
			Timeout:   resolveStageTimeout(0, entry),
			Env:       entry.Env,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:   store.TaskKindBuild,
			Name:   "build",
			Config: buildJSON,
			Deps:   []int64{store.TaskSubPlaceholderBase + 0},
		})
		testDep = store.TaskSubPlaceholderBase + 1 // build
	}

	// 2+: test stages, each depending on build (or clone). Regression
	// expands to one sub-task per case (preset) — independent logs,
	// parallel execution and per-case cells on the graph page.
	if entry.Unit != nil {
		cfg, err := marshalJSON(StageConfig{
			Command:   entry.Unit.Command.Clean(),
			Workdir:   entry.Unit.Workdir,
			Timeout:   resolveStageTimeout(entry.Unit.Timeout, entry),
			Env:       entry.Env,
			Artifacts: entry.Unit.Artifacts,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:   store.TaskKindUnit,
			Name:   "unit tests",
			Config: cfg,
			Deps:   []int64{testDep},
		})
	}
	for i := range entry.Regression {
		c := &entry.Regression[i]
		cfg, err := marshalJSON(CaseStageConfig{
			Case:      c.Name,
			Command:   c.Command.Clean(),
			Workdir:   c.Workdir,
			Timeout:   resolveStageTimeout(c.Timeout, entry),
			Env:       entry.Env,
			Artifacts: c.Artifacts,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:   store.TaskKindRegression,
			Name:   "regression: " + c.Name,
			Config: cfg,
			Deps:   []int64{testDep},
		})
	}
	if len(tasks) == 1 {
		return nil, fmt.Errorf("entry defines no stages (unit/regression/build)")
	}
	return tasks, nil
}
