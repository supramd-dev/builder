package runner

import (
	"fmt"

	"md-builder/server/store"
)

// Task graph construction: a merged matrix entry becomes a small DAG of
// sub-tasks under a root task. The graph shape is fixed today
// (clone → build → {unit, regression}) but the construction is data-driven
// so future kinds (performance tests) only extend the node list.

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
// remaining tasks follow in dependency order. Dependencies reference the
// root (store.TaskRootPlaceholder) and earlier entries by
// store.TaskSubPlaceholderBase + index.
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

	// 1: build — required (the test stages run in the code directory it
	// produces). The full entry snapshot is stored so the executor can
	// export env vars and pick the build recipe.
	buildJSON, err := marshalJSON(BuildStageConfig{
		Generator:  entry.Build.Generator,
		CMakeFlags: entry.Build.CMakeFlags,
		Threads:    entry.Build.Threads,
		Command:    entry.Build.Command,
		Timeout:    resolveStageTimeout(nil, entry),
		Env:        entry.Env,
	})
	if err != nil {
		return nil, err
	}
	tasks = append(tasks, GraphTask{
		Kind:   store.TaskKindBuild,
		Name:   fmt.Sprintf("build (%s)", entry.Build.Generator),
		Config: buildJSON,
		Deps:   []int64{store.TaskSubPlaceholderBase + 0},
	})

	// 2+: test stages, each depending on build.
	if entry.Unit != nil {
		cfg, err := stageConfig(entry, entry.Unit)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:   store.TaskKindUnit,
			Name:   "unit tests",
			Config: cfg,
			Deps:   []int64{store.TaskSubPlaceholderBase + 1},
		})
	}
	if entry.Regression != nil {
		cfg, err := stageConfig(entry, entry.Regression)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:   store.TaskKindRegression,
			Name:   "regression tests",
			Config: cfg,
			Deps:   []int64{store.TaskSubPlaceholderBase + 1},
		})
	}
	if len(tasks) == 1 {
		return nil, fmt.Errorf("entry defines no stages (unit/regression/build)")
	}
	return tasks, nil
}

// stageConfig marshals the per-stage snapshot: command, timeout and the
// entry's environment (the script exports it).
func stageConfig(entry *MergedEntry, stage *EnvConfig) (string, error) {
	cfg := StageConfig{
		Command: stage.Command,
		Timeout: resolveStageTimeout(stage, entry),
		Env:     entry.Env,
	}
	return marshalJSON(cfg)
}
