package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"md-builder/server/auth"
)

const testKey = `-----BEGIN OPENSSH PRIVATE KEY-----
b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAABFwAAAAdzc2gtcn
NhAAAAAwEAAQAAAQEAtc0vL5cXcWJM1XeA5lCo5XpOw8jKvpFBf4P
-----END OPENSSH PRIVATE KEY-----`

// loginAndGetCookie performs a login and returns the session cookie value.
func loginAndGetCookie(t *testing.T, mux *http.ServeMux, username, password string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login failed: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	t.Fatal("no session cookie after login")
	return ""
}

func TestEnvironmentAPIFlow(t *testing.T) {
	apiServer, s := newTestServer(t)
	seedUser(t, s, "alice", "alice@example.com", "s3cret")
	seedUser(t, s, "bob", "bob@example.com", "bobpass")

	mux := http.NewServeMux()
	apiServer.Register(mux)

	aliceCookie := loginAndGetCookie(t, mux, "alice", "s3cret")
	bobCookie := loginAndGetCookie(t, mux, "bob", "bobpass")

	authed := func(cookie, method, target, body string) *httptest.ResponseRecorder {
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

	// --- unauthenticated access is rejected ---
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/environments", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth list: expected 401, got %d", rec.Code)
	}

	// --- create (invalid: missing fields) ---
	rec = authed(aliceCookie, http.MethodPost, "/api/environments",
		`{"name":"","host":"","username":"","privateKey":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid create: expected 400, got %d, body %s", rec.Code, rec.Body.String())
	}

	// --- create (invalid: key not PEM) ---
	rec = authed(aliceCookie, http.MethodPost, "/api/environments",
		`{"name":"cpu","host":"h","username":"u","privateKey":"notapem"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-PEM key: expected 400, got %d", rec.Code)
	}

	// --- create (valid) ---
	rec = authed(aliceCookie, http.MethodPost, "/api/environments", fmt.Sprintf(
		`{"name":"cpu-node-1","host":"192.168.1.10","username":"runner","privateKey":%q,"description":"CPU pool"}`, testKey))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.Name != "cpu-node-1" {
		t.Fatalf("unexpected created name: %s", created.Name)
	}

	// --- the private key must never be echoed back ---
	if strings.Contains(rec.Body.String(), testKey) {
		t.Fatal("private key leaked in response")
	}

	// --- list (owner-scoped) ---
	rec = authed(aliceCookie, http.MethodGet, "/api/environments", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", rec.Code)
	}
	var list struct {
		Environments []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"environments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Environments) != 1 || list.Environments[0].ID != created.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	// bob sees none of alice's environments.
	rec = authed(bobCookie, http.MethodGet, "/api/environments", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode bob list: %v", err)
	}
	if len(list.Environments) != 0 {
		t.Fatalf("bob should see 0 environments, got %d", len(list.Environments))
	}

	// --- update: empty private key keeps the existing one ---
	rec = authed(aliceCookie, http.MethodPut, fmt.Sprintf("/api/environments/%d", created.ID),
		`{"name":"cpu-node-2","host":"10.0.0.9","username":"runner2","privateKey":"","description":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	// The stored key must be unchanged (update with empty key keeps the old one).
	env, err := s.GetEnvironment(1, created.ID) // alice has user ID 1 (first seed)
	if err != nil {
		t.Fatalf("get from store: %v", err)
	}
	if env.PrivateKey != testKey {
		t.Fatal("expected private key to be preserved on update")
	}
	if env.Name != "cpu-node-2" || env.Host != "10.0.0.9" {
		t.Fatalf("update not persisted: %+v", env)
	}

	// --- get by id ---
	rec = authed(aliceCookie, http.MethodGet, fmt.Sprintf("/api/environments/%d", created.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", rec.Code)
	}

	// --- foreign access is 404 ---
	rec = authed(bobCookie, http.MethodGet, fmt.Sprintf("/api/environments/%d", created.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign get: expected 404, got %d", rec.Code)
	}

	// --- connectivity test endpoint (host unreachable in test env) ---
	rec = authed(aliceCookie, http.MethodPost, fmt.Sprintf("/api/environments/%d/test", created.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var testRes struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &testRes); err != nil {
		t.Fatalf("decode test result: %v", err)
	}
	if testRes.Success {
		t.Fatal("expected connectivity test to fail against a fake host")
	}

	// --- toggle enabled ---
	rec = authed(aliceCookie, http.MethodPut, fmt.Sprintf("/api/environments/%d/enabled", created.ID),
		`{"enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var toggled struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &toggled); err != nil {
		t.Fatalf("decode toggled: %v", err)
	}
	if toggled.Enabled {
		t.Fatal("expected enabled=false after toggle")
	}

	// The state is persisted.
	env, err2 := s.GetEnvironment(1, created.ID) // alice has user ID 1 (first seed)
	if err2 != nil {
		t.Fatalf("get from store: %v", err2)
	}
	if env.Enabled {
		t.Fatal("expected disabled state persisted in store")
	}

	// Missing enabled field is rejected.
	rec = authed(aliceCookie, http.MethodPut, fmt.Sprintf("/api/environments/%d/enabled", created.ID), `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("toggle missing field: expected 400, got %d", rec.Code)
	}

	// Foreign user cannot toggle.
	rec = authed(bobCookie, http.MethodPut, fmt.Sprintf("/api/environments/%d/enabled", created.ID),
		`{"enabled":true}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign toggle: expected 404, got %d", rec.Code)
	}

	// Re-enable.
	rec = authed(aliceCookie, http.MethodPut, fmt.Sprintf("/api/environments/%d/enabled", created.ID),
		`{"enabled":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("re-enable: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &toggled); err != nil {
		t.Fatalf("decode re-enabled: %v", err)
	}
	if !toggled.Enabled {
		t.Fatal("expected enabled=true after re-enable")
	}

	// --- delete ---
	rec = authed(aliceCookie, http.MethodDelete, fmt.Sprintf("/api/environments/%d", created.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: expected 200, got %d", rec.Code)
	}
	rec = authed(aliceCookie, http.MethodGet, fmt.Sprintf("/api/environments/%d", created.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get after delete: expected 404, got %d", rec.Code)
	}
}

// TestEnvironmentInvalidKeyRejected covers sshcheck's key parsing error path.
func TestEnvironmentInvalidKeyRejected(t *testing.T) {
	// A structurally valid PEM blob that is not a real key: create succeeds
	// (stored), but the connectivity test reports a parse failure.
	apiServer, s := newTestServer(t)
	seedUser(t, s, "carol", "carol@example.com", "pw")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "carol", "pw")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/environments",
		strings.NewReader(fmt.Sprintf(`{"name":"bad","host":"h","username":"u","privateKey":%q}`, testKey)))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d, body %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/environments/%d/test", created.ID), nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: expected 200, got %d", rec.Code)
	}
	var res struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res.Success {
		t.Fatal("expected failure for an unparseable key")
	}
	if !strings.Contains(res.Message, "private key") {
		t.Fatalf("expected key-parse error message, got %q", res.Message)
	}
}

var _ = auth.HashPassword
