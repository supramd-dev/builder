package runner

import (
	"fmt"

	"md-builder/server/store"
)

// Task graph construction: a merged matrix entry becomes a small tree of
// tasks under a root. The shape is data-driven (build is optional; test
// stages depend on build when present, else on clone) so future kinds
// (performance tests) only extend the node list.
//
//	root (virtual)
//	 ├── clone
//	 ├── build (optional)
//	 ├── unit (optional)
//	 └── regression (virtual; only with regression cases)
//	      ├── regression_case "<name 1>"
//	      └── regression_case "<name 2>"

// RegressionStageKey is the node key of the virtual regression container. A
// case's key is this prefix plus the preset name, so the keys survive a
// re-dispatch and a case that disappears is retired while the others keep
// their history.
const RegressionStageKey = store.TaskKindRegressionStage

// RegressionCaseKey builds the node key of one regression case.
func RegressionCaseKey(name string) string {
	return store.TaskKindRegressionStage + ":" + name
}

// GraphTask is one node of the constructed graph before persistence: its
// kind, its stable node key (what a re-dispatch matches on), its display
// labels, the per-stage config snapshot, its parent's key and its
// dependencies expressed as graph placeholders (see store.UpsertTaskGraph).
type GraphTask struct {
	Kind        string
	NodeKey     string
	Name        string
	Description string // human label from md-builder.yaml ("" when absent)
	Config      string // JSON snapshot of the stage config
	Deps        []int64
	ParentKey   string // "" = the root
}

// BuildTaskGraph converts a merged matrix entry into the node list to
// persist under a root, in creation order: index 0 is the clone task (always
// present), and a node's dependencies reference earlier entries by
// store.TaskSubPlaceholderBase + index. No node depends on the root or on the
// virtual regression container: a container's status is derived from its
// children, so an edge to it could never become ready
// (store.UpsertTaskGraph rejects both).
func BuildTaskGraph(entry *MergedEntry) ([]GraphTask, error) {
	if entry == nil {
		return nil, fmt.Errorf("no entry config")
	}

	var tasks []GraphTask

	// 0: clone — the server fetches and uploads both repositories.
	tasks = append(tasks, GraphTask{
		Kind:    store.TaskKindClone,
		NodeKey: store.TaskKindClone,
		Name:    "clone repositories",
		Config:  "{}",
	})

	// 1: build — optional (a code tree that builds itself, or a manual
	// dispatch without a build stage, skips it). The full entry snapshot is
	// stored so the executor can export env vars. Test stages depend on
	// build when present, else on clone.
	testDep := store.TaskSubPlaceholderBase + 0 // clone
	if !entry.Build.Command.IsEmpty() {
		buildJSON, err := marshalJSON(BuildStageConfig{
			Command:     entry.Build.Command.Clean(),
			Description: entry.Build.Description,
			Workdir:     entry.Build.Workdir,
			Artifacts:   entry.Build.Artifacts.Clean(),
			Timeout:     resolveStageTimeout(0, entry),
			Env:         entry.Env,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:        store.TaskKindBuild,
			NodeKey:     store.TaskKindBuild,
			Name:        "build",
			Description: entry.Build.Description,
			Config:      buildJSON,
			Deps:        []int64{store.TaskSubPlaceholderBase + 0},
		})
		testDep = store.TaskSubPlaceholderBase + 1 // build
	}

	// 2: unit — optional.
	if entry.Unit != nil {
		cfg, err := marshalJSON(StageConfig{
			Command:     entry.Unit.Command.Clean(),
			Description: entry.Unit.Description,
			Workdir:     entry.Unit.Workdir,
			Timeout:     resolveStageTimeout(entry.Unit.Timeout, entry),
			Env:         entry.Env,
			Artifacts:   entry.Unit.Artifacts,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:        store.TaskKindUnit,
			NodeKey:     store.TaskKindUnit,
			Name:        "unit tests",
			Description: entry.Unit.Description,
			Config:      cfg,
			Deps:        []int64{testDep},
		})
	}

	// 3+: the regression stage and its cases. The container is virtual (it
	// runs nothing): its status and counts are the cases' aggregate, and each
	// case is a task of its own with its own log, attempt and artifacts — so
	// the cases run in parallel and keep their history across dispatches.
	if len(entry.Regression) > 0 {
		tasks = append(tasks, GraphTask{
			Kind:        store.TaskKindRegressionStage,
			NodeKey:     RegressionStageKey,
			Name:        "regression",
			Description: entry.RegressionDescription,
			Config:      "{}",
		})
		seen := make(map[string]bool, len(entry.Regression))
		for i := range entry.Regression {
			c := &entry.Regression[i]
			key := RegressionCaseKey(c.Name)
			if seen[key] {
				// Two cases with one name would fight over one node key (the
				// identity a re-dispatch matches on): reject the graph rather
				// than silently merging them.
				return nil, fmt.Errorf("md-builder.yaml: duplicate regression case name %q", c.Name)
			}
			seen[key] = true
			cfg, err := marshalJSON(CaseStageConfig{
				Case:        c.Name,
				Description: c.Description,
				Command:     c.Command.Clean(),
				Workdir:     c.Workdir,
				Timeout:     resolveStageTimeout(c.Timeout, entry),
				Env:         entry.Env,
				Artifacts:   c.Artifacts,
			})
			if err != nil {
				return nil, err
			}
			tasks = append(tasks, GraphTask{
				Kind:        store.TaskKindRegressionCase,
				NodeKey:     key,
				Name:        "regression: " + c.Name,
				Description: c.Description,
				Config:      cfg,
				Deps:        []int64{testDep},
				ParentKey:   RegressionStageKey,
			})
		}
	}
	if len(tasks) == 1 {
		return nil, fmt.Errorf("entry defines no stages (unit/regression/build)")
	}
	return tasks, nil
}
