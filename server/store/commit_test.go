package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestGetOrCreateCommit(t *testing.T) {
	s := newTestStore(t)

	c := &Commit{Repo: "group/md-code", SHA: "9c8b7a6", Ref: "main", Author: "alice", Message: "Fix integrator"}
	created, err := s.GetOrCreateCommit(c)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created {
		t.Fatal("expected first call to create")
	}
	if c.ID == 0 {
		t.Fatal("expected ID set after create")
	}

	// The same (repo, sha) is idempotent: no new row, same ID.
	c2 := &Commit{Repo: "group/md-code", SHA: "9c8b7a6"}
	created, err = s.GetOrCreateCommit(c2)
	if err != nil {
		t.Fatalf("second get: %v", err)
	}
	if created {
		t.Fatal("expected second call to load, not create")
	}
	if c2.ID != c.ID || c2.Message != "Fix integrator" {
		t.Fatalf("expected existing row, got %+v", c2)
	}

	// A different sha creates a new row.
	c3 := &Commit{Repo: "group/md-code", SHA: "aaaaaaa"}
	created, err = s.GetOrCreateCommit(c3)
	if err != nil {
		t.Fatalf("third create: %v", err)
	}
	if !created || c3.ID == c.ID {
		t.Fatal("expected new row for different sha")
	}

	// A different repo creates a new row too.
	c4 := &Commit{Repo: "group/other", SHA: "9c8b7a6"}
	created, err = s.GetOrCreateCommit(c4)
	if err != nil {
		t.Fatalf("other repo: %v", err)
	}
	if !created || c4.ID == c.ID {
		t.Fatal("expected new row for different repo")
	}
}

func TestListCommits(t *testing.T) {
	s := newTestStore(t)

	commits := []*Commit{
		{Repo: "group/code", SHA: "c1", PushedAt: mustTime("2026-09-01T10:00:00Z")},
		{Repo: "group/code", SHA: "c2", PushedAt: mustTime("2026-09-03T10:00:00Z")},
		{Repo: "group/code", SHA: "c3", PushedAt: mustTime("2026-09-02T10:00:00Z")},
		{Repo: "group/other", SHA: "c4", PushedAt: mustTime("2026-09-04T10:00:00Z")},
	}
	for _, c := range commits {
		if err := s.CreateCommit(c); err != nil {
			t.Fatalf("create %s: %v", c.SHA, err)
		}
	}

	// All repos, newest first, capped.
	got, err := s.ListCommits("", 10)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 4 || got[0].SHA != "c4" || got[1].SHA != "c2" || got[2].SHA != "c3" || got[3].SHA != "c1" {
		t.Fatalf("unexpected order: %v", shas(got))
	}

	// Repo filter.
	got, err = s.ListCommits("group/code", 10)
	if err != nil {
		t.Fatalf("list repo: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 commits for repo, got %d", len(got))
	}

	// Limit.
	got, err = s.ListCommits("group/code", 2)
	if err != nil {
		t.Fatalf("list limit: %v", err)
	}
	if len(got) != 2 || got[0].SHA != "c2" {
		t.Fatalf("expected 2 newest, got %v", shas(got))
	}
}

func TestRepoPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"https://gitlab.com/group/code", "group/code"},
		{"https://gitlab.com/group/code.git", "group/code"},
		{"http://gitlab.example.com/sub/group/code", "sub/group/code"}, // sub-group namespace
		{"git@gitlab.com:group/code.git", "group/code"},
		{"gitlab.com:group/code", "group/code"},
		{"https://gitlab.com/group/sub/code", "group/sub/code"},
		{"ssh://git@gitlab.example.com:2222/group/code.git", "group/code"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := RepoPath(tc.in); got != tc.want {
			t.Errorf("RepoPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSetCommitDispatchError(t *testing.T) {
	s := newTestStore(t)
	c := &Commit{Repo: "group/md-code", SHA: "abc1234", Message: "keep me"}
	if err := s.CreateCommit(c); err != nil {
		t.Fatal(err)
	}

	if err := s.SetCommitDispatchError(c.ID, "  md-builder.yaml: file not found  "); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.GetCommitByID(c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DispatchError != "md-builder.yaml: file not found" {
		t.Fatalf("dispatch error = %q, want the trimmed message", got.DispatchError)
	}
	// A single-column write: the rest of the row is untouched.
	if got.Message != "keep me" {
		t.Fatalf("message = %q, want the stored one", got.Message)
	}

	// An empty message clears it again (a later dispatch that worked).
	if err := s.SetCommitDispatchError(c.ID, ""); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ = s.GetCommitByID(c.ID); got.DispatchError != "" {
		t.Fatalf("dispatch error = %q, want empty after clear", got.DispatchError)
	}

	// A pathological message (a git stderr dump) is capped.
	long := strings.Repeat("x", maxDispatchErrorLen+500)
	if err := s.SetCommitDispatchError(c.ID, long); err != nil {
		t.Fatalf("long set: %v", err)
	}
	if got, _ = s.GetCommitByID(c.ID); len(got.DispatchError) != maxDispatchErrorLen {
		t.Fatalf("stored %d bytes, want the %d cap", len(got.DispatchError), maxDispatchErrorLen)
	}
}

func TestGetCommitNotFound(t *testing.T) {
	s := newTestStore(t)
	var c Commit
	err := s.DB.First(&c, 9999).Error
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func shas(commits []Commit) []string {
	out := make([]string, len(commits))
	for i, c := range commits {
		out[i] = c.SHA
	}
	return out
}

// mustTime parses a fixed RFC3339 timestamp, failing the test on error.
func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}
