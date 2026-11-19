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

// TestGetOrCreateCommitRestamps covers the re-recording of an already-stored
// SHA: one row per (repo, sha), with the later event's event/ref/message
// written onto it in place. This is what a push followed by an MR of the same
// commit looks like — the row is not duplicated and it does not keep the
// push's event kind.
func TestGetOrCreateCommitRestamps(t *testing.T) {
	s := newTestStore(t)

	pushed := &Commit{Repo: "group/code", SHA: "abc1234", Ref: "fix/drift", Author: "alice",
		Message: "fix: energy drift", Event: CommitEventPush, PushedAt: mustTime("2026-09-01T10:00:00Z")}
	created, err := s.GetOrCreateCommit(pushed)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !created {
		t.Fatal("expected the push to create the row")
	}

	// The MR of the same commit: same row, restamped under the MR's event,
	// source branch and last-commit title.
	mr := &Commit{Repo: "group/code", SHA: "abc1234", Ref: "fix/drift-mr", Author: "carol",
		Message: "fix: energy drift (v2)", Event: CommitEventMergeRequest, PushedAt: mustTime("2026-09-01T10:05:00Z")}
	created, err = s.GetOrCreateCommit(mr)
	if err != nil {
		t.Fatalf("mr: %v", err)
	}
	if created {
		t.Fatal("the MR should load the pushed row, not create one")
	}
	if mr.ID != pushed.ID {
		t.Fatalf("the MR got row %d, want the pushed row %d", mr.ID, pushed.ID)
	}
	stored, err := s.GetCommitByID(pushed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Event != CommitEventMergeRequest {
		t.Fatalf("event after the MR: want merge_request, got %q", stored.Event)
	}
	if stored.Ref != "fix/drift-mr" {
		t.Fatalf("ref after the MR: want the MR's source branch, got %q", stored.Ref)
	}
	if stored.Message != "fix: energy drift (v2)" {
		t.Fatalf("message after the MR: want the MR's title, got %q", stored.Message)
	}
	if got, err := s.ListCommits("group/code", 10); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Fatalf("want one row for the (repo, sha), got %d: %v", len(got), shas(got))
	}

	// A re-recording that carries nothing (an event with no ref, say, or a
	// store call that only wants the row) leaves the stored fields alone:
	// empty incoming values never blank a row.
	blank := &Commit{Repo: "group/code", SHA: "abc1234"}
	created, err = s.GetOrCreateCommit(blank)
	if err != nil {
		t.Fatalf("blank: %v", err)
	}
	if created || blank.ID != pushed.ID {
		t.Fatalf("blank re-record: created=%t id=%d", created, blank.ID)
	}
	stored, err = s.GetCommitByID(pushed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Event != CommitEventMergeRequest || stored.Ref != "fix/drift-mr" || stored.Message != "fix: energy drift (v2)" {
		t.Fatalf("a blank re-record changed the row: %+v", stored)
	}
}

// TestRecordCommitPolicies covers the repeated-commit policies at the point
// where they diverge: how the row is recorded. Requeue keeps one row and
// restamps it; both fork policies insert a fresh row for the new event and
// leave the earlier one exactly as it was. An unknown policy must never fork —
// it falls back to the requeue behaviour.
func TestRecordCommitPolicies(t *testing.T) {
	s := newTestStore(t)

	const repo, sha = "group/code", "abc1234"
	first := &Commit{Repo: repo, SHA: sha, Ref: "fix/drift", Author: "alice",
		Message: "wip: energy drift", Event: CommitEventPush, PushedAt: mustTime("2026-09-01T10:00:00Z")}
	created, err := s.RecordCommit(first, CommitOverlapRequeue)
	if err != nil {
		t.Fatalf("first recording: %v", err)
	}
	if !created {
		t.Fatal("the first recording should insert a row")
	}

	// Requeue: the same row, restamped with the newer event.
	again := &Commit{Repo: repo, SHA: sha, Ref: "fix/drift", Author: "carol",
		Message: "fix: energy drift", Event: CommitEventMergeRequest, PushedAt: mustTime("2026-09-01T10:05:00Z")}
	created, err = s.RecordCommit(again, CommitOverlapRequeue)
	if err != nil {
		t.Fatalf("requeue recording: %v", err)
	}
	if created || again.ID != first.ID {
		t.Fatalf("requeue: created=%t id=%d, want the stored row %d", created, again.ID, first.ID)
	}
	if again.Event != CommitEventMergeRequest || again.Message != "fix: energy drift" {
		t.Fatalf("requeue did not restamp: %+v", again)
	}
	// An unknown policy is not a licence to fork.
	unknown := &Commit{Repo: repo, SHA: sha, Event: CommitEventPush, PushedAt: mustTime("2026-09-01T10:06:00Z")}
	if created, err = s.RecordCommit(unknown, "not-a-policy"); err != nil {
		t.Fatalf("unknown policy: %v", err)
	} else if created || unknown.ID != first.ID {
		t.Fatalf("an unknown policy forked: created=%t id=%d", created, unknown.ID)
	}

	// Fork: a row of its own, and the earlier row is not touched.
	beforeFork, err := s.GetCommitByID(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	var newestID int64
	for _, policy := range []string{CommitOverlapFork, CommitOverlapForkCancel} {
		incoming := &Commit{Repo: repo, SHA: sha, Ref: "fix/drift", Author: "dave",
			Message: "fix: energy drift v2", Event: CommitEventMergeRequest,
			PushedAt: mustTime("2026-09-01T11:00:00Z")}
		created, err := s.RecordCommit(incoming, policy)
		if err != nil {
			t.Fatalf("%s: %v", policy, err)
		}
		if !created || incoming.ID == first.ID {
			t.Fatalf("%s: created=%t id=%d, want a row of its own", policy, created, incoming.ID)
		}
		stored, err := s.GetCommitByID(incoming.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Event != CommitEventMergeRequest || stored.Message != "fix: energy drift v2" {
			t.Fatalf("%s: the new row does not carry its own event: %+v", policy, stored)
		}
		// The row the push created keeps the event and message it had when
		// the fork happened.
		original, err := s.GetCommitByID(first.ID)
		if err != nil {
			t.Fatal(err)
		}
		if original.Event != beforeFork.Event || original.Message != beforeFork.Message ||
			original.Ref != beforeFork.Ref {
			t.Fatalf("%s rewrote the earlier row: %+v, want %+v", policy, original, beforeFork)
		}
		newestID = incoming.ID
	}

	// PriorCommits is what the fork-cancel path cancels: the earlier rows of
	// the revision, newest first, and never the row it was given.
	priors, err := s.PriorCommits(repo, sha, newestID)
	if err != nil {
		t.Fatalf("prior commits: %v", err)
	}
	if len(priors) != 2 || priors[0].ID == newestID || priors[1].ID != first.ID {
		t.Fatalf("priors of the newest row: %v, want the two earlier rows newest first", priors)
	}
	if priors[0].ID <= priors[1].ID {
		t.Fatalf("priors are not newest first: %v", priors)
	}
	if other, err := s.PriorCommits(repo, "different-sha", 0); err != nil {
		t.Fatal(err)
	} else if len(other) != 0 {
		t.Fatalf("another revision's rows leaked in: %v", other)
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

// TestListCommitsPage covers the paging the dashboard browses with: the same
// newest-first order as ListCommits, an offset that continues it, and a count
// to size the pager. Two rows share a push time, so the tie-break by id is
// what keeps consecutive pages from overlapping.
func TestListCommitsPage(t *testing.T) {
	s := newTestStore(t)

	commits := []*Commit{
		{Repo: "group/code", SHA: "c1", PushedAt: mustTime("2026-09-01T10:00:00Z")},
		{Repo: "group/code", SHA: "c2", PushedAt: mustTime("2026-09-03T10:00:00Z")},
		{Repo: "group/code", SHA: "c3", PushedAt: mustTime("2026-09-03T10:00:00Z")}, // same second as c2
		{Repo: "group/code", SHA: "c4", PushedAt: mustTime("2026-09-02T10:00:00Z")},
		{Repo: "group/other", SHA: "o1", PushedAt: mustTime("2026-09-05T10:00:00Z")},
	}
	for _, c := range commits {
		if err := s.CreateCommit(c); err != nil {
			t.Fatalf("create %s: %v", c.SHA, err)
		}
	}

	if n, err := s.CountCommits(""); err != nil || n != 5 {
		t.Fatalf("count all: %d, %v; want 5", n, err)
	}
	if n, err := s.CountCommits("group/code"); err != nil || n != 4 {
		t.Fatalf("count repo: %d, %v; want 4", n, err)
	}

	// Two pages of two, walked in order: newest first, no gap, no repeat.
	first, err := s.ListCommitsPage("group/code", 2, 0)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	second, err := s.ListCommitsPage("group/code", 2, 2)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if got := shas(first); len(got) != 2 || got[0] != "c3" || got[1] != "c2" {
		t.Fatalf("page 1 = %v, want [c3 c2] (newest first, id breaks the push-time tie)", got)
	}
	if got := shas(second); len(got) != 2 || got[0] != "c4" || got[1] != "c1" {
		t.Fatalf("page 2 = %v, want [c4 c1]", got)
	}

	// Past the end: an empty page, not an error (the caller clamps).
	past, err := s.ListCommitsPage("group/code", 2, 99)
	if err != nil {
		t.Fatalf("page past the end: %v", err)
	}
	if len(past) != 0 {
		t.Fatalf("expected an empty page, got %v", shas(past))
	}
}

// TestSupersededCommitIDs covers what the matrix dims: every row of a revision
// but its newest one, including a pair the page order would never show
// together (the client asks about ids, not about a page).
func TestSupersededCommitIDs(t *testing.T) {
	s := newTestStore(t)

	// The same SHA pushed twice; the second row (later push) supersedes the
	// first. And the same SHA in another repository, which does not.
	first := &Commit{Repo: "group/code", SHA: "dup", PushedAt: mustTime("2026-09-01T10:00:00Z")}
	second := &Commit{Repo: "group/code", SHA: "dup", PushedAt: mustTime("2026-09-02T10:00:00Z")}
	alone := &Commit{Repo: "group/code", SHA: "solo", PushedAt: mustTime("2026-09-03T10:00:00Z")}
	other := &Commit{Repo: "group/other", SHA: "dup", PushedAt: mustTime("2026-09-04T10:00:00Z")}
	for _, c := range []*Commit{first, second, alone, other} {
		if err := s.CreateCommit(c); err != nil {
			t.Fatalf("create %s/%s: %v", c.Repo, c.SHA, err)
		}
	}

	got, err := s.SupersededCommitIDs([]int64{first.ID, second.ID, alone.ID, other.ID})
	if err != nil {
		t.Fatalf("superseded: %v", err)
	}
	if !got[first.ID] {
		t.Errorf("the older row of the same revision must be superseded")
	}
	for _, c := range []*Commit{second, alone, other} {
		if got[c.ID] {
			t.Errorf("%s/%s must not be superseded", c.Repo, c.SHA)
		}
	}

	// A single id, and none at all: the query is skipped when the page is empty.
	one, err := s.SupersededCommitIDs([]int64{first.ID})
	if err != nil || !one[first.ID] {
		t.Fatalf("single id: %v, %v", one, err)
	}
	none, err := s.SupersededCommitIDs(nil)
	if err != nil || len(none) != 0 {
		t.Fatalf("no ids: %v, %v", none, err)
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
		// A bare value with one "/" is the path itself — the shape a webhook
		// payload's path_with_namespace has, and what the dashboard filters
		// on. Dropping its first segment would leave "code", which matches no
		// recorded commit: the matrix would stay empty while the pushes were
		// still dispatched.
		{"group/code", "group/code"},
		{"group/sub/code", "sub/code"}, // three segments: the first is the host
		{"gitlab.com/group/code", "group/code"},
		{"group/code.git", "group/code"},
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
