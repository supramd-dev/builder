package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"md-builder/server/store"
)

// accountJSON mirrors the wire shape the tests assert on.
type accountJSON struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"createdAt"`
	Source    string `json:"source"`
	Approved  bool   `json:"approved"`
	GitLabID  int64  `json:"gitlabId"`
}

// doJSON performs an authenticated request and returns the recorder. A nil
// cookie sends no session.
func doJSON(t *testing.T, mux *http.ServeMux, method, target, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// newUserFixture is the cast the account tests work with: an administrator, a
// regular user, and a second administrator.
type userFixture struct {
	apiServer *Server
	store     *store.Store
	mux       *http.ServeMux
	admin     *store.User
	user      *store.User
	admin2    *store.User
}

func newUserFixture(t *testing.T) *userFixture {
	t.Helper()
	apiServer, s := newTestServer(t)
	f := &userFixture{
		apiServer: apiServer,
		store:     s,
		mux:       http.NewServeMux(),
		admin:     seedUserWithRole(t, s, "root", "root@example.com", "root-pass", store.RoleAdmin),
		user:      seedUser(t, s, "alice", "alice@example.com", "alice-pass"),
		admin2:    seedUserWithRole(t, s, "ops", "ops@example.com", "ops-pass", store.RoleAdmin),
	}
	apiServer.Register(f.mux)
	return f
}

func (f *userFixture) login(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	return loginCookie(t, f.mux, username, password)
}

func TestLoginAndMeReportRole(t *testing.T) {
	f := newUserFixture(t)

	for _, tc := range []struct{ username, password, role string }{
		{"root", "root-pass", store.RoleAdmin},
		{"alice", "alice-pass", store.RoleUser},
	} {
		rec := doJSON(t, f.mux, http.MethodPost, "/api/login",
			`{"username":"`+tc.username+`","password":"`+tc.password+`"}`, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s login: expected 200, got %d, body: %s", tc.username, rec.Code, rec.Body.String())
		}
		var login accountJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil {
			t.Fatalf("decode login: %v", err)
		}
		if login.Role != tc.role {
			t.Fatalf("%s: expected role %q, got %q", tc.username, tc.role, login.Role)
		}
		if login.ID == 0 || login.Username != tc.username {
			t.Fatalf("%s: unexpected login response: %+v", tc.username, login)
		}

		cookie := f.login(t, tc.username, tc.password)
		rec = doJSON(t, f.mux, http.MethodGet, "/api/me", "", cookie)
		var me accountJSON
		if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
			t.Fatalf("decode me: %v", err)
		}
		if me.Role != tc.role {
			t.Fatalf("%s: /api/me expected role %q, got %q", tc.username, tc.role, me.Role)
		}
		if me.ID == 0 {
			t.Fatalf("%s: /api/me must report the account id", tc.username)
		}
	}
}

func TestUsersListRequiresAdmin(t *testing.T) {
	f := newUserFixture(t)

	// No session at all.
	rec := doJSON(t, f.mux, http.MethodGet, "/api/users", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: expected 401, got %d", rec.Code)
	}

	// A regular user must not even see the list.
	rec = doJSON(t, f.mux, http.MethodGet, "/api/users", "", f.login(t, "alice", "alice-pass"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("regular user: expected 403, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// An administrator sees every account, without any password material.
	rec = doJSON(t, f.mux, http.MethodGet, "/api/users", "", f.login(t, "root", "root-pass"))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, "passwordHash") || strings.Contains(body, "password_hash") {
		t.Fatalf("the list must not carry password material: %s", body)
	}
	var resp struct {
		Users []accountJSON `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode users: %v", err)
	}
	if len(resp.Users) != 3 {
		t.Fatalf("expected 3 accounts, got %d", len(resp.Users))
	}
	if resp.Users[0].Username != "root" || resp.Users[0].Role != store.RoleAdmin {
		t.Fatalf("unexpected first account: %+v", resp.Users[0])
	}
	if resp.Users[1].Username != "alice" || resp.Users[1].Role != store.RoleUser {
		t.Fatalf("unexpected second account: %+v", resp.Users[1])
	}
	for _, u := range resp.Users {
		if u.CreatedAt == "" {
			t.Fatalf("account %q is missing createdAt", u.Username)
		}
	}
}

// TestUserEditSelf covers the self-service half: anyone may change their own
// email, username and password.
func TestUserEditSelf(t *testing.T) {
	f := newUserFixture(t)
	cookie := f.login(t, "alice", "alice-pass")
	// A second session of the same user, from another browser.
	other := f.login(t, "alice", "alice-pass")

	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice2","email":"alice2@example.com","password":"new-alice-pass"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("self edit: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var got accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Username != "alice2" || got.Email != "alice2@example.com" || got.Disabled {
		t.Fatalf("unexpected account after edit: %+v", got)
	}
	if got.Role != store.RoleUser {
		t.Fatalf("role must be untouched, got %q", got.Role)
	}

	// The new password works, the old one does not.
	if rec := doJSON(t, f.mux, http.MethodPost, "/api/login",
		`{"username":"alice2","password":"new-alice-pass"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("login with the new password: expected 200, got %d", rec.Code)
	}
	if rec := doJSON(t, f.mux, http.MethodPost, "/api/login",
		`{"username":"alice2","password":"alice-pass"}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("login with the old password: expected 401, got %d", rec.Code)
	}

	// The browser that made the change stays logged in; the other one does not.
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("the caller's session must survive: got %d", rec.Code)
	}
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", other); rec.Code != http.StatusUnauthorized {
		t.Fatalf("the other session must be dropped: got %d", rec.Code)
	}
}

func TestUserEditDeniedForRegularUser(t *testing.T) {
	f := newUserFixture(t)
	cookie := f.login(t, "alice", "alice-pass")

	// Someone else's account.
	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.admin2.ID, 10),
		`{"username":"ops","email":"hijack@example.com"}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("editing another account: expected 403, got %d, body: %s", rec.Code, rec.Body.String())
	}
	target, err := f.store.GetUserByID(f.admin2.ID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if target.Email != "ops@example.com" {
		t.Fatalf("the target must be unchanged, got %q", target.Email)
	}

	// Disabling is an administrator field, even on your own account.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com","disabled":true}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("self-disable by a regular user: expected 403, got %d", rec.Code)
	}
	// ... and the account is still usable.
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", cookie); rec.Code != http.StatusOK {
		t.Fatalf("the account must still work: got %d", rec.Code)
	}

	// A missing id answers the same way as someone else's account: the
	// permission is decided first, so ids cannot be probed.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/9999",
		`{"username":"ghost","email":"ghost@example.com"}`, cookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("probing an unknown id: expected 403, got %d, body: %s", rec.Code, rec.Body.String())
	}
}

func TestUserDisableEndsSessions(t *testing.T) {
	f := newUserFixture(t)
	victim := f.login(t, "alice", "alice-pass")
	adminCookie := f.login(t, "root", "root-pass")

	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com","disabled":true}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	var got accountJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.Disabled {
		t.Fatal("expected the account to report disabled")
	}

	// The live session is gone, and a fresh login is refused with its own
	// message (the password was right).
	if rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "", victim); rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled user's session: expected 401, got %d", rec.Code)
	}
	rec = doJSON(t, f.mux, http.MethodPost, "/api/login", `{"username":"alice","password":"alice-pass"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("disabled login: expected 403, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "disabled") {
		t.Fatalf("expected the disabled reason in the body, got %s", rec.Body.String())
	}

	// Re-enabling restores access.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com","disabled":false}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, f.mux, http.MethodPost, "/api/login", `{"username":"alice","password":"alice-pass"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("login after re-enabling: expected 200, got %d", rec.Code)
	}
}

// TestDisabledUserSessionRejected covers the row that slipped past the
// deletion: a session that still exists for a disabled account resolves to
// nobody.
func TestDisabledUserSessionRejected(t *testing.T) {
	f := newUserFixture(t)
	if err := f.store.UpdateUser(f.user.ID, store.UserUpdate{
		Username: "alice", Email: "alice@example.com", Disabled: true,
	}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	sess := &store.Session{
		Token:     "still-here",
		UserID:    f.user.ID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := f.store.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	rec := doJSON(t, f.mux, http.MethodGet, "/api/me", "",
		&http.Cookie{Name: sessionCookie, Value: "still-here"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a disabled user's session, got %d", rec.Code)
	}
}

func TestAdminCannotDisableAdministrators(t *testing.T) {
	f := newUserFixture(t)
	cookie := f.login(t, "root", "root-pass")

	// Not their own account.
	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.admin.ID, 10),
		`{"username":"root","email":"root@example.com","disabled":true}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-disable: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// Not another administrator's either.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.admin2.ID, 10),
		`{"username":"ops","email":"ops@example.com","disabled":true}`, cookie)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabling another admin: expected 400, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if target, err := f.store.GetUserByID(f.admin2.ID); err != nil || target.Disabled {
		t.Fatalf("the other administrator must stay enabled (err %v)", err)
	}

	// Their credentials, however, are editable: the CLI can create an
	// administrator but cannot reset one, so this is the only way back in for
	// an administrator who has lost their password.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.admin2.ID, 10),
		`{"username":"ops2","email":"ops2@example.com","password":"new-ops-pass"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("editing another admin: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, f.mux, http.MethodPost, "/api/login", `{"username":"ops2","password":"new-ops-pass"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("the other admin must be able to log in with the new password: got %d", rec.Code)
	}
}

// TestRoleIsNeverEditable is the escalation test: role is not part of the
// update body, so sending one changes nothing.
func TestRoleIsNeverEditable(t *testing.T) {
	f := newUserFixture(t)
	adminCookie := f.login(t, "root", "root-pass")

	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com","role":"admin"}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if target, err := f.store.GetUserByID(f.user.ID); err != nil || target.IsAdmin() {
		t.Fatalf("the role must not change (err %v, role %q)", err, target.Role)
	}

	// A regular user promoting themselves is refused as well: the request is
	// an edit of their own account, which is allowed — of the other fields.
	userCookie := f.login(t, "alice", "alice-pass")
	rec = doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com","role":"admin"}`, userCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("self update: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}
	if target, err := f.store.GetUserByID(f.user.ID); err != nil || target.IsAdmin() {
		t.Fatalf("self-promotion must fail (err %v, role %q)", err, target.Role)
	}
}

func TestUserEditValidation(t *testing.T) {
	f := newUserFixture(t)
	cookie := f.login(t, "root", "root-pass")
	path := "/api/users/" + strconv.FormatInt(f.user.ID, 10)

	cases := []struct {
		name   string
		target string
		body   string
		want   int
	}{
		{"missing username", path, `{"username":"","email":"alice@example.com"}`, http.StatusBadRequest},
		{"username with a space", path, `{"username":"al ice","email":"alice@example.com"}`, http.StatusBadRequest},
		{"missing email", path, `{"username":"alice","email":""}`, http.StatusBadRequest},
		{"malformed email", path, `{"username":"alice","email":"alice.example.com"}`, http.StatusBadRequest},
		{"short password", path, `{"username":"alice","email":"alice@example.com","password":"short"}`, http.StatusBadRequest},
		{"duplicate username", path, `{"username":"ops","email":"alice@example.com"}`, http.StatusConflict},
		{"duplicate email", path, `{"username":"alice","email":"ops@example.com"}`, http.StatusConflict},
		{"unknown user", "/api/users/9999", `{"username":"nobody","email":"nobody@example.com"}`, http.StatusNotFound},
		{"bad id", "/api/users/abc", `{"username":"nobody","email":"nobody@example.com"}`, http.StatusBadRequest},
		{"malformed body", path, `not json`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doJSON(t, f.mux, http.MethodPut, tc.target, tc.body, cookie)
			if rec.Code != tc.want {
				t.Fatalf("expected %d, got %d, body: %s", tc.want, rec.Code, rec.Body.String())
			}
		})
	}

	// Renaming to your own current values is not a conflict.
	rec := doJSON(t, f.mux, http.MethodPut, path,
		`{"username":"alice","email":"alice@example.com"}`, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-op update: expected 200, got %d, body: %s", rec.Code, rec.Body.String())
	}

	// The wrong method on the item route.
	rec = doJSON(t, f.mux, http.MethodGet, path, "", cookie)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET item: expected 405, got %d", rec.Code)
	}
	// The wrong method on the list route.
	rec = doJSON(t, f.mux, http.MethodPost, "/api/users", `{}`, cookie)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST list: expected 405, got %d", rec.Code)
	}
}

// TestUserEditRequiresSession: the item route authenticates before it decides
// anything else.
func TestUserEditRequiresSession(t *testing.T) {
	f := newUserFixture(t)
	rec := doJSON(t, f.mux, http.MethodPut, "/api/users/"+strconv.FormatInt(f.user.ID, 10),
		`{"username":"alice","email":"alice@example.com"}`, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}
