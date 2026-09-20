package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// setupBody is a valid POST /api/setup body.
const setupBody = `{"codeRepo":"https://gitlab.example.com/group/code",` +
	`"accessToken":"glpat-secret","username":"root",` +
	`"email":"root@example.com","password":"root-pass-123"}`

// setupState performs GET /api/setup and returns the parsed answer.
func setupState(t *testing.T, mux *http.ServeMux) bool {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/setup", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("get setup state: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Required bool `json:"required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode setup state: %v", err)
	}
	return resp.Required
}

func TestSetupState(t *testing.T) {
	apiServer, s := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	// An empty site asks for setup, and says so without a session.
	if !setupState(t, mux) {
		t.Fatal("an empty database must report required: true")
	}

	// The first account — however it was created — closes the window.
	seedUser(t, s, "alice", "alice@example.com", "s3cret")
	if setupState(t, mux) {
		t.Fatal("a site with an account must report required: false")
	}
}

func TestCompleteSetup(t *testing.T) {
	apiServer, s := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/setup", setupBody))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: expected 201, got %d, body: %s", rec.Code, rec.Body.String())
	}
	// The response is the signed-in account, and nothing else: the access
	// token that was just stored must not come back out.
	body := rec.Body.String()
	for _, secret := range []string{"glpat-secret", "root-pass-123"} {
		if strings.Contains(body, secret) {
			t.Fatalf("setup response leaks %q: %s", secret, body)
		}
	}
	var resp struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Email    string `json:"email"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode setup response: %v", err)
	}
	if resp.ID == 0 || resp.Username != "root" || resp.Email != "root@example.com" {
		t.Fatalf("unexpected setup response: %+v", resp)
	}
	// The first account of a site is an administrator. This is the one place
	// a role is not decided by the CLI.
	if resp.Role != store.RoleAdmin {
		t.Fatalf("first account role = %q, want %q", resp.Role, store.RoleAdmin)
	}

	// The account is real: the password verifies against the stored hash, and
	// the cookie the response set is a working session.
	user, err := s.GetUserByUsername("root")
	if err != nil {
		t.Fatalf("load the created account: %v", err)
	}
	if user.Role != store.RoleAdmin {
		t.Fatalf("stored role = %q, want %q", user.Role, store.RoleAdmin)
	}
	if err := auth.CheckPassword(user.PasswordHash, "root-pass-123"); err != nil {
		t.Fatalf("stored password does not verify: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected a session cookie")
	}
	meRec := httptest.NewRecorder()
	meReq := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	meReq.AddCookie(cookies[0])
	mux.ServeHTTP(meRec, meReq)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me with the setup cookie: expected 200, got %d", meRec.Code)
	}

	// The configuration carries the repository and its token, and the site
	// keeps the webhook token it generated for itself.
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.CodeRepo != "https://gitlab.example.com/group/code" {
		t.Fatalf("code repo = %q", cfg.CodeRepo)
	}
	if cfg.AccessToken != "glpat-secret" {
		t.Fatalf("access token = %q, want the one that was posted", cfg.AccessToken)
	}
	if strings.TrimSpace(cfg.WebhookToken) == "" {
		t.Fatal("expected a webhook token on the site config")
	}

	// The window is closed, and a second attempt writes nothing.
	if setupState(t, mux) {
		t.Fatal("setup must no longer be required after it ran")
	}
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/setup",
		`{"codeRepo":"https://gitlab.example.com/other/repo","username":"second",`+
			`"email":"second@example.com","password":"second-pass-123"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup: expected 409, got %d, body: %s", rec.Code, rec.Body.String())
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("accounts after a refused setup = %d, want 1", len(users))
	}
	cfg, err = s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.CodeRepo != "https://gitlab.example.com/group/code" {
		t.Fatalf("a refused setup rewrote the repository: %q", cfg.CodeRepo)
	}
}

// A site that already has an account refuses setup — and must not be usable to
// create an administrator on it.
func TestCompleteSetupAfterUserExists(t *testing.T) {
	apiServer, s := newTestServer(t)
	seedUser(t, s, "alice", "alice@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, postJSON(t, "/api/setup", setupBody))
	if rec.Code != http.StatusConflict {
		t.Fatalf("setup on a site with an account: expected 409, got %d, body: %s",
			rec.Code, rec.Body.String())
	}
	if _, err := s.GetUserByUsername("root"); err == nil {
		t.Fatal("a refused setup must not create an administrator")
	}
	// The configuration is untouched too: the refusal rolls the whole
	// transaction back.
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.CodeRepo != "" {
		t.Fatalf("a refused setup wrote the repository: %q", cfg.CodeRepo)
	}
}

func TestCompleteSetupValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing repository", `{"username":"root","email":"root@example.com","password":"root-pass-123"}`},
		{"blank repository", `{"codeRepo":"   ","username":"root","email":"root@example.com","password":"root-pass-123"}`},
		{"missing username", `{"codeRepo":"https://gitlab.example.com/g/c","email":"root@example.com","password":"root-pass-123"}`},
		{"bad email", `{"codeRepo":"https://gitlab.example.com/g/c","username":"root","email":"not-an-address","password":"root-pass-123"}`},
		{"short password", `{"codeRepo":"https://gitlab.example.com/g/c","username":"root","email":"root@example.com","password":"short"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiServer, s := newTestServer(t)
			mux := http.NewServeMux()
			apiServer.Register(mux)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, postJSON(t, "/api/setup", tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d, body: %s", rec.Code, rec.Body.String())
			}
			// Nothing was written, so the site still asks for setup.
			if !setupState(t, mux) {
				t.Fatal("a rejected setup must leave the site unconfigured")
			}
			users, err := s.ListUsers()
			if err != nil {
				t.Fatalf("list users: %v", err)
			}
			if len(users) != 0 {
				t.Fatalf("a rejected setup created %d account(s)", len(users))
			}
		})
	}
}

func TestSetupMethods(t *testing.T) {
	apiServer, _ := newTestServer(t)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/api/setup", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s /api/setup: expected 405, got %d", method, rec.Code)
		}
	}
}
