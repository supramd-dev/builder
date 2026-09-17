package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthDeepAuthRequired: the deep probe reads site configuration (the
// repo URL and access token), so it must reject unauthenticated requests.
func TestHealthDeepAuthRequired(t *testing.T) {
	apiServer, _ := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health/deep", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: expected 401, got %d", rec.Code)
	}
}

// TestHealthDeepNoRepo: without a configured code repository the git check
// reports skipped (not fail — nothing is wrong, nothing is configured) and
// the reserved object-storage row reports skipped as well.
func TestHealthDeepNoRepo(t *testing.T) {
	apiServer, s := newTestServer(t)
	seedUser(t, s, "alice", "alice@example.com", "s3cret")
	mux := http.NewServeMux()
	apiServer.Register(mux)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/deep", nil)
	req.AddCookie(loginCookie(t, mux, "alice", "s3cret"))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Checks []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
		} `json:"checks"`
		CheckedAt string `json:"checkedAt"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d: %+v", len(body.Checks), body.Checks)
	}
	byName := map[string]string{}
	for _, c := range body.Checks {
		byName[c.Name] = c.Status
	}
	if byName["git repository"] != "skipped" {
		t.Errorf("git check without repo: want skipped, got %q", byName["git repository"])
	}
	if byName["object storage"] != "skipped" {
		t.Errorf("object storage check: want skipped, got %q", byName["object storage"])
	}
	if body.CheckedAt == "" {
		t.Error("checkedAt should be set")
	}
}

// loginCookie logs in through the mux and returns the session cookie (the
// health-deep tests need an authenticated request).
func loginCookie(t *testing.T, mux *http.ServeMux, username, password string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/login", `{"username":"`+username+`","password":"`+password+`"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie after login")
	return nil
}
