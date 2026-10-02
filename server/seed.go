package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"md-builder/server/auth"
	"md-builder/server/config"
	"md-builder/server/runner"
	"md-builder/server/storage"
	"md-builder/server/store"
)

// seedSubcommand populates the database with demo data so the dashboards and
// the task-graph view can be inspected without a real git host or SSH nodes:
// one demo user, three environments, six commits and the task graphs of every
// (commit, environment) pair the demo tests, with run history, logs and
// artifacts.
//
// Usage:
//
//	md-builder seed [-dsn <dsn>] [-force]
//
// A cell is a whole task graph, built with runner.BuildTaskGraph and finished
// through the same store calls the runner uses, so the dashboards, the graph
// page and the run pages show exactly what a real dispatch of these commits
// would have produced.
//
// The command is idempotent: commits are deduplicated by (repo, sha) and a
// cell whose graph is already finished is left alone. With -force the demo
// environments' tasks are deleted first (with their runs, logs and artifacts)
// so every graph is rebuilt from scratch.
func seedSubcommand() int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "database DSN (default: database.dsn from the config file)")
	force := fs.Bool("force", false, "delete leftover demo tasks before seeding")
	configPath := fs.String(config.FlagName, "", config.FlagUsage)
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}

	srvCfg, source, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: config: %v\n", err)
		return 1
	}
	if *dsn == "" {
		*dsn = srvCfg.Database.DSN
	}

	// The demo data includes artifacts, so seeding needs the same object
	// storage the server uses.
	objs, err := openObjectStorage(srvCfg, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: object storage: %v\n", err)
		return 1
	}

	s, err := store.Open(*dsn, store.WithObjects(objs))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer s.Close()

	if err := seedAll(s, *force); err != nil {
		fmt.Fprintf(os.Stderr, "error: seed: %v\n", err)
		return 1
	}
	return 0
}

// seedAll writes the demo data into s. It is separate from seedSubcommand so
// the demo matrix can be exercised by tests — with a temporary database and
// the in-memory object store — instead of only by hand.
func seedAll(s *store.Store, force bool) error {
	// --- user ------------------------------------------------------------
	const username = "demo"
	var user store.User
	switch err := s.DB.Where("username = ?", username).First(&user).Error; {
	case err == nil:
		fmt.Printf("user %q already exists (id %d)\n", user.Username, user.ID)
	case errors.Is(err, store.ErrNotFound):
		hash, err := auth.HashPassword("demo-pass-123")
		if err != nil {
			return fmt.Errorf("hash password: %w", err)
		}
		// A regular user, deliberately: the demo password is in this source
		// file, so it must not be an administrator credential.
		user = store.User{
			Username:     username,
			Email:        "demo@example.com",
			PasswordHash: hash,
			Role:         store.RoleUser,
		}
		if err := s.CreateUser(&user); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		fmt.Printf("created user %q (password demo-pass-123)\n", username)
	default:
		return fmt.Errorf("lookup user: %w", err)
	}

	// --- environments ------------------------------------------------------
	// Three fake nodes. The private key is a syntactically valid RSA PEM so
	// environment validation passes; nothing ever connects to these hosts.
	const demoKey = `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/yGWy7tLX9hKSmS7DIBJnKlXhH0dM+VdP
f0/1j3hjJZ2XvH3f4hF1p3V0pQbGq0XZJmS5+0mS1zBQwJ9f8K3hZ3iXmQo0nF2y
hwIDAQAB
-----END RSA PRIVATE KEY-----`
	envs := []*store.TestEnvironment{
		{OwnerID: user.ID, Name: "cpu-node-1", Host: "10.0.0.11", Username: "md",
			PrivateKey: demoKey, Tags: "cpu", Description: "2×8-core CPU node (demo)"},
		{OwnerID: user.ID, Name: "gpu-a100", Host: "10.0.0.12", Username: "md",
			PrivateKey: demoKey, Tags: "cpu,gpu", Description: "A100 GPU node (demo)"},
		{OwnerID: user.ID, Name: "mpi-cluster", Host: "10.0.0.13", Username: "md",
			PrivateKey: demoKey, Tags: "cpu,mpi", Description: "64-rank MPI cluster (demo)"},
	}
	for _, env := range envs {
		var existing store.TestEnvironment
		switch err := s.DB.Where("owner_id = ? AND name = ?", user.ID, env.Name).
			First(&existing).Error; {
		case err == nil:
			env.ID = existing.ID
			fmt.Printf("environment %q already exists (id %d)\n", env.Name, env.ID)
		case errors.Is(err, store.ErrNotFound):
			if err := s.CreateEnvironment(env); err != nil {
				return fmt.Errorf("create environment %s: %w", env.Name, err)
			}
			fmt.Printf("created environment %q (id %d)\n", env.Name, env.ID)
		default:
			return fmt.Errorf("lookup environment %s: %w", env.Name, err)
		}
	}

	// --- commits -------------------------------------------------------------
	// Six pushes, oldest first, so the matrix rows read as a history: the
	// newest finished push is all-green, older ones carry a failing build, a
	// failing unit test and failing regression cases. The last push is the one
	// whose graphs are still in flight.
	commits := []*store.Commit{
		{Repo: "group/md-code", SHA: "a111111", Ref: "main", Author: "alice", Message: "Add velocity Verlet integrator", PushedAt: time.Now().Add(-96 * time.Hour)},
		{Repo: "group/md-code", SHA: "b222222", Ref: "main", Author: "bob", Message: "Fix PBC image remapping", PushedAt: time.Now().Add(-72 * time.Hour)},
		{Repo: "group/md-code", SHA: "c333333", Ref: "main", Author: "alice", Message: "Tune neighbor list skin", PushedAt: time.Now().Add(-48 * time.Hour)},
		{Repo: "group/md-code", SHA: "d444444", Ref: "main", Author: "bob", Message: "Refactor pair styles", PushedAt: time.Now().Add(-24 * time.Hour)},
		{Repo: "group/md-code", SHA: "e555555", Ref: "main", Author: "carol", Message: "Switch to C++17", PushedAt: time.Now().Add(-2 * time.Hour)},
		{Repo: "group/md-code", SHA: "f666666", Ref: "main", Author: "carol", Message: "Add thermostat barostat coupling", PushedAt: time.Now().Add(-20 * time.Minute)},
	}
	for _, c := range commits {
		if _, err := s.GetOrCreateCommit(c); err != nil {
			return fmt.Errorf("create commit %s: %w", c.SHA, err)
		}
	}
	fmt.Printf("ensured %d commits on group/md-code\n", len(commits))

	// --- site config ------------------------------------------------------
	// Point the code-repo filter at the demo repository so the dashboard
	// actually shows the seeded commits (a filter for another repo, e.g.
	// left over from a smoke test, would hide every row).
	cfg, err := s.GetSiteConfig()
	if err != nil {
		return fmt.Errorf("load site config: %w", err)
	}
	if !strings.Contains(cfg.CodeRepo, "group/md-code") {
		cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
		if err := s.SaveSiteConfig(cfg); err != nil {
			return fmt.Errorf("save site config: %w", err)
		}
		fmt.Println("set site code repo to group/md-code")
	}

	if force {
		for _, env := range envs {
			if err := s.DeleteTasksForEnvironment(env.ID); err != nil {
				return fmt.Errorf("drop demo tasks of %s: %w", env.Name, err)
			}
		}
		fmt.Println("dropped the demo environments' previous task graphs")
	}

	// --- demo matrix -------------------------------------------------------
	// One cell per (commit, environment) the demo tested, as that commit's
	// graph on that machine. A pair that is absent was never tested — the
	// matrix shows an em dash there (the MPI cluster, for instance, was
	// registered after the oldest push).
	cpu, gpu, mpi := envs[0], envs[1], envs[2]
	c1, c2, c3, c4, c5 := commits[0], commits[1], commits[2], commits[3], commits[4]

	// Per-test outcomes. The unit stage is one ctest run over four tests; each
	// regression case is a task node of its own.
	unitPassed := []string{statusPassed, statusPassed, statusPassed, statusPassed}
	regPassed := []string{statusPassed, statusPassed, statusPassed}
	unitVerletFail := []string{statusPassed, statusFailed, statusPassed, statusPassed}  // TestIntegrate::verlet
	unitRebuildFail := []string{statusPassed, statusPassed, statusFailed, statusPassed} // TestNeighborList::rebuild
	regWaterFail := []string{statusPassed, statusFailed, statusPassed}                  // water-tip4p-nvt
	regArgonFail := []string{statusPassed, statusPassed, statusFailed}                  // argon-liquid-nvt

	specs := []demoCellSpec{
		// a111111: fully green on CPU and GPU.
		{env: cpu, commit: c1, pushedAgo: 96 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+ninja, 41s)"), buildArtifacts: demoBuildFiles,
				unit: unitPassed, reg: regPassed}},
		{env: gpu, commit: c1, pushedAgo: 96 * time.Hour,
			cell: demoCell{build: buildOK("build ok with CUDA arch sm_80 (8m12s)"), unit: unitPassed, reg: regPassed}},

		// b222222: a unit test fails on the CPU node, a regression case on the
		// MPI cluster, and the GPU build fails — so its unit and regression
		// stages were skipped there.
		{env: cpu, commit: c2, pushedAgo: 72 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+ninja, 39s)"), buildArtifacts: demoBuildFiles,
				unit: unitVerletFail, reg: regPassed}},
		{env: mpi, commit: c2, pushedAgo: 72 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+make -j64, 1m03s)"), unit: unitPassed, reg: regWaterFail}},
		{env: gpu, commit: c2, pushedAgo: 72 * time.Hour,
			cell: demoCell{build: buildFail("CMake Error at src/CMakeLists.txt:87: Cannot find package CUDA")}},

		// c333333: neighbor-list tuning breaks the water case on the GPU; the
		// MPI build fails (no MPI in the image), so its tests were skipped.
		{env: cpu, commit: c3, pushedAgo: 48 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+ninja, 40s)"), buildArtifacts: demoBuildFiles,
				unit: unitPassed, reg: regPassed}},
		{env: gpu, commit: c3, pushedAgo: 48 * time.Hour,
			cell: demoCell{build: buildOK("build ok with CUDA arch sm_80 (8m30s)"), unit: unitPassed, reg: regWaterFail}},
		{env: mpi, commit: c3, pushedAgo: 48 * time.Hour,
			cell: demoCell{build: buildFail("CMake Error at src/CMakeLists.txt:87 (target_link_libraries): Cannot find package MPI")}},

		// d444444: broad-green after the refactor, except one unit test and one
		// regression case on the MPI cluster.
		{env: cpu, commit: c4, pushedAgo: 24 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+ninja, 38s)"), buildArtifacts: demoBuildFiles,
				unit: unitPassed, reg: regPassed}},
		{env: gpu, commit: c4, pushedAgo: 24 * time.Hour,
			cell: demoCell{build: buildOK("build ok with CUDA arch sm_80 (7m58s)"), unit: unitPassed, reg: regPassed}},
		{env: mpi, commit: c4, pushedAgo: 24 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+make -j64, 58s)"), unit: unitRebuildFail, reg: regArgonFail}},

		// e555555 (newest finished push): fully green everywhere.
		{env: cpu, commit: c5, pushedAgo: 2 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+ninja, 37s)"), buildArtifacts: demoBuildFiles,
				unit: unitPassed, reg: regPassed}},
		{env: gpu, commit: c5, pushedAgo: 2 * time.Hour,
			cell: demoCell{build: buildOK("build ok with CUDA arch sm_80 (7m44s)"), unit: unitPassed, reg: regPassed}},
		{env: mpi, commit: c5, pushedAgo: 2 * time.Hour,
			cell: demoCell{build: buildOK("build ok (cmake+make -j64, 55s)"), unit: unitPassed, reg: regPassed}},
	}
	for _, sp := range specs {
		if err := seedCell(s, sp); err != nil {
			return fmt.Errorf("seed %s on %s: %w", demoShortSHA(sp.commit.SHA), sp.env.Name, err)
		}
	}
	fmt.Println("seeded the demo task graphs (build/unit/regression, with logs and artifacts)")

	// --- in-flight demo graphs ---------------------------------------------
	// The newest push carries two in-flight graphs (one running mid-build with
	// live-style logs, one fully queued) so the dashboards, the task detail and
	// the graph page can be seen handling pending/running states. They are
	// snapshots, not live jobs: nothing will ever advance them, but every read
	// path renders them like a real dispatch.
	//
	// Note the scheduler interplay: unlike the finished graphs, an in-flight
	// snapshot is NOT protected from the runner. With a live worker pool the
	// queued stages get claimed (and fail on the unreachable demo host, since
	// the stage snapshots carry valid configs but no real environment), and
	// ResetStaleRunning resets running rows to pending on restart. Demo with
	// `worker.enabled: false` (or MD_BUILDER_DISABLE_WORKER=1) for a frozen
	// in-flight state; with workers on, the snapshot degrades into a genuine
	// failing dispatch, which the seeded logs and configs are shaped to survive
	// gracefully.
	live := commits[5]
	for _, spec := range []demoCellSpec{
		{env: gpu, commit: live, pushedAgo: 20 * time.Minute},
		{env: mpi, commit: live, pushedAgo: 20 * time.Minute},
	} {
		if err := seedLiveCell(s, spec, livePhase(spec.env, gpu)); err != nil {
			return fmt.Errorf("seed in-flight graph on %s: %w", spec.env.Name, err)
		}
	}

	fmt.Println("seed done — log in as demo / demo-pass-123 and open the Dashboard")
	return nil
}

// --- demo matrix data ---------------------------------------------------

// The per-test statuses a demo cell is made of. They are the store's own
// vocabulary (store.StatusPassed and friends).
const (
	statusPassed  = store.StatusPassed
	statusFailed  = store.StatusFailed
	statusSkipped = store.StatusSkipped
)

// demoCellSpec is one row of the demo matrix: which commit on which
// environment, how old that push is, and how each of its stages ended.
type demoCellSpec struct {
	env       *store.TestEnvironment
	commit    *store.Commit
	pushedAgo time.Duration // the push's age at seeding time
	cell      demoCell
}

// demoStage is one stage's seeded outcome.
type demoStage struct {
	status  string // passed | failed
	summary string // the stage's summary line (the failure text when it failed)
	errMsg  string // the failure detail ("" when it passed)
}

// demoCell is one (commit, environment) of the demo matrix: what each stage of
// its graph was reported with.
//
// unit and reg hold one status per unit test / regression case. An empty list
// means the stage never ran: an upstream stage failed, so the runner skipped
// everything downstream of it, and so does this seed.
type demoCell struct {
	build          demoStage
	buildArtifacts []string
	unit           []string
	reg            []string
}

// demoBuildFiles are the files the demo build fetches back: stored verbatim as
// file artifacts, never parsed for counts — exactly like the runner's build
// path.
var demoBuildFiles = []string{"build/.ninja_log", "build/compile_commands.json"}

// buildOK / buildFail describe a cell's build stage.
func buildOK(summary string) demoStage {
	return demoStage{status: statusPassed, summary: summary}
}

func buildFail(errMsg string) demoStage {
	return demoStage{status: statusFailed, summary: errMsg, errMsg: errMsg}
}

// demoUnitTests / demoRegressionCase name the tests behind the statuses, in
// matrix order: the unit stage is one ctest invocation over four tests, the
// regression stage one case per preset.
var (
	demoUnitTests      = []string{"TestForce::compute", "TestIntegrate::verlet", "TestNeighborList::rebuild", "TestPBC::unwrap"}
	demoRegressionCase = []string{"lj-argon-nve", "water-tip4p-nvt", "argon-liquid-nvt"}
)

// The commands the demo commits were dispatched with, so every stage snapshot
// the graph carries is a real config the executor can parse.
const (
	demoBuildCommand = "cmake -S . -B build -G Ninja && ninja -C build"
	demoUnitCommand  = "ctest -L unit --output-on-failure"
)

// demoEntry is the md-builder.yaml entry the demo commits were dispatched
// with. The graphs the seed persists are built from it by
// runner.BuildTaskGraph, so they have exactly the shape a real dispatch
// produces: the same kinds, node keys, stage configs and dependencies.
func demoEntry() runner.MergedEntry {
	entry := runner.MergedEntry{
		Tags: []string{"cpu"},
		Build: runner.BuildConfig{
			Command:   runner.CommandList{demoBuildCommand},
			Artifacts: runner.ArtifactPaths(demoBuildFiles),
		},
		Unit: &runner.EnvConfig{
			Command:   runner.CommandList{demoUnitCommand},
			Artifacts: runner.ArtifactPaths{"build/test_detail.xml"},
			Timeout:   1800,
		},
		RegressionDescription: "physics regression cases",
		Timeout:               3600,
	}
	for _, name := range demoRegressionCase {
		entry.Regression = append(entry.Regression, runner.RegressionCase{
			Name:    name,
			Command: runner.CommandList{"ctest -R " + name + " -L regression --output-on-failure"},
			Timeout: 3600,
		})
	}
	return entry
}

// --- seeding one cell ---------------------------------------------------

// seedCell builds and finishes one cell's task graph: the production graph
// shape, one attempt per real node, each reported through store.FinishAttempt
// — the same call the runner makes when a stage ends. A stage downstream of a
// failure is reported skipped with the reason the runner records, so the node,
// its run and its log all explain why it never ran.
//
// The virtual nodes (the root and the regression container) are never reported
// to: their status, counts and timestamps are rolled up from their children by
// the store.
func seedCell(s *store.Store, sp demoCellSpec) error {
	if sp.env == nil || sp.commit == nil {
		return errors.New("unknown demo environment or commit")
	}
	if root, err := s.FindRootTaskByCommitEnv(sp.commit.ID, sp.env.ID); err == nil {
		if root.Attempts > 0 && store.TaskStatusTerminal(root.Status) {
			fmt.Printf("task graph for %s on %s already exists (root #%d, %s)\n",
				demoShortSHA(sp.commit.SHA), sp.env.Name, root.ID, root.Status)
			return nil
		}
	} else if !errors.Is(err, store.ErrTaskNotFound) {
		return err
	}

	byKey, err := upsertDemoGraph(s, sp.env, sp.commit)
	if err != nil {
		return err
	}
	c := sp.cell
	// The graph runs a little after the push: clone, build, then the tests.
	// Each stage gets its own window inside the cell, so the timeline of a
	// seeded run reads like a real one.
	start := time.Now().Add(-sp.pushedAgo).Add(3 * time.Minute)

	cloneEnd := start.Add(90 * time.Second)
	cloneSummary := "cloned " + demoShortSHA(sp.commit.SHA)
	res := demoAttempt(start, cloneEnd, statusPassed, cloneSummary, "", demoCounts{}, nil)
	if err := finishDemoStage(s, byKey[store.TaskKindClone], res, demoCloneLog(sp.commit, sp.env)); err != nil {
		return err
	}

	// The build: the configured command plus the files it fetches back.
	buildEnd := cloneEnd.Add(5 * time.Minute)
	res = demoAttempt(cloneEnd, buildEnd, c.build.status, c.build.summary, c.build.errMsg,
		demoCounts{}, demoBuildArtifacts(c.buildArtifacts))
	if err := finishDemoStage(s, byKey[store.TaskKindBuild], res, demoBuildLog(c.build)); err != nil {
		return err
	}

	// Everything downstream of the build: the unit stage and the regression
	// cases, each a task of its own. A failed build skips them all, with the
	// reason store.SkipDependents writes.
	testStart := buildEnd.Add(20 * time.Second)
	skipReason := fmt.Sprintf("upstream task build failed: %s", c.build.errMsg)
	if err := seedUnitStage(s, byKey[store.TaskKindUnit], testStart, c, skipReason); err != nil {
		return err
	}
	if err := seedRegressionCases(s, byKey, testStart, c, skipReason); err != nil {
		return err
	}
	// SkipTask rolled the containers up while the stages it skipped were still
	// stamped "now", and demoSkip backdates them afterwards: roll the tree up
	// again so the cell's own window is the seeded one too. A cell from three
	// days ago must not read as finished a second ago — and the regression
	// stage, whose whole state comes from its cases, must not either.
	if err := s.RollupTaskTree(byKey[store.TaskKindClone].RootID); err != nil {
		return err
	}
	fmt.Printf("seeded task graph for %s on %s\n", demoShortSHA(sp.commit.SHA), sp.env.Name)
	return nil
}

// seedUnitStage reports the unit stage: one task running a ctest invocation
// over several tests (its counts are the tests', and its artifact is the gtest
// XML they produced) — or the skip a failed build caused.
func seedUnitStage(s *store.Store, node *store.Task, start time.Time, c demoCell, skipReason string) error {
	if node == nil {
		return nil
	}
	if len(c.unit) != len(demoUnitTests) {
		return demoSkip(s, node, start, skipReason)
	}
	failed := demoCount(c.unit, statusFailed)
	skipped := demoCount(c.unit, statusSkipped)
	status, errMsg := statusPassed, ""
	summary := demoUnitSummary(c.unit)
	if failed > 0 {
		status, errMsg = statusFailed, summary
	}
	counts := demoCounts{
		total: len(c.unit), passed: len(c.unit) - failed - skipped,
		failed: failed, skipped: skipped,
	}
	// Each test takes about a minute, and the stage's log is the ctest output
	// the summary line was taken from.
	res := demoAttempt(start, start.Add(time.Duration(len(c.unit))*time.Minute),
		status, summary, errMsg, counts, demoUnitArtifact(c.unit))
	return finishDemoStage(s, node, res, demoUnitLog(c.unit))
}

// seedRegressionCases reports one attempt per regression case. Each case is a
// real task of its own (that is what lets them run in parallel and keep their
// own logs and artifacts); the container above them is virtual and takes its
// status from the rollup.
func seedRegressionCases(s *store.Store, byKey map[string]*store.Task, start time.Time, c demoCell, skipReason string) error {
	for i, name := range demoRegressionCase {
		node := byKey[runner.RegressionCaseKey(name)]
		if node == nil {
			continue
		}
		// A case the build's failure stopped is reported the way the store
		// skips a stage: no counts, no start time, and the reason in the log.
		if len(c.reg) != len(demoRegressionCase) {
			if err := demoSkip(s, node, start, skipReason); err != nil {
				return err
			}
			continue
		}
		// A case passes or fails on its own; the demo's cases compare a
		// physical quantity against a tolerance, so each carries its own note.
		status := c.reg[i]
		summary := "max rel err 3.2e-7"
		if status == statusFailed {
			summary = "energy drift above threshold"
		}
		counts := demoCounts{total: 1}
		switch status {
		case statusPassed:
			counts.passed = 1
		case statusFailed:
			counts.failed = 1
		}
		res := demoAttempt(start, start.Add(6*time.Minute), status, summary, "", counts, nil)
		if err := finishDemoStage(s, node, res, demoCaseLog(name, status, summary)); err != nil {
			return err
		}
	}
	return nil
}

// demoSkip reports a stage an upstream failure stopped, through the store's
// own skip path: SkipTask closes the attempt and writes the reason into its
// log, so the demo shows exactly what a real skipped stage looks like.
//
// The timestamps are then put back to the seeded moment (SkipTask stamps
// "now"), because the demo is a history: a cell from three days ago must not
// have a stage that was skipped a second ago. Both of the run's bounds are
// moved, the way SkipTask wrote them: the store stamps a skipped attempt's
// start and finish at the moment it skips (the stage was queued, never
// claimed — see skipTasksTx), so the demo shows the same shape it would.
//
// The containers above the stage are rolled up by SkipTask as well, before
// the backdating: the caller rolls the tree up again afterwards (see
// seedCell).
func demoSkip(s *store.Store, node *store.Task, at time.Time, reason string) error {
	if err := s.SkipTask(node.ID, reason); err != nil {
		return err
	}
	if err := s.DB.Model(&store.Task{}).Where("id = ?", node.ID).
		Update("finished_at", at).Error; err != nil {
		return err
	}
	run, err := s.FindTaskRun(node.ID, node.Attempts)
	if errors.Is(err, store.ErrTestRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.DB.Model(&store.TestRun{}).Where("id = ?", run.ID).
		Updates(map[string]any{"started_at": at, "finished_at": at}).Error
}

// demoCounts is one stage's tally.
type demoCounts struct{ total, passed, failed, skipped int }

// demoAttempt builds one stage's attempt result with the demo's explicit
// timing (the store fills in "now" only when a reporter leaves the times
// zero).
func demoAttempt(start, end time.Time, status, summary, errMsg string, counts demoCounts,
	artifacts []store.ArtifactInput) store.AttemptResult {
	return store.AttemptResult{
		Status:     status,
		Summary:    summary,
		Error:      errMsg,
		Total:      counts.total,
		Passed:     counts.passed,
		Failed:     counts.failed,
		Skipped:    counts.skipped,
		StartedAt:  start,
		FinishedAt: end,
		Artifacts:  artifacts,
	}
}

// finishDemoStage reports one stage's attempt and stores its log, the way the
// runner does: the outcome through store.FinishAttempt, then the log as the
// attempt's first part.
func finishDemoStage(s *store.Store, node *store.Task, res store.AttemptResult, lines []string) error {
	if node == nil {
		return errors.New("demo stage is missing from the graph")
	}
	if _, err := s.FinishAttempt(node.ID, res); err != nil {
		return err
	}
	return appendDemoLog(s, node, lines)
}

// appendDemoLog stores one stage's log lines as the first part of the attempt
// the node is on (see runner.LogWriter: a log is the parts stored under the
// run's prefix, and a demo log is a few lines, so one part is all of it). A
// demo seeded without object storage has no logs — the page shows the stage's
// summary and an empty log — rather than failing the seed.
func appendDemoLog(s *store.Store, node *store.Task, lines []string) error {
	text := strings.Join(lines, "")
	if text == "" {
		return nil
	}
	objs := s.Objects()
	if objs == nil {
		return nil
	}
	task, err := s.GetTask(node.ID)
	if err != nil {
		return err
	}
	run, err := s.FindTaskRun(task.ID, task.Attempts)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := objs.Put(ctx, storage.LogKey(objs.KeyPrefix(), run.ID, 0), []byte(text)); err != nil {
		return fmt.Errorf("store the demo log of task %d: %w", task.ID, err)
	}
	if _, err := s.SetRunLogPrefix(run.ID, storage.LogPrefix(objs.KeyPrefix(), run.ID), int64(len(text))); err != nil {
		return err
	}
	return nil
}

// upsertDemoGraph persists the demo entry's graph for one (commit,
// environment) and returns the stored nodes keyed by node key (the stage kinds
// for the stages, "regression:<case>" for the cases). Re-seeding the same pair
// reuses the rows and opens a new attempt per node.
func upsertDemoGraph(s *store.Store, env *store.TestEnvironment, commit *store.Commit) (map[string]*store.Task, error) {
	entry := demoEntry()
	graph, err := runner.BuildTaskGraph(&entry)
	if err != nil {
		return nil, err
	}
	entryJSON, err := json.Marshal(runner.RootConfig{Entry: entry})
	if err != nil {
		return nil, err
	}
	root := &store.Task{
		Kind:          store.TaskKindRoot,
		NodeKey:       store.TaskKindRoot,
		Name:          "test " + demoShortSHA(commit.SHA),
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          runner.SortedTagString(entry.Tags),
		Trigger:       store.TaskTriggerWebhook,
		Config:        string(entryJSON),
	}
	nodes := make([]store.TaskNode, len(graph))
	for i := range graph {
		g := graph[i]
		nodes[i] = store.TaskNode{
			Task: &store.Task{
				Kind:        g.Kind,
				NodeKey:     g.NodeKey,
				Name:        g.Name,
				Description: g.Description,
				Config:      g.Config,
			},
			Deps:      g.Deps,
			ParentKey: g.ParentKey,
		}
	}
	stored, err := s.UpsertTaskGraph(root, nodes)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*store.Task, len(stored))
	for _, n := range stored[1:] { // stored[0] is the root
		byKey[n.NodeKey] = n
	}
	return byKey, nil
}

// --- in-flight demo graphs ----------------------------------------------

// The two phases a seeded in-flight graph can be in.
const (
	phasePending = "pending" // nothing claimed yet
	phaseRunning = "running" // clone finished, build running, tests queued
)

// livePhase is the seeded phase of an environment's in-flight graph: the GPU
// node is mid-build, the MPI cluster has not been claimed yet.
func livePhase(env, running *store.TestEnvironment) string {
	if env == running {
		return phaseRunning
	}
	return phasePending
}

// seedLiveCell inserts one in-flight graph for the demo's newest push so the
// dashboards, the task detail and the graph page can be seen rendering
// pending/running states.
//
// phase "running" needs a node mid-flight, and the store only ever opens
// attempts as pending: the build's task and run are moved to running directly
// here, the same state store.ClaimReadyTask leaves behind when the scheduler
// claims a node.
func seedLiveCell(s *store.Store, sp demoCellSpec, phase string) error {
	if sp.env == nil || sp.commit == nil {
		return errors.New("unknown demo environment or commit")
	}
	if root, err := s.FindRootTaskByCommitEnv(sp.commit.ID, sp.env.ID); err == nil {
		fmt.Printf("in-flight task graph for %s on %s already exists (root #%d)\n",
			demoShortSHA(sp.commit.SHA), sp.env.Name, root.ID)
		return nil
	} else if !errors.Is(err, store.ErrTaskNotFound) {
		return err
	}
	byKey, err := upsertDemoGraph(s, sp.env, sp.commit)
	if err != nil {
		return err
	}
	if phase != phaseRunning {
		// Everything stays queued, which is how UpsertTaskGraph leaves a fresh
		// graph: the root, the container and the cases all read pending.
		fmt.Printf("seeded in-flight task graph (pending): %s on %s\n",
			demoShortSHA(sp.commit.SHA), sp.env.Name)
		return nil
	}

	now := time.Now()
	cloneEnd := now.Add(-4 * time.Minute).Add(90 * time.Second)
	res := demoAttempt(now.Add(-4*time.Minute), cloneEnd, statusPassed,
		"cloned "+demoShortSHA(sp.commit.SHA), "", demoCounts{}, nil)
	if err := finishDemoStage(s, byKey[store.TaskKindClone], res, demoCloneLog(sp.commit, sp.env)); err != nil {
		return err
	}

	build := byKey[store.TaskKindBuild]
	if build == nil {
		return errors.New("demo graph has no build node")
	}
	buildStart := cloneEnd
	if err := s.DB.Model(&store.Task{}).Where("id = ?", build.ID).Updates(map[string]any{
		"status": store.StatusRunning, "started_at": buildStart,
	}).Error; err != nil {
		return err
	}
	if err := s.DB.Model(&store.TestRun{}).Where("task_id = ? AND attempt = ?", build.ID, build.Attempts).
		Updates(map[string]any{"status": store.StatusRunning, "started_at": buildStart}).Error; err != nil {
		return err
	}
	if err := appendDemoLog(s, build, []string{
		"-- The C compiler identification is GNU 13.2.0\n-- The CXX compiler identification is GNU 13.2.0\n",
		"[17/42] Building CXX object src/CMakeFiles/md.dir/integrate/verlet.cpp.o\n",
		"[24/42] Building CXX object src/CMakeFiles/md.dir/neighbor/skin.cpp.o\n",
	}); err != nil {
		return err
	}
	// The containers follow their children: the root reads running, while the
	// regression stage — whose cases are still queued — stays pending.
	if err := s.RollupTaskTree(build.RootID); err != nil {
		return err
	}
	fmt.Printf("seeded in-flight task graph (running): %s on %s\n",
		demoShortSHA(sp.commit.SHA), sp.env.Name)
	return nil
}

// --- demo logs and artifacts --------------------------------------------

// demoCloneLog is the clone stage's log: both repositories fetched and
// uploaded to the node (the runner streams a tar, it does not clone remotely).
func demoCloneLog(commit *store.Commit, env *store.TestEnvironment) []string {
	return []string{
		fmt.Sprintf("Cloning into '%s'...\n", commit.Repo),
		fmt.Sprintf("* branch %s -> FETCH_HEAD\nHEAD is now at %s %s\n",
			commit.Ref, demoShortSHA(commit.SHA), commit.Message),
		fmt.Sprintf("uploaded 118.4 MiB to %s (tar stream)\n", env.Host),
	}
}

// demoBuildLog is the build stage's log: configure, compile, then either the
// stage's own summary line (the runner reads an "MD-BUILDER-SUMMARY:" line out
// of the output) or the failure the command reported.
func demoBuildLog(build demoStage) []string {
	lines := []string{
		"-- The C compiler identification is GNU 13.2.0\n-- The CXX compiler identification is GNU 13.2.0\n",
		"[42/42] Building CXX object src/CMakeFiles/md.dir/integrate/verlet.cpp.o\n",
	}
	if build.errMsg != "" {
		return append(lines,
			"CMake Error at src/CMakeLists.txt:87\n",
			"task failed: "+build.errMsg+"\n")
	}
	return append(lines, "MD-BUILDER-SUMMARY: "+build.summary+"\n")
}

// demoUnitLog is the unit stage's log: one ctest run over the unit tests, then
// the summary line the run carries.
func demoUnitLog(statuses []string) []string {
	lines := []string{"Test project /home/md/build\n"}
	for i, st := range statuses {
		lines = append(lines, fmt.Sprintf("%d/%d Test #%d: %s ........ %s\n",
			i+1, len(statuses), i+1, demoUnitTests[i], demoGTestVerdict(st)))
		if st == statusFailed {
			lines = append(lines, "    assert 1e-12 < |dE| failed (md_test.cc:118)\n")
		}
	}
	return append(lines, "MD-BUILDER-SUMMARY: "+demoUnitSummary(statuses)+"\n")
}

// demoUnitSummary is the unit stage's summary line ("3 of 4 unit tests
// passed"), the text the runner stores on the node and its run.
func demoUnitSummary(statuses []string) string {
	failed := demoCount(statuses, statusFailed)
	if failed > 0 {
		return fmt.Sprintf("%d of %d unit tests passed", len(statuses)-failed, len(statuses))
	}
	return fmt.Sprintf("all %d unit tests passed", len(statuses))
}

// demoCaseLog is one regression case's log: its ctest output and the outcome
// the run records.
func demoCaseLog(name, status, summary string) []string {
	lines := []string{fmt.Sprintf("Test project /home/md/build\n    Start 1: %s\n1/1 Test #1: %s ........ %s\n",
		name, name, demoGTestVerdict(status))}
	if status == statusFailed {
		lines = append(lines, "    "+summary+"\n")
	}
	return append(lines, "MD-BUILDER-SUMMARY: "+summary+"\n")
}

// demoGTestVerdict is a test's ctest verdict word for its status.
func demoGTestVerdict(status string) string {
	switch status {
	case statusFailed:
		return "***Failed"
	case statusSkipped:
		return "***Not Run"
	default:
		return "Passed"
	}
}

// demoUnitArtifact renders the gtest XML the unit run fetches back. The
// browser parses the same file on the run page, so the per-test detail and the
// stored counts agree.
func demoUnitArtifact(statuses []string) []store.ArtifactInput {
	var cases []string
	total, failed := len(statuses), 0
	for i, st := range statuses {
		var node string
		switch st {
		case statusFailed:
			failed++
			node = fmt.Sprintf(`  <testcase name="%s" classname="md" status="run" time="0.31">`+"\n"+
				`    <failure message="assert 1e-12 &lt; |dE| failed" type="">md_test.cc:118&#x0A;      Expected: 1e-12 &gt; |dE|&#x0A;        Actual: 4.2e-9</failure>`+"\n"+
				`  </testcase>`, demoUnitTests[i])
		case statusSkipped:
			node = fmt.Sprintf(`  <testcase name="%s" classname="md" status="notrun" time="0"/>`, demoUnitTests[i])
		default:
			node = fmt.Sprintf(`  <testcase name="%s" classname="md" status="run" time="0.27"/>`, demoUnitTests[i])
		}
		cases = append(cases, node)
	}
	xml := fmt.Sprintf(`<testsuites tests="%d" failures="%d" disabled="0" errors="0" time="1.24" name="AllTests">`+"\n"+
		`  <testsuite name="md" tests="%d" failures="%d" disabled="0" errors="0" time="1.24">`+"\n"+
		"%s\n  </testsuite>\n</testsuites>\n",
		total, failed, total, failed, strings.Join(cases, "\n"))
	return []store.ArtifactInput{{
		Kind:    store.ArtifactKindResults,
		Name:    "build/test_detail.xml",
		Content: xml,
	}}
}

// demoBuildArtifacts renders the build stage's fetched-back files.
func demoBuildArtifacts(names []string) []store.ArtifactInput {
	out := make([]store.ArtifactInput, 0, len(names))
	for _, n := range names {
		var content string
		switch n {
		case "build/.ninja_log":
			content = "# ninja log\n5\t10\t0\tcmake\ta1b2c3\n12\t48\t1\tlink\td4e5f6\n"
		case "build/compile_commands.json":
			content = "[\n  {\n    \"directory\": \"/tmp/md/code/build\",\n    \"command\": \"/usr/bin/c++ -O2 src/md.cc -o md.o\",\n    \"file\": \"../src/md.cc\"\n  }\n]\n"
		default:
			content = "md-builder demo build artifact: " + n + "\n"
		}
		out = append(out, store.ArtifactInput{
			Kind:    store.ArtifactKindFile,
			Name:    n,
			Content: content,
		})
	}
	return out
}

// --- small helpers ------------------------------------------------------

// demoCount counts the entries of statuses equal to status.
func demoCount(statuses []string, status string) int {
	n := 0
	for _, s := range statuses {
		if s == status {
			n++
		}
	}
	return n
}

// demoShortSHA abbreviates a commit id for display.
func demoShortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
