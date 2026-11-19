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
//	 ├── performance (optional)
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

// regressionWindow is the window shown on the virtual regression container:
// the one window every case of the entry shares. Cases that carry a window of
// their own (a preset's) are scheduled by that one, so the container shows
// nothing rather than a value that is true for only some of them.
func regressionWindow(entry *MergedEntry) string {
	var shared string
	for i := range entry.Regression {
		c := entry.Regression[i].Window
		if i == 0 {
			shared = c
			continue
		}
		if c != shared {
			return ""
		}
	}
	return shared
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
	// Window is the stage's scheduling window as the yaml spells it ("" =
	// any time). It rides on the node row rather than in Config so the
	// scheduler reads it straight off the claimed candidate.
	Window    string
	Deps      []int64
	ParentKey string // "" = the root
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
			Window:      entry.Build.Window,
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
			Window:      entry.Unit.Window,
			Deps:        []int64{testDep},
		})
	}

	// 3: performance — optional, and like unit it waits for the build rather
	// than joining the regression fan-out. Its window is the reason this stage
	// exists as a kind of its own: a benchmark is load-sensitive, so the
	// common case is a window that keeps it on the machines overnight.
	if entry.Performance != nil {
		cfg, err := marshalJSON(StageConfig{
			Command:     entry.Performance.Command.Clean(),
			Description: entry.Performance.Description,
			Workdir:     entry.Performance.Workdir,
			Timeout:     resolveStageTimeout(entry.Performance.Timeout, entry),
			Env:         entry.Env,
			Artifacts:   entry.Performance.Artifacts,
		})
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, GraphTask{
			Kind:        store.TaskKindPerformance,
			NodeKey:     store.TaskKindPerformance,
			Name:        "performance tests",
			Description: entry.Performance.Description,
			Config:      cfg,
			Window:      entry.Performance.Window,
			Deps:        []int64{testDep},
		})
	}

	// 4+: the regression stage and its cases. The container is virtual (it
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
			// The container never runs, so its window is display only: it
			// shows the entry's regression.window next to the stage, while
			// each case carries the window it is actually scheduled by.
			Window: regressionWindow(entry),
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
				Window:      c.Window,
				Deps:        []int64{testDep},
				ParentKey:   RegressionStageKey,
			})
		}
	}
	if len(tasks) == 1 {
		return nil, fmt.Errorf("entry defines no stages (unit/performance/regression/build)")
	}
	return tasks, nil
}
