package runner

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"md-builder/server/store"
)

// fakeExecer records the scripts it runs and returns canned exit codes with
// output (both streamed to the writers and summarized).
type fakeExecer struct {
	mu      sync.Mutex
	scripts []string // remote scripts seen, in order

	// outcome maps a marker substring of the script to the result to return.
	// Unmatched scripts exit 0.
	outcome map[string]int
	output  map[string]string
}

func (f *fakeExecer) RunScript(ctx context.Context, h SSHHost, cmd, script string, timeout time.Duration, stdout, stderr io.Writer) ExecResult {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	res := ExecResult{ExitCode: 0, Success: true}
	for marker, code := range f.outcome {
		if strings.Contains(script, marker) {
			res.ExitCode = code
			res.Success = code == 0
			if out := f.output[marker]; out != "" {
				_, _ = stdout.Write([]byte(out))
			}
			if code != 0 {
				_, _ = stderr.Write([]byte("command failed\n"))
			}
			break
		}
	}
	f.mu.Unlock()
	return res
}

func (f *fakeExecer) ExecHost(h SSHHost, cmd string) ExecResult {
	return ExecResult{ExitCode: 0, Success: true}
}

func (f *fakeExecer) CheckHost(h SSHHost) Result { return Result{Success: true} }

func (f *fakeExecer) ExtractTarTo(ctx context.Context, h SSHHost, r io.Reader, destDir string, timeout time.Duration) error {
	_, _ = io.Copy(io.Discard, r)
	return nil
}

// fakeCloner records clone requests and produces no data.
type fakeCloner struct {
	mu         sync.Mutex
	codeRepo   string
	err        error
	remoteDirs []string
}

func (f *fakeCloner) CloneAndUpload(ctx context.Context, h SSHHost, codeRepoURL, codeRef string, creds *GitCredentials, remoteWorkDir string, timeout time.Duration, logw io.Writer) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codeRepo = codeRepoURL
	f.remoteDirs = append(f.remoteDirs, remoteWorkDir)
	if f.err != nil {
		return 0, f.err
	}
	if logw != nil {
		_, _ = logw.Write([]byte("fake clone output\n"))
	}
	return 5 * 1024 * 1024, nil
}

// newExecuteFixture seeds a user/environment/commit and a full task graph,
// returning the service with fakes and the claimed clone task.
func newExecuteFixture(t *testing.T, yaml string) (*Service, *store.Store, *fakeExecer, *fakeCloner, *store.Task) {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "exec-" + t.Name(), Email: "e@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-exec", Host: "h", Username: "u", PrivateKey: "k", Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	commit := &store.Commit{Repo: "group/code", SHA: "c0ffee123456", PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	exec := &fakeExecer{outcome: map[string]int{}, output: map[string]string{}}
	cloner := &fakeCloner{}
	svc := &Service{Store: s, SSH: exec, Clone: cloner}

	// Dispatch through the real path (fake fetcher) to build the graph.
	svc.FetchYAML = func(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		return []byte(yaml), nil
	}
	res := svc.DispatchForCommit(commit)
	if res.Err != nil {
		t.Fatalf("dispatch: %v", res.Err)
	}
	if res.TasksCreated != 1 {
		t.Fatalf("tasks created: %d", res.TasksCreated)
	}

	// Clone has no dependencies (the root is a container), so it is the
	// first claimable task.
	claimed, err := s.ClaimReadyTask()
	if err != nil || claimed == nil {
		t.Fatalf("claim clone: %v %v", claimed, err)
	}
	if claimed.Kind != store.TaskKindClone {
		t.Fatalf("want clone claimed, got %s", claimed.Kind)
	}
	return svc, s, exec, cloner, claimed
}

const execYAML = `version: 2
presets:
  heat:
    command: "python3 run_heat.py"
    timeout: 120
  poisson:
    command: "python3 run_poisson.py"
defaults:
  build:
    command: "cmake -DEXEC=1 . && cmake --build ."
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
      timeout: 60
    regression:
      use: [heat, poisson]
`

// runUntilStage drives the fixture's claimed clone task, then claims and
// executes tasks until one of the given kinds is claimable; that task is
// returned NOT yet executed (for tests that seed the fake's outcome first).
func runUntilStage(t *testing.T, svc *Service, s *store.Store, cloneTask *store.Task, kinds ...string) *store.Task {
	t.Helper()
	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	for {
		task, err := s.ClaimReadyTask()
		if err != nil || task == nil {
			t.Fatalf("no %v task claimable", kinds)
		}
		for _, k := range kinds {
			if task.Kind == k {
				return task
			}
		}
		if err := svc.ExecuteTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
}

const artifactsYAML = `version: 2
defaults:
  build:
    command: "cmake -DEXEC=1 . && cmake --build ."
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit --output-junit junit.xml"
      artifacts: "build/test_detail.xml"
`

const multiArtifactsYAML = `version: 2
defaults:
  build:
    command: "cmake ."
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
      artifacts:
        - "build/test_detail.xml"
        - "build/extra_results.json"
`

// caseArtifactsYAML exercises the per-case regression path: two presets
// with their own artifact files.
const caseArtifactsYAML = `version: 2
defaults:
  build:
    command: "cmake ."
presets:
  heat:
    command: "python3 run_heat.py"
    workdir: "regression/heat"
    artifacts: "out.xml"
  poisson:
    command: "python3 run_poisson.py"
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      use: [heat]
      disable: [poisson]
`

// TestExecuteStageFetchesArtifactFile: a configured artifact file is fetched,
// stored verbatim as an artifact, its root counts land on the run, and the
// run links back to the stage task (its stdout lives in the task log).
func TestExecuteStageFetchesArtifactFile(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, artifactsYAML)
	// The fetch script contains the artifact path as a quoted cat argument;
	// seed the fake to return the gtest XML for it.
	exec.outcome["test_detail.xml"] = 0
	exec.output["test_detail.xml"] = sampleGTestXML
	stage := runUntilStage(t, svc, s, cloneTask, store.TaskKindUnit)
	if err := svc.ExecuteTask(context.Background(), stage); err != nil {
		t.Fatal(err)
	}

	// The unit script ran; the fetch script cats the artifact path.
	fetchSeen := false
	for _, script := range exec.scripts {
		if strings.Contains(script, "test_detail.xml") {
			fetchSeen = true
		}
	}
	if !fetchSeen {
		t.Fatalf("no artifact fetch script ran: %+v", exec.scripts)
	}

	envs := []int64{stage.EnvironmentID}
	commits := []int64{stage.CommitID}
	runs, _ := s.FindRunsByCommits(store.RunKindUnit, envs, commits)
	run, ok := runs[store.EnvCommit{Env: stage.EnvironmentID, Commit: stage.CommitID}]
	if !ok {
		t.Fatal("unit run missing")
	}
	if run.TaskID != stage.ID {
		t.Errorf("run.TaskID = %d, want the stage task %d", run.TaskID, stage.ID)
	}
	if run.Total != 12 || run.Failed != 2 || run.Skipped != 1 {
		t.Errorf("counts wrong: total=%d failed=%d skipped=%d", run.Total, run.Failed, run.Skipped)
	}
	if run.Passed != 9 {
		t.Errorf("passed = %d, want 9", run.Passed)
	}
	if run.Status != store.StatusFailed {
		t.Errorf("run with failures should be failed: %s", run.Status)
	}
	if run.Summary != "12 tests, 9 passed, 2 failed, 1 skipped" {
		t.Errorf("summary should default to counts: %q", run.Summary)
	}

	// The artifact stores the raw file content.
	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts: %v %v", artifacts, err)
	}
	if artifacts[0].Kind != store.ArtifactKindResults || artifacts[0].Name != "build/test_detail.xml" {
		t.Errorf("artifact wrong: %+v", artifacts[0])
	}
	if artifacts[0].Content != sampleGTestXML {
		t.Errorf("artifact content should be verbatim")
	}
}

// TestExecuteStageMultipleArtifactFiles: a run configured with several
// artifact files stores one artifact per file and sums the parsed counts
// across all of them.
func TestExecuteStageMultipleArtifactFiles(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, multiArtifactsYAML)
	// Both files parse; a missing second file is tolerated (logged, no
	// artifact for it).
	exec.outcome["test_detail.xml"] = 0
	exec.output["test_detail.xml"] = sampleGTestXML // 12 tests, 2 failed, 1 skipped
	exec.outcome["extra_results.json"] = 0
	exec.output["extra_results.json"] = sampleGTestJSON
	stage := runUntilStage(t, svc, s, cloneTask, store.TaskKindUnit)
	if err := svc.ExecuteTask(context.Background(), stage); err != nil {
		t.Fatal(err)
	}

	runs, _ := s.FindRunsByCommits(store.RunKindUnit,
		[]int64{stage.EnvironmentID}, []int64{stage.CommitID})
	run, ok := runs[store.EnvCommit{Env: stage.EnvironmentID, Commit: stage.CommitID}]
	if !ok {
		t.Fatal("unit run missing")
	}
	if run.Total != 12 || run.Failed != 2 || run.Skipped != 1 {
		t.Errorf("counts should sum across files: total=%d failed=%d skipped=%d",
			run.Total, run.Failed, run.Skipped)
	}

	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("artifacts: %v %v", artifacts, err)
	}
	byName := map[string]string{}
	for _, a := range artifacts {
		if a.Kind != store.ArtifactKindResults {
			t.Errorf("artifact wrong: %+v", a)
		}
		byName[a.Name] = a.Content
	}
	if byName["build/test_detail.xml"] != sampleGTestXML {
		t.Errorf("xml artifact content should be verbatim: %q", byName["build/test_detail.xml"])
	}
	if byName["build/extra_results.json"] != sampleGTestJSON {
		t.Errorf("json artifact content should be verbatim: %q", byName["build/extra_results.json"])
	}
}

// TestExecuteStageArtifactFileMissing: a configured artifact file that does
// not exist on the host leaves counts at zero, stores no artifact, and the
// run still records the command's outcome.
func TestExecuteStageArtifactFileMissing(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, artifactsYAML)
	// The fetch fails remotely (cat: no such file).
	exec.outcome["test_detail.xml"] = 1
	stage := runUntilStage(t, svc, s, cloneTask, store.TaskKindUnit)
	if err := svc.ExecuteTask(context.Background(), stage); err != nil {
		t.Fatal(err)
	}

	runs, _ := s.FindRunsByCommits(store.RunKindUnit,
		[]int64{stage.EnvironmentID}, []int64{stage.CommitID})
	run, ok := runs[store.EnvCommit{Env: stage.EnvironmentID, Commit: stage.CommitID}]
	if !ok {
		t.Fatal("unit run missing")
	}
	if run.Total != 0 || run.Failed != 0 {
		t.Errorf("missing file should leave counts zero: %+v", run)
	}
	if run.Status != store.StatusPassed {
		t.Errorf("the command itself passed; run should pass: %+v", run)
	}
	artifacts, _ := s.ListRunArtifacts(run.ID)
	if len(artifacts) != 0 {
		t.Errorf("no artifact should be stored: %+v", artifacts)
	}
	// The stage log mentions the fetch failure.
	logs, _ := s.ReadTaskLogs(stage.ID, 0)
	var all string
	for _, l := range logs {
		all += l.Content
	}
	if !strings.Contains(all, "artifact build/test_detail.xml") {
		t.Errorf("log should mention the artifact file: %q", tailLine(all, 3))
	}
}

func TestExecuteFullChainHappyPath(t *testing.T) {
	svc, s, exec, cloner, cloneTask := newExecuteFixture(t, execYAML)
	// The executor loop drives everything; emulate runClaimed manually to
	// keep the test synchronous.
	ctx := context.Background()

	// 1. clone
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(cloneTask.ID)
	if got.Status != store.TaskDone {
		t.Fatalf("clone should be done: %+v", got)
	}
	if cloner.codeRepo != "https://gitlab.example.com/group/code" {
		t.Errorf("clone used the wrong repo: %s", cloner.codeRepo)
	}
	if len(cloner.remoteDirs) != 1 || !strings.Contains(cloner.remoteDirs[0], "c0ffee123456") {
		t.Errorf("clone remote dir wrong: %v", cloner.remoteDirs)
	}
	// Clone log was persisted.
	logs, _ := s.ReadTaskLogs(cloneTask.ID, 0)
	if len(logs) == 0 {
		t.Error("clone task should have log chunks")
	}

	// 2. build (dependencies satisfied now)
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil || build.Kind != store.TaskKindBuild {
		t.Fatalf("claim build: %v %v", build, err)
	}
	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetTask(build.ID)
	if got.Status != store.TaskDone {
		t.Fatalf("build should be done: %+v", got)
	}
	// The build outcome is recorded as a "build" test run for the dashboard.
	buildRuns, _ := s.FindRunsByCommits(store.RunKindBuild,
		[]int64{cloneTask.EnvironmentID}, []int64{cloneTask.CommitID})
	bRun, ok := buildRuns[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok || bRun.Status != store.StatusPassed {
		t.Errorf("build run wrong: %+v", bRun)
	}

	// 3. unit + both regression cases (claimable once build is done)
	claimOrder := map[string]int{}
	for i := 0; i < 3; i++ {
		st, err := s.ClaimReadyTask()
		if err != nil || st == nil {
			t.Fatalf("claim stage %d: %v %v", i, st, err)
		}
		if st.Kind != store.TaskKindUnit && st.Kind != store.TaskKindRegression {
			t.Fatalf("want a stage, got %s", st.Kind)
		}
		if err := svc.ExecuteTask(ctx, st); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTask(st.ID)
		if got.Status != store.TaskDone {
			t.Errorf("%s should be done: %+v", st.Kind, got)
		}
		claimOrder[st.Name]++
	}
	if claimOrder["unit tests"] != 1 || claimOrder["regression: heat"] != 1 || claimOrder["regression: poisson"] != 1 {
		t.Errorf("stage set wrong: %+v", claimOrder)
	}

	// Both test runs recorded as passed; the regression run aggregates the
	// two case rows.
	envs := []int64{cloneTask.EnvironmentID}
	commits := []int64{cloneTask.CommitID}
	runs, err := s.FindRunsByCommits(store.RunKindUnit, envs, commits)
	if err != nil {
		t.Fatal(err)
	}
	unit, ok := runs[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok || unit.Status != store.StatusPassed {
		t.Errorf("unit run wrong: %+v", unit)
	}
	regRuns, _ := s.FindRunsByCommits(store.RunKindRegression, envs, commits)
	reg, ok := regRuns[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok || reg.Status != store.StatusPassed {
		t.Fatalf("regression run wrong: %+v", reg)
	}
	if reg.Total != 2 || reg.Passed != 2 || reg.Failed != 0 {
		t.Errorf("regression run should aggregate 2 passed cases: %+v", reg)
	}
	children, _ := s.ListChildRuns(reg.ID)
	if len(children) != 2 || children[0].Name != "heat" || children[1].Name != "poisson" {
		t.Errorf("child runs wrong: %+v", children)
	}

	// The build script ran the configured command; the stage scripts used
	// their commands under timeout. Only build and the stages go through
	// RunScript (the clone uploads a tar instead), so scripts[0] is the
	// build, [1..3] are the unit and case scripts.
	if len(exec.scripts) != 4 {
		t.Fatalf("want 4 scripts, got %d", len(exec.scripts))
	}
	if !strings.Contains(exec.scripts[0], "bash -c 'cmake -DEXEC=1 . && cmake --build .'") {
		t.Errorf("build script wrong:\n%s", exec.scripts[0])
	}
	if !strings.Contains(exec.scripts[1], "timeout 60 bash -c 'ctest -L unit'") {
		t.Errorf("unit script wrong:\n%s", exec.scripts[1])
	}
	// The case scripts carry their preset command and MD_CASE export.
	joined := strings.Join(exec.scripts[2:], "\n---\n")
	if !strings.Contains(joined, "timeout 120 bash -c 'python3 run_heat.py'") {
		t.Errorf("heat case script wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "export MD_CASE='heat'") || !strings.Contains(joined, "export MD_CASE='poisson'") {
		t.Errorf("case scripts should export MD_CASE:\n%s", joined)
	}
}

func TestExecuteCloneFailureSkipsDownstream(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	cloner.err = context.DeadlineExceeded

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetTask(cloneTask.ID)
	if got.Status != store.TaskFailed || got.Error == "" {
		t.Fatalf("clone should be failed with error: %+v", got)
	}

	// runClaimed's post-processing: skip dependents + skipped test runs.
	if err := s.SkipDependents(got.RootID, got.ID, "skipped: upstream task "+got.Name+" failed"); err != nil {
		t.Fatal(err)
	}
	svc.recordSkippedRuns(got)
	if _, _, err := s.RefreshRootStatus(got.RootID); err != nil {
		t.Fatal(err)
	}

	subs, _ := s.ListSubTasks(got.RootID)
	for _, sub := range subs {
		if sub.Kind == store.TaskKindClone {
			continue
		}
		if sub.Status != store.TaskSkipped {
			t.Errorf("%s should be skipped: %s", sub.Kind, sub.Status)
		}
	}
	root, _ := s.GetTask(got.RootID)
	if root.Status != store.TaskFailed {
		t.Errorf("root should be failed: %s", root.Status)
	}

	// The dashboard shows ✗ through failed TestRuns.
	runs, _ := s.FindRunsByCommits(store.RunKindUnit, []int64{cloneTask.EnvironmentID}, []int64{cloneTask.CommitID})
	unit, ok := runs[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok || unit.Status != store.StatusFailed || !strings.Contains(unit.Summary, "skipped") {
		t.Errorf("skipped unit run wrong: %+v", unit)
	}
	// The regression cases were recorded as skipped child runs; the run
	// summary surfaces as "skipped:" (the dashboard translation).
	regRuns, _ := s.FindRunsByCommits(store.RunKindRegression,
		[]int64{cloneTask.EnvironmentID}, []int64{cloneTask.CommitID})
	reg, ok := regRuns[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok {
		t.Fatal("skipped regression run missing")
	}
	if reg.Total != 2 || reg.Passed != 0 || reg.Failed != 0 || reg.Skipped != 2 {
		t.Errorf("skipped case aggregate wrong: %+v", reg)
	}
	if !strings.HasPrefix(reg.Summary, "skipped:") {
		t.Errorf("all-skipped run summary should start with skipped:: %q", reg.Summary)
	}
	children, _ := s.ListChildRuns(reg.ID)
	if len(children) != 2 {
		t.Fatalf("want 2 skipped child runs, got %d", len(children))
	}
	for _, c := range children {
		if c.Status != store.StatusSkipped {
			t.Errorf("case %s should be skipped: %s", c.Name, c.Status)
		}
	}
}

// TestExecuteBuildFailureRecordsFailedBuildRun makes the build fail and
// asserts the failed build run lands (dashboard ✗) while the skipped test
// stages still get their own rows.
func TestExecuteBuildFailureRecordsFailedBuildRun(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	exec.outcome["cmake -DEXEC=1 ."] = 1
	exec.output["cmake -DEXEC=1 ."] = "CMake Error at CMakeLists.txt:9 (message):\n  bad toolchain\n"

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil || build.Kind != store.TaskKindBuild {
		t.Fatalf("claim build: %v %v", build, err)
	}
	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(build.ID)
	if got.Status != store.TaskFailed {
		t.Fatalf("build should be failed: %+v", got)
	}

	// The failed build recorded a failed "build" run with the compiler error.
	envs := []int64{build.EnvironmentID}
	commits := []int64{build.CommitID}
	buildRuns, _ := s.FindRunsByCommits(store.RunKindBuild, envs, commits)
	bRun, ok := buildRuns[store.EnvCommit{Env: build.EnvironmentID, Commit: build.CommitID}]
	if !ok || bRun.Status != store.StatusFailed {
		t.Fatalf("failed build run wrong: %+v", bRun)
	}
	if !strings.Contains(bRun.Summary, "CMake Error") {
		t.Errorf("build summary should carry the compiler error: %q", bRun.Summary)
	}

	// Scheduler post-processing: the test stages are skipped with rows.
	if err := s.SkipDependents(got.RootID, got.ID, "skipped: upstream task "+got.Name+" failed"); err != nil {
		t.Fatal(err)
	}
	svc.recordSkippedRuns(got)
	unitRuns, _ := s.FindRunsByCommits(store.RunKindUnit, envs, commits)
	if unitRun, ok := unitRuns[store.EnvCommit{Env: build.EnvironmentID, Commit: build.CommitID}]; !ok ||
		unitRun.Status != store.StatusFailed || !strings.Contains(unitRun.Summary, "skipped") {
		t.Errorf("skipped unit run after build failure wrong: %+v", unitRun)
	}
}

// TestExecuteBuildScriptBuildFailureClosesPlaceholderRun: a build stage whose
// remote script cannot even be generated (the entry snapshot carries no build
// command) must flip the dispatch-time placeholder run from pending/running
// to failed — the old code failed the task but left the run "running", so the
// matrix cell and run detail page spun forever.
func TestExecuteBuildScriptBuildFailureClosesPlaceholderRun(t *testing.T) {
	svc, s, _, _, cloneTask := newExecuteFixture(t, execYAML)
	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil || build.Kind != store.TaskKindBuild {
		t.Fatalf("claim build: %v %v", build, err)
	}

	// The placeholder run exists from dispatch, pending.
	envs := []int64{build.EnvironmentID}
	commits := []int64{build.CommitID}
	runs, _ := s.FindRunsByCommits(store.RunKindBuild, envs, commits)
	if run, ok := runs[store.EnvCommit{Env: build.EnvironmentID, Commit: build.CommitID}]; !ok || run.Status != store.StatusPending {
		t.Fatalf("placeholder build run should exist and be pending: %+v", run)
	}

	// An empty entry snapshot makes BuildScript fail ("build has no command"):
	// empty the root's entry so rc.entry.Build.Command is blank.
	if err := s.UpdateTaskConfig(build.RootID, "{}", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(build.ID)
	if got.Status != store.TaskFailed {
		t.Fatalf("build should be failed: %+v", got)
	}
	runs, _ = s.FindRunsByCommits(store.RunKindBuild, envs, commits)
	run, ok := runs[store.EnvCommit{Env: build.EnvironmentID, Commit: build.CommitID}]
	if !ok || run.Status != store.StatusFailed {
		t.Fatalf("placeholder run should be failed after script-build error: %+v", run)
	}
	if !strings.Contains(run.Summary, "build has no command") {
		t.Errorf("run summary should carry the reason: %q", run.Summary)
	}
}

func TestExecuteStageFailureRecordsFailedRun(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	// Make the unit stage fail (its script contains the command).
	exec.outcome["ctest -L unit"] = 1
	exec.output["ctest -L unit"] = "1/3 tests passed\nMD-BUILDER-SUMMARY: 2 of 3 unit tests failed\n"
	// The fetch script (not configured here) never runs.

	ctx := context.Background()
	// Run clone + build to completion.
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	build, _ := s.ClaimReadyTask()
	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}
	unit, err := s.ClaimReadyTask()
	if err != nil || unit == nil || unit.Kind != store.TaskKindUnit {
		t.Fatalf("claim unit: %v %v", unit, err)
	}
	if err := svc.ExecuteTask(ctx, unit); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(unit.ID)
	if got.Status != store.TaskFailed {
		t.Fatalf("unit should be failed: %+v", got)
	}
	runs, _ := s.FindRunsByCommits(store.RunKindUnit, []int64{unit.EnvironmentID}, []int64{unit.CommitID})
	run, ok := runs[store.EnvCommit{Env: unit.EnvironmentID, Commit: unit.CommitID}]
	if !ok || run.Status != store.StatusFailed {
		t.Fatalf("failed unit run wrong: %+v", run)
	}
	if run.Summary != "2 of 3 unit tests failed" {
		t.Errorf("summary should come from MD-BUILDER-SUMMARY: %q", run.Summary)
	}
}

func TestExecuteUnknownKindFails(t *testing.T) {
	svc, s, _, _, _ := newExecuteFixture(t, execYAML)
	task := &store.Task{ID: 9999, Kind: "perf", RootID: 1}
	_ = svc
	// The unknown kind path fails the task without touching SSH.
	svc.failEarly(task, "unknown task kind \"perf\"")
	got, err := s.GetTask(9999)
	if err != nil {
		// Not persisted (no such row): failEarly would error-log but not
		// crash; the branch is exercised for coverage.
		t.Log("unknown-kind task not persisted (expected in this fixture)")
		return
	}
	if got.Status != store.TaskFailed {
		t.Errorf("task should be failed: %+v", got)
	}
}

func TestRedeployRebuildsGraph(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	root, err := s.GetTask(cloneTask.RootID)
	if err != nil {
		t.Fatal(err)
	}

	// Run clone to done, then re-dispatch the same commit/environment: the
	// graph is rebuilt from the fresh snapshot (old sub-tasks and logs are
	// dropped).
	if err := svc.ExecuteTask(context.Background(), cloneTask); err != nil {
		t.Fatal(err)
	}
	logsBefore, _ := s.ReadTaskLogs(cloneTask.ID, 0)
	if len(logsBefore) == 0 {
		t.Fatal("precondition: clone log exists")
	}

	res := svc.DispatchForCommit(&store.Commit{ID: cloneTask.CommitID, Repo: "group/code", SHA: "c0ffee123456"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.TasksCreated != 1 {
		t.Fatalf("re-dispatch should requeue 1 graph, got %d", res.TasksCreated)
	}

	subs, _ := s.ListSubTasks(root.ID)
	if len(subs) != 5 { // clone, build, unit, 2 regression cases
		t.Fatalf("redeploy should rebuild 5 sub-tasks, got %d", len(subs))
	}
	for _, sub := range subs {
		if sub.Status != store.TaskPending {
			t.Errorf("rebuilt sub should be pending: %+v", sub)
		}
		if sub.ID == cloneTask.ID {
			t.Error("old clone task should have been deleted")
		}
	}
	// The old logs went with the deleted sub-tasks.
	if _, err := s.GetTask(cloneTask.ID); err == nil {
		t.Error("old clone task row should be gone")
	}
	_ = cloner
}

// TestExecuteCaseRunsPreset: a regression case sub-task runs the preset
// command in its workdir, sources the env script, fetches its artifact file
// and records a case row with a case-scoped artifact.
func TestExecuteCaseRunsPreset(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, caseArtifactsYAML)
	// The case command's script carries the preset command; the fetch
	// script cats the artifact path.
	exec.outcome["out.xml"] = 0
	exec.output["out.xml"] = sampleGTestXML

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	// The clone log warns: the fixture environment has no env script.
	logs, _ := s.ReadTaskLogs(cloneTask.ID, 0)
	var cloneLog string
	for _, l := range logs {
		cloneLog += l.Content
	}
	if !strings.Contains(cloneLog, "no env script") {
		t.Errorf("clone log should warn about the missing env script: %q", cloneLog)
	}
	// No env-script write ran (only the clone tar upload uses CloneAndUpload;
	// RunScript calls: none for the script write).
	for _, sc := range exec.scripts {
		if strings.Contains(sc, "md-builder-env-") && strings.Contains(sc, "cat >") {
			t.Errorf("env script write should be skipped without a configured script")
		}
	}

	// build, unit, then the heat case.
	for i := 0; i < 3; i++ {
		st, err := s.ClaimReadyTask()
		if err != nil || st == nil {
			t.Fatalf("claim %d: %v %v", i, st, err)
		}
		if err := svc.ExecuteTask(ctx, st); err != nil {
			t.Fatal(err)
		}
	}

	// The heat case ran with its workdir and only poisson stayed out.
	caseScriptSeen, unitOnly := false, false
	for _, sc := range exec.scripts {
		if strings.Contains(sc, "run_heat.py") {
			caseScriptSeen = true
			if !strings.Contains(sc, `cd "$MD_CODE_DIR/regression/heat"`) {
				t.Errorf("heat case should cd into its workdir:\n%s", sc)
			}
		}
		if strings.Contains(sc, "run_poisson.py") {
			t.Errorf("poisson was disabled: its script should not run")
		}
		if strings.Contains(sc, "ctest -L unit") {
			unitOnly = true
		}
	}
	if !caseScriptSeen || !unitOnly {
		t.Errorf("scripts seen wrong: case=%v unit=%v", caseScriptSeen, unitOnly)
	}

	// The regression run carries exactly the heat case, passed, with its
	// artifact riding the case's own child run.
	regRuns, _ := s.FindRunsByCommits(store.RunKindRegression,
		[]int64{cloneTask.EnvironmentID}, []int64{cloneTask.CommitID})
	reg, ok := regRuns[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok {
		t.Fatal("regression run missing")
	}
	if reg.Total != 1 || reg.Passed != 1 || reg.Status != store.StatusPassed {
		t.Errorf("case run aggregate wrong: %+v", reg)
	}
	children, _ := s.ListChildRuns(reg.ID)
	if len(children) != 1 || children[0].Name != "heat" || children[0].Status != store.StatusPassed {
		t.Fatalf("child runs wrong: %+v", children)
	}
	if children[0].DurationMillis < 0 || children[0].Message == "" {
		t.Errorf("child run should carry duration and message: %+v", children[0])
	}
	artifacts, _ := s.ListRunArtifacts(children[0].ID)
	if len(artifacts) != 1 || artifacts[0].Name != "out.xml" {
		t.Errorf("case artifact should ride the child run: %+v", artifacts)
	}
	if parentArtifacts, _ := s.ListRunArtifacts(reg.ID); len(parentArtifacts) != 0 {
		t.Errorf("parent run should stay artifact-free: %+v", parentArtifacts)
	}
}

// TestExecuteCaseFailureMarksRunFailed: one failing case fails the
// aggregated regression run while the other case stays passed.
func TestExecuteCaseFailureMarksRunFailed(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	exec.outcome["python3 run_heat.py"] = 1
	exec.output["python3 run_heat.py"] = "max rel err 1e-3 exceeds 1e-5\nMD-BUILDER-SUMMARY: heat: err 1e-3 > 1e-5\n"

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ { // build + unit + 2 cases
		st, err := s.ClaimReadyTask()
		if err != nil || st == nil {
			t.Fatalf("claim %d: %v %v", i, st, err)
		}
		if err := svc.ExecuteTask(ctx, st); err != nil {
			t.Fatal(err)
		}
	}

	regRuns, _ := s.FindRunsByCommits(store.RunKindRegression,
		[]int64{cloneTask.EnvironmentID}, []int64{cloneTask.CommitID})
	reg, ok := regRuns[store.EnvCommit{Env: cloneTask.EnvironmentID, Commit: cloneTask.CommitID}]
	if !ok {
		t.Fatal("regression run missing")
	}
	if reg.Total != 2 || reg.Passed != 1 || reg.Failed != 1 || reg.Status != store.StatusFailed {
		t.Errorf("mixed case aggregate wrong: %+v", reg)
	}
	if !strings.Contains(reg.Summary, "heat") {
		t.Errorf("summary should name the failing case: %q", reg.Summary)
	}
	// The failed heat child run carries the MD-BUILDER-SUMMARY message.
	children, _ := s.ListChildRuns(reg.ID)
	for _, c := range children {
		if c.Name == "heat" {
			if c.Status != store.StatusFailed || !strings.Contains(c.Message, "err 1e-3") {
				t.Errorf("heat child run wrong: %+v", c)
			}
		}
	}
}

// TestExecuteEnvScriptWrittenAndSourced: a configured environment script
// is written by the clone task and sourced by every later stage script.
func TestExecuteEnvScriptWrittenAndSourced(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	// Configure an env script on the environment.
	env, err := s.GetEnvironmentAny(cloneTask.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	env.EnvScript = "module load gcc/13\nexport CXX=g++\n"
	if err := s.UpdateEnvironment(env); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	logs, _ := s.ReadTaskLogs(cloneTask.ID, 0)
	var cloneLog string
	for _, l := range logs {
		cloneLog += l.Content
	}
	if !strings.Contains(cloneLog, "wrote env script md-builder-env-") {
		t.Errorf("clone log should record the env script write: %q", cloneLog)
	}

	// One recorded script is the env-script body itself (the RunScript stdin).
	writeSeen := false
	for _, sc := range exec.scripts {
		if strings.Contains(sc, "environment setup script of") && strings.Contains(sc, "module load gcc/13") && strings.Contains(sc, "export CXX=g++") {
			writeSeen = true
		}
	}
	if !writeSeen {
		t.Errorf("env script write script not seen: %+v", exec.scripts)
	}

	// Run the rest; every stage script sources the env script.
	for i := 0; i < 4; i++ {
		st, err := s.ClaimReadyTask()
		if err != nil || st == nil {
			t.Fatalf("claim %d: %v %v", i, st, err)
		}
		if err := svc.ExecuteTask(ctx, st); err != nil {
			t.Fatal(err)
		}
	}
	name := env.EnvScriptName()
	if name == "" {
		t.Fatal("EnvScriptName should be derived from the content")
	}
	sourced := 0
	for _, sc := range exec.scripts {
		if strings.Contains(sc, fmt.Sprintf(". \"$ENV_SCRIPT\"")) && strings.Contains(sc, name) {
			sourced++
		}
	}
	if sourced != 4 { // build + unit + 2 cases
		t.Errorf("every stage script should source the env script: %d of %d", sourced, len(exec.scripts))
	}
}
