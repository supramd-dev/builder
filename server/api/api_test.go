package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/auth"
	"md-builder/server/storage"
	"md-builder/server/store"
)

// newTestServer opens an in-memory SQLite store and mounts the API on a test
// mux.
func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newTestServerWithObjects(t, storage.NewMemory())
}

// newTestServerWithObjects uses a specific artifact backend, for tests that
// inspect or break the object store.
func newTestServerWithObjects(t *testing.T, objs storage.Store) (*Server, *store.Store) {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(objs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	apiServer := New(s)
	return apiServer, s
}

// seedUser creates a user with a bcrypt-hashed password directly in the store.
func seedUser(t *testing.T, s *store.Store, username, email, password string) *store.User {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	u := &store.User{Username: username, Email: email, PasswordHash: hash}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

func TestHealth(t *testing.T) {
	apiServer, _ := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func TestLoginLogoutMe(t *testing.T) {
	apiServer, s := newTestServer(t)
	seedUser(t, s, "alice", "alice@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)

	// --- login with wrong password ---
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/login", `{"username":"alice","password":"wrong"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password: expected 401, got %d", rec.Code)
	}

	// --- login with correct credentials ---
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/login", `{"username":"alice","password":"s3cret"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("login: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode login response: %v", err)
	}
	if resp["username"] != "alice" || resp["email"] != "alice@example.com" {
		t.Fatalf("unexpected login response: %v", resp)
	}

	// Extract the session cookie.
	cookies := rec.Result().Cookies()
	var sessionToken string
	for _, c := range cookies {
		if c.Name == sessionCookie {
			sessionToken = c.Value
		}
	}
	if sessionToken == "" {
		t.Fatal("expected md_session cookie to be set")
	}

	// --- /api/me with the cookie ---
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("me: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// --- /api/me without the cookie ---
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me without cookie: expected 401, got %d", rec.Code)
	}

	// --- logout ---
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: expected 200, got %d", rec.Code)
	}

	// After logout the session token is invalid.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout: expected 401, got %d", rec.Code)
	}
}

func TestLoginValidation(t *testing.T) {
	apiServer, _ := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	// Missing fields.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/login", `{"username":"","password":""}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing fields: expected 400, got %d", rec.Code)
	}

	// Unknown user.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/login", `{"username":"ghost","password":"pw"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user: expected 401, got %d", rec.Code)
	}

	// Malformed JSON.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader("not json"))
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: expected 400, got %d", rec.Code)
	}

	// Wrong method.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/login", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET login: expected 405, got %d", rec.Code)
	}
}

// TestExpiredSession verifies that expired sessions are rejected.
func TestExpiredSession(t *testing.T) {
	apiServer, s := newTestServer(t)
	u := seedUser(t, s, "bob", "bob@example.com", "pw")

	sess := &store.Session{
		Token:     "expired-token",
		UserID:    u.ID,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	mux := http.NewServeMux()
	apiServer.Register(mux)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "expired-token"})
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: expected 401, got %d", rec.Code)
	}
}

// postJSON builds a POST request with a JSON body.
func postJSON(t *testing.T, target, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}
