package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/runner"
	"md-builder/server/store"
)

// newDispatchTestServer wires a runner Service with an in-memory YAML
// fetcher so webhook pushes create task graphs without any git/network
// access.
func newDispatchTestServer(t *testing.T, yaml string) (*Server, *store.Store) {
	t.Helper()
	apiServer, s := newTestServer(t)
	fetcher := func(ctx context.Context, repoURL, sha string, creds *runner.GitCredentials) ([]byte, error) {
		if yaml == "" {
			return nil, fmt.Errorf("repo unreachable")
		}
		return []byte(yaml), nil
	}
	svc := &runner.Service{Store: s, FetchYAML: fetcher}
	apiServer.SetRunner(svc)
	return apiServer, s
}

const dispatchYAML = `version: 2
defaults:
  build:
    command: "cmake . && cmake --build ."
presets:
  main:
    command: "python3 run.py"
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      use: [main]
  - tags: [gpu, cuda]
    unit:
      command: "ctest -L unit"
`

func seedDispatchEnv(t *testing.T, s *store.Store, name, tags string, enabled bool) *store.TestEnvironment {
	t.Helper()
	u := &store.User{Username: "env-" + name + t.Name(), Email: name + "@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: name, Host: "h", Username: "u", PrivateKey: "k",
		Tags: tags, Enabled: enabled,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	return env
}

func pushBody(repo, sha string) string {
	return fmt.Sprintf(`{
		"object_kind": "push",
		"project": {"name": "code", "path_with_namespace": %q, "web_url": "https://gitlab.com/%s"},
		"ref": "refs/heads/main",
		"before": "0000000",
		"after": %q,
		"user_name": "alice",
		"commits": [{"id": %q, "message": "test commit"}]
	}`, repo, repo, sha, sha)
}

func TestWebhookDispatchesTasks(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	// Site config: code repo matches the webhook project.
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	seedDispatchEnv(t, s, "cpu-node", "cpu", true)
	seedDispatchEnv(t, s, "gpu-node", "gpu,cuda,extra", true)
	seedDispatchEnv(t, s, "disabled-node", "cpu,mpi", false) // not eligible

	post := func(body string) map[string]any {
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return res
	}

	res := post(pushBody("group/code", "abc123def"))
	if res["jobsCreated"].(float64) != 2 {
		t.Fatalf("jobsCreated: want 2, got %v", res["jobsCreated"])
	}

	roots, err := s.ListRootTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 2 {
		t.Fatalf("want 2 root tasks, got %d", len(roots))
	}
	for _, r := range roots {
		if r.Status != store.TaskPending {
			t.Errorf("root should be pending: %+v", r)
		}
		subs, err := s.ListSubTasks(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		// clone + build + (unit) (+ regression case for the cpu entry).
		want := 3
		if r.Tags == "cpu" {
			want = 4
		}
		if len(subs) != want {
			t.Errorf("root %d (%s) subtask count: want %d, got %d", r.ID, r.Tags, want, len(subs))
		}
		if r.Tags != "cpu" && r.Tags != "cuda,gpu" {
			t.Errorf("root tags wrong: %q", r.Tags)
		}
	}

	// Repeat push: graphs are requeued (not duplicated), attempts bumped.
	res = post(pushBody("group/code", "abc123def"))
	if res["jobsCreated"].(float64) != 2 {
		t.Fatalf("re-push jobsCreated: want 2, got %v", res["jobsCreated"])
	}
	roots, _ = s.ListRootTasks(10)
	if len(roots) != 2 {
		t.Fatalf("re-push should requeue, not duplicate: %d roots", len(roots))
	}
	for _, r := range roots {
		if r.Attempts != 1 {
			t.Errorf("re-push attempts: want 1, got %d", r.Attempts)
		}
		subs, _ := s.ListSubTasks(r.ID)
		if len(subs) == 0 {
			t.Errorf("re-push should rebuild sub-tasks of root %d", r.ID)
		}
	}

	// Push to another repo: recorded, no tasks.
	res = post(pushBody("group/other", "fff222"))
	if _, ok := res["jobsCreated"]; ok {
		t.Fatalf("no tasks expected for non-code repo: %v", res)
	}
}

func TestWebhookDispatchErrorSurfaces(t *testing.T) {
	// Empty yaml: the fetcher fails.
	apiServer, s := newDispatchTestServer(t, "")
	mux := http.NewServeMux()
	apiServer.Register(mux)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cpu-node2", "cpu", true)

	rec := postWebhook(t, mux, s, pushBody("group/code", "abc"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook should stay 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["dispatchError"] == "" || res["dispatchError"] == nil {
		t.Fatalf("dispatchError should be surfaced: %v", res)
	}
	// The commit is still recorded.
	if res["commitId"] == nil || res["commitId"].(float64) <= 0 {
		t.Fatalf("commit should be recorded: %v", res)
	}

	// ... and so is the error, on the commit row: the response is gone by the
	// time someone looks at the dashboard, which has to explain the empty
	// columns on its own.
	commitID := int64(res["commitId"].(float64))
	commit, err := s.GetCommitByID(commitID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.DispatchError, "repo unreachable") {
		t.Fatalf("stored dispatch error = %q", commit.DispatchError)
	}

	// The matrix exposes it: the frontend shows it on the cells that have no
	// graph (the graph column of the full view).
	seedUser(t, s, "matrix-viewer", "matrix@example.com", "s3cret12")
	mux2 := http.NewServeMux()
	apiServer.Register(mux2)
	session := loginAndGetCookie(t, mux2, "matrix-viewer", "s3cret12")
	rec = doJSON(t, mux2, http.MethodGet, "/api/dashboard/full", "",
		&http.Cookie{Name: sessionCookie, Value: session})
	if rec.Code != http.StatusOK {
		t.Fatalf("full dashboard: %d, body %s", rec.Code, rec.Body.String())
	}
	var dash fullDashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if len(dash.Rows) != 1 {
		t.Fatalf("dashboard rows: %d, want the one commit", len(dash.Rows))
	}
	if !strings.Contains(dash.Rows[0].Commit.DispatchError, "repo unreachable") {
		t.Fatalf("dashboard dispatch error = %q", dash.Rows[0].Commit.DispatchError)
	}
	// No graph was created, so the row has no task to link either — this is
	// the state the graph column renders as an error icon.
	if len(dash.Rows[0].TaskIDs) != 0 {
		t.Fatalf("task ids: %v, want none", dash.Rows[0].TaskIDs)
	}
}

// TestWebhookRecordsMissingCodeRepo: a push that is not dispatched at all
// (no code repository configured) records the reason too — the matrix would
// otherwise show a row of empty cells with nothing to explain it.
func TestWebhookRecordsMissingCodeRepo(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)
	// No SaveSiteConfig: the code repository is unset.
	seedDispatchEnv(t, s, "cpu-node", "cpu", true)

	rec := postWebhook(t, mux, s, pushBody("group/code", "abc"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook should stay 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if _, ok := res["jobsCreated"]; ok {
		t.Fatalf("nothing should be dispatched: %v", res)
	}
	if res["dispatchSkipped"] == "" || res["dispatchSkipped"] == nil {
		t.Fatalf("the skip reason should be surfaced: %v", res)
	}

	commit, err := s.GetCommitByID(int64(res["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(commit.DispatchError, "no code repository set") {
		t.Fatalf("stored dispatch error = %q", commit.DispatchError)
	}

	// A push to some *other* repository while a code repo IS configured is
	// not recorded: that row never reaches the matrix (it is filtered out),
	// so the message could only ever be stale.
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	rec = postWebhook(t, mux, s, pushBody("group/other", "def"), "X-Gitlab-Event", "Push Hook")
	var res2 map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res2); err != nil {
		t.Fatal(err)
	}
	other, err := s.GetCommitByID(int64(res2["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if other.DispatchError != "" {
		t.Fatalf("other-repo dispatch error = %q, want empty", other.DispatchError)
	}
}

func TestJobsAPIListAndTrigger(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedUser(t, s, "jobsuser", "jobs@example.com", "pw")

	// A commit to trigger against.
	commit := &store.Commit{Repo: "group/code", SHA: "beef99", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "jobsuser", "pw")

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

	// Unauthenticated: 401.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth jobs: expected 401, got %d", rec.Code)
	}
	// Unauthenticated tasks API: 401.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tasks/1", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth tasks: expected 401, got %d", rec.Code)
	}

	// Trigger without environments: everything skipped, no error.
	rec = authed(http.MethodPost, "/api/jobs", fmt.Sprintf(`{"commitId":%d}`, commit.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}

	// Now add an environment and trigger again.
	seedDispatchEnv(t, s, "cpu-trigger", "cpu", true)
	rec = authed(http.MethodPost, "/api/jobs", fmt.Sprintf(`{"commitId":%d}`, commit.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("trigger: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("jobsCreated: want 1, got %v", res)
	}

	// List shows the root task.
	rec = authed(http.MethodGet, "/api/jobs", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list struct {
		Jobs []jobJSON `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Jobs) != 1 {
		t.Fatalf("want 1 job listed, got %d", len(list.Jobs))
	}
	if list.Jobs[0].Status != "pending" || list.Jobs[0].CommitID != commit.ID {
		t.Fatalf("listed job wrong: %+v", list.Jobs[0])
	}

	// Validation: bad limit, missing commit, unknown task.
	rec = authed(http.MethodGet, "/api/jobs?limit=0", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad limit: expected 400, got %d", rec.Code)
	}
	rec = authed(http.MethodPost, "/api/jobs", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no commit: expected 400, got %d", rec.Code)
	}
	rec = authed(http.MethodPost, "/api/jobs", `{"commitId":99999}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown commit: expected 404, got %d", rec.Code)
	}
	rec = authed(http.MethodGet, "/api/tasks/99999", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task: expected 404, got %d", rec.Code)
	}
	rec = authed(http.MethodGet, "/api/tasks/notanumber", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad task id: expected 400, got %d", rec.Code)
	}
}

// TestTaskDetailAndLogs covers GET /api/tasks/{id} and /log on a seeded
// graph: the root detail carries sub-tasks; logs are read incrementally.
func TestTaskDetailAndLogs(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedUser(t, s, "taskuser", "task@example.com", "pw")
	env := seedDispatchEnv(t, s, "cpu-detail", "cpu", true)
	commit := &store.Commit{Repo: "group/code", SHA: "detail01", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}

	root := &store.Task{
		Kind: store.TaskKindRoot, Name: "test detail01", CommitID: commit.ID,
		EnvironmentID: env.ID, Tags: "cpu", Config: `{"entry":{"tags":["cpu"]},"testInputRef":"main"}`,
	}
	subs := []*store.Task{
		{Kind: store.TaskKindClone, Name: "clone repositories", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: store.TaskKindBuild, Name: "build", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: store.TaskKindUnit, Name: "unit tests", CommitID: commit.ID, EnvironmentID: env.ID},
	}
	deps := [][]int64{
		{},
		{store.TaskSubPlaceholderBase + 0},
		{store.TaskSubPlaceholderBase + 1},
	}
	stored, err := store.CreateTaskGraph(s, root, subs, deps)
	if err != nil {
		t.Fatal(err)
	}

	// Logs on the clone task.
	if err := s.AppendTaskLog(stored[1].ID, 1, "cloning...\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendTaskLog(stored[1].ID, 2, "uploading 12.3 MiB\n"); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "taskuser", "pw")
	authed := func(method, target string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Root detail: carries sub-tasks and commit/environment context.
	rec := authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d", root.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("task detail: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var detail taskDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Kind != store.TaskKindRoot || len(detail.SubTasks) != 3 {
		t.Fatalf("root detail wrong: kind=%s subs=%d", detail.Kind, len(detail.SubTasks))
	}
	if detail.Commit == nil || detail.Commit.SHA != "detail01" {
		t.Fatalf("commit context missing: %+v", detail.Commit)
	}
	if detail.Environment == nil || detail.Environment.Name != "cpu-detail" {
		t.Fatalf("environment context missing: %+v", detail.Environment)
	}
	if len(detail.SubTasks[0].DependsOn) != 0 {
		t.Fatalf("clone should have no dependencies (the root is a container): %+v", detail.SubTasks[0])
	}

	// Sub-task detail: no sub-tasks of its own.
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d", stored[1].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("sub detail: expected 200, got %d", rec.Code)
	}
	var subDetail taskDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &subDetail); err != nil {
		t.Fatal(err)
	}
	if subDetail.Kind != store.TaskKindClone || len(subDetail.SubTasks) != 0 {
		t.Fatalf("sub detail wrong: %+v", subDetail)
	}

	// Logs: full read then incremental.
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d/log", stored[1].ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("log read: expected 200, got %d", rec.Code)
	}
	var logs struct {
		Chunks  []logChunkJSON `json:"chunks"`
		LastSeq int            `json:"lastSeq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Chunks) != 2 || logs.LastSeq != 2 {
		t.Fatalf("log read wrong: %+v", logs)
	}
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d/log?after=1", stored[1].ID))
	if err := json.Unmarshal(rec.Body.Bytes(), &logs); err != nil {
		t.Fatal(err)
	}
	if len(logs.Chunks) != 1 || logs.Chunks[0].Seq != 2 {
		t.Fatalf("incremental log read wrong: %+v", logs)
	}

	// Log of an unknown task: 404; bad after: 400.
	rec = authed(http.MethodGet, "/api/tasks/99999/log")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task log: expected 404, got %d", rec.Code)
	}
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d/log?after=x", stored[1].ID))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad after: expected 400, got %d", rec.Code)
	}
}

// TestDashboardBuildKind checks the third dashboard kind: build runs are
// reported, listed on /api/dashboard/build and rejected for unknown kinds.
func TestDashboardBuildKind(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	seedUser(t, s, "builduser", "bu@example.com", "pw")
	env := seedDispatchEnv(t, s, "cpu-build", "cpu", true)
	commit := &store.Commit{Repo: "group/code", SHA: "build99", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "builduser", "pw")
	authed := func(method, target string, body string) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, rd)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Empty matrix first.
	rec := authed(http.MethodGet, "/api/dashboard/build", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("build dashboard: expected 200, got %d", rec.Code)
	}

	// Report a passed build run (simplified path: no cases).
	body := fmt.Sprintf(`{"environmentId":%d,"commitId":%d,"kind":"build","status":"passed","summary":"build ok"}`,
		env.ID, commit.ID)
	rec = authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("report build run: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}

	// The build matrix shows the run; other kinds stay empty.
	rec = authed(http.MethodGet, "/api/dashboard/build", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("build dashboard: expected 200, got %d", rec.Code)
	}
	var dash dashboardJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if len(dash.Rows) != 1 || dash.Rows[0].Cells[0] == nil {
		t.Fatalf("build matrix wrong: %+v", dash.Rows)
	}
	if dash.Rows[0].Cells[0].RunID == 0 || dash.Rows[0].Cells[0].Status != store.StatusPassed {
		t.Fatalf("build cell wrong: %+v", dash.Rows[0].Cells[0])
	}

	// A failed build with a compiler error in the summary.
	body = fmt.Sprintf(`{"environmentId":%d,"commitId":%d,"kind":"build","status":"failed","summary":"CMake Error: bad flag"}`,
		env.ID, commit.ID)
	rec = authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("replace build run: expected 201, got %d", rec.Code)
	}
	rec = authed(http.MethodGet, "/api/dashboard/build", "")
	dash = dashboardJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if dash.Rows[0].Cells[0].Status != store.StatusFailed {
		t.Fatalf("failed build cell wrong: %+v", dash.Rows[0].Cells[0])
	}

	// kind=build accepted on test-runs; an unknown kind is still rejected.
	body = fmt.Sprintf(`{"environmentId":%d,"commitId":%d,"kind":"perf"}`, env.ID, commit.ID)
	rec = authed(http.MethodPost, "/api/test-runs", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: expected 400, got %d", rec.Code)
	}

	// The unit dashboard is unaffected by the build runs (no unit runs).
	rec = authed(http.MethodGet, "/api/dashboard/unit", "")
	dash = dashboardJSON{}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
		t.Fatal(err)
	}
	if dash.Rows[0].Cells[0] != nil {
		t.Fatalf("unit cell should be empty: %+v", dash.Rows[0].Cells[0])
	}
}

func TestDashboardOverlaysTaskState(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	seedUser(t, s, "overlayuser", "ov@example.com", "pw")

	env := seedDispatchEnv(t, s, "cpu-overlay", "cpu", true)
	envStageless := seedDispatchEnv(t, s, "cpu-stageless", "cpu", true)
	commit := &store.Commit{Repo: "group/code", SHA: "cafe11", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	// A pending task graph for (env, commit) with no run: clone → regression.
	root := &store.Task{
		Kind: store.TaskKindRoot, Name: "test cafe11", CommitID: commit.ID,
		EnvironmentID: env.ID, Tags: "cpu", Config: "{}",
	}
	subs := []*store.Task{
		{Kind: store.TaskKindClone, Name: "clone", CommitID: commit.ID, EnvironmentID: env.ID},
		{Kind: store.TaskKindRegression, Name: "regression", CommitID: commit.ID, EnvironmentID: env.ID},
	}
	created, err := store.CreateTaskGraph(s, root, subs, [][]int64{
		{}, {store.TaskSubPlaceholderBase + 0},
	})
	if err != nil {
		t.Fatal(err)
	}
	regSub := created[2] // created = [root, clone, regression]

	// A graph with no regression stage at all (e.g. a manual dispatch with
	// only a build command): the regression matrix must show "—", not a
	// failed 0/0 invented from the root state.
	rootBuildOnly := &store.Task{
		Kind: store.TaskKindRoot, Name: "test cafe11 build-only", CommitID: commit.ID,
		EnvironmentID: envStageless.ID, Tags: "cpu", Config: "{}",
	}
	subsBuildOnly := []*store.Task{
		{Kind: store.TaskKindClone, Name: "clone", CommitID: commit.ID, EnvironmentID: envStageless.ID},
		{Kind: store.TaskKindBuild, Name: "build", CommitID: commit.ID, EnvironmentID: envStageless.ID},
	}
	if _, err := store.CreateTaskGraph(s, rootBuildOnly, subsBuildOnly, [][]int64{
		{}, {store.TaskSubPlaceholderBase + 0},
	}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "overlayuser", "pw")

	fetch := func() dashboardJSON {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/dashboard/regression", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("dashboard: expected 200, got %d", rec.Code)
		}
		var dash dashboardJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil {
			t.Fatal(err)
		}
		return dash
	}

	dash := fetch()
	if len(dash.Rows) != 1 || len(dash.Rows[0].Cells) != 2 {
		t.Fatalf("unexpected matrix shape: %d rows, %d cells", len(dash.Rows), len(dash.Rows[0].Cells))
	}
	envIdx := func(id int64) int {
		for i, e := range dash.Environments {
			if e.ID == id {
				return i
			}
		}
		t.Fatalf("environment %d not found", id)
		return -1
	}
	cell := dash.Rows[0].Cells[envIdx(env.ID)]
	if cell == nil {
		t.Fatal("expected a task overlay cell, got null")
	}
	if cell.Status != "pending" || cell.RunID != 0 || cell.TaskID != regSub.ID {
		t.Fatalf("overlay cell wrong: %+v", cell)
	}
	// The build-only graph has no regression stage: no overlay, no invented
	// failure — the cell stays null ("—").
	if c := dash.Rows[0].Cells[envIdx(envStageless.ID)]; c != nil {
		t.Fatalf("stage-less graph should show no cell, got %+v", c)
	}

	// A done root is not overlaid (the run takes over).
	if err := s.FinishTask(root.ID, store.TaskDone, ""); err != nil {
		t.Fatal(err)
	}
	dash = fetch()
	if c := dash.Rows[0].Cells[envIdx(env.ID)]; c != nil {
		t.Fatalf("done root should not overlay: %+v", c)
	}

	// Environment columns now include tags.
	if dash.Environments[0].Tags != "cpu" {
		t.Fatalf("environment tags missing: %+v", dash.Environments[0])
	}
}

// TestManualTrigger creates graphs through POST /api/jobs/manual and checks
// the trigger flag, the recorded commit and the validation paths.
func TestManualTrigger(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedUser(t, s, "manualuser", "manual@example.com", "pw")
	envCPU := seedDispatchEnv(t, s, "cpu-manual", "cpu", true)
	envGPU := seedDispatchEnv(t, s, "gpu-manual", "gpu", true)

	// The runner resolves refs without git: inject a fake.
	apiServer.Runner.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *runner.GitCredentials) (string, error) {
		return "abc123abc123abc123abc123abc123abc123abc1", nil
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "manualuser", "pw")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Validation: no environments.
	rec := authed(http.MethodPost, "/api/jobs/manual", `{"repo":"https://gitlab.com/group/code","buildCommand":"make"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no envs: expected 422, got %d, body %s", rec.Code, rec.Body.String())
	}
	// Validation: all commands empty.
	rec = authed(http.MethodPost, "/api/jobs/manual",
		fmt.Sprintf(`{"environmentIds":[%d,%d],"unitCommand":""}`, envCPU.ID, envGPU.ID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no commands: expected 422, got %d, body %s", rec.Code, rec.Body.String())
	}

	// Happy path: build + unit on two environments, repo omitted (site
	// default). unitArtifacts exercises the list form; a scalar string is
	// also accepted (see the regression guard below).
	rec = authed(http.MethodPost, "/api/jobs/manual", fmt.Sprintf(`{
		"buildCommand": "make -j4",
		"unitCommand": "ctest -L unit --gtest_output",
		"unitArtifacts": ["build/test_detail.xml", "build/extra_results.json"],
		"environmentIds": [%d, %d]
	}`, envCPU.ID, envGPU.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("manual trigger: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res struct {
		Roots []manualTestRoot `json:"roots"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Roots) != 2 {
		t.Fatalf("expected 2 roots, got %d (%s)", len(res.Roots), rec.Body.String())
	}

	// The commit is recorded (normalized repo path) and each root is a
	// manual graph over it, with build and unit sub-tasks.
	commit, err := s.GetCommitByID(loadRootCommitID(t, s, res.Roots[0].TaskID))
	if err != nil {
		t.Fatal(err)
	}
	if commit.Repo != "group/code" || commit.SHA != "abc123abc123abc123abc123abc123abc123abc1" {
		t.Fatalf("unexpected commit: %+v", commit)
	}
	if commit.Author != "manualuser" {
		t.Fatalf("commit author: want manualuser, got %q", commit.Author)
	}
	for _, rootRef := range res.Roots {
		root, err := s.GetTask(rootRef.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		if root.Trigger != store.TaskTriggerManual {
			t.Fatalf("root %d trigger: want manual(%d), got %d", root.ID, store.TaskTriggerManual, root.Trigger)
		}
		subs, err := s.ListSubTasks(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		kinds := map[string]bool{}
		var unitArtifacts []string
		for i := range subs {
			kinds[subs[i].Kind] = true
			if subs[i].Kind == store.TaskKindUnit {
				var sc struct {
					Artifacts []string `json:"artifacts"`
				}
				if err := json.Unmarshal([]byte(subs[i].Config), &sc); err != nil {
					t.Fatal(err)
				}
				unitArtifacts = sc.Artifacts
			}
		}
		if !kinds[store.TaskKindBuild] || !kinds[store.TaskKindUnit] || kinds[store.TaskKindRegression] {
			t.Fatalf("root %d sub-task kinds wrong: %v", root.ID, kinds)
		}
		if len(unitArtifacts) != 2 || unitArtifacts[0] != "build/test_detail.xml" || unitArtifacts[1] != "build/extra_results.json" {
			t.Fatalf("root %d unit artifact paths: want [build/test_detail.xml build/extra_results.json], got %v", root.ID, unitArtifacts)
		}
	}

	// The task detail API exposes the trigger.
	rec = authed(http.MethodGet, fmt.Sprintf("/api/tasks/%d", res.Roots[0].TaskID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("task detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail taskDetailJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Trigger != store.TaskTriggerManual {
		t.Fatalf("detail trigger: want %d, got %d", store.TaskTriggerManual, detail.Trigger)
	}

	// Webhook-created roots keep trigger 0.
	commitWH := &store.Commit{Repo: "group/code", SHA: "ffff01", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commitWH); err != nil {
		t.Fatal(err)
	}
	rec = postWebhook(t, mux, s, pushBody("group/code", "ffff01"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
	whRoot, err := s.FindRootTaskByCommitEnv(commitWH.ID, envCPU.ID)
	if err != nil {
		t.Fatal(err)
	}
	if whRoot.Trigger != store.TaskTriggerWebhook {
		t.Fatalf("webhook root trigger: want %d, got %d", store.TaskTriggerWebhook, whRoot.Trigger)
	}

	// A disabled environment is rejected. SetEnvironmentEnabled is
	// owner-scoped, so disable via a direct column update (no BeforeSave).
	if err := s.DB.Model(&store.TestEnvironment{}).Where("id = ?", envGPU.ID).
		UpdateColumn("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	rec = authed(http.MethodPost, "/api/jobs/manual",
		fmt.Sprintf(`{"unitCommand":"go test ./...","environmentIds":[%d]}`, envGPU.ID))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("disabled env: expected 422, got %d, body %s", rec.Code, rec.Body.String())
	}
}

// loadRootCommitID returns a root task's CommitID.
func loadRootCommitID(t *testing.T, s *store.Store, taskID int64) int64 {
	t.Helper()
	task, err := s.GetTask(taskID)
	if err != nil {
		t.Fatal(err)
	}
	return task.CommitID
}

// TestManualYAMLTrigger exercises POST /api/jobs/manual-yaml: a ref resolves
// against the site code repository, the commit is recorded (deduplicated on
// re-trigger) and the yaml matrix dispatches one graph per matching
// environment, with the manual-yaml trigger flag on the roots.
func TestManualYAMLTrigger(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedUser(t, s, "yamluser", "yaml@example.com", "pw")
	envCPU := seedDispatchEnv(t, s, "cpu-yaml", "cpu", true)
	envGPU := seedDispatchEnv(t, s, "gpu-yaml", "gpu", true)

	apiServer.Runner.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *runner.GitCredentials) (string, error) {
		if repoURL != "https://gitlab.com/group/code" {
			t.Errorf("resolve repo: got %q", repoURL)
		}
		if ref != "v2.0" {
			t.Errorf("resolve ref: want v2.0, got %q", ref)
		}
		return "fedcba98fedcba98fedcba98fedcba98fedcba98", nil
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "yamluser", "pw")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// GET is not allowed.
	rec := authed(http.MethodGet, "/api/jobs/manual-yaml", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: expected 405, got %d", rec.Code)
	}

	// Dispatch the matrix at v2.0: both environments match (the gpu entry
	// needs cpu+gpu... only the cpu entry matches cpu; the gpu entry wants
	// [gpu, cuda] which no env satisfies), so 1 graph and 1 skip.
	rec = authed(http.MethodPost, "/api/jobs/manual-yaml", `{"ref":"v2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("manual-yaml: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res struct {
		CommitID       int64  `json:"commitId"`
		CommitSHA      string `json:"commitSha"`
		CommitCreated  bool   `json:"commitCreated"`
		JobsCreated    int    `json:"jobsCreated"`
		EntriesSkipped int    `json:"entriesSkipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.CommitID == 0 || res.CommitSHA != "fedcba98fedcba98fedcba98fedcba98fedcba98" {
		t.Fatalf("commit fields wrong: %+v", res)
	}
	if !res.CommitCreated {
		t.Error("first dispatch should create the commit")
	}
	if res.JobsCreated != 1 || res.EntriesSkipped != 1 {
		t.Errorf("dispatch counts: jobs %d skipped %d, want 1/1", res.JobsCreated, res.EntriesSkipped)
	}

	// The created root is a manual-yaml graph over the recorded commit.
	roots, err := s.ListRootTasks(10)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots: %v %d", err, len(roots))
	}
	if roots[0].Trigger != store.TaskTriggerManualYAML {
		t.Errorf("root trigger: want manual-yaml(%d), got %d", store.TaskTriggerManualYAML, roots[0].Trigger)
	}
	if roots[0].CommitID != res.CommitID || roots[0].EnvironmentID != envCPU.ID {
		t.Errorf("root over wrong commit/env: %+v (commit %d, cpu %d)", roots[0], res.CommitID, envCPU.ID)
	}

	// Re-triggering the same ref deduplicates the commit and requeues the
	// same (commit, environment) graph instead of duplicating it.
	rec = authed(http.MethodPost, "/api/jobs/manual-yaml", `{"ref":"v2.0"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-dispatch: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res2 struct {
		CommitID      int64 `json:"commitId"`
		CommitCreated bool  `json:"commitCreated"`
		JobsCreated   int   `json:"jobsCreated"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res2); err != nil {
		t.Fatal(err)
	}
	if res2.CommitCreated || res2.CommitID != res.CommitID {
		t.Errorf("re-dispatch must reuse the commit: %+v", res2)
	}
	if res2.JobsCreated != 1 {
		t.Errorf("re-dispatch jobs: %d, want 1 (requeue)", res2.JobsCreated)
	}
	roots2, _ := s.ListRootTasks(10)
	if len(roots2) != 1 {
		t.Errorf("roots after re-dispatch: %d, want 1", len(roots2))
	}

	// A failing ref resolution surfaces as 422 with dispatchError.
	apiServer.Runner.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *runner.GitCredentials) (string, error) {
		return "", fmt.Errorf("git ls-remote: ref %q not found", ref)
	}
	rec = authed(http.MethodPost, "/api/jobs/manual-yaml", `{"ref":"nope"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad ref: expected 422, got %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "dispatchError") {
		t.Errorf("bad ref response missing dispatchError: %s", rec.Body.String())
	}

	// Environment rows the graph did not use stay untouched.
	var envCount int64
	if err := s.DB.Model(&store.TestEnvironment{}).Where("id = ?", envGPU.ID).Count(&envCount).Error; err != nil || envCount != 1 {
		t.Fatalf("env count: %v %d", err, envCount)
	}
}
