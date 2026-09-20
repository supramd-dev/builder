package runner

import (
	"context"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// TestDispatchForRef checks the manual yaml-matrix trigger: the ref resolves
// to a commit, the yaml matrix at it dispatches exactly like a webhook push
// (one graph per matching environment), the graphs carry the manual-yaml
// trigger flag, and re-triggering the same ref deduplicates the commit row
// and requeues the same graphs.
func TestDispatchForRef(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "refman", Email: "r@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-ref", Host: "h", Username: "u", PrivateKey: "k",
		Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	const sha = "abcdef1234567890abcdef1234567890abcdef12"
	svc := &Service{Store: s}
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		if repoURL != "https://gitlab.example.com/group/code" {
			t.Errorf("resolve ref repo: got %q", repoURL)
		}
		if ref != "v1.2" {
			t.Errorf("resolve ref: want v1.2, got %q", ref)
		}
		return sha, nil
	}
	yamlFetches := 0
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, atSHA string, creds *GitCredentials) ([]byte, error) {
		yamlFetches++
		if atSHA != sha {
			t.Errorf("fetch yaml at %q, want %q", atSHA, sha)
		}
		return []byte(`version: 2
defaults:
  build:
    command: "make -j8"
presets:
  heat:
    command: "mpirun ./run_heat"
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      use: [heat]
`), nil
	}

	commit, res := svc.DispatchForRef(testContext(), "v1.2")
	if res.Err != nil {
		t.Fatalf("dispatch: %v", res.Err)
	}
	if res.TasksCreated != 1 {
		t.Fatalf("tasks created: %d, want 1", res.TasksCreated)
	}
	if res.EntriesSkipped != 0 {
		t.Errorf("entries skipped: %d", res.EntriesSkipped)
	}
	if !res.CommitCreated {
		t.Error("first dispatch should create the commit row")
	}
	if commit.SHA != sha || commit.Ref != "v1.2" || commit.Author != "manual" {
		t.Errorf("commit row wrong: %+v", commit)
	}

	// The graph carries the manual-yaml trigger and the yaml's stages.
	roots, err := s.ListRootTasks(10)
	if err != nil || len(roots) != 1 {
		t.Fatalf("roots: %v %d", err, len(roots))
	}
	if roots[0].Trigger != store.TaskTriggerManualYAML {
		t.Errorf("trigger: %d, want %d", roots[0].Trigger, store.TaskTriggerManualYAML)
	}
	subs, err := s.ListSubTasks(roots[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, sub := range subs {
		kinds = append(kinds, sub.Kind)
	}
	if len(subs) != 4 || // clone, build (from yaml build.command), unit, regression
		!strings.Contains(strings.Join(kinds, ","), store.TaskKindUnit) {
		t.Errorf("sub-tasks wrong: %v", kinds)
	}

	// Re-trigger the same ref: same commit row, same graphs requeued.
	commit2, res2 := svc.DispatchForRef(testContext(), "v1.2")
	if res2.Err != nil {
		t.Fatalf("re-dispatch: %v", res2.Err)
	}
	if res2.CommitCreated {
		t.Error("second dispatch must deduplicate the commit row")
	}
	if commit2.ID != commit.ID {
		t.Errorf("second dispatch commit %d, want the same %d", commit2.ID, commit.ID)
	}
	if res2.TasksCreated != 1 {
		t.Errorf("re-dispatch tasks created: %d, want 1 (requeue)", res2.TasksCreated)
	}
	if yamlFetches != 2 {
		t.Errorf("yaml fetched %d times, want 2 (once per dispatch)", yamlFetches)
	}
}

// TestDispatchStoresDescriptions verifies the dispatch-time placeholder
// runs carry the yaml stage descriptions into the database: the build /
// unit / regression parent runs and every regression case child row.
func TestDispatchStoresDescriptions(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "descman", Email: "d@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-desc", Host: "h", Username: "u", PrivateKey: "k",
		Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	const sha = "dddddddddddddddddddddddddddddddddddddddd"
	svc := &Service{Store: s}
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		return sha, nil
	}
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, atSHA string, creds *GitCredentials) ([]byte, error) {
		return []byte(`version: 2
defaults:
  build:
    command: "make -j8"
presets:
  simple:
    description: "Simple regression test"
    command: "./run_simple"
matrix:
  - tags: [cpu]
    build:
      description: "Build the code with gcc and cmake"
    unit:
      command: "ctest -L unit"
      description: "Run unit tests"
    regression:
      description: "Run regression tests"
      use: [simple]
`), nil
	}

	commit, res := svc.DispatchForRef(testContext(), "main")
	if res.Err != nil {
		t.Fatalf("dispatch: %v", res.Err)
	}
	if res.TasksCreated != 1 {
		t.Fatalf("tasks created: %d, want 1", res.TasksCreated)
	}

	// The seeded placeholder runs carry the descriptions.
	runs, err := s.FindRunsByCommits(store.RunKindBuild, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatal(err)
	}
	build := runs[store.EnvCommit{Env: env.ID, Commit: commit.ID}]
	if build.Description != "Build the code with gcc and cmake" {
		t.Errorf("build run description: %q", build.Description)
	}
	if build.Status != store.StatusPending {
		t.Errorf("build run should be the pending placeholder: %q", build.Status)
	}
	runs, err = s.FindRunsByCommits(store.RunKindUnit, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatal(err)
	}
	unit := runs[store.EnvCommit{Env: env.ID, Commit: commit.ID}]
	if unit.Description != "Run unit tests" {
		t.Errorf("unit run description: %q", unit.Description)
	}
	runs, err = s.FindRunsByCommits(store.RunKindRegression, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatal(err)
	}
	reg := runs[store.EnvCommit{Env: env.ID, Commit: commit.ID}]
	if reg.Description != "Run regression tests" {
		t.Errorf("regression parent description: %q", reg.Description)
	}
	children, err := s.ListChildRuns(reg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 {
		t.Fatalf("want 1 case child, got %d", len(children))
	}
	if children[0].Name != "simple" || children[0].Description != "Simple regression test" {
		t.Errorf("case child: %+v", children[0])
	}
	if children[0].Status != store.StatusPending {
		t.Errorf("case child should be pending: %q", children[0].Status)
	}
}

// TestDispatchForRefBadRef surfaces resolver failures to the API caller.
func TestDispatchForRefBadRef(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Store: s}
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		return "", context.DeadlineExceeded
	}

	_, res := svc.DispatchForRef(testContext(), "no-such-branch")
	if res.Err == nil {
		t.Fatal("unresolvable ref must error")
	}
	roots, _ := s.ListRootTasks(10)
	if len(roots) != 0 {
		t.Errorf("no graphs should be created, got %d", len(roots))
	}
}

// TestDispatchForRefNoRepo reports the site-config gap like the webhook path.
func TestDispatchForRefNoRepo(t *testing.T) {
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	svc := &Service{Store: s}
	_, res := svc.DispatchForRef(testContext(), "")
	if res.Err == nil || !strings.Contains(res.Err.Error(), "no code repository") {
		t.Fatalf("want no-code-repo error, got %v", res.Err)
	}
}

// testContext is a background context for dispatch tests.
func testContext() context.Context { return context.Background() }
