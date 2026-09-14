package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// seedSubcommand populates the database with demo data so the dashboards and
// the task-graph view can be inspected without a real git host or SSH nodes:
// one demo user, three environments, five commits, regression/unit/build run
// reports and finished task graphs with logs.
//
// Usage:
//
//	md-builder seed [-dsn <dsn>] [-force]
//
// The command is idempotent: commits are deduplicated by (repo, sha), run
// reports are replaced by UpsertTestRun and existing demo objects (the user,
// the environments) are detected and kept. With -force it also deletes rows
// left over from a previous interrupted seed (e.g. live tasks without runs).
func seedSubcommand() int {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	dsn := fs.String("dsn", defaultDSN(), "database DSN (sqlite path or postgres URL)")
	force := fs.Bool("force", false, "delete leftover demo tasks before seeding")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return 2
	}

	s, err := store.Open(*dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open db: %v\n", err)
		return 1
	}
	defer s.Close()

	// --- user ------------------------------------------------------------
	const username = "demo"
	var user store.User
	switch err := s.DB.Where("username = ?", username).First(&user).Error; {
	case err == nil:
		fmt.Printf("user %q already exists (id %d)\n", user.Username, user.ID)
	case errors.Is(err, store.ErrNotFound):
		hash, err := auth.HashPassword("demo-pass-123")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: hash password: %v\n", err)
			return 1
		}
		user = store.User{Username: username, Email: "demo@example.com", PasswordHash: hash}
		if err := s.CreateUser(&user); err != nil {
			fmt.Fprintf(os.Stderr, "error: create user: %v\n", err)
			return 1
		}
		fmt.Printf("created user %q (password demo-pass-123)\n", username)
	default:
		fmt.Fprintf(os.Stderr, "error: lookup user: %v\n", err)
		return 1
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
				fmt.Fprintf(os.Stderr, "error: create environment %s: %v\n", env.Name, err)
				return 1
			}
			fmt.Printf("created environment %q (id %d)\n", env.Name, env.ID)
		default:
			fmt.Fprintf(os.Stderr, "error: lookup environment %s: %v\n", env.Name, err)
			return 1
		}
	}

	// --- commits -------------------------------------------------------------
	// Five pushes, oldest first, so the matrix rows read as a history: the
	// newest commit is all-green, an older one has a failing build, etc.
	commits := []*store.Commit{
		{Repo: "group/md-code", SHA: "a111111", Ref: "main", Author: "alice", Message: "Add velocity Verlet integrator", PushedAt: time.Now().Add(-96 * time.Hour)},
		{Repo: "group/md-code", SHA: "b222222", Ref: "main", Author: "bob", Message: "Fix PBC image remapping", PushedAt: time.Now().Add(-72 * time.Hour)},
		{Repo: "group/md-code", SHA: "c333333", Ref: "main", Author: "alice", Message: "Tune neighbor list skin", PushedAt: time.Now().Add(-48 * time.Hour)},
		{Repo: "group/md-code", SHA: "d444444", Ref: "main", Author: "bob", Message: "Refactor pair styles", PushedAt: time.Now().Add(-24 * time.Hour)},
		{Repo: "group/md-code", SHA: "e555555", Ref: "main", Author: "carol", Message: "Switch to C++17", PushedAt: time.Now().Add(-2 * time.Hour)},
	}
	for _, c := range commits {
		if _, err := s.GetOrCreateCommit(c); err != nil {
			fmt.Fprintf(os.Stderr, "error: create commit %s: %v\n", c.SHA, err)
			return 1
		}
	}
	fmt.Printf("ensured %d commits on group/md-code\n", len(commits))

	// --- site config ------------------------------------------------------
	// Point the code-repo filter at the demo repository so the dashboard
	// actually shows the seeded commits (a filter for another repo, e.g.
	// left over from a smoke test, would hide every row).
	cfg, err := s.GetSiteConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load site config: %v\n", err)
		return 1
	}
	if !strings.Contains(cfg.CodeRepo, "group/md-code") {
		cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
		if err := s.SaveSiteConfig(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "error: save site config: %v\n", err)
			return 1
		}
		fmt.Println("set site code repo to group/md-code")
	}

	// --- test runs -------------------------------------------------------------
	now := time.Now()
	// report stores one run. statusOverride is only meaningful for runs
	// without cases (the simplified path); pass "" to derive it from the
	// cases, or store.StatusFailed/StatusPassed to force it.
	report := func(envID, commitID int64, kind, summary string, startedAgo time.Duration, cases []store.TestCaseResult, statusOverride string) {
		status := statusOverride
		if status == "" {
			status = runStatusOf(cases)
		}
		t0 := now.Add(-startedAgo)
		_, err := s.UpsertTestRun(&store.RunInput{
			EnvironmentID: envID, CommitID: commitID, Kind: kind,
			Cases: cases, Status: status, Summary: summary,
			StartedAt: t0, FinishedAt: t0.Add(4 * time.Minute),
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: report %s run (env %d, commit %d): %v\n", kind, envID, commitID, err)
			os.Exit(1)
		}
	}
	reg := func(statuses ...string) []store.TestCaseResult {
		names := []string{"lj-argon-nve", "water-tip4p-npt", "argon-liquid-nvt"}
		errs := map[string]float64{"passed": 3.2e-07, "failed": 0.021}
		cases := make([]store.TestCaseResult, len(statuses))
		for i, st := range statuses {
			cases[i] = store.TestCaseResult{Name: names[i], Status: st, ErrorValue: errs[st],
				Message: map[string]string{"passed": "max rel err", "failed": "energy drift above threshold"}[st]}
		}
		return cases
	}
	unit := func(statuses ...string) []store.TestCaseResult {
		names := []string{"TestForce::compute", "TestIntegrate::verlet", "TestNeighborList::rebuild", "TestPBC::unwrap"}
		cases := make([]store.TestCaseResult, len(statuses))
		for i, st := range statuses {
			cases[i] = store.TestCaseResult{Name: names[i], Status: st,
				Message: map[string]string{"failed": "assert 1e-12 < |dE| failed"}[st]}
		}
		return cases
	}

	cpu, gpu, mpi := envs[0].ID, envs[1].ID, envs[2].ID
	c1, c2, c3, c4, c5 := commits[0].ID, commits[1].ID, commits[2].ID, commits[3].ID, commits[4].ID

	// Oldest commit (a111111): fully green on CPU and GPU. The MPI cluster
	// was registered after this push, so it has no runs and no task graph
	// for it — the dashboards show an em dash in that cell (and no graph
	// link), the "environment never tested this push" case.
	report(cpu, c1, store.RunKindRegression, "all 3 regression cases within tolerance", 95*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c1, store.RunKindRegression, "all 3 regression cases within tolerance", 95*time.Hour, reg("passed", "passed", "passed"), "")
	report(cpu, c1, store.RunKindUnit, "all 4 unit tests passed", 95*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(gpu, c1, store.RunKindUnit, "all 4 unit tests passed", 95*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(cpu, c1, store.RunKindBuild, "build ok (cmake+ninja, 41s)", 96*time.Hour, nil, "")
	report(gpu, c1, store.RunKindBuild, "build ok with CUDA arch sm_80 (8m12s)", 96*time.Hour, nil, "")

	// b222222: a unit regression on the MPI cluster; the GPU build fails so
	// its unit/regression runs are failed "skipped" reports (matching the
	// seeded failed task graph and the runner's recordSkippedRuns).
	report(cpu, c2, store.RunKindRegression, "all 3 regression cases within tolerance", 71*time.Hour, reg("passed", "passed", "passed"), "")
	report(mpi, c2, store.RunKindRegression, "2 of 3 regression cases within tolerance", 70*time.Hour, reg("passed", "failed", "passed"), "")
	report(cpu, c2, store.RunKindUnit, "3 of 4 unit tests passed", 71*time.Hour, unit("passed", "passed", "failed", "passed"), "")
	report(mpi, c2, store.RunKindUnit, "all 4 unit tests passed", 70*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(cpu, c2, store.RunKindBuild, "build ok (cmake+ninja, 39s)", 72*time.Hour, nil, "")
	report(mpi, c2, store.RunKindBuild, "build ok (cmake+make -j64, 1m03s)", 72*time.Hour, nil, "")
	// The GPU build failed, so its build run and the two skipped-stage runs
	// are recorded as failed (the simplified path's explicit status).
	report(gpu, c2, store.RunKindBuild, "CMake Error at src/CMakeLists.txt:87: Cannot find package CUDA", 72*time.Hour, nil, store.StatusFailed)
	skipped := "skipped: build (ninja) failed: CMake Error at src/CMakeLists.txt:87: Cannot find package CUDA"
	report(gpu, c2, store.RunKindUnit, skipped, 71*time.Hour, nil, store.StatusFailed)
	report(gpu, c2, store.RunKindRegression, skipped, 71*time.Hour, nil, store.StatusFailed)

	// c333333: neighbor-list tuning breaks water on GPU; MPI build fails (so
	// MPI unit/regression are skipped runs, recorded here just like the
	// runner's recordSkippedRuns).
	report(cpu, c3, store.RunKindRegression, "all 3 regression cases within tolerance", 47*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c3, store.RunKindRegression, "2 of 3 regression cases within tolerance", 47*time.Hour, reg("passed", "failed", "passed"), "")
	report(cpu, c3, store.RunKindUnit, "all 4 unit tests passed", 47*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(gpu, c3, store.RunKindUnit, "all 4 unit tests passed", 47*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(cpu, c3, store.RunKindBuild, "build ok (cmake+ninja, 40s)", 48*time.Hour, nil, "")
	report(gpu, c3, store.RunKindBuild, "build ok with CUDA arch sm_80 (8m30s)", 48*time.Hour, nil, "")
	mpiBuildFail := "build failed: CMake Error at src/CMakeLists.txt:87 (target_link_libraries): Cannot find package MPI"
	report(mpi, c3, store.RunKindBuild, mpiBuildFail, 48*time.Hour, nil, store.StatusFailed)
	mpiSkipped := "skipped: build (ninja) failed: CMake Error at src/CMakeLists.txt:87 (target_link_libraries): Cannot find package MPI"
	report(mpi, c3, store.RunKindUnit, mpiSkipped, 47*time.Hour, nil, store.StatusFailed)
	report(mpi, c3, store.RunKindRegression, mpiSkipped, 47*time.Hour, nil, store.StatusFailed)

	// d444444: broad-green commit after the refactor (except one MPI case).
	report(cpu, c4, store.RunKindRegression, "all 3 regression cases within tolerance", 23*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c4, store.RunKindRegression, "all 3 regression cases within tolerance", 23*time.Hour, reg("passed", "passed", "passed"), "")
	report(mpi, c4, store.RunKindRegression, "2 of 3 regression cases within tolerance", 22*time.Hour, reg("passed", "passed", "failed"), "")
	report(cpu, c4, store.RunKindUnit, "all 4 unit tests passed", 23*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(gpu, c4, store.RunKindUnit, "all 4 unit tests passed", 23*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(mpi, c4, store.RunKindUnit, "3 of 4 unit tests passed", 22*time.Hour, unit("passed", "failed", "passed", "passed"), "")
	report(cpu, c4, store.RunKindBuild, "build ok (cmake+ninja, 38s)", 24*time.Hour, nil, "")
	report(gpu, c4, store.RunKindBuild, "build ok with CUDA arch sm_80 (7m58s)", 24*time.Hour, nil, "")
	report(mpi, c4, store.RunKindBuild, "build ok (cmake+make -j64, 58s)", 24*time.Hour, nil, "")

	// e555555 (newest): fully green everywhere — the good state.
	for _, envID := range []int64{cpu, gpu, mpi} {
		report(envID, c5, store.RunKindRegression, "all 3 regression cases within tolerance", 2*time.Hour, reg("passed", "passed", "passed"), "")
		report(envID, c5, store.RunKindUnit, "all 4 unit tests passed", 2*time.Hour, unit("passed", "passed", "passed", "passed"), "")
		summary := "build ok (cmake+ninja, 37s)"
		if envID == gpu {
			summary = "build ok with CUDA arch sm_80 (7m44s)"
		} else if envID == mpi {
			summary = "build ok (cmake+make -j64, 55s)"
		}
		report(envID, c5, store.RunKindBuild, summary, 2*time.Hour, nil, "")
	}
	fmt.Println("reported regression/unit/build runs for the demo matrix")

	// --- task graphs -----------------------------------------------------------
	// Realistic graphs for the two oldest commits (the newest ones keep
	// clean cells): finished graphs with logs, so the dependency-graph page
	// has something to show. Statuses are terminal (done/failed/skipped) so
	// the live scheduler will not claim them.
	//
	// The newest push (f666666) carries two in-flight graphs (one running
	// mid-build with live-style logs, one fully queued) so the dashboards,
	// the job detail and the graph page can be seen handling pending/running
	// states. These are seeds, not live tasks: no worker will ever claim
	// them (nothing writes new logs), but every read path renders them.
	liveCommit := &store.Commit{Repo: "group/md-code", SHA: "f666666", Ref: "main",
		Author: "carol", Message: "Add thermostat barostat coupling",
		PushedAt: time.Now().Add(-20 * time.Minute)}
	if _, err := s.GetOrCreateCommit(liveCommit); err != nil {
		fmt.Fprintf(os.Stderr, "error: create commit %s: %v\n", liveCommit.SHA, err)
		return 1
	}
	if err := seedGraphs(s, *force, commits, envs,
		liveSpec{env: gpu, commit: liveCommit.ID, phase: "running"},
		liveSpec{env: mpi, commit: liveCommit.ID, phase: "pending"},
	); err != nil {
		fmt.Fprintf(os.Stderr, "error: seed task graphs: %v\n", err)
		return 1
	}

	fmt.Println("seed done — log in as demo / demo-pass-123 and open the Dashboard")
	return 0
}

// runStatusOf derives the run status from the case list (mirrors the
// UpsertTestRun rule: a run passes when every case passes).
func runStatusOf(cases []store.TestCaseResult) string {
	if len(cases) == 0 {
		return store.StatusPassed // not used for build runs (no cases)
	}
	for _, c := range cases {
		if c.Status == store.StatusFailed {
			return store.StatusFailed
		}
	}
	return store.StatusPassed
}

// seedGraphs creates a finished task graph (root + clone/build/unit/
// regression sub-tasks with logs) for every (commit, environment) pair, so
// every full-matrix cell carries a graph link. Stage statuses are derived
// from the run reports: a failed run makes the stage failed (and the test
// stages skipped, mirroring the runner's failure propagation). Statuses are
// terminal so the live scheduler will not claim them.
func seedGraphs(s *store.Store, force bool, commits []*store.Commit, envs []*store.TestEnvironment, live ...liveSpec) error {
	if force {
		// Drop all demo graphs (roots, sub-tasks and logs) so they are
		// rebuilt. Run reports are kept: they are idempotent.
		var rootIDs []int64
		if err := s.DB.Model(&store.Task{}).Where("kind = ?", store.TaskKindRoot).
			Pluck("id", &rootIDs).Error; err != nil {
			return err
		}
		if len(rootIDs) > 0 {
			var subIDs []int64
			if err := s.DB.Model(&store.Task{}).Where("root_id IN ?", rootIDs).
				Pluck("id", &subIDs).Error; err != nil {
				return err
			}
			for _, id := range append(subIDs, rootIDs...) {
				if err := s.DeleteTaskLogs(id); err != nil {
					return err
				}
			}
			if err := s.DB.Where("id IN ?", append(subIDs, rootIDs...)).Delete(&store.Task{}).Error; err != nil {
				return err
			}
			fmt.Printf("dropped %d leftover demo graph(s)\n", len(rootIDs))
		}
	}

	// Look up the run statuses per (env, commit, kind) to mirror the state
	// the real runner would have produced when reporting these runs.
	var runs []store.TestRun
	if err := s.DB.Find(&runs).Error; err != nil {
		return err
	}
	runStateOf := map[store.EnvCommit]map[string]seedRunState{}
	for _, r := range runs {
		key := store.EnvCommit{Env: r.EnvironmentID, Commit: r.CommitID}
		if runStateOf[key] == nil {
			runStateOf[key] = map[string]seedRunState{}
		}
		st := r.Status
		// A failed run whose summary starts with "skipped:" is the runner's
		// recordSkippedRuns artifact: the stage itself was skipped (see
		// SkipDependents), so the graph node shows skipped, not failed.
		if st == store.StatusFailed && strings.HasPrefix(r.Summary, "skipped:") {
			st = "skipped"
		}
		runStateOf[key][r.Kind] = seedRunState{status: st, summary: r.Summary}
	}

	for _, env := range envs {
		for _, commit := range commits {
			key := store.EnvCommit{Env: env.ID, Commit: commit.ID}
			stageRuns := runStateOf[key]
			if len(stageRuns) == 0 {
				// No runs at all on this environment: nothing was ever tested
				// for this push there (e.g. the environment was registered
				// later). Skip the graph so the dashboards show the em dash —
				// a graph without any run would render as blank nodes.
				continue
			}
			if err := seedGraph(s, env, commit, specFor(stageRuns)); err != nil {
				return err
			}
		}
	}

	// In-flight demo graphs: seeded after the terminal ones (they are on
	// their own commit, so the idempotence check never collides).
	for _, ls := range live {
		env := envByID(envs, ls.env)
		if env == nil {
			continue
		}
		if err := seedLiveGraph(s, env, ls.commit, ls.phase); err != nil {
			return err
		}
	}
	return nil
}

// liveSpec requests one in-flight demo graph: phase is "running" (clone
// done, build running with log output, tests queued) or "pending" (nothing
// claimed yet).
type liveSpec struct {
	env    int64
	commit int64
	phase  string
}

// seedRunState is one stage's run status/summary snapshot used to derive a
// terminal graph spec.
type seedRunState struct {
	status  string
	summary string
}

// envByID finds an environment by id (nil when absent).
func envByID(envs []*store.TestEnvironment, id int64) *store.TestEnvironment {
	for _, e := range envs {
		if e.ID == id {
			return e
		}
	}
	return nil
}

// specFor derives the terminal graph spec from the stage runs (passed →
// done, failed → failed, skipped or missing → skipped).
func specFor(stageRuns map[string]seedRunState) *graphSpec {
	spec := &graphSpec{cloneStatus: store.TaskDone}
	// Every graph carries all four nodes. A stage's status comes from its
	// run: passed → done, failed → failed, skipped (or no run at all) →
	// skipped. A stage without its own run never executed — it is
	// downstream of a failure, exactly the runner's
	// SkipDependents/recordSkippedRuns outcome.
	for _, pair := range []struct {
		kind   string
		status *string
	}{
		{store.RunKindBuild, &spec.buildStatus},
		{store.RunKindUnit, &spec.unitStatus},
		{store.RunKindRegression, &spec.regStatus},
	} {
		switch runSt := stageRuns[pair.kind].status; {
		case runSt == "skipped", runSt == "":
			*pair.status = store.TaskSkipped
		case runSt == store.StatusFailed:
			*pair.status = store.TaskFailed
			// The failed build's error text (the CMake error) feeds the
			// node's error and log, like the runner's fail path.
			if pair.kind == store.RunKindBuild {
				spec.buildErr = stageRuns[pair.kind].summary
				spec.buildSummary = stageRuns[pair.kind].summary
			}
		default: // passed
			*pair.status = store.TaskDone
			if pair.kind == store.RunKindBuild {
				spec.buildSummary = stageRuns[pair.kind].summary
			}
		}
	}
	return spec
}

// graphSpec describes one seeded graph's stage outcomes. Every stage has a
// status: the graph always carries all four nodes.
type graphSpec struct {
	cloneStatus   string
	buildStatus   string
	unitStatus    string
	regStatus     string
	buildSummary  string
	buildErr      string
	cloneDuration time.Duration
}

// rootStatus derives the root's terminal state from its stages (mirrors the
// runner: failed when any stage failed or was skipped, done otherwise).
func rootStatus(spec *graphSpec) string {
	for _, st := range []string{spec.cloneStatus, spec.buildStatus, spec.unitStatus, spec.regStatus} {
		if st == store.TaskFailed || st == store.TaskSkipped {
			return store.TaskFailed
		}
	}
	return store.TaskDone
}

// seedGraph inserts one root + four sub-tasks with logs, directly through
// the store layer. The graph shape mirrors runner.BuildTaskGraph: clone ←
// root, build ← clone, unit/regression ← build. A graph for the same
// (environment, commit) is skipped so re-running the seed does not
// duplicate rows.
func seedGraph(s *store.Store, env *store.TestEnvironment, commit *store.Commit, spec *graphSpec) error {
	var existing store.Task
	if err := s.DB.Where("kind = ? AND environment_id = ? AND commit_id = ?",
		store.TaskKindRoot, env.ID, commit.ID).First(&existing).Error; err == nil {
		fmt.Printf("task graph for %s on %s already exists (root #%d)\n",
			commit.SHA[:7], env.Name, existing.ID)
		return nil
	}
	root := &store.Task{
		Kind: store.TaskKindRoot, Name: "test " + commit.SHA, Status: store.TaskDone,
		CommitID: commit.ID, EnvironmentID: env.ID, Tags: env.Tags,
		Config: `{"build":{"command":"ninja"},"unit":{"command":"ctest -L unit"},"regression":{"command":"ctest -L regression"}}`,
	}
	// The graph always carries all four nodes: clone ← root, build ← clone,
	// unit/regression ← build.
	clone := &store.Task{Kind: store.TaskKindClone, Name: "clone repositories",
		CommitID: commit.ID, EnvironmentID: env.ID}
	build := &store.Task{Kind: store.TaskKindBuild, Name: "build",
		CommitID: commit.ID, EnvironmentID: env.ID}
	unitT := &store.Task{Kind: store.TaskKindUnit, Name: "unit tests",
		CommitID: commit.ID, EnvironmentID: env.ID}
	regT := &store.Task{Kind: store.TaskKindRegression, Name: "regression tests",
		CommitID: commit.ID, EnvironmentID: env.ID}
	subs := []*store.Task{clone, build, unitT, regT}
	deps := [][]int64{
		{store.TaskRootPlaceholder}, // clone ← root (container, not a gate)
		{store.TaskSubPlaceholderBase + 0},
		{store.TaskSubPlaceholderBase + 1},
		{store.TaskSubPlaceholderBase + 1},
	}
	stored, err := store.CreateTaskGraph(s, root, subs, deps)
	if err != nil {
		return err
	}

	// Statuses, timestamps and log chunks. The CreateTaskGraph call forces
	// pending; overwrite with the demo outcome.
	start := time.Now().Add(-26 * time.Hour)
	stamps := func(t *store.Task, dur time.Duration) {
		t.StartedAt = &start
		fin := start.Add(dur)
		t.FinishedAt = &fin
	}
	statuses := map[*store.Task]struct {
		status string
		errMsg string
		dur    time.Duration
	}{
		stored[0]: {rootStatus(spec), spec.buildErr, 15 * time.Minute},
		stored[1]: {spec.cloneStatus, "", spec.cloneDuration},
		stored[2]: {spec.buildStatus, spec.buildErr, 5 * time.Minute},
		stored[3]: {spec.unitStatus, "", 3 * time.Minute},
		stored[4]: {spec.regStatus, "", 6 * time.Minute},
	}
	for t, st := range statuses {
		t.Status = st.status
		t.Error = st.errMsg
		stamps(t, st.dur)
		if err := s.DB.Select("status", "error", "started_at", "finished_at").
			Updates(t).Error; err != nil {
			return err
		}
	}

	// Per-stage logs (matching the statuses) for the log viewer. Skip logs
	// read as one line, exactly like the runner's skip path.
	const unitSkipReason = "skipped: upstream task build (ninja) failed"
	logs := map[int64][]string{
		stored[1].ID: {fmt.Sprintf("Cloning into '%s'...\n", commit.Repo),
			fmt.Sprintf("* branch main -> FETCH_HEAD\nHEAD is now at %s %s\n", commit.SHA[:7], commit.Message)},
		stored[2].ID: {"-- The C compiler identification is GNU 13.2.0\n-- The CXX compiler identification is GNU 13.2.0\n",
			"[42/42] Building CXX object src/CMakeFiles/md.dir/integrate/verlet.cpp.o\n"},
		stored[3].ID: {"Test project /home/md/build\n    Start 1: TestForce::compute\n1/4 Test #1: TestForce::compute ........ Passed\n"},
		stored[4].ID: {"Test project /home/md/build\n    Start 1: lj-argon-nve\n1/3 Test #1: lj-argon-nve ........ Passed\n"},
	}
	switch spec.buildStatus {
	case store.TaskFailed:
		logs[stored[2].ID] = append(logs[stored[2].ID],
			"CMake Error at src/CMakeLists.txt:87\n"+spec.buildSummary+"\n")
	case store.TaskSkipped:
		logs[stored[2].ID] = []string{"skipped: upstream task clone repositories failed\n"}
	}
	if spec.unitStatus == store.TaskSkipped {
		logs[stored[3].ID] = []string{unitSkipReason + "\n"}
	} else if spec.unitStatus == store.TaskDone {
		logs[stored[3].ID] = append(logs[stored[3].ID], "MD-BUILDER-SUMMARY: all 4 unit tests passed\n")
	}
	if spec.regStatus == store.TaskSkipped {
		logs[stored[4].ID] = []string{unitSkipReason + "\n"}
	} else if spec.regStatus == store.TaskDone {
		logs[stored[4].ID] = append(logs[stored[4].ID], "MD-BUILDER-SUMMARY: all 3 regression cases within tolerance\n")
	}
	if spec.buildStatus == store.TaskDone {
		logs[stored[2].ID] = append(logs[stored[2].ID], "MD-BUILDER-SUMMARY: "+spec.buildSummary+"\n")
	}
	for taskID, chunks := range logs {
		for i, content := range chunks {
			if err := s.AppendTaskLog(taskID, i+1, content); err != nil {
				return err
			}
		}
	}
	fmt.Printf("seeded task graph: root #%d (%s × %s on %s)\n",
		root.ID, commit.SHA[:7], "pipeline", env.Name)
	return nil
}

// seedLiveGraph inserts one in-flight graph (root + four sub-tasks) for the
// demo's newest push, so the dashboards, the job detail and the graph page
// can be seen rendering pending/running states. phase "running" gives a
// mid-build snapshot (clone done with its log, build running with partial
// output, tests pending); phase "pending" leaves everything unclaimed. The
// graph is a static snapshot — no worker will ever advance it — but every
// read path (matrix live overlay, task detail, log polling, graph page)
// handles it exactly like a real dispatch.
func seedLiveGraph(s *store.Store, env *store.TestEnvironment, commitID int64, phase string) error {
	var commit store.Commit
	if err := s.DB.Where("id = ?", commitID).First(&commit).Error; err != nil {
		return err
	}
	var existing store.Task
	if err := s.DB.Where("kind = ? AND environment_id = ? AND commit_id = ?",
		store.TaskKindRoot, env.ID, commitID).First(&existing).Error; err == nil {
		fmt.Printf("live task graph for %s on %s already exists (root #%d)\n",
			commit.SHA[:7], env.Name, existing.ID)
		return nil
	}
	root := &store.Task{
		Kind: store.TaskKindRoot, Name: "test " + commit.SHA[:7], Status: store.TaskPending,
		CommitID: commitID, EnvironmentID: env.ID, Tags: env.Tags,
		Config: `{"build":{"command":"ninja"},"unit":{"command":"ctest -L unit"},"regression":{"command":"ctest -L regression"}}`,
	}
	clone := &store.Task{Kind: store.TaskKindClone, Name: "clone repositories",
		CommitID: commitID, EnvironmentID: env.ID}
	build := &store.Task{Kind: store.TaskKindBuild, Name: "build",
		CommitID: commitID, EnvironmentID: env.ID}
	unitT := &store.Task{Kind: store.TaskKindUnit, Name: "unit tests",
		CommitID: commitID, EnvironmentID: env.ID}
	regT := &store.Task{Kind: store.TaskKindRegression, Name: "regression tests",
		CommitID: commitID, EnvironmentID: env.ID}
	subs := []*store.Task{clone, build, unitT, regT}
	deps := [][]int64{
		{store.TaskRootPlaceholder},
		{store.TaskSubPlaceholderBase + 0},
		{store.TaskSubPlaceholderBase + 1},
		{store.TaskSubPlaceholderBase + 1},
	}
	stored, err := store.CreateTaskGraph(s, root, subs, deps)
	if err != nil {
		return err
	}

	// Statuses and timestamps: the running phase marks root+build running
	// and clone done (with a started_at each); pending leaves everything
	// untouched (CreateTaskGraph wrote pending). The running build carries
	// partial log output, like a session mid-flight.
	now := time.Now()
	statuses := map[*store.Task]string{}
	logs := map[int64][]string{}
	if phase == "running" {
		statuses[root] = store.TaskRunning
		statuses[stored[1]] = store.TaskDone    // clone
		statuses[stored[2]] = store.TaskRunning // build
		logs[stored[1].ID] = []string{
			"Cloning into '" + commit.Repo + "'...\n",
			"* branch main -> FETCH_HEAD\nHEAD is now at " + commit.SHA[:7] + " " + commit.Message + "\n",
			"uploaded 118.4 MiB to " + env.Host + " (tar stream)\n",
		}
		logs[stored[2].ID] = []string{
			"-- The C compiler identification is GNU 13.2.0\n-- The CXX compiler identification is GNU 13.2.0\n",
			"[17/42] Building CXX object src/CMakeFiles/md.dir/integrate/verlet.cpp.o\n",
			"[24/42] Building CXX object src/CMakeFiles/md.dir/neighbor/skin.cpp.o\n",
		}
	}
	for t, st := range statuses {
		updates := map[string]any{"status": st}
		if st == store.TaskRunning || st == store.TaskDone {
			updates["started_at"] = now.Add(-4 * time.Minute)
		}
		if err := s.DB.Model(t).Updates(updates).Error; err != nil {
			return err
		}
	}
	for taskID, chunks := range logs {
		for i, content := range chunks {
			if err := s.AppendTaskLog(taskID, i+1, content); err != nil {
				return err
			}
		}
	}
	fmt.Printf("seeded live task graph (%s): root #%d (%s on %s)\n",
		phase, root.ID, commit.SHA[:7], env.Name)
	return nil
}
