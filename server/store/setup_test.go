package store

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestSetupRequired(t *testing.T) {
	s := newTestStore(t)

	required, err := s.SetupRequired()
	if err != nil {
		t.Fatalf("setup required: %v", err)
	}
	if !required {
		t.Fatal("an empty database must ask for setup")
	}

	// Any account closes the window — it does not have to be the one the
	// setup page would have created.
	if err := s.CreateUser(&User{
		Username: "cli-user", Email: "cli@example.com", PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	required, err = s.SetupRequired()
	if err != nil {
		t.Fatalf("setup required: %v", err)
	}
	if required {
		t.Fatal("a site with an account must not ask for setup")
	}
}

func TestCompleteSetup(t *testing.T) {
	s := newTestStore(t)

	u := &User{Username: "root", Email: "root@example.com", PasswordHash: "hash"}
	if err := s.CompleteSetup(u, "https://gitlab.example.com/group/code", "glpat-secret"); err != nil {
		t.Fatalf("complete setup: %v", err)
	}
	if u.ID == 0 {
		t.Fatal("expected the account to be written")
	}
	if u.Role != RoleAdmin {
		t.Fatalf("first account role = %q, want %q", u.Role, RoleAdmin)
	}

	// The configuration carries the repository and its token.
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.CodeRepo != "https://gitlab.example.com/group/code" {
		t.Fatalf("code repo = %q", cfg.CodeRepo)
	}
	if cfg.AccessToken != "glpat-secret" {
		t.Fatalf("access token = %q, want the one that was stored", cfg.AccessToken)
	}
	// The webhook token the site generated for itself survives the write: the
	// setup page never sees it, and Settings → Webhook must still show it.
	if strings.TrimSpace(cfg.WebhookToken) == "" {
		t.Fatal("expected a webhook token on the site config")
	}

	// A second setup is refused and changes nothing.
	second := &User{Username: "second", Email: "second@example.com", PasswordHash: "hash"}
	err = s.CompleteSetup(second, "https://gitlab.example.com/other/repo", "glpat-other")
	if !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup error = %v, want ErrSetupDone", err)
	}
	if second.ID != 0 {
		t.Fatal("a refused setup must not write the account")
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 || users[0].Username != "root" {
		t.Fatalf("unexpected users: %+v", users)
	}
	cfg, err = s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.CodeRepo != "https://gitlab.example.com/group/code" {
		t.Fatalf("a refused setup rewrote the repository: %q", cfg.CodeRepo)
	}
	if cfg.AccessToken != "glpat-secret" {
		t.Fatalf("a refused setup rewrote the access token: %q", cfg.AccessToken)
	}
}

// A setup with no access token (a public repository) must leave the stored
// one alone rather than clearing it — the same convention the settings API
// follows.
func TestCompleteSetupWithoutAccessToken(t *testing.T) {
	s := newTestStore(t)

	if err := s.CompleteSetup(&User{
		Username: "root", Email: "root@example.com", PasswordHash: "hash",
	}, "https://gitlab.example.com/group/code", ""); err != nil {
		t.Fatalf("complete setup: %v", err)
	}
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("get site config: %v", err)
	}
	if cfg.AccessToken != "" {
		t.Fatalf("access token = %q, want empty for a public repository", cfg.AccessToken)
	}
	if strings.TrimSpace(cfg.WebhookToken) == "" {
		t.Fatal("expected a webhook token on the site config")
	}
}

// Two setup requests racing each other must produce one account: the row lock
// on the site config serializes them, and the loser is refused.
func TestCompleteSetupConcurrent(t *testing.T) {
	s := newTestStore(t)

	const attempts = 4
	errs := make([]error, attempts)
	var wg sync.WaitGroup
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.CompleteSetup(&User{
				Username:     "racer-" + string(rune('a'+i)),
				Email:        "racer" + string(rune('a'+i)) + "@example.com",
				PasswordHash: "hash",
			}, "https://gitlab.example.com/group/code", "")
		}()
	}
	wg.Wait()

	created := 0
	for i, err := range errs {
		switch {
		case err == nil:
			created++
		case errors.Is(err, ErrSetupDone):
			// The expected answer for everyone who lost the race.
		default:
			t.Fatalf("attempt %d: unexpected error: %v", i, err)
		}
	}
	if created != 1 {
		t.Fatalf("accounts created = %d, want exactly 1", created)
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("users in the database = %d, want 1", len(users))
	}
	if users[0].Role != RoleAdmin {
		t.Fatalf("role = %q, want %q", users[0].Role, RoleAdmin)
	}
}
