package runner

import (
	"context"
	"strings"
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
		if root.Status == store.TaskDone || root.Status == store.TaskFailed {
			if root.Status != store.TaskDone {
				subs, _ := s.ListSubTasks(root.ID)
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
			subs, _ := s.ListSubTasks(root.ID)
			for _, sub := range subs {
				if sub.Status != store.TaskDone {
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
		if root.Status == store.TaskFailed {
			subs, _ := s.ListSubTasks(root.ID)
			var unitState string
			regStates := map[string]int{}
			for _, sub := range subs {
				switch sub.Kind {
				case store.TaskKindBuild:
					if sub.Status != store.TaskFailed || !strings.Contains(sub.Error, "exit 1") {
						t.Errorf("build state: %s / %q", sub.Status, sub.Error)
					}
				case store.TaskKindUnit:
					unitState = sub.Status
				case store.TaskKindRegression:
					regStates[sub.Status]++
				}
			}
			if unitState != store.TaskSkipped {
				t.Errorf("unit should be skipped: %s", unitState)
			}
			if regStates[store.TaskSkipped] != 2 {
				t.Errorf("both regression cases should be skipped: %v", regStates)
			}
			// The skipped stages got failed dashboard runs.
			runs, _ := s.FindRunsByCommits(store.RunKindUnit,
				[]int64{roots[0].EnvironmentID}, []int64{roots[0].CommitID})
			run, ok := runs[store.EnvCommit{Env: roots[0].EnvironmentID, Commit: roots[0].CommitID}]
			if !ok || run.Status != store.StatusFailed || !strings.Contains(run.Summary, "skipped") {
				t.Errorf("skipped unit run wrong: %+v", run)
			}
			return
		}
		if root.Status == store.TaskDone {
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
