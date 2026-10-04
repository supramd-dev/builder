package api

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/auth"
	"md-builder/server/storage"
	"md-builder/server/store"
)

// --- graph fixtures ---
//
// A dashboard cell is the state of one node of a dispatched task graph, not a
// run row: the fixture below dispatches the same graphs the runner builds
// (clone [→ build] [→ unit] [→ regression container → cases]) and reports the
// stages a spec finishes.

// stageSpec describes one stage node of a graph to dispatch: its description
// (the md-builder.yaml label) and the attempt result to report, if any. A nil
// res leaves the node queued — the state a freshly dispatched graph is in.
type stageSpec struct {
	desc string
	res  *store.AttemptResult
}

// caseSpec is one regression case node: its preset name plus its outcome.
type caseSpec struct {
	name string
	desc string
	res  *store.AttemptResult
}

// graphSpec is a whole test graph to dispatch for one (environment, commit):
// the optional stages it defines. The clone stage is always present; a nil
// *stageSpec means "this graph does not define that stage".
type graphSpec struct {
	clone *stageSpec
	build *stageSpec
	unit  *stageSpec
	cases []caseSpec
}

// passed, failed and skipped build the attempt results a spec reports.
func passed() *store.AttemptResult {
	return &store.AttemptResult{Status: store.StatusPassed, Total: 1, Passed: 1}
}

func failed(summary string) *store.AttemptResult {
	return &store.AttemptResult{Status: store.StatusFailed, Summary: summary, Total: 1, Failed: 1}
}

func skipped(summary string) *store.AttemptResult {
	return &store.AttemptResult{Status: store.StatusSkipped, Summary: summary, Total: 1, Skipped: 1}
}

// regressionCaseKey mirrors runner.RegressionCaseKey: the stable node key of
// one regression case node.
func regressionCaseKey(name string) string {
	return store.TaskKindRegressionStage + ":" + name
}

// dispatchTestGraph persists one test graph for (environment, commit) and
// reports the stages the spec gives a result for. It returns the root task and
// the stored nodes keyed by node key (the root included, under
// store.TaskKindRoot).
//
// The shape mirrors what the runner builds: a clone, then the optional build
// and unit stages, then the virtual regression container with one case node
// per preset. Every real node gets its attempt's run at dispatch, so a queued
// stage already has a run and a detail page.
func dispatchTestGraph(t *testing.T, s *store.Store, env *store.TestEnvironment, commit *store.Commit, spec graphSpec) (*store.Task, map[string]*store.Task) {
	t.Helper()

	root := &store.Task{
		Kind: store.TaskKindRoot, Name: "test " + shortSHA(commit.SHA),
		CommitID: commit.ID, EnvironmentID: env.ID,
		Tags: env.Tags, Config: "{}", Trigger: store.TaskTriggerWebhook,
	}
	nodes := []store.TaskNode{{
		Task: &store.Task{Kind: store.TaskKindClone, NodeKey: store.TaskKindClone,
			Name: "clone repositories", Config: "{}"},
	}}
	testDep := int64(0) // index of the clone node
	if spec.build != nil {
		nodes = append(nodes, store.TaskNode{
			Task: &store.Task{Kind: store.TaskKindBuild, NodeKey: store.TaskKindBuild,
				Name: "build", Description: spec.build.desc, Config: "{}"},
			Deps: []int64{store.TaskSubPlaceholderBase + testDep},
		})
		testDep = int64(len(nodes) - 1)
	}
	if spec.unit != nil {
		nodes = append(nodes, store.TaskNode{
			Task: &store.Task{Kind: store.TaskKindUnit, NodeKey: store.TaskKindUnit,
				Name: "unit tests", Description: spec.unit.desc, Config: "{}"},
			Deps: []int64{store.TaskSubPlaceholderBase + testDep},
		})
	}
	if len(spec.cases) > 0 {
		nodes = append(nodes, store.TaskNode{
			Task: &store.Task{Kind: store.TaskKindRegressionStage, NodeKey: store.TaskKindRegressionStage,
				Name: "regression", Description: "regression cases", Config: "{}"},
		})
		for i := range spec.cases {
			c := &spec.cases[i]
			nodes = append(nodes, store.TaskNode{
				Task: &store.Task{Kind: store.TaskKindRegressionCase, NodeKey: regressionCaseKey(c.name),
					Name: "regression: " + c.name, Description: c.desc, Config: "{}"},
				Deps:      []int64{store.TaskSubPlaceholderBase + testDep},
				ParentKey: store.TaskKindRegressionStage,
			})
		}
	}

	stored, err := s.UpsertTaskGraph(root, nodes)
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	byKey := make(map[string]*store.Task, len(stored)+1)
	byKey[root.NodeKey] = root
	for i := range stored {
		byKey[stored[i].NodeKey] = stored[i]
	}

	finish := func(sp *stageSpec, key string) {
		if sp == nil || sp.res == nil {
			return
		}
		if _, err := s.FinishAttempt(byKey[key].ID, *sp.res); err != nil {
			t.Fatalf("finish %s: %v", key, err)
		}
	}
	finish(spec.clone, store.TaskKindClone)
	finish(spec.build, store.TaskKindBuild)
	finish(spec.unit, store.TaskKindUnit)
	for i := range spec.cases {
		c := &spec.cases[i]
		finish(&stageSpec{res: c.res}, regressionCaseKey(c.name))
	}
	return byKey[store.TaskKindRoot], byKey
}

// nodeOf returns the node of one graph with the given node key.
func nodeOf(t *testing.T, byKey map[string]*store.Task, key string) *store.Task {
	t.Helper()
	node, ok := byKey[key]
	if !ok {
		t.Fatalf("graph has no node %q (has %v)", key, keysOf2(byKey))
	}
	return node
}

// keysOf2 lists a node map's keys for failure messages.
func keysOf2(m map[string]*store.Task) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}

// envByName loads a seeded environment by name.
func envByName(t *testing.T, s *store.Store, name string) *store.TestEnvironment {
	t.Helper()
	var env store.TestEnvironment
	if err := s.DB.Where("name = ?", name).First(&env).Error; err != nil {
		t.Fatalf("load environment %q: %v", name, err)
	}
	return &env
}

// commitBySHA loads a seeded commit row by SHA.
func commitBySHA(t *testing.T, s *store.Store, sha string) *store.Commit {
	t.Helper()
	var c store.Commit
	if err := s.DB.Where("sha = ?", sha).First(&c).Error; err != nil {
		t.Fatalf("load commit %q: %v", sha, err)
	}
	return &c
}

// taskOf resolves one node of a dispatched graph by (environment, commit SHA,
// node key).
func taskOf(t *testing.T, s *store.Store, envName, sha, nodeKey string) *store.Task {
	t.Helper()
	env := envByName(t, s, envName)
	commit := commitBySHA(t, s, sha)
	root, err := s.FindRootTaskByCommitEnv(commit.ID, env.ID)
	if err != nil {
		t.Fatalf("root task of %s@%s: %v", envName, sha, err)
	}
	nodes, err := s.ListActiveNodes(root.ID)
	if err != nil {
		t.Fatalf("active nodes: %v", err)
	}
	for i := range nodes {
		if nodes[i].NodeKey == nodeKey {
			return &nodes[i]
		}
	}
	t.Fatalf("node %q not found in the graph of %s@%s", nodeKey, envName, sha)
	return nil
}

// rootTaskOf resolves the root task of one (environment, commit) pair.
func rootTaskOf(t *testing.T, s *store.Store, envName, sha string) *store.Task {
	t.Helper()
	env := envByName(t, s, envName)
	commit := commitBySHA(t, s, sha)
	root, err := s.FindRootTaskByCommitEnv(commit.ID, env.ID)
	if err != nil {
		t.Fatalf("root task of %s@%s: %v", envName, sha, err)
	}
	return root
}

// runOfTask loads the run of a task's current attempt. The task row is
// re-read first: reporting an outcome opens the next attempt, so a stale
// pointer would name the wrong run.
func runOfTask(t *testing.T, s *store.Store, task *store.Task) *store.TestRun {
	t.Helper()
	fresh, err := s.GetTask(task.ID)
	if err != nil {
		t.Fatalf("reload task %d: %v", task.ID, err)
	}
	run, err := s.FindTaskRun(fresh.ID, fresh.Attempts)
	if err != nil {
		t.Fatalf("run of task %d (attempt %d): %v", fresh.ID, fresh.Attempts, err)
	}
	return run
}

// seedDashboardData creates two owners, three environments (one disabled),
// four commits and the task graphs the matrices are read from: cpu-node-1 at
// all three md-code commits, mpi-cluster at the two newest, gpu-a100 never.
func seedDashboardData(t *testing.T, srv *Server) {
	t.Helper()
	s := srv.Store

	// Two owners so the site-wide environment list is exercised.
	for _, u := range []struct{ name, email string }{
		{"alice", "alice@example.com"},
		{"bob", "bob@example.com"},
	} {
		if err := s.CreateUser(&store.User{
			Username: u.name, Email: u.email, PasswordHash: "hash",
		}); err != nil {
			t.Fatalf("create user %s: %v", u.name, err)
		}
	}
	var alice, bob store.User
	s.DB.Where("username = ?", "alice").First(&alice)
	s.DB.Where("username = ?", "bob").First(&bob)

	envs := []*store.TestEnvironment{
		{OwnerID: alice.ID, Name: "cpu-node-1", Host: "h1", Username: "u", PrivateKey: "k", Tags: "cpu", Description: "CPU pool"},
		{OwnerID: alice.ID, Name: "gpu-a100", Host: "h2", Username: "u", PrivateKey: "k", Tags: "gpu"},
		{OwnerID: bob.ID, Name: "mpi-cluster", Host: "h3", Username: "u", PrivateKey: "k", Tags: "mpi"},
	}
	for _, env := range envs {
		if err := s.CreateEnvironment(env); err != nil {
			t.Fatalf("create env %s: %v", env.Name, err)
		}
	}
	// Disable gpu-a100: it must still appear as a matrix row.
	if _, err := s.SetEnvironmentEnabled(envs[1].ID, false); err != nil {
		t.Fatalf("disable env: %v", err)
	}

	commits := []*store.Commit{
		{Repo: "group/md-code", SHA: "1111111", Ref: "main", Author: "alice", Message: "first", PushedAt: time.Now().Add(-3 * time.Hour)},
		{Repo: "group/md-code", SHA: "2222222", Ref: "main", Author: "bob", Message: "second", PushedAt: time.Now().Add(-2 * time.Hour)},
		{Repo: "group/md-code", SHA: "3333333", Ref: "main", Author: "alice", Message: "third", PushedAt: time.Now().Add(-1 * time.Hour)},
		{Repo: "group/other", SHA: "4444444", Ref: "main", Author: "bob", Message: "other repo", PushedAt: time.Now()},
	}
	for _, c := range commits {
		if err := s.CreateCommit(c); err != nil {
			t.Fatalf("create commit %s: %v", c.SHA, err)
		}
	}

	cpu := envByName(t, s, "cpu-node-1")
	mpi := envByName(t, s, "mpi-cluster")
	at := func(sha string) *store.Commit { return commitBySHA(t, s, sha) }

	// Oldest commit: two cases, one of them failing.
	dispatchTestGraph(t, s, cpu, at("1111111"), graphSpec{
		cases: []caseSpec{
			{name: "lj-argon-nve", desc: "Lennard-Jones argon in NVE", res: passed()},
			{name: "water-tip4p-npt", desc: "TIP4P water in NPT", res: failed("drift above threshold")},
		},
	})
	// Second commit: every case passes on cpu; on mpi an upstream failure
	// skipped the only case, so the stage rolls up as skipped.
	dispatchTestGraph(t, s, cpu, at("2222222"), graphSpec{
		cases: []caseSpec{
			{name: "lj-argon-nve", res: passed()},
			{name: "water-tip4p-npt", res: passed()},
		},
	})
	dispatchTestGraph(t, s, mpi, at("2222222"), graphSpec{
		cases: []caseSpec{
			{name: "lj-argon-nve", res: skipped("upstream failure: build failed")},
		},
	})
	// Newest commit: a unit stage plus three cases on cpu, two cases on mpi.
	dispatchTestGraph(t, s, cpu, at("3333333"), graphSpec{
		unit: &stageSpec{desc: "Unit test suite",
			res: &store.AttemptResult{Total: 3, Passed: 2, Failed: 1}},
		cases: []caseSpec{
			{name: "lj-argon-nve", res: passed()},
			{name: "water-tip4p-npt", res: passed()},
			{name: "argon-liquid-nvt", desc: "Argon liquid in NVT", res: failed("energy drift")},
		},
	})
	dispatchTestGraph(t, s, mpi, at("3333333"), graphSpec{
		cases: []caseSpec{
			{name: "lj-argon-nve", res: passed()},
			{name: "water-tip4p-npt", res: passed()},
		},
	})
}

// dashboardTestEnv bundles a test server, mux and an authenticated cookie.
type dashboardTestEnv struct {
	server *Server
	mux    *http.ServeMux
	cookie string
}

// authed issues an authenticated request against the dashboard test mux.
func (e dashboardTestEnv) authed(method, target, body string) *httptest.ResponseRecorder {
	return e.authedWith(e.cookie, method, target, body)
}

// authedWith issues a request as an arbitrary session, for the tests that sign
// in more than one account.
func (e dashboardTestEnv) authedWith(cookie, method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	e.mux.ServeHTTP(rec, req)
	return rec
}

// newDashboardEnv seeds data and logs in as a real user (with a real bcrypt
// password, since loginAndGetCookie performs the full login flow).
func newDashboardEnv(t *testing.T) dashboardTestEnv {
	t.Helper()
	return newDashboardEnvWithObjects(t, storage.NewMemory())
}

// newDashboardEnvWithObjects uses a specific artifact backend, for tests that
// inspect or break the object store.
func newDashboardEnvWithObjects(t *testing.T, objs storage.Store) dashboardTestEnv {
	t.Helper()
	apiServer, s := newTestServerWithObjects(t, objs)

	// Re-seed alice with a usable password for login: seedDashboardData
	// creates users with a dummy hash.
	seedDashboardData(t, apiServer)
	hash, err := authHash("s3cret")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	s.DB.Model(&store.User{}).Where("username = ?", "alice").Update("password_hash", hash)

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "alice", "s3cret")
	return dashboardTestEnv{server: apiServer, mux: mux, cookie: cookie}
}

// authHash is indirection over auth.HashPassword to keep the import list tidy.
func authHash(pw string) (string, error) {
	return auth.HashPassword(pw)
}

// envColumnIndex returns the index of one environment's column in the matrix.
func envColumnIndex(t *testing.T, dash dashboardJSON, envID int64) int {
	t.Helper()
	for i, e := range dash.Environments {
		if e.ID == envID {
			return i
		}
	}
	t.Fatalf("environment %d not among the columns", envID)
	return -1
}

// rowForSHA returns the unique matrix row of one commit SHA.
func rowForSHA(t *testing.T, rows []fullRowJSON, sha string) fullRowJSON {
	t.Helper()
	for i := range rows {
		if rows[i].Commit.SHA == sha {
			return rows[i]
		}
	}
	t.Fatalf("no row for commit %s", sha)
	return fullRowJSON{}
}

// stageOf returns the stage of one kind in a full-matrix row.
func stageOf(t *testing.T, stages []fullStageJSON, kind string) *fullStageJSON {
	t.Helper()
	for i := range stages {
		if stages[i].Kind == kind {
			return &stages[i]
		}
	}
	return nil
}

func TestDashboardRegressionMatrix(t *testing.T) {
	env := newDashboardEnv(t)

	// Unauthenticated access is rejected.
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/dashboard/regression", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth dashboard: expected 401, got %d", rec.Code)
	}

	// Unknown kind is 404.
	rec = env.authed(http.MethodGet, "/api/dashboard/other", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown kind: expected 404, got %d", rec.Code)
	}

	// Wrong method on a valid kind.
	rec = env.authed(http.MethodPost, "/api/dashboard/regression", "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST dashboard: expected 405, got %d", rec.Code)
	}

	// Invalid commits parameter.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression?commits=0", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("commits=0: expected 400, got %d", rec.Code)
	}
	rec = env.authed(http.MethodGet, "/api/dashboard/regression?commits=abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("commits=abc: expected 400, got %d", rec.Code)
	}

	// Matrix layout.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}

	// Environment columns: all site environments, name-ordered.
	if len(dash.Environments) != 3 {
		t.Fatalf("expected 3 environments, got %d", len(dash.Environments))
	}
	if dash.Environments[0].Name != "cpu-node-1" ||
		dash.Environments[1].Name != "gpu-a100" ||
		dash.Environments[2].Name != "mpi-cluster" {
		t.Fatalf("unexpected environment order: %v", dash.Environments)
	}
	// Each column names the account that manages it: the matrix is site-wide,
	// so a column may be someone else's machine and has to say so.
	if dash.Environments[0].Owner != "alice" || dash.Environments[1].Owner != "alice" {
		t.Fatalf("expected alice to own the first two columns, got %v", dash.Environments)
	}
	if dash.Environments[2].Owner != "bob" {
		t.Fatalf("expected bob to own mpi-cluster, got %q", dash.Environments[2].Owner)
	}

	// Rows: no codeRepo configured yet, so all repos are shown as commit
	// rows, newest first.
	if len(dash.Rows) != 4 {
		t.Fatalf("expected 4 commit rows (no repo filter), got %d", len(dash.Rows))
	}
	if dash.Rows[0].Commit.SHA != "4444444" || dash.Rows[1].Commit.SHA != "3333333" {
		t.Fatalf("rows not newest-first: %+v", dash.Rows)
	}
	if dash.Rows[1].Commit.ShortSHA != "3333333" {
		t.Fatalf("unexpected shortSha: %q", dash.Rows[1].Commit.ShortSHA)
	}

	// Cells align with environment columns: cpu-node-1 has graphs at 3 of 4
	// commits, gpu-a100 none, mpi-cluster two.
	cpu, gpu, mpi := envByName(t, env.server.Store, "cpu-node-1"), envByName(t, env.server.Store, "gpu-a100"), envByName(t, env.server.Store, "mpi-cluster")
	envIdx := func(env *store.TestEnvironment) int {
		for i, e := range dash.Environments {
			if e.ID == env.ID {
				return i
			}
		}
		t.Fatalf("environment %s not found", env.Name)
		return -1
	}
	cpuIdx, gpuIdx, mpiIdx := envIdx(cpu), envIdx(gpu), envIdx(mpi)
	for _, row := range dash.Rows {
		if len(row.Cells) != 3 {
			t.Fatalf("expected 3 cells per row, got %d", len(row.Cells))
		}
	}
	// Row 0 = other-repo commit: no graphs anywhere.
	if dash.Rows[0].Cells[cpuIdx] != nil || dash.Rows[0].Cells[gpuIdx] != nil || dash.Rows[0].Cells[mpiIdx] != nil {
		t.Fatalf("expected all-null row for the other-repo commit: %+v", dash.Rows[0].Cells)
	}
	// Row 1 = newest md-code commit: cpu failed 1/3 cases, mpi passed 2/2, gpu none.
	if c := dash.Rows[1].Cells[cpuIdx]; c == nil || c.Status != store.StatusFailed || c.Total != 3 || c.Failed != 1 {
		t.Fatalf("unexpected cpu cell at newest commit: %+v", dash.Rows[1].Cells[cpuIdx])
	}
	if c := dash.Rows[1].Cells[mpiIdx]; c == nil || c.Status != store.StatusPassed || c.Total != 2 {
		t.Fatalf("unexpected mpi cell at newest commit: %+v", dash.Rows[1].Cells[mpiIdx])
	}
	if dash.Rows[1].Cells[gpuIdx] != nil {
		t.Fatalf("expected no gpu graph, got %+v", dash.Rows[1].Cells[gpuIdx])
	}
	// Row 2 = second commit: cpu passed 2/2, mpi skipped (its only case never
	// ran because an upstream stage failed), gpu none.
	if c := dash.Rows[2].Cells[cpuIdx]; c == nil || c.Status != store.StatusPassed || c.Passed != 2 {
		t.Fatalf("unexpected cpu cell at second commit: %+v", dash.Rows[2].Cells[cpuIdx])
	}
	if c := dash.Rows[2].Cells[mpiIdx]; c == nil || c.Status != store.StatusSkipped || c.Skipped != 1 {
		t.Fatalf("unexpected mpi cell at second commit: %+v", dash.Rows[2].Cells[mpiIdx])
	}
	if dash.Rows[2].Cells[gpuIdx] != nil {
		t.Fatal("expected a null gpu cell at second commit")
	}
	// Row 3 = oldest commit: cpu failed 1/2, others none.
	if c := dash.Rows[3].Cells[cpuIdx]; c == nil || c.Status != store.StatusFailed || c.Failed != 1 {
		t.Fatalf("unexpected cpu cell at oldest commit: %+v", dash.Rows[3].Cells[cpuIdx])
	}
	// gpu-a100 has no graph at any commit.
	for _, row := range dash.Rows {
		if row.Cells[gpuIdx] != nil {
			t.Fatalf("expected all-null column for gpu-a100, got %+v", row.Cells[gpuIdx])
		}
	}

	// A regression cell is the virtual stage container: it has no run of its
	// own, so it links to the task detail instead of a run page.
	container := taskOf(t, env.server.Store, "cpu-node-1", "1111111", store.TaskKindRegressionStage)
	old := dash.Rows[3].Cells[cpuIdx]
	if old.TaskID != container.ID || old.RunID != 0 {
		t.Fatalf("regression cell should link the container task %d: %+v", container.ID, old)
	}
	// The graph's trigger travels with the cell (webhook = 0 here).
	if old.Trigger != store.TaskTriggerWebhook {
		t.Fatalf("cell trigger = %d, want webhook", old.Trigger)
	}
}

func TestDashboardRepoFilter(t *testing.T) {
	env := newDashboardEnv(t)

	// Configure the code repo: only pushes to that repository remain.
	cfg, err := env.server.Store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
	if err := env.server.Store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	rec := env.authed(http.MethodGet, "/api/dashboard/regression", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d", rec.Code)
	}
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dash.RepoFilter != "group/md-code" {
		t.Fatalf("expected repoFilter group/md-code, got %q", dash.RepoFilter)
	}
	if len(dash.Rows) != 3 {
		t.Fatalf("expected 3 md-code commit rows, got %d", len(dash.Rows))
	}
	for _, row := range dash.Rows {
		if row.Commit.SHA == "4444444" {
			t.Fatal("other-repo commit should be filtered out")
		}
	}

	// The commits parameter caps rows.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression?commits=1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode capped: %v", err)
	}
	if len(dash.Rows) != 1 || dash.Rows[0].Commit.SHA != "3333333" {
		t.Fatalf("expected single newest commit row, got %+v", dash.Rows)
	}
}

// TestDashboardRepoFilterBarePath is the same matrix with the code repository
// configured as a bare "group/project" path instead of a URL. The webhook
// stores a push's path_with_namespace as-is and matches a bare config by
// suffix, so the filter has to keep the whole path too: dropping the group
// would leave "md-code" as the filter, which matches no commit row and
// blanks the matrix while the pushes are still dispatched.
func TestDashboardRepoFilterBarePath(t *testing.T) {
	env := newDashboardEnv(t)

	cfg, err := env.server.Store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.CodeRepo = "group/md-code"
	if err := env.server.Store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	rec := env.authed(http.MethodGet, "/api/dashboard/regression", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d", rec.Code)
	}
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dash.RepoFilter != "group/md-code" {
		t.Fatalf("expected repoFilter group/md-code, got %q", dash.RepoFilter)
	}
	if len(dash.Rows) != 3 {
		t.Fatalf("expected 3 md-code commit rows for the bare path, got %d", len(dash.Rows))
	}
	for _, row := range dash.Rows {
		if row.Commit.SHA == "4444444" {
			t.Fatal("other-repo commit should be filtered out")
		}
	}
}

// TestReportTestRunAndDetail covers POST /api/test-runs: a report is addressed
// by the task it is about, opens the next attempt of that task when the
// current one already ended, and the run detail exposes the attempt list, the
// task identity and the owning graph.
func TestReportTestRunAndDetail(t *testing.T) {
	env := newDashboardEnv(t)

	// A report without a task cannot be addressed.
	rec := env.authed(http.MethodPost, "/api/test-runs", `{"status":"passed"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing taskId: expected 400, got %d", rec.Code)
	}

	// Unknown task id: 404.
	rec = env.authed(http.MethodPost, "/api/test-runs", `{"taskId":999999,"status":"passed"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: expected 404, got %d, body %s", rec.Code, rec.Body.String())
	}

	// A virtual container records no run: reporting against it is refused.
	root := rootTaskOf(t, env.server.Store, "cpu-node-1", "1111111")
	rec = env.authed(http.MethodPost, "/api/test-runs", fmt.Sprintf(`{"taskId":%d,"status":"passed"}`, root.ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("virtual task: expected 400, got %d, body %s", rec.Code, rec.Body.String())
	}
	container := taskOf(t, env.server.Store, "cpu-node-1", "1111111", store.TaskKindRegressionStage)
	rec = env.authed(http.MethodPost, "/api/test-runs", fmt.Sprintf(`{"taskId":%d,"status":"passed"}`, container.ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("virtual container: expected 400, got %d, body %s", rec.Code, rec.Body.String())
	}

	// An unknown status is rejected before anything is written.
	caseTask := taskOf(t, env.server.Store, "cpu-node-1", "1111111", regressionCaseKey("water-tip4p-npt"))
	rec = env.authed(http.MethodPost, "/api/test-runs", fmt.Sprintf(`{"taskId":%d,"status":"pending"}`, caseTask.ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: expected 400, got %d", rec.Code)
	}
	if after := runOfTask(t, env.server.Store, caseTask); after.Attempt != 1 {
		t.Fatalf("a rejected report must not open an attempt: %+v", after)
	}

	// The owner reports the failed case again: a second attempt of the same
	// task, with its own timestamps, never overwriting the first.
	body := fmt.Sprintf(`{
		"taskId": %d,
		"status": "passed",
		"summary": "max rel err",
		"total": 1,
		"passed": 1,
		"startedAt": "2026-09-08T03:00:00Z",
		"finishedAt": "2026-09-08T03:04:00Z"
	}`, caseTask.ID)
	rec = env.authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	var run runJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Attempt != 2 || run.Status != store.StatusPassed || run.TaskID != caseTask.ID {
		t.Fatalf("unexpected run: %+v", run)
	}
	if run.StartedAt != "2026-09-08T03:00:00Z" || run.FinishedAt != "2026-09-08T03:04:00Z" {
		t.Fatalf("reported timestamps not honoured: %+v", run)
	}
	if run.EnvironmentID == 0 || run.CommitID == 0 {
		t.Fatalf("run should carry the task's environment and commit: %+v", run)
	}

	// Detail: the task identity, the owning graph and every attempt, newest
	// first.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", run.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.TaskName == nil || *detail.TaskName != "regression: water-tip4p-npt" {
		t.Fatalf("taskName wrong: %v", detail.TaskName)
	}
	if detail.TaskKind == nil || *detail.TaskKind != store.TaskKindRegressionCase {
		t.Fatalf("taskKind wrong: %v", detail.TaskKind)
	}
	if detail.TaskKey == nil || *detail.TaskKey != regressionCaseKey("water-tip4p-npt") {
		t.Fatalf("taskKey wrong: %v", detail.TaskKey)
	}
	if detail.RootTaskID != root.ID {
		t.Fatalf("rootTaskId = %d, want %d", detail.RootTaskID, root.ID)
	}
	if detail.EnvironmentName == nil || *detail.EnvironmentName != "cpu-node-1" {
		t.Fatalf("environmentName wrong: %v", detail.EnvironmentName)
	}
	if detail.CommitShortSHA == nil || *detail.CommitShortSHA != "1111111" {
		t.Fatalf("commitShortSha wrong: %v", detail.CommitShortSHA)
	}
	if detail.CommitAuthor == nil || *detail.CommitAuthor != "alice" {
		t.Fatalf("commitAuthor wrong: %v", detail.CommitAuthor)
	}
	if len(detail.Attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %+v", detail.Attempts)
	}
	if detail.Attempts[0].Attempt != 2 || detail.Attempts[0].Status != store.StatusPassed {
		t.Fatalf("attempts not newest-first: %+v", detail.Attempts)
	}
	if detail.Attempts[1].Attempt != 1 || detail.Attempts[1].Status != store.StatusFailed {
		t.Fatalf("the previous attempt must stay readable: %+v", detail.Attempts)
	}

	// The dashboard now shows the case's stage as passed: both cases of the
	// oldest commit pass, so the container rolls up passed with no failure.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression", "")
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	// Rows are newest-first: 1111111 is the oldest md-code commit, last of
	// the 4 rows (no repo filter).
	if len(dash.Rows) != 4 || dash.Rows[3].Commit.SHA != "1111111" {
		t.Fatalf("expected the oldest commit row, got %+v", dash.Rows)
	}
	cpu := envByName(t, env.server.Store, "cpu-node-1")
	cell := dash.Rows[3].Cells[envColumnIndex(t, dash, cpu.ID)]
	if cell == nil || cell.Status != store.StatusPassed || cell.Failed != 0 || cell.Passed != 2 {
		t.Fatalf("expected the replaced cell to be passed, got %+v", cell)
	}

	// Unknown run id: 404.
	rec = env.authed(http.MethodGet, "/api/test-runs/9999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run: expected 404, got %d", rec.Code)
	}

	// Wrong method on the collection endpoint.
	rec = env.authed(http.MethodGet, "/api/test-runs", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET test-runs: expected 405, got %d", rec.Code)
	}
}

// TestReportRunOwnership covers the permission on POST /api/test-runs: the
// environment's owner (or an administrator) may record an attempt's outcome,
// anybody else may not — the report names the task and the task names the
// environment.
func TestReportRunOwnership(t *testing.T) {
	env := newDashboardEnv(t)
	s := env.server.Store

	// carol owns no environment; root is an administrator.
	seedUser(t, s, "carol", "carol@example.com", "carolpw")
	seedUserWithRole(t, s, "root", "root@example.com", "rootpw", store.RoleAdmin)
	carolCookie := loginAndGetCookie(t, env.mux, "carol", "carolpw")
	rootCookie := loginAndGetCookie(t, env.mux, "root", "rootpw")

	unit := taskOf(t, s, "cpu-node-1", "3333333", store.TaskKindUnit)
	before := runOfTask(t, s, unit)

	// A stranger's report is refused and changes nothing.
	rec := env.authedWith(carolCookie, http.MethodPost, "/api/test-runs",
		fmt.Sprintf(`{"taskId":%d,"status":"passed"}`, unit.ID))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner report: expected 403, got %d, body %s", rec.Code, rec.Body.String())
	}
	after := runOfTask(t, s, unit)
	if after.Status != before.Status || after.Summary != before.Summary || after.Attempt != before.Attempt {
		t.Fatalf("a refused report must not touch the run: %+v (was %+v)", after, before)
	}
	if node := taskOf(t, s, "cpu-node-1", "3333333", store.TaskKindUnit); node.Status != before.Status {
		t.Fatalf("a refused report must not touch the task: %+v", node)
	}

	// The owner may report it.
	rec = env.authed(http.MethodPost, "/api/test-runs",
		fmt.Sprintf(`{"taskId":%d,"status":"passed"}`, unit.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("owner report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	after = runOfTask(t, s, unit)
	if after.Attempt != before.Attempt+1 || after.Status != store.StatusPassed {
		t.Fatalf("owner report did not open an attempt: %+v", after)
	}

	// An administrator may report it too (nobody else's environment is out of
	// an administrator's reach).
	rec = env.authedWith(rootCookie, http.MethodPost, "/api/test-runs",
		fmt.Sprintf(`{"taskId":%d,"status":"failed"}`, unit.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	if after = runOfTask(t, s, unit); after.Attempt != before.Attempt+2 || after.Status != store.StatusFailed {
		t.Fatalf("admin report wrong: %+v", after)
	}
}

// TestDashboardFullMatrix checks GET /api/dashboard/full: one row per commit,
// per environment the build/unit/regression stages in display order, and the
// task graph link.
func TestDashboardFullMatrix(t *testing.T) {
	env := newDashboardEnv(t)

	// Filter the matrix to the md-code repo (the seed also created a
	// group/other commit with no graphs).
	cfg, err := env.server.Store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
	if err := env.server.Store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	rec := env.authed(http.MethodGet, "/api/dashboard/full", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("full dashboard: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var dash fullDashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if len(dash.Rows) != 3 {
		t.Fatalf("expected 3 md-code commit rows, got %d", len(dash.Rows))
	}
	// The full matrix labels its columns with the owning account too.
	owners := map[string]string{}
	for _, e := range dash.Environments {
		owners[e.Name] = e.Owner
	}
	if owners["cpu-node-1"] != "alice" || owners["mpi-cluster"] != "bob" {
		t.Fatalf("unexpected column owners: %+v", owners)
	}

	cpu := envByName(t, env.server.Store, "cpu-node-1")
	gpu := envByName(t, env.server.Store, "gpu-a100")
	mpi := envByName(t, env.server.Store, "mpi-cluster")

	// Rows are newest-first; take the newest commit row.
	row := rowForSHA(t, dash.Rows, "3333333")

	// cpu-node-1 defines a unit and a regression stage: two entries in
	// display order, the unit one carrying its run.
	cpuStages := row.Stages[cpu.ID]
	if len(cpuStages) != 2 {
		t.Fatalf("cpu stages: expected 2 (unit+regression), got %+v", cpuStages)
	}
	if cpuStages[0].Kind != store.RunKindUnit || cpuStages[0].RunID == 0 || cpuStages[0].Status != store.StatusFailed {
		t.Fatalf("cpu unit stage wrong: %+v", cpuStages[0])
	}
	if cpuStages[0].TaskID != taskOf(t, env.server.Store, "cpu-node-1", "3333333", store.TaskKindUnit).ID {
		t.Fatalf("cpu unit stage should link its task: %+v", cpuStages[0])
	}
	// The regression column is the virtual container: no run of its own, and
	// its summary says how many cases the stage has.
	if cpuStages[1].Kind != store.RunKindRegression || cpuStages[1].RunID != 0 ||
		cpuStages[1].TaskID == 0 || cpuStages[1].Status != store.StatusFailed {
		t.Fatalf("cpu regression stage wrong: %+v", cpuStages[1])
	}
	if cpuStages[1].Summary != "3 cases" {
		t.Fatalf("cpu regression summary = %q, want \"3 cases\"", cpuStages[1].Summary)
	}
	if row.TaskIDs[cpu.ID] != rootTaskOf(t, env.server.Store, "cpu-node-1", "3333333").ID {
		t.Fatalf("cpu graph link wrong: %+v", row.TaskIDs)
	}

	// mpi-cluster defines only the regression stage.
	mpiStages := row.Stages[mpi.ID]
	if len(mpiStages) != 1 || mpiStages[0].Kind != store.RunKindRegression ||
		mpiStages[0].RunID != 0 || mpiStages[0].TaskID == 0 || mpiStages[0].Status != store.StatusPassed {
		t.Fatalf("mpi stages wrong: %+v", mpiStages)
	}
	if mpiStages[0].Summary != "2 cases" {
		t.Fatalf("mpi regression summary = %q, want \"2 cases\"", mpiStages[0].Summary)
	}

	// gpu-a100 has no graph at all: no stage entries for it.
	if stages := row.Stages[gpu.ID]; len(stages) != 0 {
		t.Fatalf("gpu stages should be empty, got %+v", stages)
	}

	// The second commit's mpi graph reports skipped, and an older commit with
	// only the regression stage shows exactly one stage.
	second := rowForSHA(t, dash.Rows, "2222222")
	if st := stageOf(t, second.Stages[mpi.ID], store.RunKindRegression); st == nil || st.Status != store.StatusSkipped {
		t.Fatalf("mpi stage at the second commit should be skipped: %+v", second.Stages[mpi.ID])
	}
	oldRow := rowForSHA(t, dash.Rows, "1111111")
	if stages := oldRow.Stages[cpu.ID]; len(stages) != 1 || stages[0].Kind != store.RunKindRegression {
		t.Fatalf("oldest cpu stages wrong: %+v", stages)
	}

	// commits=1 limits the rows; an invalid value is rejected.
	rec = env.authed(http.MethodGet, "/api/dashboard/full?commits=1", "")
	dash = fullDashboardJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if len(dash.Rows) != 1 {
		t.Fatalf("commits=1: expected 1 row, got %d", len(dash.Rows))
	}
	rec = env.authed(http.MethodGet, "/api/dashboard/full?commits=nope", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("commits=nope: expected 400, got %d", rec.Code)
	}
}

// TestDashboardFullLiveOverlay checks that a graph whose stage is still queued
// or running surfaces as a live stage (taskId set, the stage's own state), per
// environment, and that the task detail view links each node to the run of its
// current attempt.
func TestDashboardFullLiveOverlay(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	user := seedUser(t, s, "fulluser", "fu@example.com", "pw")
	envA := seedOwnedDispatchEnv(t, s, user, "cpu-full-a", "cpu", true)
	envB := seedOwnedDispatchEnv(t, s, user, "cpu-full-b", "cpu", true)
	commit := &store.Commit{Repo: "group/code", SHA: "f111ca7", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}

	// Env A: a queued graph whose clone finished and whose build the scheduler
	// has claimed — it must read as running, with its own attempt's run.
	rootA, aNodes := dispatchTestGraph(t, s, envA, commit, graphSpec{
		clone: &stageSpec{res: passed()},
		build: &stageSpec{},
		unit:  &stageSpec{},
		cases: []caseSpec{{name: "smoke"}},
	})
	claimed, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil || claimed.ID != nodeOf(t, aNodes, store.TaskKindBuild).ID {
		t.Fatalf("expected the build node to be claimed, got %+v", claimed)
	}

	// Env B: the same commit, dispatched later, still fully queued.
	rootB, bNodes := dispatchTestGraph(t, s, envB, commit, graphSpec{
		cases: []caseSpec{{name: "smoke"}},
	})

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "fulluser", "pw")
	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	rec := authed(http.MethodGet, "/api/dashboard/full", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("full dashboard: expected 200, got %d", rec.Code)
	}
	var dash fullDashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if len(dash.Rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(dash.Rows))
	}
	row := dash.Rows[0]

	// Both graphs are linked via taskIds.
	if row.TaskIDs[envA.ID] != rootA.ID || row.TaskIDs[envB.ID] != rootB.ID {
		t.Fatalf("taskIds wrong: %+v", row.TaskIDs)
	}

	// Env A: the claimed build stage reads running; the unit stage is queued
	// behind it; the regression container is pending with its case count.
	aBuild := stageOf(t, row.Stages[envA.ID], store.RunKindBuild)
	if aBuild == nil {
		t.Fatalf("env A build stage missing: %+v", row.Stages[envA.ID])
	}
	if aBuild.TaskID != nodeOf(t, aNodes, store.TaskKindBuild).ID || aBuild.Status != store.StatusRunning {
		t.Fatalf("env A running build stage wrong: %+v", aBuild)
	}
	if aBuild.RunID != runOfTask(t, s, nodeOf(t, aNodes, store.TaskKindBuild)).ID {
		t.Fatalf("a live stage links the attempt's run: %+v", aBuild)
	}
	aReg := stageOf(t, row.Stages[envA.ID], store.RunKindRegression)
	if aReg == nil || aReg.RunID != 0 || aReg.TaskID != nodeOf(t, aNodes, store.TaskKindRegressionStage).ID ||
		aReg.Status != store.StatusPending || aReg.Summary != "1 cases" {
		t.Fatalf("env A regression stage wrong: %+v", aReg)
	}

	// Env B: only the regression slot exists (its graph defines no build
	// stage), and it is queued — per-environment isolation.
	for _, st := range row.Stages[envB.ID] {
		if st.Kind == store.RunKindBuild {
			t.Fatalf("env B should have no build stage: %+v", row.Stages[envB.ID])
		}
	}
	bReg := stageOf(t, row.Stages[envB.ID], store.RunKindRegression)
	if bReg == nil || bReg.TaskID != nodeOf(t, bNodes, store.TaskKindRegressionStage).ID || bReg.Status != store.StatusPending {
		t.Fatalf("env B regression stage wrong: %+v", bReg)
	}

	// Task detail: every real node links the run of its current attempt, and
	// the virtual container links none.
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d", rootA.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("task detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail taskDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	byName := map[string]subTaskJSON{}
	for _, st := range detail.SubTasks {
		byName[st.Name] = st
	}
	buildSub := byName["build"]
	if buildSub.RunID != runOfTask(t, s, nodeOf(t, aNodes, store.TaskKindBuild)).ID {
		t.Fatalf("build sub-task should link its attempt's run: %+v", buildSub)
	}
	if reg := byName["regression"]; reg.RunID != 0 {
		t.Fatalf("the virtual container has no run to link: %+v", reg)
	}

	// The owner reports the build: the dashboard stage follows the attempt.
	rec = authed(http.MethodPost, "/api/test-runs",
		fmt.Sprintf(`{"taskId":%d,"status":"passed","summary":"ok"}`, nodeOf(t, aNodes, store.TaskKindBuild).ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("report build run: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	rec = authed(http.MethodGet, "/api/dashboard/full", "")
	dash = fullDashboardJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if aBuild = stageOf(t, dash.Rows[0].Stages[envA.ID], store.RunKindBuild); aBuild == nil || aBuild.Status != store.StatusPassed {
		t.Fatalf("build stage should read passed: %+v", aBuild)
	}
}

// TestDashboardSupersededRows checks the manual re-dispatch presentation:
// a second commit row with the same (repo, sha) keeps both rows in the
// matrix, and every row but the newest of the group is flagged superseded
// (both the single-kind and the full matrix).
func TestDashboardSupersededRows(t *testing.T) {
	env := newDashboardEnv(t)

	cfg, err := env.server.Store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
	if err := env.server.Store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// A manual re-dispatch of the newest seeded commit: a fresh row with
	// the same (repo, sha), pushed later.
	if err := env.server.Store.CreateCommit(&store.Commit{
		Repo: "group/md-code", SHA: "3333333", Ref: "master", Author: "alice",
		Message: "manual test", PushedAt: time.Now().Add(10 * time.Minute),
	}); err != nil {
		t.Fatalf("create commit: %v", err)
	}

	check := func(t *testing.T, target string) {
		t.Helper()
		rec := env.authed(http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", target, rec.Code)
		}
		var dash struct {
			Rows []struct {
				Commit struct {
					SHA        string `json:"sha"`
					Superseded bool   `json:"superseded"`
				} `json:"commit"`
			} `json:"rows"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
			t.Fatal(err)
		}
		var newest, older bool
		for _, row := range dash.Rows {
			if row.Commit.SHA != "3333333" {
				continue
			}
			if row.Commit.Superseded {
				older = true
			} else {
				newest = true
			}
		}
		if !newest || !older {
			t.Fatalf("%s: expected one live and one superseded 3333333 row, got %+v", target, dash.Rows)
		}
	}
	check(t, "/api/dashboard/full")
	check(t, "/api/dashboard/regression")
}

// TestDashboardLiveFlagOnAnOlderRow covers the fork policy's presentation: an
// older row of the same revision keeps its own task graph, and while that graph
// can still change the row must not be dimmed like dead history. superseded
// says "a newer row of this revision exists"; live says "this one is still
// moving", and the matrix dims a row only when the second is false.
func TestDashboardLiveFlagOnAnOlderRow(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	env := seedDispatchEnv(t, s, "cpu-live", "cpu", true)
	setOverlapPolicy(t, s, store.CommitOverlapFork)
	seedUser(t, s, "dashboard-live", "dashboard-live@example.com", "s3cret")
	cookie := loginAndGetCookie(t, mux, "dashboard-live", "s3cret")

	const sha = "9999999999999999999999999999999999999999"
	// Two events for one SHA: under the fork policy each gets its own row and
	// its own graph, and both graphs stay pending (nothing claims them here).
	var rows []int64
	for _, body := range []string{
		pushBody("group/code", sha),
		mrBody("group/code", sha, "fix: energy drift"),
	} {
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, int64(res["commitId"].(float64)))
	}
	older, newer := rows[0], rows[1]
	if older == newer {
		t.Fatalf("the fork policy recorded one row for both events: %d", older)
	}

	rowState := func(t *testing.T) map[int64]commitJSON {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/dashboard/full", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard: %d %s", rec.Code, rec.Body.String())
		}
		var dash fullDashboardJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
			t.Fatal(err)
		}
		out := map[int64]commitJSON{}
		for _, row := range dash.Rows {
			out[row.Commit.ID] = row.Commit
		}
		return out
	}

	state := rowState(t)
	if got := state[older]; !got.Superseded || !got.Live {
		t.Fatalf("the older row while its graph is pending: want superseded+live, got %+v", got)
	}
	if got := state[newer]; got.Superseded || !got.Live {
		t.Fatalf("the newer row: want live and not superseded, got %+v", got)
	}

	// Once the older row's graph is finished — here, cancelled by the same
	// policy the fork_cancel mode applies — it stops being live and is dimmed.
	root, err := s.FindRootTaskByCommitEnv(older, env.ID)
	if err != nil {
		t.Fatalf("the older graph: %v", err)
	}
	if _, err := s.CancelGraph(root.ID, store.CancelledSummary); err != nil {
		t.Fatal(err)
	}
	if got := rowState(t)[older]; !got.Superseded || got.Live {
		t.Fatalf("the older row after its graph ended: want superseded and not live, got %+v", got)
	}
}

// TestTaskDetailRetiredNodes checks the re-dispatch presentation on the task
// graph: a node the new graph no longer defines stays readable as history
// (retiredTasks) and drops out of the active node list and of its parent's
// rollup.
func TestTaskDetailRetiredNodes(t *testing.T) {
	env := newDashboardEnv(t)
	s := env.server.Store
	cpu := envByName(t, s, "cpu-node-1")
	commit := &store.Commit{Repo: "group/md-code", SHA: "d1d1d1d", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The first dispatch defines two cases; the second one drops the second.
	root, _ := dispatchTestGraph(t, s, cpu, commit, graphSpec{
		cases: []caseSpec{
			{name: "lj-argon-nve", res: passed()},
			{name: "water-tip4p-npt", res: passed()},
		},
	})
	dispatchTestGraph(t, s, cpu, commit, graphSpec{
		cases: []caseSpec{{name: "lj-argon-nve", res: passed()}},
	})

	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d", root.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("task detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail taskDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	// clone + regression container + its one remaining case: the dropped case
	// is not part of the graph any more.
	if len(detail.SubTasks) != 3 {
		t.Fatalf("active nodes wrong: %+v", detail.SubTasks)
	}
	for _, st := range detail.SubTasks {
		if st.Name == "regression: water-tip4p-npt" {
			t.Fatalf("the dropped case must not stay active: %+v", st)
		}
	}
	if len(detail.RetiredTasks) != 1 || detail.RetiredTasks[0].Name != "regression: water-tip4p-npt" {
		t.Fatalf("retired nodes wrong: %+v", detail.RetiredTasks)
	}
	if !detail.RetiredTasks[0].Retired || detail.RetiredTasks[0].RunID == 0 {
		t.Fatalf("a retired node keeps its history, run included: %+v", detail.RetiredTasks[0])
	}

	// The container counts only the case the current graph defines.
	var container *subTaskJSON
	for i := range detail.SubTasks {
		if detail.SubTasks[i].Kind == store.TaskKindRegressionStage {
			container = &detail.SubTasks[i]
		}
	}
	if container == nil || container.Total != 1 || container.Passed != 1 {
		t.Fatalf("container should count the active case only: %+v", container)
	}

	// ... and so does the matrix cell.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression", "")
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	var cell *runCellJSON
	for _, row := range dash.Rows {
		if row.Commit.SHA == "d1d1d1d" {
			cell = row.Cells[envColumnIndex(t, dash, cpu.ID)]
		}
	}
	if cell == nil || cell.Total != 1 || cell.Passed != 1 || cell.Status != store.StatusPassed {
		t.Fatalf("cell should count the active case only: %+v", cell)
	}
}

// TestRunDetailArtifactFields checks the run detail's artifact references,
// taskId/skipped fields, and GET /api/test-artifacts/{id} content delivery
// (the browser-side results parsing fetch path).
func TestRunDetailArtifactFields(t *testing.T) {
	env := newDashboardEnv(t)
	s := env.server.Store

	commit := &store.Commit{Repo: "group/md-code", SHA: "aaaaaaa", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("commit: %v", err)
	}
	root, byKey := dispatchTestGraph(t, s, envByName(t, s, "cpu-node-1"), commit, graphSpec{
		unit: &stageSpec{desc: "Unit test suite", res: &store.AttemptResult{
			Total: 12, Passed: 9, Failed: 2, Skipped: 1,
			Artifacts: []store.ArtifactInput{
				{Kind: store.ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
			},
		}},
	})
	unit := nodeOf(t, byKey, store.TaskKindUnit)
	run := runOfTask(t, s, unit)

	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", run.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.TaskID != unit.ID || detail.RootTaskID != root.ID {
		t.Fatalf("task identity wrong: taskId=%d rootTaskId=%d (want %d/%d)", detail.TaskID, detail.RootTaskID, unit.ID, root.ID)
	}
	if detail.Skipped != 1 || detail.Total != 12 || detail.Passed != 9 || detail.Failed != 2 {
		t.Fatalf("counts wrong: %+v", detail)
	}
	if detail.CommitRepo == nil || *detail.CommitRepo != "group/md-code" {
		t.Fatalf("commitRepo wrong: %v", detail.CommitRepo)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Kind != "results" ||
		detail.Artifacts[0].Name != "build/test_detail.xml" || detail.Artifacts[0].Size != len("<testsuites/>") {
		t.Fatalf("artifact ref wrong: %+v", detail.Artifacts)
	}

	// The artifact content endpoint delivers the raw file.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d", detail.Artifacts[0].ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("artifact: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var artifact struct {
		RunID   int64  `json:"runId"`
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &artifact); err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if artifact.RunID != run.ID || artifact.Kind != "results" || artifact.Content != "<testsuites/>" {
		t.Fatalf("artifact content wrong: %+v", artifact)
	}

	// Unknown artifact id: 404; bad id: 400.
	rec = env.authed(http.MethodGet, "/api/test-artifacts/9999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown artifact: expected 404, got %d", rec.Code)
	}
	rec = env.authed(http.MethodGet, "/api/test-artifacts/abc", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad artifact id: expected 400, got %d", rec.Code)
	}
}

// TestRunDetailRootTaskID checks that the run detail resolves the producing
// stage task's root (the graph-page breadcrumb link), and degrades to no
// breadcrumb when the task row is gone.
func TestRunDetailRootTaskID(t *testing.T) {
	env := newDashboardEnv(t)
	s := env.server.Store

	commit := &store.Commit{Repo: "group/md-code", SHA: "ddddddd", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("commit: %v", err)
	}
	root, byKey := dispatchTestGraph(t, s, envByName(t, s, "cpu-node-1"), commit, graphSpec{
		unit: &stageSpec{res: &store.AttemptResult{Total: 3, Passed: 3}},
	})
	unit := nodeOf(t, byKey, store.TaskKindUnit)
	run := runOfTask(t, s, unit)

	fetch := func() runDetailJSON {
		t.Helper()
		rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", run.ID), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("detail: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var detail runDetailJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
			t.Fatalf("decode detail: %v", err)
		}
		return detail
	}

	detail := fetch()
	if detail.TaskID != unit.ID || detail.RootTaskID != root.ID {
		t.Fatalf("taskId/rootTaskId = %d/%d, want %d/%d", detail.TaskID, detail.RootTaskID, unit.ID, root.ID)
	}
	if detail.TaskName == nil || *detail.TaskName != "unit tests" {
		t.Fatalf("taskName wrong: %v", detail.TaskName)
	}

	// The task row deleted after the run was recorded: the page stays
	// viewable, it just has no task to link any more.
	if err := s.DB.Where("id = ?", unit.ID).Delete(&store.Task{}).Error; err != nil {
		t.Fatalf("delete task: %v", err)
	}
	detail = fetch()
	if detail.RootTaskID != 0 || detail.TaskName != nil || detail.TaskKind != nil {
		t.Fatalf("a missing task should leave the breadcrumb empty: %+v", detail)
	}
}

// TestReportRunAggregateCounts checks POST /api/test-runs with aggregate
// counts and no cases (the external-report form of the unit path), including
// the derived statuses.
func TestReportRunAggregateCounts(t *testing.T) {
	env := newDashboardEnv(t)

	unit := taskOf(t, env.server.Store, "cpu-node-1", "3333333", store.TaskKindUnit)
	body := fmt.Sprintf(`{
		"taskId": %d,
		"total": 10,
		"passed": 8,
		"failed": 2,
		"skipped": 0,
		"status": "failed",
		"summary": "8 of 10 passed"
	}`, unit.ID)
	rec := env.authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	var run runJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != store.StatusFailed || run.Total != 10 || run.Passed != 8 || run.Failed != 2 {
		t.Fatalf("unexpected run: %+v", run)
	}

	// No status at all: it is derived from the counts. A run whose cases were
	// all skipped reads skipped, not passed.
	caseTask := taskOf(t, env.server.Store, "cpu-node-1", "3333333", regressionCaseKey("argon-liquid-nvt"))
	rec = env.authed(http.MethodPost, "/api/test-runs",
		fmt.Sprintf(`{"taskId":%d,"total":2,"skipped":2}`, caseTask.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("derived report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	run = runJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != store.StatusSkipped || run.Total != 2 || run.Skipped != 2 {
		t.Fatalf("an all-skipped report should read skipped: %+v", run)
	}
}

// TestRunDescriptionAPI checks that md-builder.yaml's descriptions reach the
// run detail: they are stored on the task node at dispatch and reported as
// taskDescription (the run detail page renders them).
func TestRunDescriptionAPI(t *testing.T) {
	env := newDashboardEnv(t)

	unit := taskOf(t, env.server.Store, "cpu-node-1", "3333333", store.TaskKindUnit)
	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", runOfTask(t, env.server.Store, unit).ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.TaskDescription == nil || *detail.TaskDescription != "Unit test suite" {
		t.Fatalf("unit description = %v, want %q", detail.TaskDescription, "Unit test suite")
	}

	// A stage the yaml gives no label for reports none: the field is optional.
	clone := taskOf(t, env.server.Store, "cpu-node-1", "3333333", store.TaskKindClone)
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", runOfTask(t, env.server.Store, clone).ID), "")
	detail = runDetailJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode clone detail: %v", err)
	}
	if detail.TaskDescription != nil {
		t.Fatalf("clone has no description, got %q", *detail.TaskDescription)
	}
}

// TestArtifactDownloadAndRunZip checks the download endpoints: per-artifact
// raw bytes with a Content-Disposition filename, the per-run zip of a run's
// own files, and the per-task zip of a whole subtree (a container downloads as
// one bundle of its children).
func TestArtifactDownloadAndRunZip(t *testing.T) {
	env := newDashboardEnv(t)
	s := env.server.Store

	commit := &store.Commit{Repo: "group/md-code", SHA: "bbbbbbb", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ninjaLog := "# ninja log\n5\t10\t0\tcmake\n"
	_, byKey := dispatchTestGraph(t, s, envByName(t, s, "cpu-node-1"), commit, graphSpec{
		build: &stageSpec{res: &store.AttemptResult{
			Status: store.StatusPassed,
			Artifacts: []store.ArtifactInput{
				{Kind: store.ArtifactKindFile, Name: "build/.ninja_log", Content: ninjaLog},
				{Kind: store.ArtifactKindFile, Name: "build/compile_commands.json", Content: "[]\n"},
			},
		}},
		cases: []caseSpec{{name: "heat", res: &store.AttemptResult{
			Status: store.StatusPassed,
			Artifacts: []store.ArtifactInput{
				{Kind: store.ArtifactKindResults, Name: "out.xml", Content: "<testsuites/>"},
			},
		}}},
	})
	buildRun := runOfTask(t, s, nodeOf(t, byKey, store.TaskKindBuild))

	// Run detail lists the build's two file artifacts.
	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", buildRun.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail runDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Artifacts) != 2 || detail.Artifacts[0].Kind != store.ArtifactKindFile {
		t.Fatalf("artifact refs wrong: %+v", detail.Artifacts)
	}

	// Per-artifact download: raw bytes, attachment filename from the path
	// basename.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d/download", detail.Artifacts[0].ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("download: %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename=".ninja_log"` {
		t.Errorf("Content-Disposition = %q", got)
	}
	if rec.Body.String() != ninjaLog {
		t.Errorf("download body wrong: %q", rec.Body.String())
	}

	readZip := func(target string, wantName string) map[string]string {
		t.Helper()
		rec := env.authed(http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("zip %s: %d %s", target, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Content-Type"); got != "application/zip" {
			t.Errorf("zip Content-Type = %q", got)
		}
		if got := rec.Header().Get("Content-Disposition"); got != fmt.Sprintf(`attachment; filename="%s"`, wantName) {
			t.Errorf("zip Content-Disposition = %q", got)
		}
		zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
		if err != nil {
			t.Fatalf("zip reader: %v", err)
		}
		out := map[string]string{}
		for _, f := range zr.File {
			rc, err := f.Open()
			if err != nil {
				t.Fatalf("zip entry %s: %v", f.Name, err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			out[f.Name] = string(b)
		}
		return out
	}

	// Zip of the build run: its own two files at the archive root.
	got := readZip(fmt.Sprintf("/api/test-runs/%d/artifacts/zip", buildRun.ID),
		fmt.Sprintf("run-%d-artifacts.zip", buildRun.ID))
	if got[".ninja_log"] != ninjaLog {
		t.Errorf("zip .ninja_log wrong: %q (has %v)", got[".ninja_log"], keysOf(got))
	}
	if got["compile_commands.json"] != "[]\n" {
		t.Errorf("zip compile_commands.json wrong: %q (has %v)", got["compile_commands.json"], keysOf(got))
	}

	// Zip of the regression container: each case nests under a directory named
	// after its node key, so the stage downloads as one bundle.
	container := nodeOf(t, byKey, store.TaskKindRegressionStage)
	got = readZip(fmt.Sprintf("/api/tasks/%d/artifacts/zip", container.ID),
		fmt.Sprintf("task-%d-artifacts.zip", container.ID))
	if got["regression-heat/out.xml"] != "<testsuites/>" {
		t.Errorf("zip regression-heat/out.xml wrong: %q (has %v)", got["regression-heat/out.xml"], keysOf(got))
	}

	// A subtree with no artifacts anywhere gets 404, not an empty archive.
	clone := nodeOf(t, byKey, store.TaskKindClone)
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d/artifacts/zip", clone.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bare subtree zip: expected 404, got %d", rec.Code)
	}
	// A run with no artifacts also gets 404 rather than an empty zip.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d/artifacts/zip", runOfTask(t, s, clone).ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("bare run zip: expected 404, got %d", rec.Code)
	}

	// Unknown run/task: 404; unknown artifact download: 404; bad subpath: 404.
	rec = env.authed(http.MethodGet, "/api/test-runs/999999/artifacts/zip", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown run zip: expected 404, got %d", rec.Code)
	}
	rec = env.authed(http.MethodGet, "/api/tasks/999999/artifacts/zip", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task zip: expected 404, got %d", rec.Code)
	}
	rec = env.authed(http.MethodGet, "/api/test-artifacts/999999/download", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown artifact download: expected 404, got %d", rec.Code)
	}
	rec = env.authed(http.MethodGet, "/api/test-artifacts/1/other", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown subpath: expected 404, got %d", rec.Code)
	}
}

// keysOf lists a zip map's entry names for failure messages.
func keysOf(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	return names
}
