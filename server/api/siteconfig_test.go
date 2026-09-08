package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiteConfigAPIFlow(t *testing.T) {
	apiServer, _ := newTestServer(t)
	seedUser(t, apiServer.Store, "alice", "alice@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "alice", "s3cret")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Unauthenticated access is rejected.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/site-config", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth get: expected 401, got %d", rec.Code)
	}

	// Initial read returns an empty default config.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg["codeRepo"] != "" || cfg["testInputRepo"] != "" || cfg["testRepoRef"] != "" {
		t.Fatalf("expected empty defaults, got %v", cfg)
	}

	// Validation: empty fields rejected.
	rec = authed(http.MethodPut, "/api/site-config", `{"codeRepo":"","testInputRepo":"","testRepoRef":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty update: expected 400, got %d", rec.Code)
	}

	// Repository hosts are NOT validated: self-hosted GitLab instances live
	// on arbitrary hosts, so any URL is accepted (including github.com).
	for _, repo := range []string{
		"https://gitlab.com/group/code",
		"https://gitlab.example.com/group/code", // self-hosted
		"git@gitlab.com:group/code.git",         // SSH
		"https://github.com/g/code",             // other platform, accepted
		"http://10.0.0.5/group/code",            // bare host, accepted
	} {
		rec = authed(http.MethodPut, "/api/site-config",
			fmt.Sprintf(`{"codeRepo":%q,"testInputRepo":"https://gitlab.com/g/in","testRepoRef":"main"}`, repo))
		if rec.Code != http.StatusOK {
			t.Fatalf("repo %q: expected 200 (no host validation), got %d, body %s", repo, rec.Code, rec.Body.String())
		}
	}

	// Valid update round-trips.
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/group/code","testInputRepo":"git@gitlab.com:group/test-inputs.git","testRepoRef":"main"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if cfg["codeRepo"] != "https://gitlab.com/group/code" ||
		cfg["testInputRepo"] != "git@gitlab.com:group/test-inputs.git" ||
		cfg["testRepoRef"] != "main" {
		t.Fatalf("update not echoed: %v", cfg)
	}

	// A commit id works as ref too.
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/group/code","testInputRepo":"https://gitlab.com/g/in","testRepoRef":"9c8b7a6"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit id update: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}

	// Re-read: persisted.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-get: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode re-get: %v", err)
	}
	if cfg["testRepoRef"] != "9c8b7a6" {
		t.Fatalf("expected persisted ref, got %v", cfg["testRepoRef"])
	}

	// Unsupported method is rejected.
	rec = authed(http.MethodDelete, "/api/site-config", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete: expected 405, got %d", rec.Code)
	}
}

func TestGitLabWebhook(t *testing.T) {
	apiServer, _ := newTestServer(t)

	mux := http.NewServeMux()
	apiServer.Register(mux)

	post := func(headers map[string]string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Non-POST is rejected.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/webhooks/gitlab", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET webhook: expected 405, got %d", rec.Code)
	}

	// Malformed JSON is rejected.
	rec = post(nil, `{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad payload: expected 400, got %d", rec.Code)
	}

	// A push event is accepted and recorded as a commit.
	push := `{
		"object_kind": "push",
		"project": {"name": "md-code", "path_with_namespace": "group/md-code", "web_url": "https://gitlab.com/group/md-code"},
		"ref": "refs/heads/main",
		"before": "0000000",
		"after": "9c8b7a6d5e4f",
		"user_name": "alice",
		"commits": [{"id": "9c8b7a6d5e4f", "message": "Fix integrator drift\n\nLonger body."}]
	}`
	rec = post(map[string]string{"X-Gitlab-Event": "Push Hook"}, push)
	if rec.Code != http.StatusOK {
		t.Fatalf("push webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode webhook result: %v", err)
	}
	if res["status"] != "received" || res["event"] != "push" {
		t.Fatalf("unexpected webhook result: %v", res)
	}
	if res["project"] != "group/md-code" || res["ref"] != "main" {
		t.Fatalf("push details not extracted: %v", res)
	}
	commitID, ok := res["commitId"].(float64)
	if !ok || commitID <= 0 {
		t.Fatalf("expected positive commitId, got %v", res["commitId"])
	}
	if res["created"] != true {
		t.Fatalf("expected created=true for a new push, got %v", res["created"])
	}

	// The commit is persisted with the head commit's title.
	c, err := apiServer.Store.GetCommitByID(int64(commitID))
	if err != nil {
		t.Fatalf("load commit: %v", err)
	}
	if c.SHA != "9c8b7a6d5e4f" || c.Repo != "group/md-code" || c.Ref != "main" || c.Author != "alice" {
		t.Fatalf("unexpected commit: %+v", c)
	}
	if c.Message != "Fix integrator drift" {
		t.Fatalf("expected commit title as message, got %q", c.Message)
	}

	// The same push again is idempotent: same commit, created=false.
	rec = post(map[string]string{"X-Gitlab-Event": "Push Hook"}, push)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat push: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode repeat result: %v", err)
	}
	if res["created"] != false {
		t.Fatalf("expected created=false for repeat push, got %v", res["created"])
	}
	if res["commitId"].(float64) != commitID {
		t.Fatalf("expected same commitId, got %v", res["commitId"])
	}

	// Other event kinds are accepted but marked ignored.
	rec = post(map[string]string{"X-Gitlab-Event": "Pipeline Hook"},
		`{"object_kind":"pipeline"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pipeline webhook: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode ignored result: %v", err)
	}
	if res["status"] != "ignored" {
		t.Fatalf("expected ignored status, got %v", res)
	}
}
