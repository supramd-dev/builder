package runner

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"md-builder/server/store"
)

func newRunnerStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedRunnerFixture(t *testing.T, s *store.Store, entry MergedEntry) (*store.TestEnvironment, *store.Commit, *store.Job) {
	t.Helper()
	u := &store.User{Username: "runner-" + t.Name(), Email: "r@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-" + t.Name(), Host: "h", Username: "u", PrivateKey: "k", Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	commit := &store.Commit{Repo: "group/code", SHA: "deadbeef" + t.Name(), PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	cfgJSON, _ := json.Marshal(entry)
	_, err := s.CreateJobs([]*store.Job{{
		CommitID: commit.ID, EnvironmentID: env.ID, Tags: "cpu",
		Config: string(cfgJSON), TestInputRef: "main",
	}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimNextJob(0)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	return env, commit, claimed
}

func sampleReportOutput(unitStatus, unitSum, regStatus, regSum string) string {
	return fmt.Sprintf(`===MD-BUILDER-REPORT-BEGIN===
unit-status %s
unit-summary %s
regression-status %s
regression-summary %s
===MD-BUILDER-REPORT-END===
`, unitStatus, unitSum, regStatus, regSum)
}

func TestRunnerRecordsTestRunsFromReport(t *testing.T) {
	s := newRunnerStore(t)
	entry := MergedEntry{
		Tags:       []string{"cpu"},
		Timeout:    300,
		Build:      BuildConfig{Generator: GeneratorCMake, CMakeFlags: "-DX=1", Threads: 4},
		Unit:       &EnvConfig{Command: "ctest -L unit", Timeout: 100},
		Regression: &EnvConfig{Command: "python3 run.py", Timeout: 200},
	}
	env, commit, job := seedRunnerFixture(t, s, entry)

	exec := func(env *store.TestEnvironment, script string, timeout int) (string, int, error) {
		if timeout <= 0 {
			t.Errorf("timeout must be positive: %d", timeout)
		}
		return sampleReportOutput("passed", "all 8 tests passed", "failed", "max err 3e-7"), 0, nil
	}
	(&Runner{Store: s, Exec: exec}).Run(job)

	// Job done.
	got, _ := s.ListJobs(5)
	var row store.Job
	for _, j := range got {
		if j.ID == job.ID {
			row = j
		}
	}
	if row.Status != store.JobDone {
		t.Errorf("job status: want done, got %s", row.Status)
	}

	// Two test runs recorded.
	runs, err := s.FindRunsByCommits(store.RunKindUnit, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatal(err)
	}
	unit, ok := runs[store.EnvCommit{Env: env.ID, Commit: commit.ID}]
	if !ok || unit.Status != store.StatusPassed || unit.Summary != "all 8 tests passed" {
		t.Errorf("unit run wrong: %+v", unit)
	}
	regRuns, err := s.FindRunsByCommits(store.RunKindRegression, []int64{env.ID}, []int64{commit.ID})
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := regRuns[store.EnvCommit{Env: env.ID, Commit: commit.ID}]
	if !ok || reg.Status != store.StatusFailed || reg.Summary != "max err 3e-7" {
		t.Errorf("regression run wrong: %+v", reg)
	}
}

func TestRunnerFailsJobOnSSHError(t *testing.T) {
	s := newRunnerStore(t)
	entry := MergedEntry{Tags: []string{"cpu"}, Timeout: 100, Unit: &EnvConfig{Command: "true"}}
	_, _, job := seedRunnerFixture(t, s, entry)

	exec := func(env *store.TestEnvironment, script string, timeout int) (string, int, error) {
		return "", -1, fmt.Errorf("dial tcp: connection refused")
	}
	(&Runner{Store: s, Exec: exec}).Run(job)

	got, _ := s.ListJobs(5)
	var row store.Job
	for _, j := range got {
		if j.ID == job.ID {
			row = j
		}
	}
	if row.Status != store.JobFailed || row.Error == "" {
		t.Errorf("failed job wrong: %+v", row)
	}
}

func TestRunnerFailsJobOnMissingReport(t *testing.T) {
	s := newRunnerStore(t)
	entry := MergedEntry{Tags: []string{"cpu"}, Timeout: 100, Unit: &EnvConfig{Command: "true"}}
	_, _, job := seedRunnerFixture(t, s, entry)

	exec := func(env *store.TestEnvironment, script string, timeout int) (string, int, error) {
		return "clone: fatal: not found\n", 0, nil
	}
	(&Runner{Store: s, Exec: exec}).Run(job)

	got, _ := s.ListJobs(5)
	var row store.Job
	for _, j := range got {
		if j.ID == job.ID {
			row = j
		}
	}
	if row.Status != store.JobFailed {
		t.Errorf("job should be failed: %+v", row)
	}
	if row.Error == "" {
		t.Error("error message should be populated")
	}
}

func TestDispatcherFullChain(t *testing.T) {
	s := newRunnerStore(t)
	// Seed site config.
	cfg := &store.SiteConfig{ID: 1, CodeRepo: "https://gitlab.example.com/group/code",
		TestInputRepo: "https://gitlab.example.com/group/tests", TestRepoRef: "main"}
	if err := s.SaveSiteConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Seed enabled environment with a matching tag.
	u := &store.User{Username: "disp-" + t.Name(), Email: "d@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-disp", Host: "h", Username: "u", PrivateKey: "k",
		Tags: "cpu,mpi", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	commit := &store.Commit{Repo: "group/code", SHA: "cafe" + t.Name(), PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}

	yaml := `version: 1
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      command: "python3 run.py"
  - tags: [gpu, cuda]
    unit:
      command: "ctest -L unit"
`
	fetcher := func(repoURL, sha string) ([]byte, error) {
		if repoURL == "" {
			t.Error("fetcher should receive the code repo URL")
		}
		return []byte(yaml), nil
	}
	d := &dispatcherShim{store: s, fetch: fetcher}
	jobsCreated, entriesSkipped, err := d.DispatchForCommit(commit)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if jobsCreated != 1 {
		t.Errorf("jobs created: want 1, got %d", jobsCreated)
	}
	if entriesSkipped != 1 {
		t.Errorf("entries skipped: want 1 (gpu, no matching env), got %d", entriesSkipped)
	}

	// The created job should target the cpu env.
	jobs, err := s.ListJobs(10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, j := range jobs {
		if j.CommitID == commit.ID && j.EnvironmentID == env.ID && j.Tags == "cpu" {
			found = true
		}
	}
	if !found {
		t.Errorf("job targeting cpu env not found: %+v", jobs)
	}
}

// dispatcherShim wraps the worker.Dispatcher-like behavior without importing
// the worker package (which would create an import cycle in tests). It
// reimplements DispatchForCommit using the same store + runner functions.
type dispatcherShim struct {
	store *store.Store
	fetch YAMLFetcher
}

func (d *dispatcherShim) DispatchForCommit(commit *store.Commit) (jobsCreated, entriesSkipped int, err error) {
	cfg, err := d.store.GetSiteConfig()
	if err != nil {
		return 0, 0, err
	}
	yamlBytes, err := d.fetch(cfg.CodeRepo, commit.SHA)
	if err != nil {
		return 0, 0, err
	}
	entries, err := ParseConfig(yamlBytes)
	if err != nil {
		return 0, 0, err
	}
	envs, err := d.store.ListEnabledEnvironments()
	if err != nil {
		return 0, 0, err
	}
	var jobs []*store.Job
	for _, entry := range entries {
		env := PickEnvironment(entry, envs)
		if env == nil {
			entriesSkipped++
			continue
		}
		cfgJSON, _ := json.Marshal(entry)
		jobs = append(jobs, &store.Job{
			CommitID: commit.ID, EnvironmentID: env.ID,
			Tags: SortedTagString(entry.Tags), Config: string(cfgJSON),
			TestInputRef: cfg.TestRepoRef,
		})
	}
	if len(jobs) > 0 {
		created, err := d.store.CreateJobs(jobs)
		if err != nil {
			return 0, entriesSkipped, err
		}
		jobsCreated = len(created)
	}
	return jobsCreated, entriesSkipped, nil
}
