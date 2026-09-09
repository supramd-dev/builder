package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/store"
	"md-builder/server/worker"
)

// newDispatchTestServer wires a Dispatcher with an in-memory YAML fetcher so
// webhook pushes trigger job creation without any git/network access.
func newDispatchTestServer(t *testing.T, yaml string) (*Server, *store.Store) {
	t.Helper()
	apiServer, s := newTestServer(t)
	fetcher := func(repoURL, sha string) ([]byte, error) {
		if yaml == "" {
			return nil, fmt.Errorf("repo unreachable")
		}
		return []byte(yaml), nil
	}
	apiServer.SetDispatcher(&worker.Dispatcher{Store: s, FetchYAML: fetcher})
	return apiServer, s
}

const dispatchYAML = `version: 1
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      command: "python3 run.py"
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

func TestWebhookDispatchesJobs(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	// Site config: code repo matches the webhook project.
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo:      "https://gitlab.com/group/code",
		TestInputRepo: "https://gitlab.com/group/tests",
		TestRepoRef:   "main"}); err != nil {
		t.Fatal(err)
	}

	seedDispatchEnv(t, s, "cpu-node", "cpu", true)
	seedDispatchEnv(t, s, "gpu-node", "gpu,cuda,extra", true)
	seedDispatchEnv(t, s, "disabled-node", "cpu,mpi", false) // not eligible

	post := func(body string) map[string]any {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", strings.NewReader(body))
		req.Header.Set("X-Gitlab-Event", "Push Hook")
		mux.ServeHTTP(rec, req)
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

	jobs, err := s.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if j.Status != store.JobPending {
			t.Errorf("job should be pending: %+v", j)
		}
		if j.TestInputRef != "main" {
			t.Errorf("job should snapshot TestInputRef: %+v", j)
		}
		if j.Tags != "cpu" && j.Tags != "cuda,gpu" {
			t.Errorf("job tags wrong: %q", j.Tags)
		}
	}

	// Repeat push: jobs are requeued (not duplicated), attempts bumped.
	res = post(pushBody("group/code", "abc123def"))
	if res["jobsCreated"].(float64) != 2 {
		t.Fatalf("re-push jobsCreated: want 2, got %v", res["jobsCreated"])
	}
	jobs, _ = s.ListJobs(10)
	if len(jobs) != 2 {
		t.Fatalf("re-push should requeue, not duplicate: %d jobs", len(jobs))
	}
	for _, j := range jobs {
		if j.Attempts != 1 {
			t.Errorf("re-push attempts: want 1, got %d", j.Attempts)
		}
	}

	// Push to another repo: recorded, no jobs.
	res = post(pushBody("group/other", "fff222"))
	if _, ok := res["jobsCreated"]; ok {
		t.Fatalf("no jobs expected for non-code repo: %v", res)
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

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab",
		strings.NewReader(pushBody("group/code", "abc")))
	req.Header.Set("X-Gitlab-Event", "Push Hook")
	mux.ServeHTTP(rec, req)
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
}

func TestJobsAPIListAndTrigger(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo:      "https://gitlab.com/group/code",
		TestInputRepo: "https://gitlab.com/group/tests",
		TestRepoRef:   "main"}); err != nil {
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

	// List shows the job.
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

	// Validation: bad limit, missing commit.
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
}

func TestDashboardOverlaysJobState(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	seedUser(t, s, "overlayuser", "ov@example.com", "pw")

	env := seedDispatchEnv(t, s, "cpu-overlay", "cpu", true)
	commit := &store.Commit{Repo: "group/code", SHA: "cafe11", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	// A pending job for (env, commit) with no run.
	if _, err := s.CreateJobs([]*store.Job{{
		CommitID: commit.ID, EnvironmentID: env.ID, Tags: "cpu", Config: "{}", TestInputRef: "main",
	}}); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "overlayuser", "pw")

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
	if len(dash.Rows) != 1 || len(dash.Rows[0].Cells) != 1 {
		t.Fatalf("unexpected matrix shape: %d rows", len(dash.Rows))
	}
	cell := dash.Rows[0].Cells[0]
	if cell == nil {
		t.Fatal("expected a job overlay cell, got null")
	}
	if cell.Status != "pending" || cell.RunID != 0 {
		t.Fatalf("overlay cell wrong: %+v", cell)
	}

	// Environment columns now include tags.
	if dash.Environments[0].Tags != "cpu" {
		t.Fatalf("environment tags missing: %+v", dash.Environments[0])
	}
}
