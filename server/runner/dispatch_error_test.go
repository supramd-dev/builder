package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// dispatchFixture is a store with one enabled environment and the site's
// code repository configured — the minimum a yaml dispatch needs.
func dispatchFixture(t *testing.T, envTags string) (*store.Store, *Service) {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "dispatcher-" + t.Name(), Email: "d@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-" + t.Name(), Host: "h", Username: "u", PrivateKey: "k",
		Tags: envTags, Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	return s, &Service{Store: s}
}

// commitFor records a commit row and returns it (a dispatch annotates the
// row, so it has to exist first).
func commitFor(t *testing.T, s *store.Store, sha string) *store.Commit {
	t.Helper()
	c := &store.Commit{Repo: "group/code", SHA: sha, Ref: "main", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestDispatchRecordsErrorOnCommit checks the dashboard's side of a failed
// dispatch: the yaml cannot be read, so no graph exists — and the commit row
// carries the reason, which is what the matrix shows on the empty cells.
func TestDispatchRecordsErrorOnCommit(t *testing.T) {
	s, svc := dispatchFixture(t, "cpu")
	commit := commitFor(t, s, "aaaa1111")

	svc.FetchYAML = func(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		return nil, errors.New("md-builder.yaml: file not found")
	}
	res := svc.DispatchForCommit(commit)
	if res.Err == nil {
		t.Fatal("dispatch should fail")
	}
	if res.TasksCreated != 0 {
		t.Fatalf("tasks created: %d, want 0", res.TasksCreated)
	}

	stored, err := s.GetCommitByID(commit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DispatchError != "md-builder.yaml: file not found" {
		t.Fatalf("stored dispatch error = %q", stored.DispatchError)
	}
	if commit.DispatchError != stored.DispatchError {
		t.Errorf("the in-memory commit should carry it too: %q", commit.DispatchError)
	}

	// A later dispatch that works clears it again — otherwise the row would
	// keep claiming a failure that has since been fixed.
	svc.FetchYAML = func(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		return []byte("version: 2\ndefaults:\n  build:\n    command: \"make\"\nmatrix:\n  - tags: [cpu]\n    unit:\n      command: \"ctest\"\n"), nil
	}
	if res := svc.DispatchForCommit(commit); res.Err != nil {
		t.Fatalf("second dispatch: %v", res.Err)
	}
	if stored, _ = s.GetCommitByID(commit.ID); stored.DispatchError != "" {
		t.Fatalf("dispatch error = %q, want cleared by the successful dispatch", stored.DispatchError)
	}
}

// TestDispatchRecordsNoMatchingEntry: a valid yaml whose entries match no
// enabled environment is not a failure, but it leaves exactly the same empty
// cells — so the row explains it too.
func TestDispatchRecordsNoMatchingEntry(t *testing.T) {
	s, svc := dispatchFixture(t, "gpu") // the yaml asks for cpu
	commit := commitFor(t, s, "bbbb2222")

	svc.FetchYAML = func(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		return []byte("version: 2\ndefaults:\n  build:\n    command: \"make\"\nmatrix:\n  - tags: [cpu]\n    unit:\n      command: \"ctest\"\n"), nil
	}
	res := svc.DispatchForCommit(commit)
	if res.Err != nil {
		t.Fatalf("dispatch: %v", res.Err)
	}
	if res.TasksCreated != 0 || res.EntriesSkipped != 1 {
		t.Fatalf("created %d, skipped %d; want 0 and 1", res.TasksCreated, res.EntriesSkipped)
	}

	stored, err := s.GetCommitByID(commit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.DispatchError, "no entry matched an enabled environment") ||
		!strings.Contains(stored.DispatchError, "1 entry") {
		t.Fatalf("stored dispatch error = %q", stored.DispatchError)
	}
}

// TestRecordDispatchOutcomeMessages pins the message the dashboard shows for
// each outcome. The error wins even when graphs were created (a dispatch
// that failed on its second entry keeps the first entry's graph).
func TestRecordDispatchOutcomeMessages(t *testing.T) {
	s, svc := dispatchFixture(t, "cpu")
	commit := commitFor(t, s, "cccc3333")

	svc.recordDispatchOutcome(commit, &DispatchResult{Err: errors.New("boom"), TasksCreated: 1})
	if stored, _ := s.GetCommitByID(commit.ID); stored.DispatchError != "boom" {
		t.Fatalf("partial failure: %q", stored.DispatchError)
	}

	svc.recordDispatchOutcome(commit, &DispatchResult{EntriesSkipped: 2})
	if stored, _ := s.GetCommitByID(commit.ID); !strings.Contains(stored.DispatchError, "all 2 entries skipped") {
		t.Fatalf("no match: %q", stored.DispatchError)
	}

	// A dispatch that created graphs clears whatever was there.
	svc.recordDispatchOutcome(commit, &DispatchResult{TasksCreated: 1})
	if stored, _ := s.GetCommitByID(commit.ID); stored.DispatchError != "" {
		t.Fatalf("success: %q, want empty", stored.DispatchError)
	}

	// A nil commit (the failure happened before the row existed) is a no-op.
	svc.recordDispatchOutcome(nil, &DispatchResult{Err: errors.New("boom")})
	svc.recordDispatchOutcome(&store.Commit{}, &DispatchResult{Err: errors.New("boom")})
}

// TestDispatchForRefWithoutRepoRecordsNothing: the failure happens before a
// commit row exists, so there is nothing to annotate — and nothing to crash
// on either.
func TestDispatchForRefWithoutRepoRecordsNothing(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	svc := &Service{Store: s}

	commit, res := svc.DispatchForRef(testContext(), "main")
	if res.Err == nil {
		t.Fatal("dispatch without a code repo must fail")
	}
	if commit.ID != 0 {
		t.Fatalf("no commit row should exist, got %d", commit.ID)
	}
	commits, err := s.ListCommits("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 0 {
		t.Fatalf("commits recorded: %d, want 0", len(commits))
	}
}

// TestDispatchManualRecordsError: the manual (no yaml) dispatch annotates its
// commit row too — the disabled environment's column would otherwise stay
// empty with no explanation.
func TestDispatchManualRecordsError(t *testing.T) {
	s, svc := dispatchFixture(t, "cpu")
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		return "dddd4444", nil
	}
	enabled, err := s.ListEnabledEnvironments()
	if err != nil || len(enabled) != 1 {
		t.Fatalf("environments: %v %d", err, len(enabled))
	}
	off := &store.TestEnvironment{
		OwnerID: enabled[0].OwnerID, Name: "off-" + t.Name(), Host: "h", Username: "u",
		PrivateKey: "k", Tags: "cpu",
	}
	if err := s.CreateEnvironment(off); err != nil {
		t.Fatal(err)
	}
	// The column defaults to true, so a fresh row is enabled: flip it off
	// through the setter rather than relying on a zero value.
	if _, err := s.SetEnvironmentEnabled(off.OwnerID, off.ID, false); err != nil {
		t.Fatal(err)
	}

	roots, err := svc.DispatchManual(ManualDispatch{
		BuildCommand:   "make",
		EnvironmentIDs: []int64{enabled[0].ID, off.ID},
		Username:       "alice",
	})
	if err == nil {
		t.Fatal("dispatching onto a disabled environment must fail")
	}
	if len(roots) != 1 {
		t.Fatalf("roots created: %d, want the enabled environment's", len(roots))
	}

	commits, err := s.ListCommits("", 10)
	if err != nil || len(commits) != 1 {
		t.Fatalf("commits: %v %d", err, len(commits))
	}
	if !strings.Contains(commits[0].DispatchError, "disabled") {
		t.Fatalf("stored dispatch error = %q, want the disabled-environment message", commits[0].DispatchError)
	}
}
