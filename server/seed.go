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

	// Oldest commit (a111111): green on CPU (full pipeline, matching the
	// seeded task graph) and a green regression+build on GPU.
	report(cpu, c1, store.RunKindRegression, "all 3 regression cases within tolerance", 95*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c1, store.RunKindRegression, "all 3 regression cases within tolerance", 95*time.Hour, reg("passed", "passed", "passed"), "")
	report(cpu, c1, store.RunKindUnit, "all 4 unit tests passed", 95*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(cpu, c1, store.RunKindBuild, "build ok (cmake+ninja, 41s)", 96*time.Hour, nil, "")
	report(gpu, c1, store.RunKindBuild, "build ok with CUDA arch sm_80 (8m12s)", 96*time.Hour, nil, "")

	// b222222: a unit regression on the MPI cluster; the GPU build fails so
	// its unit/regression runs are failed "skipped" reports (matching the
	// seeded failed task graph and the runner's recordSkippedRuns).
	report(cpu, c2, store.RunKindRegression, "all 3 regression cases within tolerance", 71*time.Hour, reg("passed", "passed", "passed"), "")
	report(mpi, c2, store.RunKindRegression, "2 of 3 regression cases within tolerance", 70*time.Hour, reg("passed", "failed", "passed"), "")
	report(cpu, c2, store.RunKindUnit, "3 of 4 unit tests passed", 71*time.Hour, unit("passed", "passed", "failed", "passed"), "")
	report(cpu, c2, store.RunKindBuild, "build ok (cmake+ninja, 39s)", 72*time.Hour, nil, "")
	report(mpi, c2, store.RunKindBuild, "build ok (cmake+make -j64, 1m03s)", 72*time.Hour, nil, "")
	// The GPU build failed, so its build run and the two skipped-stage runs
	// are recorded as failed (the simplified path's explicit status).
	report(gpu, c2, store.RunKindBuild, "CMake Error at src/CMakeLists.txt:87: Cannot find package CUDA", 72*time.Hour, nil, store.StatusFailed)
	skipped := "skipped: build (ninja) failed: CMake Error at src/CMakeLists.txt:87: Cannot find package CUDA"
	report(gpu, c2, store.RunKindUnit, skipped, 71*time.Hour, nil, store.StatusFailed)
	report(gpu, c2, store.RunKindRegression, skipped, 71*time.Hour, nil, store.StatusFailed)

	// c333333: neighbor-list tuning breaks water on GPU; MPI build fails.
	report(cpu, c3, store.RunKindRegression, "all 3 regression cases within tolerance", 47*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c3, store.RunKindRegression, "2 of 3 regression cases within tolerance", 47*time.Hour, reg("passed", "failed", "passed"), "")
	report(cpu, c3, store.RunKindUnit, "all 4 unit tests passed", 47*time.Hour, unit("passed", "passed", "passed", "passed"), "")
	report(cpu, c3, store.RunKindBuild, "build ok (cmake+ninja, 40s)", 48*time.Hour, nil, "")
	report(gpu, c3, store.RunKindBuild, "build ok with CUDA arch sm_80 (8m30s)", 48*time.Hour, nil, "")
	report(mpi, c3, store.RunKindBuild, "build failed: CMake Error at src/CMakeLists.txt:87 (target_link_libraries): Cannot find package MPI", 48*time.Hour, nil, store.StatusFailed)

	// d444444: broad-green commit after the refactor (except one MPI case).
	report(cpu, c4, store.RunKindRegression, "all 3 regression cases within tolerance", 23*time.Hour, reg("passed", "passed", "passed"), "")
	report(gpu, c4, store.RunKindRegression, "all 3 regression cases within tolerance", 23*time.Hour, reg("passed", "passed", "passed"), "")
	report(mpi, c4, store.RunKindRegression, "2 of 3 regression cases within tolerance", 22*time.Hour, reg("passed", "passed", "failed"), "")
	report(cpu, c4, store.RunKindUnit, "all 4 unit tests passed", 23*time.Hour, unit("passed", "passed", "passed", "passed"), "")
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
	if err := seedGraphs(s, *force, commits, envs); err != nil {
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
func seedGraphs(s *store.Store, force bool, commits []*store.Commit, envs []*store.TestEnvironment) error {
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
	runStatus := map[store.EnvCommit]map[string]string{}
	for _, r := range runs {
		key := store.EnvCommit{Env: r.EnvironmentID, Commit: r.CommitID}
		if runStatus[key] == nil {
			runStatus[key] = map[string]string{}
		}
		runStatus[key][r.Kind] = r.Status
	}

	for _, env := range envs {
		for _, commit := range commits {
			key := store.EnvCommit{Env: env.ID, Commit: commit.ID}
			statuses := runStatus[key]
			spec := &graphSpec{
				cloneStatus: store.TaskDone,
			}
			// Derive stage outcomes from the runs: a failed run makes the
			// stage failed, a passed run makes it done; a stage without a
			// run has no node in the graph (the real graph contains exactly
			// the configured stages).
			for _, pair := range []struct {
				kind   string
				status **string
			}{
				{store.RunKindBuild, &spec.buildStatus},
				{store.RunKindUnit, &spec.unitStatus},
				{store.RunKindRegression, &spec.regStatus},
			} {
				runSt, ok := statuses[pair.kind]
				if !ok {
					*pair.status = nil
				} else if runSt == store.StatusFailed {
					s := store.TaskFailed
					*pair.status = &s
				} else {
					s := store.TaskDone
					*pair.status = &s
				}
			}
			if err := seedGraph(s, env, commit, spec); err != nil {
				return err
			}
		}
	}
	return nil
}

// graphSpec describes one seeded graph's stage outcomes. A nil stage status
// means the stage is not part of the graph (no run was reported for it).
type graphSpec struct {
	cloneStatus   string
	buildStatus   *string
	unitStatus    *string
	regStatus     *string
	buildSummary  string
	buildErr      string
	cloneDuration time.Duration
}

// stageStatus maps a run status to the sub-task status recorded by the
// runner: done for a passed run, failed for a failed one.
func stageStatus(runStatus string) string {
	if runStatus == store.TaskFailed {
		return store.TaskFailed
	}
	return store.TaskDone
}

// rootStatus derives the root's terminal state from its stages (mirrors the
// runner: failed when any stage failed, done when everything finished).
func rootStatus(spec *graphSpec) string {
	if spec.buildStatus != nil && *spec.buildStatus == store.TaskFailed {
		return store.TaskFailed
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
		Config: `{"build":{"generator":"ninja"},"unit":{"command":"ctest -L unit"},"regression":{"command":"ctest -L regression"}}`,
	}
	// clone and build are always part of the graph; a test stage only when
	// its run exists (the real graph contains exactly the configured stages).
	clone := &store.Task{Kind: store.TaskKindClone, Name: "clone repositories",
		CommitID: commit.ID, EnvironmentID: env.ID}
	build := &store.Task{Kind: store.TaskKindBuild, Name: "build (ninja)",
		CommitID: commit.ID, EnvironmentID: env.ID}
	subs := []*store.Task{clone, build}
	deps := [][]int64{
		{store.TaskRootPlaceholder}, // clone ← root (container, not a gate)
		{store.TaskSubPlaceholderBase + 0},
	}
	if spec.unitStatus != nil {
		subs = append(subs, &store.Task{Kind: store.TaskKindUnit, Name: "unit tests",
			CommitID: commit.ID, EnvironmentID: env.ID})
		deps = append(deps, []int64{store.TaskSubPlaceholderBase + 1})
	}
	if spec.regStatus != nil {
		subs = append(subs, &store.Task{Kind: store.TaskKindRegression, Name: "regression tests",
			CommitID: commit.ID, EnvironmentID: env.ID})
		deps = append(deps, []int64{store.TaskSubPlaceholderBase + 1})
	}
	stored, err := store.CreateTaskGraph(s, root, subs, deps)
	if err != nil {
		return err
	}

	// Statuses, timestamps and (for the tests) run summary lines. The
	// CreateTaskGraph call forces pending; overwrite with the demo outcome.
	start := time.Now().Add(-26 * time.Hour)
	stamps := func(t *store.Task, dur time.Duration) {
		t.StartedAt = &start
		fin := start.Add(dur)
		t.FinishedAt = &fin
	}
	// stored[0] is the root, stored[1] clone, stored[2] build; the test
	// stages follow in the order they were appended above. The build stage
	// always exists (every cell has a build run) — guard anyway.
	buildStatus := store.TaskDone
	if spec.buildStatus != nil {
		buildStatus = *spec.buildStatus
	}
	statuses := map[*store.Task]struct {
		status string
		errMsg string
		dur    time.Duration
	}{
		stored[0]: {rootStatus(spec), spec.buildErr, 15 * time.Minute},
		stored[1]: {store.TaskDone, "", spec.cloneDuration},
		stored[2]: {stageStatus(buildStatus), spec.buildErr, 5 * time.Minute},
	}
	next := 3
	if spec.unitStatus != nil {
		statuses[stored[next]] = struct {
			status string
			errMsg string
			dur    time.Duration
		}{stageStatus(*spec.unitStatus), "", 3 * time.Minute}
		next++
	}
	if spec.regStatus != nil {
		statuses[stored[next]] = struct {
			status string
			errMsg string
			dur    time.Duration
		}{stageStatus(*spec.regStatus), "", 6 * time.Minute}
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
	logs := map[int64][]string{
		stored[1].ID: {fmt.Sprintf("Cloning into '%s'...\n", commit.Repo),
			fmt.Sprintf("* branch main -> FETCH_HEAD\nHEAD is now at %s %s\n", commit.SHA[:7], commit.Message)},
		stored[2].ID: {"-- The C compiler identification is GNU 13.2.0\n-- The CXX compiler identification is GNU 13.2.0\n",
			"[42/42] Building CXX object src/CMakeFiles/md.dir/integrate/verlet.cpp.o\n"},
	}
	next = 3
	if spec.unitStatus != nil {
		logs[stored[next].ID] = []string{"Test project /home/md/build\n    Start 1: TestForce::compute\n1/4 Test #1: TestForce::compute ........ Passed\n"}
		if *spec.unitStatus == store.TaskSkipped {
			logs[stored[next].ID] = []string{"skipped: build stage failed\n"}
		}
		if *spec.unitStatus == store.TaskDone {
			logs[stored[next].ID] = append(logs[stored[next].ID], "MD-BUILDER-SUMMARY: all 4 unit tests passed\n")
		}
		next++
	}
	if spec.regStatus != nil {
		logs[stored[next].ID] = []string{"Test project /home/md/build\n    Start 1: lj-argon-nve\n1/3 Test #1: lj-argon-nve ........ Passed\n"}
		if *spec.regStatus == store.TaskSkipped {
			logs[stored[next].ID] = []string{"skipped: build stage failed\n"}
		}
		if *spec.regStatus == store.TaskDone {
			logs[stored[next].ID] = append(logs[stored[next].ID], "MD-BUILDER-SUMMARY: all 3 regression cases within tolerance\n")
		}
	}
	if spec.buildStatus != nil && *spec.buildStatus == store.TaskFailed {
		logs[stored[2].ID] = append(logs[stored[2].ID],
			"CMake Error at src/CMakeLists.txt:87\n"+spec.buildSummary+"\n")
	} else if spec.buildStatus == nil || *spec.buildStatus == store.TaskDone {
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
