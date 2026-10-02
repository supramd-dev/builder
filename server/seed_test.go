package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"md-builder/server/runner"
	"md-builder/server/storage"
	"md-builder/server/store"
)

// The seed is the demo data every screenshot and manual end-to-end session
// starts from, and it writes through the same store calls the runner uses
// (UpsertTaskGraph, FinishAttempt, SetRunLogPrefix), so the graph it produces
// is also the cheapest end-to-end check of the task/run model there is: if the
// rollup, the attempts or the artifact plumbing drift, these tests fail.

// storedLog reads a task attempt's log the way a reader does: the parts the run
// points at, in order. A stage that never ran has none.
func storedLog(t *testing.T, s *store.Store, taskID int64, attempt int) string {
	t.Helper()
	run, err := s.FindTaskRun(taskID, attempt)
	if err != nil {
		t.Fatalf("find the run of task %d attempt %d: %v", taskID, attempt, err)
	}
	if run.LogPrefix == "" {
		return ""
	}
	ctx := context.Background()
	parts, err := s.ListRunLogParts(ctx, run.LogPrefix)
	if err != nil {
		t.Fatalf("list the log parts of task %d: %v", taskID, err)
	}
	var out strings.Builder
	for _, p := range parts {
		data, err := s.Objects().Get(ctx, p.Key)
		if err != nil {
			t.Fatalf("read the log part %s: %v", p.Key, err)
		}
		out.Write(data)
	}
	if out.Len() != int(run.LogBytes) {
		t.Fatalf("task %d's log reads %d bytes, but the run records %d", taskID, out.Len(), run.LogBytes)
	}
	return out.String()
}

// newSeedStore opens a throwaway SQLite database backed by the in-memory
// object store, so the demo matrix can be seeded and inspected without a real
// MinIO or a real node.
func newSeedStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "seed.db"), store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// seedEnv returns a demo environment's id, failing the test when the seed did
// not create it.
func seedEnv(t *testing.T, s *store.Store, name string) int64 {
	t.Helper()
	var env store.TestEnvironment
	if err := s.DB.Where("name = ?", name).First(&env).Error; err != nil {
		t.Fatalf("environment %q: %v", name, err)
	}
	return env.ID
}

// seedRoot returns the (only) root task of one push on one machine.
func seedRoot(t *testing.T, s *store.Store, sha string, envID int64) *store.Task {
	t.Helper()
	var c store.Commit
	if err := s.DB.Where("sha = ?", sha).First(&c).Error; err != nil {
		t.Fatalf("commit %q: %v", sha, err)
	}
	root, err := s.FindRootTaskByCommitEnv(c.ID, envID)
	if err != nil {
		t.Fatalf("root of %s: %v", sha, err)
	}
	return root
}

// seedNode returns one active node of a root by the node key the graph gave
// it (a stage kind, or "regression:<case>").
func seedNode(t *testing.T, s *store.Store, root *store.Task, key string) *store.Task {
	t.Helper()
	nodes, err := s.ListActiveNodes(root.ID)
	if err != nil {
		t.Fatalf("nodes of root %d: %v", root.ID, err)
	}
	for i := range nodes {
		if nodes[i].NodeKey == key {
			return &nodes[i]
		}
	}
	t.Fatalf("root %d has no node %q (has %d nodes)", root.ID, key, len(nodes))
	return nil
}

// seedCounts is the part of a stage's state the assertions care about.
type seedCounts struct {
	status                string
	total, passed, failed int
	skipped               int
}

func countsOf(t *testing.T, node *store.Task) seedCounts {
	t.Helper()
	return seedCounts{
		status:  node.Status,
		total:   node.Total,
		passed:  node.Passed,
		failed:  node.Failed,
		skipped: node.Skipped,
	}
}

// The demo matrix as seeded: one entry per cell the tests assert on. unit and
// reg are the tallies of those two stages; "" means the stage is not checked
// (the cells where the build failed are asserted through their skip instead).
func TestSeedDemoMatrix(t *testing.T) {
	s := newSeedStore(t)
	if err := seedAll(s, false); err != nil {
		t.Fatalf("seedAll: %v", err)
	}

	// The demo credentials are in this repository, so the account they open
	// must not be an administrator and the password must not be stored in the
	// clear.
	var user store.User
	if err := s.DB.Where("username = ?", "demo").First(&user).Error; err != nil {
		t.Fatalf("demo user: %v", err)
	}
	if user.Role != store.RoleUser {
		t.Errorf("demo user role = %q, want %q", user.Role, store.RoleUser)
	}
	if user.PasswordHash == "" || strings.Contains(user.PasswordHash, "demo-pass-123") {
		t.Errorf("demo user password hash looks wrong: %q", user.PasswordHash)
	}
	var envCount int64
	if err := s.DB.Model(&store.TestEnvironment{}).Where("owner_id = ?", user.ID).
		Count(&envCount).Error; err != nil {
		t.Fatalf("count environments: %v", err)
	}
	if envCount != 3 {
		t.Errorf("seeded %d environments, want 3", envCount)
	}

	cpu := seedEnv(t, s, "cpu-node-1")
	gpu := seedEnv(t, s, "gpu-a100")
	mpi := seedEnv(t, s, "mpi-cluster")

	const (
		clone = store.TaskKindClone
		build = store.TaskKindBuild
		unit  = store.TaskKindUnit
		reg   = store.TaskKindRegressionStage
	)

	t.Run("green cell", func(t *testing.T) {
		root := seedRoot(t, s, "a111111", cpu)
		if root.Status != store.StatusPassed {
			t.Fatalf("root status = %q, want %q", root.Status, store.StatusPassed)
		}
		if root.Virtual != true || root.Attempts != 1 {
			t.Errorf("root virtual/attempts = %v/%d, want true/1", root.Virtual, root.Attempts)
		}
		for _, key := range []string{clone, build, unit} {
			node := seedNode(t, s, root, key)
			if node.Status != store.StatusPassed {
				t.Errorf("%s status = %q, want %q", key, node.Status, store.StatusPassed)
			}
			if node.Virtual {
				t.Errorf("%s is virtual, want a real task", key)
			}
		}
		if got := countsOf(t, seedNode(t, s, root, unit)); got != (seedCounts{status: store.StatusPassed, total: 4, passed: 4}) {
			t.Errorf("unit counts = %+v, want 4/4 passed", got)
		}
		// The regression stage is a container: its counts are its cases'.
		if got := countsOf(t, seedNode(t, s, root, reg)); got != (seedCounts{status: store.StatusPassed, total: 3, passed: 3}) {
			t.Errorf("regression counts = %+v, want 3/3 passed", got)
		}
		// Every case is a task of its own.
		for _, name := range []string{"lj-argon-nve", "water-tip4p-nvt", "argon-liquid-nvt"} {
			node := seedNode(t, s, root, runner.RegressionCaseKey(name))
			if node.Status != store.StatusPassed || node.ParentID == 0 {
				t.Errorf("case %s = %q (parent %d), want passed under a parent", name, node.Status, node.ParentID)
			}
		}
		// The virtual nodes carry no run of their own: a run is what makes a
		// task real.
		if _, err := s.FindTaskRun(root.ID, 1); !errors.Is(err, store.ErrTestRunNotFound) {
			t.Errorf("root run = %v, want %v", err, store.ErrTestRunNotFound)
		}
		// Clone is real, so it has one too.
		cloneRun, err := s.FindTaskRun(seedNode(t, s, root, clone).ID, 1)
		if err != nil {
			t.Fatalf("clone run: %v", err)
		}
		if cloneRun.Status != store.StatusPassed || cloneRun.DurationMillis <= 0 {
			t.Errorf("clone run = %q/%vms, want passed with a duration", cloneRun.Status, cloneRun.DurationMillis)
		}
		unitNode := seedNode(t, s, root, unit)
		run, err := s.FindTaskRun(unitNode.ID, 1)
		if err != nil {
			t.Fatalf("unit run: %v", err)
		}
		if got := storedLog(t, s, unitNode.ID, run.Attempt); !strings.Contains(got, "MD-BUILDER-SUMMARY: all 4 unit tests passed") {
			t.Errorf("unit log missing its summary line: %q", got)
		}
		// The unit run's gtest XML is the artifact the run page parses.
		arts, err := s.ListRunArtifacts(run.ID)
		if err != nil {
			t.Fatalf("unit artifacts: %v", err)
		}
		if len(arts) != 1 || arts[0].Kind != store.ArtifactKindResults {
			t.Fatalf("unit artifacts = %+v, want one results artifact", arts)
		}
		if !strings.Contains(arts[0].Name, "test_detail.xml") {
			t.Errorf("unit artifact name = %q", arts[0].Name)
		}
		// The build's fetched-back files are stored verbatim.
		buildArts, err := s.ListRunArtifacts(mustRun(t, s, seedNode(t, s, root, build)).ID)
		if err != nil {
			t.Fatalf("build artifacts: %v", err)
		}
		if len(buildArts) != len(demoBuildFiles) {
			t.Errorf("build artifacts = %d, want %d", len(buildArts), len(demoBuildFiles))
		}
	})

	t.Run("failing unit test", func(t *testing.T) {
		root := seedRoot(t, s, "b222222", cpu)
		if root.Status != store.StatusFailed {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusFailed)
		}
		unitNode := seedNode(t, s, root, unit)
		if got := countsOf(t, unitNode); got != (seedCounts{status: store.StatusFailed, total: 4, passed: 3, failed: 1}) {
			t.Errorf("unit counts = %+v, want 3/4 passed", got)
		}
		if unitNode.Error == "" {
			t.Error("a failed unit stage should carry the failure as its error")
		}
		// Only the unit stage failed: the regression cases still ran.
		if got := countsOf(t, seedNode(t, s, root, reg)); got.status != store.StatusPassed || got.passed != 3 {
			t.Errorf("regression counts = %+v, want 3/3 passed", got)
		}
	})

	t.Run("failing build skips the tests", func(t *testing.T) {
		root := seedRoot(t, s, "b222222", gpu)
		if root.Status != store.StatusFailed {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusFailed)
		}
		if got := countsOf(t, seedNode(t, s, root, build)); got.status != store.StatusFailed {
			t.Errorf("build counts = %+v, want failed", got)
		}
		unitNode := seedNode(t, s, root, unit)
		if unitNode.Status != store.StatusSkipped {
			t.Fatalf("unit status = %q, want %q", unitNode.Status, store.StatusSkipped)
		}
		// A skipped stage explains itself in its summary — it produced no
		// output, so it has no log to explain it in.
		if !strings.Contains(unitNode.Summary, "upstream task build failed") {
			t.Errorf("skipped unit summary = %q, want the skip reason", unitNode.Summary)
		}
		if got := storedLog(t, s, unitNode.ID, unitNode.Attempts); got != "" {
			t.Errorf("a stage that never ran has no output, got %q", got)
		}
		// A stage that never ran has no start, only the moment it was skipped
		// (that is what store.skipTasksTx leaves behind).
		if unitNode.StartedAt != nil || unitNode.FinishedAt == nil {
			t.Errorf("skipped unit start/finish = %v/%v, want nil then a time", unitNode.StartedAt, unitNode.FinishedAt)
		}
		// The container above the skipped cases rolls up to skipped; each case
		// counts as one skipped unit of the container.
		container := seedNode(t, s, root, reg)
		if got := countsOf(t, container); got != (seedCounts{status: store.StatusSkipped, total: 3, skipped: 3}) {
			t.Errorf("regression counts = %+v, want 3 skipped cases", got)
		}
		for _, name := range demoRegressionCase {
			if node := seedNode(t, s, root, runner.RegressionCaseKey(name)); node.Status != store.StatusSkipped {
				t.Errorf("case %s status = %q, want %q", name, node.Status, store.StatusSkipped)
			}
		}
		// The skip is backdated to the seeded moment, and so are the
		// containers above it: SkipTask stamps "now" and rolls the tree up
		// before demoSkip rewrites the timestamps, so without the second
		// rollup the cell of a three-day-old push would read as finished a
		// second ago. (The store stamps a skipped run's window at the moment
		// it skips — start and finish alike; the demo shows the same shape.)
		seededAgo := time.Now().Add(-48 * time.Hour)
		unitRun := mustRun(t, s, unitNode)
		if unitRun.StartedAt.IsZero() || unitRun.FinishedAt.IsZero() {
			t.Errorf("skipped unit run window = %v..%v, want both stamped", unitRun.StartedAt, unitRun.FinishedAt)
		} else {
			if unitRun.StartedAt.After(seededAgo) {
				t.Errorf("skipped unit run started at %v, want the seeded moment, not the seed run's", unitRun.StartedAt)
			}
			if unitRun.FinishedAt.After(seededAgo) {
				t.Errorf("skipped unit run finished at %v, want the seeded moment days ago", unitRun.FinishedAt)
			}
		}
		if container.FinishedAt == nil || container.FinishedAt.After(seededAgo) {
			t.Errorf("the container above the skipped cases finished at %v, want the seeded moment", container.FinishedAt)
		}
		if root.FinishedAt == nil || root.FinishedAt.After(seededAgo) {
			t.Errorf("the cell finished at %v, want the seeded moment", root.FinishedAt)
		}
	})

	t.Run("failing regression case", func(t *testing.T) {
		root := seedRoot(t, s, "c333333", gpu)
		if root.Status != store.StatusFailed {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusFailed)
		}
		if got := countsOf(t, seedNode(t, s, root, unit)); got.status != store.StatusPassed || got.total != 4 {
			t.Errorf("unit counts = %+v, want 4/4 passed", got)
		}
		container := seedNode(t, s, root, reg)
		if got := countsOf(t, container); got != (seedCounts{status: store.StatusFailed, total: 3, passed: 2, failed: 1}) {
			t.Errorf("regression counts = %+v, want 2/3 passed", got)
		}
		if !container.Virtual {
			t.Error("the regression container should be virtual")
		}
		if node := seedNode(t, s, root, runner.RegressionCaseKey("water-tip4p-nvt")); node.Status != store.StatusFailed {
			t.Errorf("water case status = %q, want %q", node.Status, store.StatusFailed)
		}
	})

	t.Run("two failing stages on one cell", func(t *testing.T) {
		root := seedRoot(t, s, "d444444", mpi)
		if root.Status != store.StatusFailed {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusFailed)
		}
		if got := countsOf(t, seedNode(t, s, root, unit)); got.total != 4 || got.failed != 1 {
			t.Errorf("unit counts = %+v, want one of four failed", got)
		}
		if got := countsOf(t, seedNode(t, s, root, reg)); got.total != 3 || got.failed != 1 {
			t.Errorf("regression counts = %+v, want one of three failed", got)
		}
	})

	t.Run("untested pair has no graph", func(t *testing.T) {
		var c store.Commit
		if err := s.DB.Where("sha = ?", "f666666").First(&c).Error; err != nil {
			t.Fatalf("commit: %v", err)
		}
		if _, err := s.FindRootTaskByCommitEnv(c.ID, cpu); !errors.Is(err, store.ErrTaskNotFound) {
			t.Errorf("root for f666666 on cpu = %v, want %v", err, store.ErrTaskNotFound)
		}
	})

	t.Run("in-flight cell", func(t *testing.T) {
		root := seedRoot(t, s, "f666666", gpu)
		if root.Status != store.StatusRunning {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusRunning)
		}
		buildNode := seedNode(t, s, root, build)
		if buildNode.Status != store.StatusRunning {
			t.Errorf("build status = %q, want %q", buildNode.Status, store.StatusRunning)
		}
		run := mustRun(t, s, buildNode)
		if run.Status != store.StatusRunning || !run.FinishedAt.IsZero() {
			t.Errorf("build run = %q, finished %v; want running and open", run.Status, run.FinishedAt)
		}
		// The stages after it are still queued, and the regression container
		// with them — a pending container is what the dashboard shows as
		// "queued", not "running".
		for _, node := range []*store.Task{
			seedNode(t, s, root, unit),
			seedNode(t, s, root, reg),
			seedNode(t, s, root, runner.RegressionCaseKey("water-tip4p-nvt")),
		} {
			if node.Status != store.StatusPending {
				t.Errorf("%s status = %q, want %q", node.NodeKey, node.Status, store.StatusPending)
			}
		}
	})

	t.Run("fully queued cell", func(t *testing.T) {
		root := seedRoot(t, s, "f666666", mpi)
		if root.Status != store.StatusPending {
			t.Errorf("root status = %q, want %q", root.Status, store.StatusPending)
		}
		nodes, err := s.ListActiveNodes(root.ID)
		if err != nil {
			t.Fatalf("nodes: %v", err)
		}
		if len(nodes) == 0 {
			t.Fatal("no nodes")
		}
		for _, n := range nodes {
			if n.Status != store.StatusPending {
				t.Errorf("%s status = %q, want every node pending", n.NodeKey, n.Status)
			}
		}
	})

	t.Run("re-seeding changes nothing", func(t *testing.T) {
		before := seedCountsOfRoots(t, s)
		if err := seedAll(s, false); err != nil {
			t.Fatalf("second seedAll: %v", err)
		}
		if after := seedCountsOfRoots(t, s); after != before {
			t.Errorf("roots after re-seeding = %d, want %d", after, before)
		}
		// A finished cell is left alone: its root is still on its first
		// attempt, not carrying a retry from the second seed run.
		root := seedRoot(t, s, "a111111", cpu)
		if root.Attempts != 1 {
			t.Errorf("root attempts after re-seeding = %d, want 1", root.Attempts)
		}
	})

	t.Run("force rebuilds the graphs", func(t *testing.T) {
		if err := seedAll(s, true); err != nil {
			t.Fatalf("forced seedAll: %v", err)
		}
		root := seedRoot(t, s, "a111111", cpu)
		if root.Status != store.StatusPassed || root.Attempts != 1 {
			t.Errorf("root after force = %q (attempts %d), want passed on attempt 1", root.Status, root.Attempts)
		}
		if got := countsOf(t, seedNode(t, s, root, reg)); got.total != 3 || got.passed != 3 {
			t.Errorf("regression counts after force = %+v, want 3/3 passed", got)
		}
	})
}

// mustRun returns a node's current attempt's run, failing the test when it has
// none (every real task gets one at dispatch).
func mustRun(t *testing.T, s *store.Store, node *store.Task) *store.TestRun {
	t.Helper()
	run, err := s.FindTaskRun(node.ID, node.Attempts)
	if err != nil {
		t.Fatalf("run of %s (attempt %d): %v", node.NodeKey, node.Attempts, err)
	}
	return run
}

// seedCountsOfRoots counts the seeded roots, to show re-seeding adds none.
func seedCountsOfRoots(t *testing.T, s *store.Store) int64 {
	t.Helper()
	var n int64
	if err := s.DB.Model(&store.Task{}).Where("kind = ?", store.TaskKindRoot).
		Count(&n).Error; err != nil {
		t.Fatalf("count roots: %v", err)
	}
	return n
}
