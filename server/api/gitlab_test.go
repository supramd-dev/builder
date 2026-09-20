package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// testPublicURL is the site address the fixture configures. The GitLab
// redirect URI is built from it, so the tests assert on absolute URLs.
const testPublicURL = "https://md.example.com"

// fakeGitLab stands in for the two GitLab endpoints the sign-in integration
// calls, so the tests never touch the network. It is deliberately strict: a
// token request that does not carry the configured credentials, or a user
// request with the wrong bearer token, fails the exchange rather than
// silently succeeding.
type fakeGitLab struct {
	server *httptest.Server
	// user is what GET /api/v4/user answers with.
	user gitlabUser
	// code is the only authorization code POST /oauth/token accepts.
	code string
	// issued is the access token the token endpoint hands out.
	issued string

	// tokenForms records the body of every token request, so a test can
	// check what was sent.
	tokenForms []url.Values
	// bearerSeen records the Authorization header of every user request.
	bearerSeen []string
	// userCalls counts the user lookups, for the paths that must not reach
	// GitLab at all.
	userCalls int
}

func newFakeGitLab(t *testing.T, user gitlabUser) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{user: user, code: "test-code", issued: "test-access-token"}
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		f.tokenForms = append(f.tokenForms, form)
		if form.Get("code") != f.code {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"` + f.issued + `","token_type":"bearer"}`))
	})
	mux.HandleFunc("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		f.userCalls++
		f.bearerSeen = append(f.bearerSeen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer "+f.issued {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(f.user)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// gitLabFixture is a test server with the GitLab sign-in integration wired to
// a fake GitLab instance, plus an administrator to approve with.
type gitLabFixture struct {
	apiServer *Server
	store     *store.Store
	mux       *http.ServeMux
	gitlab    *fakeGitLab
	admin     *store.User
}

// login returns a session cookie for a seeded account.
func (f *gitLabFixture) login(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	return loginCookie(t, f.mux, username, password)
}

// newGitLabFixture builds the fixture with the integration fully configured
// and switched on. Tests that need a broken configuration use configure.
func newGitLabFixture(t *testing.T, gu gitlabUser) *gitLabFixture {
	t.Helper()
	apiServer, s := newTestServer(t)
	f := &gitLabFixture{
		apiServer: apiServer,
		store:     s,
		mux:       http.NewServeMux(),
		gitlab:    newFakeGitLab(t, gu),
		admin:     seedUserWithRole(t, s, "root", "root@example.com", "root-pass", store.RoleAdmin),
	}
	apiServer.Register(f.mux)
	apiServer.SetPublicURL(testPublicURL)
	f.configure(t, func(cfg *store.SiteConfig) {
		cfg.GitLabURL = f.gitlab.server.URL
		cfg.GitLabClientID = "app-id"
		cfg.GitLabClientSecret = "app-secret"
		cfg.GitLabLoginEnabled = true
	})
	return f
}

// configure loads the singleton site config, applies mutate and saves it.
func (f *gitLabFixture) configure(t *testing.T, mutate func(*store.SiteConfig)) {
	t.Helper()
	cfg, err := f.store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	mutate(cfg)
	if err := f.store.SaveSiteConfig(cfg); err != nil {
		t.Fatalf("save site config: %v", err)
	}
}

// callback drives the second leg of the flow with a matching state cookie.
func (f *gitLabFixture) callback(t *testing.T, query string) *httptest.ResponseRecorder {
	t.Helper()
	return f.callbackWithState(t, "state-value", query)
}

// callbackWithState sends the callback with an explicit cookie value, so the
// mismatch cases can be exercised.
func (f *gitLabFixture) callbackWithState(t *testing.T, cookieValue, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/callback?"+query, nil)
	if cookieValue != "" {
		req.AddCookie(&http.Cookie{Name: gitlabStateCookie, Value: cookieValue})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

// loginStatus reads the ?gitlab= word out of the redirect the callback ends
// with. Every *refusal* sends the browser back to the login page carrying the
// reason, so this is how a test learns what happened; a successful sign-in
// goes to the dashboard instead and is asserted on directly.
func loginStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("expected a 302 back to the login page, got %d, body: %s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, testPublicURL+"/#/?gitlab=") {
		t.Fatalf("expected a redirect to the login page, got %q", loc)
	}
	return strings.TrimPrefix(loc, testPublicURL+"/#/?gitlab=")
}

// sessionCookieIn returns the session cookie a response set, or nil.
func sessionCookieIn(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.Value != "" {
			return c
		}
	}
	return nil
}

// TestGitLabRegistrationIsPending is the core of the feature: a first GitLab
// sign-in registers an ordinary, unapproved account and does not sign it in.
func TestGitLabRegistrationIsPending(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	rec := f.callback(t, "state=state-value&code=test-code")
	if status := loginStatus(t, rec); status != "pending" {
		t.Fatalf("expected status pending, got %q", status)
	}
	if c := sessionCookieIn(rec); c != nil {
		t.Fatal("a pending registration must not open a session")
	}

	// The account exists, is an ordinary user, came from GitLab, and carries
	// no password material.
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("expected the account to be registered: %v", err)
	}
	if u.Username != "guser" || u.Email != "guser@example.com" {
		t.Fatalf("unexpected account: %+v", u)
	}
	if u.Role != store.RoleUser {
		t.Fatalf("a GitLab registration must never be an administrator, got %q", u.Role)
	}
	if u.Source != store.SourceGitLab {
		t.Fatalf("expected source %q, got %q", store.SourceGitLab, u.Source)
	}
	if u.Approved {
		t.Fatal("a GitLab registration must start unapproved")
	}
	if u.PasswordHash != "" {
		t.Fatalf("a GitLab account must carry no password hash, got %q", u.PasswordHash)
	}

	// The password login path stays shut for it: the account carries no
	// hash, so no password can match.
	rec = doJSON(t, f.mux, http.MethodPost, "/api/login",
		`{"username":"guser","password":"anything"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("password login on a GitLab account: expected 401, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// The credentials really were sent to GitLab, and the secret went in the
	// token request body rather than the URL.
	if len(f.gitlab.tokenForms) != 1 {
		t.Fatalf("expected one token exchange, got %d", len(f.gitlab.tokenForms))
	}
	form := f.gitlab.tokenForms[0]
	if form.Get("client_id") != "app-id" || form.Get("client_secret") != "app-secret" {
		t.Fatalf("the token request must carry the configured credentials, got %v", form)
	}
	if form.Get("redirect_uri") != testPublicURL+"/api/auth/gitlab/callback" {
		t.Fatalf("unexpected redirect_uri in the token request: %q", form.Get("redirect_uri"))
	}
	if len(f.gitlab.bearerSeen) != 1 || f.gitlab.bearerSeen[0] != "Bearer test-access-token" {
		t.Fatalf("unexpected bearer tokens: %v", f.gitlab.bearerSeen)
	}
}

// TestGitLabApprovalUnlocksSignIn walks the whole point of the approval step:
// the same GitLab account is refused while pending and admitted afterwards.
func TestGitLabApprovalUnlocksSignIn(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("first sign-in: expected pending, got %q", status)
	}
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get registered account: %v", err)
	}

	// A second sign-in while still pending must not create a duplicate
	// account, and must stay refused.
	rec := f.callback(t, "state=state-value&code=test-code")
	if status := loginStatus(t, rec); status != "pending" {
		t.Fatalf("second sign-in while pending: expected pending, got %q", status)
	}
	if c := sessionCookieIn(rec); c != nil {
		t.Fatal("a pending account must not get a session")
	}
	if all, err := f.store.ListUsers(); err != nil {
		t.Fatalf("list users: %v", err)
	} else if len(all) != 2 { // the administrator and the one GitLab account
		t.Fatalf("expected exactly 2 accounts, got %d", len(all))
	}

	// The administrator approves it.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(u.ID, 10),
		`{"username":"guser","email":"guser@example.com","approved":true}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var approved accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &approved); err != nil {
		t.Fatalf("decode approval response: %v", err)
	}
	if !approved.Approved || approved.Source != store.SourceGitLab || approved.GitLabID != 42 {
		t.Fatalf("unexpected account after approval: %+v", approved)
	}

	// Now the GitLab sign-in goes through and opens a session. Success is not
	// a loginStatus case: it lands on the dashboard, with no status word.
	rec = f.callback(t, "state=state-value&code=test-code")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != testPublicURL+"/#/" {
		t.Fatalf("sign-in after approval: expected the dashboard, got %d %q", rec.Code, loc)
	}
	c := sessionCookieIn(rec)
	if c == nil {
		t.Fatal("an approved account must get a session")
	}
	// The session belongs to the GitLab account.
	rec = doJSON(t, f.mux, http.MethodGet, "/api/me", "", c)
	if rec.Code != http.StatusOK {
		t.Fatalf("me with the GitLab session: expected 200, got %d", rec.Code)
	}
	var me accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.Username != "guser" || me.Role != store.RoleUser {
		t.Fatalf("unexpected signed-in account: %+v", me)
	}
}

// TestGitLabRegistrationRefusesExistingEmail covers the linking decision: an
// address that already belongs to an account here is never taken over.
func TestGitLabRegistrationRefusesExistingEmail(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "alice@example.com"})
	local := seedUser(t, f.store, "alice", "alice@example.com", "alice-pass")

	rec := f.callback(t, "state=state-value&code=test-code")
	if status := loginStatus(t, rec); status != "email_taken" {
		t.Fatalf("expected status email_taken, got %q", status)
	}
	if c := sessionCookieIn(rec); c != nil {
		t.Fatal("a refused sign-in must not open a session")
	}

	// Nothing was created, and the local account is untouched — in
	// particular it did not acquire the GitLab id.
	if _, err := f.store.GetUserByGitLabID(42); err == nil {
		t.Fatal("no account may be registered when the email is taken")
	}
	got, err := f.store.GetUserByID(local.ID)
	if err != nil {
		t.Fatalf("get local account: %v", err)
	}
	if got.GitLabID != nil || got.Source != store.SourceLocal {
		t.Fatalf("the local account must not be linked: %+v", got)
	}
	// Its password still works.
	if rec := doJSON(t, f.mux, http.MethodPost, "/api/login",
		`{"username":"alice","password":"alice-pass"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("the local account must still sign in: got %d", rec.Code)
	}
}

// TestGitLabRegistrationNeedsEmail: without an address there is nothing to
// build an account on, and the request never reaches the account code.
func TestGitLabRegistrationNeedsEmail(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser"})

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "no_email" {
		t.Fatalf("expected status no_email, got %q", status)
	}
	if all, err := f.store.ListUsers(); err != nil {
		t.Fatalf("list users: %v", err)
	} else if len(all) != 1 {
		t.Fatalf("no account may be created without an email, got %d", len(all))
	}
}

// TestGitLabRegistrationAvoidsUsernameCollision: two GitLab accounts may
// share a username, and a GitLab name may collide with a local one. The
// account's identity is its GitLab id, so the name gets a suffix.
func TestGitLabRegistrationAvoidsUsernameCollision(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "alice", Email: "gitlab-alice@example.com"})
	seedUser(t, f.store, "alice", "alice@example.com", "alice-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected status pending, got %q", status)
	}
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("expected the account to be registered: %v", err)
	}
	if u.Username != "alice-2" {
		t.Fatalf("expected the colliding name to be suffixed, got %q", u.Username)
	}
}

// TestGitLabCallbackStateIsChecked: the state cookie is the CSRF defence, and
// a callback that does not match it must not reach GitLab at all.
func TestGitLabCallbackStateIsChecked(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	cases := []struct {
		name        string
		cookieValue string
		query       string
	}{
		{"mismatched state", "other-state", "state=state-value&code=test-code"},
		{"missing state in the query", "state-value", "code=test-code"},
		{"missing state cookie", "", "state=state-value&code=test-code"},
		{"replayed callback with no cookie", "", "code=test-code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.callbackWithState(t, tc.cookieValue, tc.query)
			if status := loginStatus(t, rec); status != "error" {
				t.Fatalf("expected status error, got %q", status)
			}
			if c := sessionCookieIn(rec); c != nil {
				t.Fatal("a rejected callback must not open a session")
			}
		})
	}
	if f.gitlab.userCalls != 0 {
		t.Fatalf("a rejected callback must not reach GitLab, got %d user calls", f.gitlab.userCalls)
	}
	if all, err := f.store.ListUsers(); err != nil {
		t.Fatalf("list users: %v", err)
	} else if len(all) != 1 {
		t.Fatalf("a rejected callback must not create an account, got %d", len(all))
	}

	// The state cookie is cleared on the way through, whatever the outcome —
	// including this one, which was rejected before it reached GitLab. The
	// check is on the Set-Cookie the handler emitted, not on the absence of a
	// cookie in the response: a handler that never touched the cookie would
	// satisfy the weaker form.
	rec := f.callbackWithState(t, "state-value", "state=state-value&code=test-code")
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == gitlabStateCookie && c.Value == "" {
			cleared = true
		}
	}
	if !cleared {
		t.Fatal("the callback must emit a clearing Set-Cookie for the state cookie")
	}
}

// TestGitLabCallbackDenied: a user who declines on GitLab comes back with an
// error instead of a code, which is a decision, not a failure.
func TestGitLabCallbackDenied(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	rec := f.callback(t, "state=state-value&error=access_denied")
	if status := loginStatus(t, rec); status != "denied" {
		t.Fatalf("expected status denied, got %q", status)
	}
	if f.gitlab.userCalls != 0 {
		t.Fatalf("a denied authorization must not reach GitLab, got %d user calls", f.gitlab.userCalls)
	}
}

// TestGitLabDisabledAccountRefused: an account disabled after registration is
// kept out of the GitLab path too, with the same reason as a password login.
func TestGitLabDisabledAccountRefused(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(u.ID, 10),
		`{"username":"guser","email":"guser@example.com","approved":true,"disabled":true}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}

	rec = f.callback(t, "state=state-value&code=test-code")
	if status := loginStatus(t, rec); status != "disabled" {
		t.Fatalf("expected status disabled, got %q", status)
	}
	if c := sessionCookieIn(rec); c != nil {
		t.Fatal("a disabled account must not get a session")
	}
}

// TestGitLabStartRedirectsToGitLab checks the first leg: the state cookie and
// the authorization URL handed to the browser.
func TestGitLabStartRedirectsToGitLab(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/start", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("start: expected 302, got %d, body: %s", rec.Code, rec.Body.String())
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse location: %v", err)
	}
	if loc.Scheme+"://"+loc.Host+loc.Path != f.gitlab.server.URL+"/oauth/authorize" {
		t.Fatalf("unexpected authorization URL: %s", loc)
	}
	q := loc.Query()
	if q.Get("client_id") != "app-id" || q.Get("response_type") != "code" || q.Get("scope") != gitlabScope {
		t.Fatalf("unexpected authorization parameters: %v", q)
	}
	if q.Get("redirect_uri") != testPublicURL+"/api/auth/gitlab/callback" {
		t.Fatalf("unexpected redirect_uri: %q", q.Get("redirect_uri"))
	}

	// The state travels in an HttpOnly cookie and in the URL, and the two
	// match — that is what the callback checks.
	var stateCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == gitlabStateCookie {
			stateCookie = c
		}
	}
	if stateCookie == nil || stateCookie.Value == "" {
		t.Fatal("start must set the state cookie")
	}
	if !stateCookie.HttpOnly {
		t.Fatal("the state cookie must be HttpOnly")
	}
	if q.Get("state") != stateCookie.Value {
		t.Fatalf("the state in the URL must match the cookie: %q vs %q", q.Get("state"), stateCookie.Value)
	}
	// Nothing about the client secret may reach the browser.
	if strings.Contains(rec.Header().Get("Location"), "app-secret") {
		t.Fatal("the client secret must never appear in a redirect URL")
	}
}

// TestGitLabEnabledEndpoint covers the unauthenticated probe the login page
// uses, across the states the configuration can be in.
func TestGitLabEnabledEndpoint(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	enabled := func() bool {
		t.Helper()
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/enabled", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("enabled: expected 200, got %d", rec.Code)
		}
		var resp struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Enabled
	}

	if !enabled() {
		t.Fatal("a fully configured integration must report enabled")
	}
	// Switched off.
	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabLoginEnabled = false })
	if enabled() {
		t.Fatal("a switched-off integration must report disabled")
	}
	// On, but incomplete.
	f.configure(t, func(cfg *store.SiteConfig) {
		cfg.GitLabLoginEnabled = true
		cfg.GitLabClientSecret = ""
	})
	if enabled() {
		t.Fatal("an integration with no client secret must report disabled")
	}
	// On and complete, but the server has no public URL to build the
	// callback from.
	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabClientSecret = "app-secret" })
	f.apiServer.SetPublicURL("")
	if enabled() {
		t.Fatal("an integration with no public URL must report disabled")
	}
}

// TestGitLabStartRefusedWhenUnavailable: reaching /start while the
// integration cannot run sends the user back to the login page rather than
// into a half-broken flow.
func TestGitLabStartRefusedWhenUnavailable(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabLoginEnabled = false })

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/start", nil))
	if status := loginStatus(t, rec); status != "unavailable" {
		t.Fatalf("expected status unavailable, got %q", status)
	}

	// With no public URL there is nowhere to redirect to, so the answer is a
	// plain error instead of a broken Location header.
	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabLoginEnabled = true })
	f.apiServer.SetPublicURL("")
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/start", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "app-secret") {
		t.Fatal("the refusal must not leak the client secret")
	}
}

// TestGitLabRoutesRejectOtherMethods: the three routes are GETs.
func TestGitLabRoutesRejectOtherMethods(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	for _, path := range []string{
		"/api/auth/gitlab/enabled",
		"/api/auth/gitlab/start",
		"/api/auth/gitlab/callback",
	} {
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: expected 405, got %d", path, rec.Code)
		}
	}
}

// TestGitLabConfigIsAdminOnly: the tab that holds an OAuth client secret is
// closed to regular users, both on the shared site-config endpoint and in
// what the read returns.
func TestGitLabConfigIsAdminOnly(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	seedUser(t, f.store, "alice", "alice@example.com", "alice-pass")
	userCookie := f.login(t, "alice", "alice-pass")

	// The secret never comes back, to anyone.
	for _, cookie := range []*http.Cookie{userCookie, f.login(t, "root", "root-pass")} {
		rec := doJSON(t, f.mux, http.MethodGet, "/api/site-config", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("get: expected 200, got %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), "app-secret") {
			t.Fatalf("the client secret must never be returned: %s", rec.Body.String())
		}
	}

	// A regular user sees the booleans (the login page needs them) but not
	// the credentials.
	rec := doJSON(t, f.mux, http.MethodGet, "/api/site-config", "", userCookie)
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg["gitlabLoginEnabled"] != true || cfg["gitlabClientSecretSet"] != true {
		t.Fatalf("expected the booleans to be visible, got %v", cfg)
	}
	if cfg["gitlabUrl"] != "" || cfg["gitlabClientId"] != "" || cfg["gitlabRedirectUri"] != "" {
		t.Fatalf("a regular user must not see the GitLab credentials: %v", cfg)
	}

	// An administrator sees them, and the callback URL comes from the
	// configured public URL rather than from the request.
	rec = doJSON(t, f.mux, http.MethodGet, "/api/site-config", "", f.login(t, "root", "root-pass"))
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode admin config: %v", err)
	}
	if cfg["gitlabClientId"] != "app-id" || cfg["gitlabUrl"] != f.gitlab.server.URL {
		t.Fatalf("unexpected admin config: %v", cfg)
	}
	if cfg["gitlabRedirectUri"] != testPublicURL+"/api/auth/gitlab/callback" {
		t.Fatalf("unexpected redirect URI: %v", cfg["gitlabRedirectUri"])
	}

	// A regular user may not change any part of the integration.
	for _, body := range []string{
		`{"codeRepo":"https://gitlab.com/g/c","gitlabLoginEnabled":false}`,
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"https://evil.example.com"}`,
		`{"codeRepo":"https://gitlab.com/g/c","gitlabClientSecret":"attacker"}`,
		`{"codeRepo":"https://gitlab.com/g/c","clearGitlabClientSecret":true}`,
	} {
		rec := doJSON(t, f.mux, http.MethodPut, "/api/site-config", body, userCookie)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for %s, got %d, body: %s", body, rec.Code, rec.Body.String())
		}
	}

	// The stored configuration survived all of that.
	stored, err := f.store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get stored config: %v", err)
	}
	if !stored.GitLabLoginEnabled || stored.GitLabURL != f.gitlab.server.URL ||
		stored.GitLabClientID != "app-id" || stored.GitLabClientSecret != "app-secret" {
		t.Fatalf("the stored configuration must be unchanged: %+v", stored)
	}

	// The rest of the endpoint stays open: a regular user may still set the
	// repository, as before.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/group/code"}`, userCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("a regular user must still be able to set the repository: %d, body: %s", rec.Code, rec.Body.String())
	}
}

// TestGitLabEnableRequiresCompleteConfig: switching the integration on with a
// piece missing would advertise a login button that cannot work.
func TestGitLabEnableRequiresCompleteConfig(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")
	f.configure(t, func(cfg *store.SiteConfig) {
		cfg.GitLabLoginEnabled = false
		cfg.GitLabURL = ""
		cfg.GitLabClientID = ""
		cfg.GitLabClientSecret = ""
	})

	// Missing everything.
	rec := doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabLoginEnabled":true}`, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enabling with nothing configured: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	// The address and the id, but no secret.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"https://gitlab.com","gitlabClientId":"app-id","gitlabLoginEnabled":true}`,
		adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enabling without a secret: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSiteConfig(); err != nil || stored.GitLabLoginEnabled {
		t.Fatalf("the refused update must not be stored (err %v, enabled %v)", err, stored.GitLabLoginEnabled)
	}

	// With the secret in the same request it goes through, and the secret is
	// stored write-only.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"https://gitlab.com","gitlabClientId":"app-id","gitlabClientSecret":"a-secret","gitlabLoginEnabled":true}`,
		adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("enabling with a complete configuration: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := f.store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get stored config: %v", err)
	}
	if !stored.GitLabLoginEnabled || stored.GitLabClientSecret != "a-secret" {
		t.Fatalf("unexpected stored config: %+v", stored)
	}
	if strings.Contains(rec.Body.String(), "a-secret") {
		t.Fatalf("the response must not echo the secret: %s", rec.Body.String())
	}

	// A later update that leaves the secret blank keeps it; the clear flag
	// removes it.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"https://gitlab.com","gitlabClientId":"app-id","gitlabLoginEnabled":true}`,
		adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("blank secret update: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSiteConfig(); err != nil || stored.GitLabClientSecret != "a-secret" {
		t.Fatalf("a blank secret must keep the stored one (err %v)", err)
	}

	// Clearing it while the integration is on is refused: the site would be
	// left advertising a button that cannot work.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"https://gitlab.com","gitlabClientId":"app-id","clearGitlabClientSecret":true}`,
		adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearing the secret while enabled: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSiteConfig(); err != nil || stored.GitLabClientSecret != "a-secret" {
		t.Fatalf("the refused clear must not be stored (err %v)", err)
	}
}

// TestUserApprovalPermissions: approving is an administrator action with the
// same shape as disabling.
func TestUserApprovalPermissions(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}
	pending, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	path := "/api/users/" + strconv.FormatInt(pending.ID, 10)

	// A regular user cannot approve anyone — not even themselves. The
	// pending account has no password, so it cannot sign in to try; a local
	// regular user stands in for "a non-administrator".
	seedUser(t, f.store, "bob", "bob@example.com", "bob-pass")
	bobCookie := f.login(t, "bob", "bob-pass")
	rec := doJSON(t, f.mux, http.MethodPut, path,
		`{"username":"guser","email":"guser@example.com","approved":true}`, bobCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a regular user approving: expected 403, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, err := f.store.GetUserByID(pending.ID); err != nil || got.Approved {
		t.Fatalf("the account must stay unapproved (err %v)", err)
	}

	// An administrator cannot approve themselves, and there is nothing to
	// approve on another administrator.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.admin.ID, 10),
		`{"username":"root","email":"root@example.com","approved":true}`, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-approval: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	second := seedUserWithRole(t, f.store, "ops", "ops@example.com", "ops-pass", store.RoleAdmin)
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(second.ID, 10),
		`{"username":"ops","email":"ops@example.com","approved":false}`, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("un-approving an administrator: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, err := f.store.GetUserByID(second.ID); err != nil || !got.Approved {
		t.Fatalf("an administrator must stay approved (err %v)", err)
	}

	// An ordinary edit that says nothing about approval leaves it alone.
	rec = doJSON(t, f.mux, http.MethodPut, path,
		`{"username":"guser","email":"guser@example.com"}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("unrelated edit: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if got, err := f.store.GetUserByID(pending.ID); err != nil || got.Approved {
		t.Fatalf("an edit must not approve by omission (err %v)", err)
	}
}

// TestUsersListReportsSourceAndApproval: the account list is how an
// administrator sees that an account is self-registered and waiting.
func TestUsersListReportsSourceAndApproval(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}

	rec := doJSON(t, f.mux, http.MethodGet, "/api/users", "", f.login(t, "root", "root-pass"))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []accountJSON `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(resp.Users))
	}
	// Ordered by id: the administrator first, the GitLab registration second.
	if got := resp.Users[0]; got.Source != store.SourceLocal || !got.Approved || got.GitLabID != 0 {
		t.Fatalf("unexpected administrator row: %+v", got)
	}
	if got := resp.Users[1]; got.Source != store.SourceGitLab || got.Approved || got.GitLabID != 42 {
		t.Fatalf("unexpected GitLab row: %+v", got)
	}
}

// TestPendingAccountCannotUsePasswordLogin: the shared refusal is what keeps
// an unapproved account out of the password path as well, so the two entry
// points cannot drift apart.
func TestPendingAccountCannotUsePasswordLogin(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}
	pending, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}

	// Give the account a password directly, so the only thing standing
	// between it and a login is the approval flag. UpdateUser writes the
	// flags as given, so the current approval state is passed back
	// explicitly rather than left to the zero value.
	hash, err := auth.HashPassword("guser-pass")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if err := f.store.UpdateUser(pending.ID, store.UserUpdate{
		Username: "guser", Email: "guser@example.com", PasswordHash: hash,
		Approved: pending.Approved, Disabled: pending.Disabled,
	}); err != nil {
		t.Fatalf("set password: %v", err)
	}

	rec := doJSON(t, f.mux, http.MethodPost, "/api/login",
		`{"username":"guser","password":"guser-pass"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("an unapproved account signing in with a password: expected 403, got %d, body: %s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "approval") {
		t.Fatalf("expected the approval reason in the body, got %s", rec.Body.String())
	}
}

// TestUnapprovingEndsSessions: withdrawing an approval must take effect when
// the decision is made, not when the account's session happens to expire.
// Otherwise an administrator who un-approves an account still leaves it with
// full access for up to the session lifetime.
func TestUnapprovingEndsSessions(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	path := "/api/users/" + strconv.FormatInt(u.ID, 10)
	approve := func(body string) {
		t.Helper()
		if rec := doJSON(t, f.mux, http.MethodPut, path, body, adminCookie); rec.Code != http.StatusOK {
			t.Fatalf("update %s: expected 200, got %d, body: %s", body, rec.Code, rec.Body.String())
		}
	}
	approve(`{"username":"guser","email":"guser@example.com","approved":true}`)

	// The approved account signs in and holds a live session. A successful
	// sign-in lands on the dashboard, so this is not a loginStatus case.
	rec := f.callback(t, "state=state-value&code=test-code")
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusFound || loc != testPublicURL+"/#/" {
		t.Fatalf("expected the dashboard, got %d %q", rec.Code, loc)
	}
	session := sessionCookieIn(rec)
	if session == nil {
		t.Fatal("an approved account must get a session")
	}
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", session); rec.Code != http.StatusOK {
		t.Fatalf("the live session must work: got %d", rec.Code)
	}

	// Un-approving it ends that session at once, and the account cannot
	// start a new one either.
	approve(`{"username":"guser","email":"guser@example.com","approved":false}`)
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", session); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the session must be refused once the account is unapproved: got %d", rec.Code)
	}
	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending after un-approving, got %q", status)
	}
}

// TestGitLabSuccessLandsOnDashboard: a signed-in user is sent to the
// dashboard, not back to the login page with a status word.
func TestGitLabSuccessLandsOnDashboard(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	if status := loginStatus(t, f.callback(t, "state=state-value&code=test-code")); status != "pending" {
		t.Fatalf("expected pending, got %q", status)
	}
	u, err := f.store.GetUserByGitLabID(42)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(u.ID, 10),
		`{"username":"guser","email":"guser@example.com","approved":true}`, adminCookie); rec.Code != http.StatusOK {
		t.Fatalf("approve: got %d, body: %s", rec.Code, rec.Body.String())
	}

	rec := f.callback(t, "state=state-value&code=test-code")
	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != testPublicURL+"/#/" {
		t.Fatalf("expected the dashboard, got %q", loc)
	}
	if sessionCookieIn(rec) == nil {
		t.Fatal("expected a session cookie")
	}
}

// TestGitLabStartMintsFreshState: each flow gets its own state value, so one
// sign-in's state cannot be replayed into another.
func TestGitLabStartMintsFreshState(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	states := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/start", nil))
		loc, err := url.Parse(rec.Header().Get("Location"))
		if err != nil {
			t.Fatalf("parse location: %v", err)
		}
		states = append(states, loc.Query().Get("state"))
	}
	if states[0] == "" || states[1] == "" {
		t.Fatalf("expected a state on both flows, got %q", states)
	}
	if states[0] == states[1] {
		t.Fatalf("two flows must not share a state value: %q", states[0])
	}
}

// TestGitLabCallbackWhenDisabledMidFlow: an administrator can switch the
// integration off while a sign-in is in flight; the callback must not carry
// it through on the strength of the state alone.
func TestGitLabCallbackWhenDisabledMidFlow(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/start", nil))
	state := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == gitlabStateCookie {
			state = c.Value
		}
	}
	if state == "" {
		t.Fatal("start must set the state cookie")
	}

	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabLoginEnabled = false })

	rec = f.callbackWithState(t, state, "state="+url.QueryEscape(state)+"&code=test-code")
	if status := loginStatus(t, rec); status != "unavailable" {
		t.Fatalf("expected unavailable, got %q", status)
	}
	if f.gitlab.userCalls != 0 {
		t.Fatalf("a disabled integration must not reach GitLab, got %d user calls", f.gitlab.userCalls)
	}
	if all, err := f.store.ListUsers(); err != nil {
		t.Fatalf("list users: %v", err)
	} else if len(all) != 1 {
		t.Fatalf("no account may be created, got %d", len(all))
	}
}

// TestSiteConfigPartialUpdateKeepsGitLabCredentials: a request that only
// flips the switch says nothing about the address, the id or the secret, so
// it must leave all three alone. Treating an absent field as "clear it" would
// make switching the integration off erase the configuration.
func TestSiteConfigPartialUpdateKeepsGitLabCredentials(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")

	rec := doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabLoginEnabled":false}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch off: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	stored, err := f.store.GetSiteConfig()
	if err != nil {
		t.Fatalf("get stored config: %v", err)
	}
	if stored.GitLabLoginEnabled {
		t.Fatal("the switch must have been turned off")
	}
	if stored.GitLabURL != f.gitlab.server.URL || stored.GitLabClientID != "app-id" ||
		stored.GitLabClientSecret != "app-secret" {
		t.Fatalf("the credentials must survive a switch-only update: %+v", stored)
	}

	// Switching it back on needs no retyping.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabLoginEnabled":true}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch on: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// An explicit empty value still clears, and is refused while the
	// integration would be left enabled without it.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":""}`, adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("clearing the address while enabled: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabLoginEnabled":false,"gitlabUrl":"","gitlabClientId":""}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing while disabled: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSiteConfig(); err != nil || stored.GitLabURL != "" || stored.GitLabClientID != "" {
		t.Fatalf("an explicit empty value must clear (err %v, cfg %+v)", err, stored)
	}
}

// TestSiteConfigRejectsBareHost: a GitLab address with no scheme would build
// relative request URLs and a relative redirect_uri, failing at the first
// call with an error that says nothing about the cause.
func TestSiteConfigRejectsBareHost(t *testing.T) {
	f := newGitLabFixture(t, gitlabUser{ID: 42, Username: "guser", Email: "guser@example.com"})
	adminCookie := f.login(t, "root", "root-pass")
	f.configure(t, func(cfg *store.SiteConfig) { cfg.GitLabLoginEnabled = false })

	rec := doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/g/c","gitlabUrl":"gitlab.example.com","gitlabClientId":"app-id","gitlabClientSecret":"s","gitlabLoginEnabled":true}`,
		adminCookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a bare host: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if stored, err := f.store.GetSiteConfig(); err != nil || stored.GitLabLoginEnabled {
		t.Fatalf("the refused update must not be stored (err %v)", err)
	}

	// The same rule applies to the site's own address, which the redirect URI
	// is built from: with no scheme there is no absolute callback URL.
	f.apiServer.SetPublicURL("md.example.com")
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/gitlab/enabled", nil))
	var resp struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Fatal("a public URL with no scheme must not report the integration as enabled")
	}
}
