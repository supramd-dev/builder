package runner

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"md-builder/server/store"
)

// TestSchedulerRunsFullGraph starts the real scheduling pool over fake
// transport/clone fakes and waits for the whole graph to complete.
func TestSchedulerRunsFullGraph(t *testing.T) {
	svc, s, exec, _, _ := newExecuteFixture(t, execYAML)
	svc.Workers = 2

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	// All four sub-tasks complete within the first poll tick (the loop
	// drains until the queue is empty), so the root reaches done.
	deadline := time.Now().Add(15 * time.Second)
	roots, _ := s.ListRootTasks(10)
	if len(roots) != 1 {
		t.Fatalf("roots: %d", len(roots))
	}
	for time.Now().Before(deadline) {
		root, err := s.GetTask(roots[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if root.Status == store.StatusPassed || root.Status == store.StatusFailed {
			if root.Status != store.StatusPassed {
				subs, _ := s.ListActiveNodes(root.ID)
				var detail []string
				for _, sub := range subs {
					detail = append(detail, sub.Kind+"/"+sub.Status+": "+sub.Error)
				}
				t.Fatalf("root failed: %s\n%s", root.Error, strings.Join(detail, "\n"))
			}
			// Full chain ran: build + unit + both case scripts, in order.
			if len(exec.scripts) != 4 {
				t.Fatalf("want 4 scripts, got %d", len(exec.scripts))
			}
			subs, _ := s.ListActiveNodes(root.ID)
			for _, sub := range subs {
				if sub.Status != store.StatusPassed {
					t.Errorf("%s not done: %s", sub.Kind, sub.Status)
				}
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("root did not finish in time")
}

// TestSchedulerSkipsOnFailure makes the build fail and asserts the scheduler
// skips the test stages, records skipped runs and fails the root.
func TestSchedulerSkipsOnFailure(t *testing.T) {
	svc, s, exec, _, _ := newExecuteFixture(t, execYAML)
	exec.outcome["cmake -DEXEC=1"] = 1
	exec.output["cmake -DEXEC=1"] = "CMake Error: bad flag\n"
	svc.Workers = 1

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	roots, _ := s.ListRootTasks(10)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		root, err := s.GetTask(roots[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if root.Status == store.StatusFailed {
			subs, _ := s.ListActiveNodes(root.ID)
			var unitState string
			var regStage *store.Task
			regStates := map[string]int{}
			for _, sub := range subs {
				switch sub.Kind {
				case store.TaskKindBuild:
					if sub.Status != store.StatusFailed || !strings.Contains(sub.Error, "exit 1") {
						t.Errorf("build state: %s / %q", sub.Status, sub.Error)
					}
				case store.TaskKindUnit:
					unitState = sub.Status
				case store.TaskKindRegressionStage:
					stage := sub
					regStage = &stage
				case store.TaskKindRegressionCase:
					regStates[sub.Status]++
				}
			}
			if unitState != store.StatusSkipped {
				t.Errorf("unit should be skipped: %s", unitState)
			}
			if regStates[store.StatusSkipped] != 2 {
				t.Errorf("both regression cases should be skipped: %v", regStates)
			}
			if regStage == nil || regStage.Status != store.StatusSkipped {
				t.Errorf("the regression container should be skipped: %+v", regStage)
			}
			// The skipped stages got their dashboard rows too, marked skipped
			// with the reason that stopped them.
			runs, _ := runsByKey(s, &roots[0])
			run := runs[store.TaskKindUnit]
			if run == nil || run.Status != store.StatusSkipped || !strings.Contains(run.Summary, "build") {
				t.Errorf("skipped unit run wrong: %+v", run)
			}
			return
		}
		if root.Status == store.StatusPassed {
			t.Fatal("root should be failed")
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("root did not finish in time")
}

// TestNewServiceWorkersDefault checks that NewService leaves the pool size
// unset: the configuration (worker.count / MD_BUILDER_WORKERS) is what fills
// it in, and 0 means the package default.
func TestNewServiceWorkersDefault(t *testing.T) {
	s := openTestStore(t)

	if svc := NewService(s); svc.Workers != 0 {
		t.Errorf("workers: want 0 (default), got %d", svc.Workers)
	}
}

// caseStatuses returns the regression cases' statuses keyed by case name —
// the rows the run page's case list, the case detail page and the graph all
// read.
func caseStatuses(s *store.Store, task *store.Task) (map[string]string, error) {
	stage, err := regressionStage(s, task)
	if err != nil {
		return nil, err
	}
	children, err := s.ListChildren(stage.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for i := range children {
		out[strings.TrimPrefix(children[i].NodeKey, RegressionStageKey+":")] = children[i].Status
	}
	return out, nil
}

// regressionStage returns the (virtual) regression container of the graph
// task belongs to.
func regressionStage(s *store.Store, task *store.Task) (*store.Task, error) {
	nodes, err := s.ListActiveNodes(task.RootID)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].NodeKey == RegressionStageKey {
			return &nodes[i], nil
		}
	}
	return nil, errors.New("no regression stage in the graph")
}

// TestCaseRunFollowsItsTask pins the fix for a case that read pending while
// it was executing: a regression stage runs one sub-task per case, and that
// case's child run must be running for exactly as long as its task runs.
// The case list, the case's detail page (whose log follows the status it
// reads) and the graph node all hang off this one row.
func TestCaseRunFollowsItsTask(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	svc.Workers = 1 // the cases run one at a time: queued vs executing is observable

	// Sample the case rows from inside each case's own script — the moment
	// the command would be running on the host.
	var mu sync.Mutex
	observed := map[string]map[string]string{}
	var observeErr error
	exec.onScript = func(script string) {
		name := ""
		switch {
		case strings.Contains(script, "run_heat.py"):
			name = "heat"
		case strings.Contains(script, "run_poisson.py"):
			name = "poisson"
		default:
			return // clone/build/unit: no case row involved
		}
		got, err := caseStatuses(s, cloneTask)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			observeErr = err
			return
		}
		observed[name] = got
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.Start(ctx)

	roots, err := s.ListRootTasks(10)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots: %v %v", roots, err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		root, err := s.GetTask(roots[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if root.Status == store.StatusPassed || root.Status == store.StatusFailed {
			if root.Status != store.StatusPassed {
				t.Fatalf("root failed: %s", root.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("root did not finish in time")
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if observeErr != nil {
		t.Fatalf("observe case rows: %v", observeErr)
	}
	// While heat ran, its own row was running and the queued case pending —
	// this is what the run page showed as a long "pending".
	if got := observed["heat"]; got["heat"] != store.StatusRunning || got["poisson"] != store.StatusPending {
		t.Errorf("while heat ran: want heat running / poisson pending, got %v", got)
	}
	// By the time poisson ran, heat had reported.
	if got := observed["poisson"]; got["poisson"] != store.StatusRunning || got["heat"] != store.StatusPassed {
		t.Errorf("while poisson ran: want poisson running / heat passed, got %v", got)
	}
	// Both cases land as terminal rows and the run aggregates them.
	got, err := caseStatuses(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if got["heat"] != store.StatusPassed || got["poisson"] != store.StatusPassed {
		t.Fatalf("both cases should be passed: %v", got)
	}
	runs, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if run := runs[RegressionCaseKey("heat")]; run == nil || run.Status != store.StatusPassed {
		t.Fatalf("the heat case run should be passed: %+v", run)
	}
}

// runsByKey returns the latest run of every node of task's graph, keyed by
// the node key ("build", "unit", "regression:<case>") — the rows the stage
// pages read. Virtual nodes have no run and are absent.
func runsByKey(s *store.Store, task *store.Task) (map[string]*store.TestRun, error) {
	nodes, err := s.ListActiveNodes(task.RootID)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(nodes))
	for i := range nodes {
		ids = append(ids, nodes[i].ID)
	}
	latest, err := s.LatestRunsByTaskIDs(ids)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*store.TestRun, len(nodes))
	for i := range nodes {
		if r, ok := latest[nodes[i].ID]; ok {
			run := r
			out[nodes[i].NodeKey] = &run
		}
	}
	return out, nil
}
