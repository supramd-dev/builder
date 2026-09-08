package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// seedDashboardData creates a user, two environments (one disabled), three
// commits (via the webhook path so they are realistic) and reports runs for
// a subset of (environment, commit) pairs.
func seedDashboardData(t *testing.T, apiServer *Server) {
	t.Helper()

	// Two owners so the site-wide environment list is exercised.
	for _, u := range []struct{ name, email string }{
		{"alice", "alice@example.com"},
		{"bob", "bob@example.com"},
	} {
		if err := apiServer.Store.CreateUser(&store.User{
			Username: u.name, Email: u.email, PasswordHash: "hash",
		}); err != nil {
			t.Fatalf("create user %s: %v", u.name, err)
		}
	}
	var alice, bob store.User
	apiServer.Store.DB.Where("username = ?", "alice").First(&alice)
	apiServer.Store.DB.Where("username = ?", "bob").First(&bob)

	envs := []*store.TestEnvironment{
		{OwnerID: alice.ID, Name: "cpu-node-1", Host: "h1", Username: "u", PrivateKey: "k", Description: "CPU pool"},
		{OwnerID: alice.ID, Name: "gpu-a100", Host: "h2", Username: "u", PrivateKey: "k"},
		{OwnerID: bob.ID, Name: "mpi-cluster", Host: "h3", Username: "u", PrivateKey: "k"},
	}
	for _, env := range envs {
		if err := apiServer.Store.CreateEnvironment(env); err != nil {
			t.Fatalf("create env %s: %v", env.Name, err)
		}
	}
	// Disable gpu-a100: it must still appear as a matrix row.
	if _, err := apiServer.Store.SetEnvironmentEnabled(alice.ID, envs[1].ID, false); err != nil {
		t.Fatalf("disable env: %v", err)
	}

	// Commits via the public helper so pushed_at stays ordered.
	commits := []*store.Commit{
		{Repo: "group/md-code", SHA: "1111111", Ref: "main", Author: "alice", Message: "first", PushedAt: time.Now().Add(-3 * time.Hour)},
		{Repo: "group/md-code", SHA: "2222222", Ref: "main", Author: "bob", Message: "second", PushedAt: time.Now().Add(-2 * time.Hour)},
		{Repo: "group/md-code", SHA: "3333333", Ref: "main", Author: "alice", Message: "third", PushedAt: time.Now().Add(-1 * time.Hour)},
		{Repo: "group/other", SHA: "4444444", Ref: "main", Author: "bob", Message: "other repo", PushedAt: time.Now()},
	}
	commitIDs := map[string]int64{}
	for _, c := range commits {
		created, err := apiServer.Store.GetOrCreateCommit(c)
		if err != nil {
			t.Fatalf("create commit %s: %v", c.SHA, err)
		}
		_ = created
		commitIDs[c.SHA] = c.ID
	}

	// Runs: cpu-node-1 at all three md-code commits; mpi-cluster only at the
	// newest; gpu-a100 none.
	must := func(run *store.TestRun, err error) *store.TestRun {
		t.Helper()
		if err != nil {
			t.Fatalf("upsert run: %v", err)
		}
		return run
	}
	envID := func(name string) int64 {
		var env store.TestEnvironment
		apiServer.Store.DB.Where("name = ?", name).First(&env)
		return env.ID
	}
	must(apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: envID("cpu-node-1"), CommitID: commitIDs["1111111"], Kind: "regression",
		Cases: []store.TestCaseResult{
			{Name: "lj-argon-nve", Status: "passed", ErrorValue: 1.2e-07, Message: "max rel err"},
			{Name: "water-tip4p-npt", Status: "failed", ErrorValue: 0.02, Message: "drift above threshold"},
		},
		StartedAt: time.Now().Add(-3 * time.Hour), FinishedAt: time.Now().Add(-3 * time.Hour).Add(4 * time.Minute),
	}))
	must(apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: envID("cpu-node-1"), CommitID: commitIDs["2222222"], Kind: "regression",
		Cases: []store.TestCaseResult{
			{Name: "lj-argon-nve", Status: "passed", ErrorValue: 1.1e-07},
			{Name: "water-tip4p-npt", Status: "passed", ErrorValue: 9e-05},
		},
	}))
	must(apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: envID("cpu-node-1"), CommitID: commitIDs["3333333"], Kind: "regression",
		Cases: []store.TestCaseResult{
			{Name: "lj-argon-nve", Status: "passed", ErrorValue: 1.3e-07},
			{Name: "water-tip4p-npt", Status: "passed", ErrorValue: 8.5e-05},
			{Name: "argon-liquid-nvt", Status: "failed", ErrorValue: 0.01, Message: "energy drift"},
		},
	}))
	must(apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: envID("mpi-cluster"), CommitID: commitIDs["3333333"], Kind: "regression",
		Cases: []store.TestCaseResult{
			{Name: "lj-argon-nve", Status: "passed", ErrorValue: 2e-07},
			{Name: "water-tip4p-npt", Status: "passed", ErrorValue: 9.1e-05},
		},
	}))
	// Unit runs for the unit dashboard.
	must(apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: envID("cpu-node-1"), CommitID: commitIDs["3333333"], Kind: "unit",
		Cases: []store.TestCaseResult{
			{Name: "TestForce", Status: "passed"},
			{Name: "TestIntegrate", Status: "passed"},
			{Name: "TestNeighborList", Status: "failed"},
		},
	}))
}

// dashboardTestEnv bundles a test server, mux and an authenticated cookie.
type dashboardTestEnv struct {
	mux    *http.ServeMux
	cookie string
}

// authed issues an authenticated request against the dashboard test mux.
func (e dashboardTestEnv) authed(method, target, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.cookie})
	e.mux.ServeHTTP(rec, req)
	return rec
}

// newDashboardEnv seeds data and logs in as a real user (with a real bcrypt
// password, since loginAndGetCookie performs the full login flow).
func newDashboardEnv(t *testing.T) (*Server, dashboardTestEnv) {
	t.Helper()
	apiServer, s := newTestServer(t)

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
	return apiServer, dashboardTestEnv{mux: mux, cookie: cookie}
}

// authHash is indirection over auth.HashPassword to keep the import list tidy.
func authHash(pw string) (string, error) {
	return auth.HashPassword(pw)
}

func TestDashboardRegressionMatrix(t *testing.T) {
	apiServer, env := newDashboardEnv(t)

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
	var dash struct {
		Kind         string `json:"kind"`
		Environments []struct {
			ID      int64  `json:"id"`
			Name    string `json:"name"`
			Enabled bool   `json:"enabled"`
		} `json:"environments"`
		Rows []struct {
			Commit struct {
				ID       int64  `json:"id"`
				SHA      string `json:"sha"`
				ShortSHA string `json:"shortSha"`
				Repo     string `json:"repo"`
				Author   string `json:"author"`
			} `json:"commit"`
			Cells []*struct {
				RunID  int64  `json:"runId"`
				Status string `json:"status"`
				Total  int    `json:"total"`
				Passed int    `json:"passed"`
				Failed int    `json:"failed"`
			} `json:"cells"`
		} `json:"rows"`
	}
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

	// Cells align with environment columns: cpu-node-1 has runs at 3 of 4
	// commits, gpu-a100 none, mpi-cluster one.
	envIdx := func(name string) int {
		for i, e := range dash.Environments {
			if e.Name == name {
				return i
			}
		}
		t.Fatalf("environment %q not found", name)
		return -1
	}
	cpu, gpu, mpi := envIdx("cpu-node-1"), envIdx("gpu-a100"), envIdx("mpi-cluster")
	for _, row := range dash.Rows {
		if len(row.Cells) != 3 {
			t.Fatalf("expected 3 cells per row, got %d", len(row.Cells))
		}
	}
	// Row 0 = other-repo commit: no runs anywhere.
	if dash.Rows[0].Cells[cpu] != nil || dash.Rows[0].Cells[gpu] != nil || dash.Rows[0].Cells[mpi] != nil {
		t.Fatalf("expected all-null row for the other-repo commit: %+v", dash.Rows[0].Cells)
	}
	// Row 1 = newest md-code commit: cpu failed 1/3, mpi passed, gpu none.
	if c := dash.Rows[1].Cells[cpu]; c == nil || c.Status != "failed" || c.Total != 3 || c.Failed != 1 {
		t.Fatalf("unexpected cpu cell at newest commit: %+v", dash.Rows[1].Cells[cpu])
	}
	if c := dash.Rows[1].Cells[mpi]; c == nil || c.Status != "passed" {
		t.Fatalf("unexpected mpi cell at newest commit: %+v", dash.Rows[1].Cells[mpi])
	}
	if dash.Rows[1].Cells[gpu] != nil {
		t.Fatalf("expected no gpu run, got %+v", dash.Rows[1].Cells[gpu])
	}
	// Row 2 = second commit: cpu passed 2/2, others none.
	if c := dash.Rows[2].Cells[cpu]; c == nil || c.Status != "passed" || c.Passed != 2 {
		t.Fatalf("unexpected cpu cell at second commit: %+v", dash.Rows[2].Cells[cpu])
	}
	if dash.Rows[2].Cells[gpu] != nil || dash.Rows[2].Cells[mpi] != nil {
		t.Fatal("expected nulls elsewhere at second commit")
	}
	// Row 3 = oldest commit: cpu failed 1/2, others none.
	if c := dash.Rows[3].Cells[cpu]; c == nil || c.Status != "failed" || c.Failed != 1 {
		t.Fatalf("unexpected cpu cell at oldest commit: %+v", dash.Rows[3].Cells[cpu])
	}
	// gpu-a100 has no runs at any commit.
	for _, row := range dash.Rows {
		if row.Cells[gpu] != nil {
			t.Fatalf("expected all-null column for gpu-a100, got %+v", row.Cells[gpu])
		}
	}

	_ = apiServer
}

func TestDashboardRepoFilter(t *testing.T) {
	apiServer, env := newDashboardEnv(t)

	// Configure the code repo: only pushes to that repository remain.
	cfg, err := apiServer.Store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	cfg.CodeRepo = "https://gitlab.com/group/md-code.git"
	if err := apiServer.Store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	rec := env.authed(http.MethodGet, "/api/dashboard/regression", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d", rec.Code)
	}
	var dash struct {
		RepoFilter string `json:"repoFilter"`
		Rows       []struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		} `json:"rows"`
	}
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

func TestReportTestRunAndDetail(t *testing.T) {
	apiServer, env := newDashboardEnv(t)

	// A report with an invalid kind is rejected.
	rec := env.authed(http.MethodPost, "/api/test-runs",
		`{"environmentId":1,"commitId":1,"kind":"perf","cases":[]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid kind: expected 400, got %d", rec.Code)
	}

	// Invalid case status.
	rec = env.authed(http.MethodPost, "/api/test-runs",
		`{"environmentId":1,"commitId":1,"kind":"regression","cases":[{"name":"a","status":"skipped"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: expected 400, got %d", rec.Code)
	}

	// Missing commit reference.
	rec = env.authed(http.MethodPost, "/api/test-runs",
		`{"environmentId":1,"kind":"regression"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing commit: expected 400, got %d", rec.Code)
	}

	// Unknown environment id: 404.
	rec = env.authed(http.MethodPost, "/api/test-runs",
		`{"environmentId":999,"commitSha":"1111111","commitRepo":"group/md-code","kind":"regression"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown env: expected 404, got %d, body %s", rec.Code, rec.Body.String())
	}

	// Resolve environment + commit ids from the seeded data.
	var env1 store.TestEnvironment
	apiServer.Store.DB.Where("name = ?", "cpu-node-1").First(&env1)
	commit := &store.Commit{Repo: "group/md-code", SHA: "1111111"}
	if _, err := apiServer.Store.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("get commit: %v", err)
	}

	// Report by commitSha (no commitId): replaces the seeded run.
	body := fmt.Sprintf(`{
		"environmentId": %d,
		"commitSha": "1111111",
		"commitRepo": "group/md-code",
		"kind": "regression",
		"startedAt": "2026-09-08T03:00:00Z",
		"finishedAt": "2026-09-08T03:04:00Z",
		"cases": [
			{"name": "lj-argon-nve", "status": "passed", "errorValue": 1.2e-07, "message": "max rel err"},
			{"name": "water-tip4p-npt", "status": "passed", "errorValue": 9e-05}
		]
	}`, env1.ID)
	rec = env.authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("report: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	var run struct {
		ID            int64  `json:"id"`
		Status        string `json:"status"`
		Total         int    `json:"total"`
		Passed        int    `json:"passed"`
		EnvironmentID int64  `json:"environmentId"`
		CommitID      int64  `json:"commitId"`
		StartedAt     string `json:"startedAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if run.Status != "passed" || run.Total != 2 || run.Passed != 2 {
		t.Fatalf("unexpected run: %+v", run)
	}
	if run.CommitID != commit.ID {
		t.Fatalf("expected commit %d, got %d", commit.ID, run.CommitID)
	}
	if run.StartedAt != "2026-09-08T03:00:00Z" {
		t.Fatalf("unexpected startedAt: %q", run.StartedAt)
	}

	// Detail: case list with error values, environment and commit context.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", run.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Status          string `json:"status"`
		EnvironmentName string `json:"environmentName"`
		CommitShortSHA  string `json:"commitShortSha"`
		CommitAuthor    string `json:"commitAuthor"`
		Cases           []struct {
			Name       string  `json:"name"`
			Status     string  `json:"status"`
			ErrorValue float64 `json:"errorValue"`
			Message    string  `json:"message"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.EnvironmentName != "cpu-node-1" || detail.CommitShortSHA != "1111111" || detail.CommitAuthor != "alice" {
		t.Fatalf("unexpected context: %+v", detail)
	}
	if len(detail.Cases) != 2 || detail.Cases[0].Name != "lj-argon-nve" {
		t.Fatalf("unexpected cases: %+v", detail.Cases)
	}
	if detail.Cases[0].ErrorValue != 1.2e-07 {
		t.Fatalf("unexpected error value: %v", detail.Cases[0].ErrorValue)
	}

	// The dashboard now shows the replaced cell as passed.
	rec = env.authed(http.MethodGet, "/api/dashboard/regression", "")
	var dash struct {
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
		Rows []struct {
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
			Cells []*struct {
				Status string `json:"status"`
			} `json:"cells"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	cpuIdx := -1
	for i, e := range dash.Environments {
		if e.Name == "cpu-node-1" {
			cpuIdx = i
		}
	}
	if cpuIdx < 0 {
		t.Fatal("cpu-node-1 not among environment columns")
	}
	// Rows are newest-first: 1111111 is the oldest md-code commit, last of
	// the 4 rows (no repo filter).
	row := dash.Rows[3]
	if row.Commit.SHA != "1111111" {
		t.Fatalf("expected oldest commit row, got %+v", row.Commit)
	}
	if cell := row.Cells[cpuIdx]; cell == nil || cell.Status != "passed" {
		t.Fatalf("expected replaced cell passed, got %+v", cell)
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
