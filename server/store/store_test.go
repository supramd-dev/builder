package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"

	"md-builder/server/storage"
)

// newTestStore opens an in-memory SQLite store for each test, backed by an
// in-memory object store (the artifact backend is mandatory, so every store
// that records artifacts needs one).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, _ := newTestStoreWithObjects(t)
	return s
}

// newTestStoreWithObjects also returns the object store, for tests that
// inspect what was uploaded.
func newTestStoreWithObjects(t *testing.T) (*Store, *storage.Memory) {
	t.Helper()
	objs := storage.NewMemory()
	s, err := Open("file::memory:?cache=shared", WithObjects(objs))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, objs
}

func TestCreateAndGetUser(t *testing.T) {
	s := newTestStore(t)

	u := &User{Username: "alice", Email: "alice@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("expected user ID to be set after create")
	}

	got, err := s.GetUserByUsername("alice")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "alice" || got.Email != "alice@example.com" {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.PasswordHash != "hash" {
		t.Fatalf("unexpected password hash: %q", got.PasswordHash)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestGetUserByUsername_NotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.GetUserByUsername("nobody")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound, got %v", err)
	}
}

func TestCreateUser_DuplicateUsername(t *testing.T) {
	s := newTestStore(t)
	u1 := &User{Username: "bob", Email: "bob@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u1); err != nil {
		t.Fatalf("create first user: %v", err)
	}
	u2 := &User{Username: "bob", Email: "other@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u2); err == nil {
		t.Fatal("expected error creating duplicate username")
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "carol", Email: "carol@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	now := time.Now()
	sess := &Session{
		Token:     "tok123",
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	gotSess, gotUser, err := s.GetSessionByToken("tok123")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if gotSess.Token != "tok123" || gotUser.ID != u.ID || gotUser.Username != "carol" {
		t.Fatalf("unexpected session/user: sess=%+v user=%+v", gotSess, gotUser)
	}

	// Expired sessions must not resolve.
	if err := s.DeleteSession("tok123"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_, _, err = s.GetSessionByToken("tok123")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound after delete, got %v", err)
	}
}

func TestGetSessionByToken_Expired(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "dave", Email: "dave@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sess := &Session{
		Token:     "expired",
		UserID:    u.ID,
		CreatedAt: time.Now().Add(-2 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := s.CreateSession(sess); err != nil {
		t.Fatalf("create session: %v", err)
	}
	_, _, err := s.GetSessionByToken("expired")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected ErrRecordNotFound for expired session, got %v", err)
	}
}

func TestCleanExpiredSessions(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "erin", Email: "erin@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Now()
	stale := &Session{Token: "stale", UserID: u.ID, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	fresh := &Session{Token: "fresh", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := s.CreateSession(stale); err != nil {
		t.Fatalf("create stale session: %v", err)
	}
	if err := s.CreateSession(fresh); err != nil {
		t.Fatalf("create fresh session: %v", err)
	}

	n, err := s.CleanExpiredSessions()
	if err != nil {
		t.Fatalf("clean expired: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 expired session removed, got %d", n)
	}
	if _, _, err := s.GetSessionByToken("fresh"); err != nil {
		t.Fatalf("fresh session should remain: %v", err)
	}
}

// TestUserRoleDefaultsToRegular covers both a row created without a role and
// one created before the column existed: both read back as a regular user.
func TestUserRoleDefaultsToRegular(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "grace", Email: "grace@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	got, err := s.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Role != RoleUser {
		t.Fatalf("expected role %q, got %q", RoleUser, got.Role)
	}
	if got.IsAdmin() {
		t.Fatal("a default user must not be an administrator")
	}
	if got.Disabled {
		t.Fatal("a new account must not be disabled")
	}
}

func TestListUsersOrdersByID(t *testing.T) {
	s := newTestStore(t)
	for _, name := range []string{"alice", "bob", "carol"} {
		u := &User{Username: name, Email: name + "@example.com", PasswordHash: "hash"}
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("expected 3 users, got %d", len(users))
	}
	for i, want := range []string{"alice", "bob", "carol"} {
		if users[i].Username != want {
			t.Fatalf("user %d: expected %q, got %q", i, want, users[i].Username)
		}
	}
}

// TestUpdateUserKeepsRole is the store-side half of "no API path can change a
// role": UpdateUser has nowhere to put one.
func TestUpdateUserKeepsRole(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "henry", Email: "henry@example.com", PasswordHash: "old", Role: RoleAdmin}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	if err := s.UpdateUser(u.ID, UserUpdate{
		Username: "henry2",
		Email:    "henry2@example.com",
		Disabled: true,
	}); err != nil {
		t.Fatalf("update user: %v", err)
	}

	got, err := s.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.Username != "henry2" || got.Email != "henry2@example.com" || !got.Disabled {
		t.Fatalf("unexpected user after update: %+v", got)
	}
	if got.Role != RoleAdmin {
		t.Fatalf("role must survive an update, got %q", got.Role)
	}
	if got.PasswordHash != "old" {
		t.Fatalf("an empty PasswordHash must keep the stored one, got %q", got.PasswordHash)
	}

	// A non-empty hash replaces it.
	if err := s.UpdateUser(u.ID, UserUpdate{
		Username: "henry2", Email: "henry2@example.com", PasswordHash: "new", Disabled: true,
	}); err != nil {
		t.Fatalf("update password: %v", err)
	}
	got, err = s.GetUserByID(u.ID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if got.PasswordHash != "new" {
		t.Fatalf("expected the new hash, got %q", got.PasswordHash)
	}
}

func TestUsernameAndEmailTaken(t *testing.T) {
	s := newTestStore(t)
	alice := &User{Username: "alice", Email: "alice@example.com", PasswordHash: "hash"}
	bob := &User{Username: "bob", Email: "bob@example.com", PasswordHash: "hash"}
	for _, u := range []*User{alice, bob} {
		if err := s.CreateUser(u); err != nil {
			t.Fatalf("create %s: %v", u.Username, err)
		}
	}

	cases := []struct {
		name     string
		field    string
		value    string
		exceptID int64
		want     bool
	}{
		{"other user's name", "username", "alice", bob.ID, true},
		{"own name is not a conflict", "username", "bob", bob.ID, false},
		{"unused name", "username", "carol", bob.ID, false},
		{"other user's email", "email", "alice@example.com", bob.ID, true},
		{"own email is not a conflict", "email", "bob@example.com", bob.ID, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				got bool
				err error
			)
			if tc.field == "username" {
				got, err = s.UsernameTaken(tc.value, tc.exceptID)
			} else {
				got, err = s.EmailTaken(tc.value, tc.exceptID)
			}
			if err != nil {
				t.Fatalf("taken check: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %v, got %v", tc.want, got)
			}
		})
	}
}

// TestDeleteSessionsForUser covers both callers: disabling drops every
// session, a password change keeps the one named by exceptToken.
func TestDeleteSessionsForUser(t *testing.T) {
	s := newTestStore(t)
	u := &User{Username: "ivan", Email: "ivan@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	other := &User{Username: "judy", Email: "judy@example.com", PasswordHash: "hash"}
	if err := s.CreateUser(other); err != nil {
		t.Fatalf("create other user: %v", err)
	}
	now := time.Now()
	for _, sess := range []*Session{
		{Token: "keep", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Token: "drop", UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
		{Token: "elsewhere", UserID: other.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)},
	} {
		if err := s.CreateSession(sess); err != nil {
			t.Fatalf("create session %s: %v", sess.Token, err)
		}
	}

	if err := s.DeleteSessionsForUser(u.ID, "keep"); err != nil {
		t.Fatalf("delete sessions: %v", err)
	}
	if _, _, err := s.GetSessionByToken("keep"); err != nil {
		t.Fatalf("the excepted session should remain: %v", err)
	}
	if _, _, err := s.GetSessionByToken("drop"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected the other session to be gone, got %v", err)
	}
	if _, _, err := s.GetSessionByToken("elsewhere"); err != nil {
		t.Fatalf("another user's session must be untouched: %v", err)
	}

	// "" drops everything.
	if err := s.DeleteSessionsForUser(u.ID, ""); err != nil {
		t.Fatalf("delete all sessions: %v", err)
	}
	if _, _, err := s.GetSessionByToken("keep"); !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected every session to be gone, got %v", err)
	}
}

// TestOpenFileDB verifies a file-backed SQLite database round-trips across
// reopen (migrations are idempotent).
func TestOpenFileDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.CreateUser(&User{Username: "frank", Email: "frank@example.com", PasswordHash: "hash"}); err != nil {
		_ = s.Close()
		t.Fatalf("create user: %v", err)
	}
	_ = s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if _, err := s2.GetUserByUsername("frank"); err != nil {
		t.Fatalf("user should survive reopen: %v", err)
	}
}
