package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
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
// reports skipped (not fail — nothing is wrong, nothing is configured), while
// the object storage the test server is backed with reports ok.
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
	body := decodeHealthDeep(t, rec.Body.Bytes())
	if len(body.Checks) != 2 {
		t.Fatalf("expected 2 checks, got %d: %+v", len(body.Checks), body.Checks)
	}
	if got := body.status("git repository"); got != "skipped" {
		t.Errorf("git check without repo: want skipped, got %q", got)
	}
	if got := body.status("object storage"); got != "ok" {
		t.Errorf("object storage check: want ok, got %q", got)
	}
	if body.CheckedAt == "" {
		t.Error("checkedAt should be set")
	}
}

// TestHealthDeepObjectStorage: the object-storage row reports what the probe
// found — the backend it reached, or the failure it hit. There is no
// "skipped" state in a running server: the backend is mandatory, so the row
// only reads skipped when the store was opened without one.
func TestHealthDeepObjectStorage(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		apiServer, s := newTestServer(t)
		seedUser(t, s, "alice", "alice@example.com", "s3cret")
		mux := http.NewServeMux()
		apiServer.Register(mux)

		body := healthDeep(t, mux)
		check := body.check(t, "object storage")
		if check.Status != "ok" {
			t.Fatalf("status = %q, detail %q", check.Status, check.Detail)
		}
		if check.Target != "memory" {
			t.Errorf("target = %q, want the backend description", check.Target)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		objs := storage.NewMemory()
		objs.PingErr = errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
		apiServer, s := newTestServerWithObjects(t, objs)
		seedUser(t, s, "alice", "alice@example.com", "s3cret")
		mux := http.NewServeMux()
		apiServer.Register(mux)

		body := healthDeep(t, mux)
		check := body.check(t, "object storage")
		if check.Status != "fail" {
			t.Fatalf("status = %q, want fail", check.Status)
		}
		if !strings.Contains(check.Detail, "connection refused") {
			t.Errorf("detail = %q, want the backend's error", check.Detail)
		}
	})

	t.Run("no backend", func(t *testing.T) {
		s, err := store.Open("file::memory:?cache=shared")
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		t.Cleanup(func() { _ = s.Close() })
		apiServer := New(s)
		seedUser(t, s, "alice", "alice@example.com", "s3cret")
		mux := http.NewServeMux()
		apiServer.Register(mux)

		body := healthDeep(t, mux)
		check := body.check(t, "object storage")
		if check.Status != "skipped" {
			t.Fatalf("status = %q, want skipped", check.Status)
		}
	})
}

// healthDeep runs an authenticated deep probe against mux.
func healthDeep(t *testing.T, mux *http.ServeMux) healthDeepBody {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/health/deep", nil)
	req.AddCookie(loginCookie(t, mux, "alice", "s3cret"))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	return decodeHealthDeep(t, rec.Body.Bytes())
}

// healthCheckBody is one decoded probe row.
type healthCheckBody struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Target string `json:"target"`
	Detail string `json:"detail"`
}

// healthDeepBody is the decoded /api/health/deep response.
type healthDeepBody struct {
	Checks    []healthCheckBody `json:"checks"`
	CheckedAt string            `json:"checkedAt"`
}

func decodeHealthDeep(t *testing.T, raw []byte) healthDeepBody {
	t.Helper()
	var body healthDeepBody
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// status returns one probe's status, or "" when it is absent (for tests that
// only assert a single row).
func (b healthDeepBody) status(name string) string {
	for _, c := range b.Checks {
		if c.Name == name {
			return c.Status
		}
	}
	return ""
}

// check returns one probe's full row, failing the test when it is absent.
func (b healthDeepBody) check(t *testing.T, name string) healthCheckBody {
	t.Helper()
	for _, c := range b.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no %q check in %+v", name, b.Checks)
	return healthCheckBody{}
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
