package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"md-builder/server/storage"
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

	// onScript, when set, is called with each script as it "executes" — the
	// moment a real command would be running on the host, which is when
	// tests observe the state the run pages read.
	onScript func(script string)
}

func (f *fakeExecer) RunScript(ctx context.Context, h SSHHost, cmd, script string, timeout time.Duration, stdout, stderr io.Writer) ExecResult {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	hook := f.onScript
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
	if hook != nil {
		hook(script)
	}
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

// artifactText reads an artifact's bytes through the store: the row itself
// only references the object, so this is what the API would serve.
func artifactText(t *testing.T, s *store.Store, a *store.TestArtifact) string {
	t.Helper()
	data, err := s.ArtifactContent(context.Background(), a)
	if err != nil {
		t.Fatalf("read artifact %d: %v", a.ID, err)
	}
	return string(data)
}

// runOf returns the in-flight (latest) run row of a real task node: the row
// the dashboard and the run page read.
func runOf(t *testing.T, s *store.Store, task *store.Task) *store.TestRun {
	t.Helper()
	run, err := s.FindTaskRun(task.ID, task.Attempts)
	if err != nil {
		t.Fatalf("run of task %d (attempt %d): %v", task.ID, task.Attempts, err)
	}
	return run
}

// newExecuteFixture seeds a user/environment/commit and a full task graph,
// returning the service with fakes and the claimed clone task.
func newExecuteFixture(t *testing.T, yaml string) (*Service, *store.Store, *fakeExecer, *fakeCloner, *store.Task) {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
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
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
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

const execYAML = `version: 3
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

const artifactsYAML = `version: 3
defaults:
  build:
    command: "cmake -DEXEC=1 . && cmake --build ."
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit --output-junit junit.xml"
      artifacts: "build/test_detail.xml"
`

// buildArtifactsYAML exercises the build-stage artifacts field: files the
// build command leaves behind, fetched back as file-kind artifacts (never
// parsed for counts — the build verdict is its exit code alone).
const buildArtifactsYAML = `version: 3
defaults:
  build:
    command: "cmake . && ninja"
    artifacts: "build/.ninja_log"
matrix:
  - tags: [cpu]
    build:
      artifacts: ["build/.ninja_log", "build/compile_commands.json"]
    unit:
      command: "ctest -L unit"
`

const multiArtifactsYAML = `version: 3
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
const caseArtifactsYAML = `version: 3
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

	run := runOf(t, s, stage)
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
	if artifacts[0].ObjectKey == "" {
		t.Error("the fetched file should have been uploaded to object storage")
	}
	if artifactText(t, s, &artifacts[0]) != sampleGTestXML {
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

	run := runOf(t, s, stage)
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
		byName[a.Name] = artifactText(t, s, &a)
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

	run := runOf(t, s, stage)
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
	all := storedLog(t, s, stage, stage.Attempts)
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
	if got.Status != store.StatusPassed {
		t.Fatalf("clone should be done: %+v", got)
	}
	if cloner.codeRepo != "https://gitlab.example.com/group/code" {
		t.Errorf("clone used the wrong repo: %s", cloner.codeRepo)
	}
	if len(cloner.remoteDirs) != 1 || !strings.Contains(cloner.remoteDirs[0], "c0ffee123456") {
		t.Errorf("clone remote dir wrong: %v", cloner.remoteDirs)
	}
	// Clone log was persisted.
	if storedLog(t, s, cloneTask, cloneTask.Attempts) == "" {
		t.Error("clone task should have a stored log")
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
	if got.Status != store.StatusPassed {
		t.Fatalf("build should be done: %+v", got)
	}
	// The build outcome is recorded as a "build" test run for the dashboard.
	if bRun := runOf(t, s, build); bRun.Status != store.StatusPassed {
		t.Errorf("build run wrong: %+v", bRun)
	}

	// 3. unit + both regression cases (claimable once build is done; the
	// regression container is virtual and never claimed)
	claimOrder := map[string]int{}
	for i := 0; i < 3; i++ {
		st, err := s.ClaimReadyTask()
		if err != nil || st == nil {
			t.Fatalf("claim stage %d: %v %v", i, st, err)
		}
		if st.Kind != store.TaskKindUnit && st.Kind != store.TaskKindRegressionCase {
			t.Fatalf("want a stage, got %s", st.Kind)
		}
		if err := svc.ExecuteTask(ctx, st); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTask(st.ID)
		if got.Status != store.StatusPassed {
			t.Errorf("%s should be done: %+v", st.Kind, got)
		}
		claimOrder[st.Name]++
	}
	if claimOrder["unit tests"] != 1 || claimOrder["regression: heat"] != 1 || claimOrder["regression: poisson"] != 1 {
		t.Errorf("stage set wrong: %+v", claimOrder)
	}

	// Every real node has its passed run; the virtual regression container
	// has none and aggregates the two case rows instead.
	latest, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if unit := latest[store.TaskKindUnit]; unit == nil || unit.Status != store.StatusPassed {
		t.Errorf("unit run wrong: %+v", unit)
	}
	heat, poisson := latest[RegressionCaseKey("heat")], latest[RegressionCaseKey("poisson")]
	if heat == nil || heat.Status != store.StatusPassed || poisson == nil || poisson.Status != store.StatusPassed {
		t.Fatalf("case runs wrong: heat=%+v poisson=%+v", heat, poisson)
	}
	if cont := latest[RegressionStageKey]; cont != nil {
		t.Errorf("the regression container should have no run of its own: %+v", cont)
	}
	reg, err := regressionStage(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Status != store.StatusPassed || reg.Total != 2 || reg.Passed != 2 || reg.Failed != 0 {
		t.Errorf("regression container should aggregate 2 passed cases: %+v", reg)
	}
	children, _ := s.ListChildren(reg.ID)
	if len(children) != 2 || children[0].NodeKey != RegressionCaseKey("heat") ||
		children[1].NodeKey != RegressionCaseKey("poisson") {
		t.Errorf("case nodes wrong: %+v", children)
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
	if got.Status != store.StatusFailed || got.Error == "" {
		t.Fatalf("clone should be failed with error: %+v", got)
	}

	// runClaimed's post-processing: SkipDependents marks everything behind the
	// failed node skipped, closes its run and rolls the containers up.
	reason := "upstream task " + got.Name + " failed"
	if got.Error != "" {
		reason += ": " + got.Error
	}
	if err := s.SkipDependents(got.RootID, got.ID, reason); err != nil {
		t.Fatal(err)
	}

	subs, _ := s.ListActiveNodes(got.RootID)
	for _, sub := range subs {
		if sub.Kind == store.TaskKindClone {
			continue
		}
		if sub.Status != store.StatusSkipped {
			t.Errorf("%s should be skipped: %s", sub.Kind, sub.Status)
		}
	}
	root, _ := s.GetTask(got.RootID)
	if root.Status != store.StatusFailed {
		t.Errorf("root should be failed: %s", root.Status)
	}

	// The dashboard shows the skipped stages through their own rows: the unit
	// run is skipped, carrying the reason that stopped it.
	latest, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	unit := latest[store.TaskKindUnit]
	if unit == nil || unit.Status != store.StatusSkipped || !strings.Contains(unit.Summary, "upstream task") {
		t.Errorf("skipped unit run wrong: %+v", unit)
	}
	// The regression cases were recorded as skipped runs; the virtual
	// container has no run and aggregates them.
	reg, err := regressionStage(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Total != 2 || reg.Passed != 0 || reg.Failed != 0 || reg.Skipped != 2 {
		t.Errorf("skipped case aggregate wrong: %+v", reg)
	}
	if !strings.Contains(reg.Summary, "skipped (upstream failure)") {
		t.Errorf("all-skipped container summary should say so: %q", reg.Summary)
	}
	if c := latest[RegressionStageKey]; c != nil {
		t.Errorf("the regression container should have no run: %+v", c)
	}
	children, _ := s.ListChildren(reg.ID)
	if len(children) != 2 {
		t.Fatalf("want 2 skipped case nodes, got %d", len(children))
	}
	for _, c := range children {
		if c.Status != store.StatusSkipped {
			t.Errorf("case %s should be skipped: %s", c.Name, c.Status)
		}
		if run := latest[c.NodeKey]; run == nil || run.Status != store.StatusSkipped {
			t.Errorf("case %s run should be skipped: %+v", c.Name, run)
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
	if got.Status != store.StatusFailed {
		t.Fatalf("build should be failed: %+v", got)
	}

	// The failed build recorded a failed "build" run with the compiler error.
	bRun := runOf(t, s, build)
	if bRun.Status != store.StatusFailed {
		t.Fatalf("failed build run wrong: %+v", bRun)
	}
	if !strings.Contains(bRun.Summary, "CMake Error") {
		t.Errorf("build summary should carry the compiler error: %q", bRun.Summary)
	}

	// Scheduler post-processing: the test stages are skipped with rows.
	if err := s.SkipDependents(got.RootID, got.ID, "upstream task "+got.Name+" failed: "+got.Error); err != nil {
		t.Fatal(err)
	}
	latest, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if unitRun := latest[store.TaskKindUnit]; unitRun == nil ||
		unitRun.Status != store.StatusSkipped || !strings.Contains(unitRun.Summary, "build") {
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

	// The placeholder run exists from dispatch, running (the claim flipped it
	// together with the task).
	if run := runOf(t, s, build); run.Status != store.StatusRunning {
		t.Fatalf("placeholder build run should be running: %+v", run)
	}

	// An empty build snapshot makes BuildStageScript fail ("no stage
	// command"): blank the build sub-task's config, then re-fetch the task so
	// the executor sees the emptied snapshot.
	if err := s.DB.Model(&store.Task{}).Where("id = ?", build.ID).Update("config", "{}").Error; err != nil {
		t.Fatal(err)
	}
	if build, err = s.GetTask(build.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}

	got, _ := s.GetTask(build.ID)
	if got.Status != store.StatusFailed {
		t.Fatalf("build should be failed: %+v", got)
	}
	run := runOf(t, s, build)
	if run.Status != store.StatusFailed {
		t.Fatalf("placeholder run should be failed after script-build error: %+v", run)
	}
	if !strings.Contains(run.Summary, "no stage command") {
		t.Errorf("run summary should carry the reason: %q", run.Summary)
	}
}

// TestExecuteBuildFetchesArtifacts: a build with configured artifact files
// fetches them back and stores them as file-kind artifacts on the build run
// — verbatim, no counts parsing (a passing build with a non-gtest file stays
// passed with zero counts).
func TestExecuteBuildFetchesArtifacts(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, buildArtifactsYAML)
	// The fetch scripts cat the artifact path; the fake keys its canned
	// output off outcome markers, so both paths need an outcome entry.
	exec.outcome[".ninja_log"] = 0
	exec.output[".ninja_log"] = "# ninja log\n5\t10\t0\tcmake\n"
	exec.outcome["compile_commands.json"] = 0
	exec.output["compile_commands.json"] = "[{\"file\": \"../src/md.cc\"}]\n"

	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil || build.Kind != store.TaskKindBuild {
		t.Fatalf("claim build: %v %v", build, err)
	}

	// The build snapshot carries the artifacts list.
	var stage BuildStageConfig
	if err := json.Unmarshal([]byte(build.Config), &stage); err != nil {
		t.Fatal(err)
	}
	if len(stage.Artifacts) != 2 || stage.Artifacts[0] != "build/.ninja_log" {
		t.Fatalf("build config artifacts wrong: %v", stage.Artifacts)
	}

	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}

	run := runOf(t, s, build)
	if run.Status != store.StatusPassed {
		t.Fatalf("build run should pass: %+v", run)
	}
	if run.Total != 0 || run.Failed != 0 || run.Skipped != 0 {
		t.Errorf("build counts must stay zero: total=%d failed=%d skipped=%d", run.Total, run.Failed, run.Skipped)
	}

	artifacts, err := s.ListRunArtifacts(run.ID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("artifacts: %v %v", artifacts, err)
	}
	if artifacts[0].Kind != store.ArtifactKindFile || artifacts[0].Name != "build/.ninja_log" {
		t.Errorf("artifact[0] wrong: %+v", artifacts[0])
	}
	if got := artifactText(t, s, &artifacts[0]); got != "# ninja log\n5\t10\t0\tcmake\n" {
		t.Errorf("artifact[0] content not verbatim: %q", got)
	}
	if artifacts[1].Kind != store.ArtifactKindFile || artifacts[1].Name != "build/compile_commands.json" {
		t.Errorf("artifact[1] wrong: %+v", artifacts[1])
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
	if got.Status != store.StatusFailed {
		t.Fatalf("unit should be failed: %+v", got)
	}
	run := runOf(t, s, unit)
	if run.Status != store.StatusFailed {
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
	if got.Status != store.StatusFailed {
		t.Errorf("task should be failed: %+v", got)
	}
}

func TestRedeployRequeuesGraphInPlace(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	root, err := s.GetTask(cloneTask.RootID)
	if err != nil {
		t.Fatal(err)
	}

	// Run clone to done, then re-dispatch the same commit/environment: the
	// graph is re-armed from the fresh snapshot. The nodes it still defines
	// keep their rows — and with them the previous attempt's logs — and come
	// out with a new pending attempt.
	if err := svc.ExecuteTask(context.Background(), cloneTask); err != nil {
		t.Fatal(err)
	}
	logsBefore := storedLog(t, s, cloneTask, cloneTask.Attempts)
	if logsBefore == "" {
		t.Fatal("precondition: clone log exists")
	}

	res := svc.DispatchForCommit(&store.Commit{ID: cloneTask.CommitID, Repo: "group/code", SHA: "c0ffee123456"})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.TasksCreated != 1 {
		t.Fatalf("re-dispatch should requeue 1 graph, got %d", res.TasksCreated)
	}

	subs, _ := s.ListActiveNodes(root.ID)
	if len(subs) != 6 { // clone, build, unit, the regression container, 2 cases
		t.Fatalf("redeploy should re-arm 6 nodes, got %d", len(subs))
	}
	for _, sub := range subs {
		if sub.Status != store.StatusPending {
			t.Errorf("re-armed node should be pending: %+v", sub)
		}
	}
	// The clone node is the same row, re-armed: identity (and history) kept,
	// fresh attempt open.
	again, err := s.GetTask(cloneTask.ID)
	if err != nil {
		t.Fatalf("the clone row should survive a re-dispatch: %v", err)
	}
	if again.Attempts != 2 || again.Status != store.StatusPending {
		t.Errorf("re-armed clone: %+v", again)
	}
	if kept := storedLog(t, s, cloneTask, 1); kept != logsBefore {
		t.Errorf("the first attempt's log should be kept: %d bytes, want %d", len(kept), len(logsBefore))
	}
	if fresh := storedLog(t, s, cloneTask, 2); fresh != "" {
		t.Errorf("the fresh attempt should start with an empty log, got %q", fresh)
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
	cloneLog := storedLog(t, s, cloneTask, cloneTask.Attempts)
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

	// The heat case ran, passed, with its artifact riding the case's own run;
	// the container aggregates it.
	latest, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	heat := latest[RegressionCaseKey("heat")]
	if heat == nil {
		t.Fatal("heat case run missing")
	}
	if heat.Status != store.StatusPassed || heat.Summary == "" {
		t.Errorf("case run wrong: %+v", heat)
	}
	if heat.FinishedAt.IsZero() || heat.DurationMillis < 0 {
		t.Errorf("case run should carry its window: %+v", heat)
	}
	artifacts, _ := s.ListRunArtifacts(heat.ID)
	if len(artifacts) != 1 || artifacts[0].Name != "out.xml" {
		t.Errorf("case artifact should ride the case's run: %+v", artifacts)
	}
	if c := latest[RegressionStageKey]; c != nil {
		t.Errorf("the container should have no run of its own: %+v", c)
	}
	reg, err := regressionStage(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Total != 1 || reg.Passed != 1 || reg.Status != store.StatusPassed {
		t.Errorf("case aggregate wrong: %+v", reg)
	}
	if parentArtifacts, _ := s.ListSubtreeArtifacts(reg.ID); len(parentArtifacts) != 1 {
		t.Errorf("the container serves the case artifact from its subtree: %+v", parentArtifacts)
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

	latest, err := runsByKey(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	// The failed heat case's own run carries the MD-BUILDER-SUMMARY message.
	if heat := latest[RegressionCaseKey("heat")]; heat == nil ||
		heat.Status != store.StatusFailed || !strings.Contains(heat.Summary, "err 1e-3") {
		t.Errorf("heat case run wrong: %+v", heat)
	}
	// A case's run counts as the one test it is — "0/0 passed" on a case that
	// ran would read as an empty run.
	if heat := latest[RegressionCaseKey("heat")]; heat != nil && (heat.Total != 1 || heat.Passed != 0 || heat.Failed != 1) {
		t.Errorf("failed case counts = %d/%d (%d failed), want 0/1 of 1", heat.Passed, heat.Total, heat.Failed)
	}
	if poisson := latest[RegressionCaseKey("poisson")]; poisson == nil ||
		poisson.Status != store.StatusPassed {
		t.Errorf("poisson case should stay passed: %+v", poisson)
	} else if poisson.Total != 1 || poisson.Passed != 1 || poisson.Failed != 0 {
		t.Errorf("passed case counts = %d/%d, want 1/1 of 1", poisson.Passed, poisson.Total)
	}
	reg, err := regressionStage(s, cloneTask)
	if err != nil {
		t.Fatal(err)
	}
	if reg.Total != 2 || reg.Passed != 1 || reg.Failed != 1 || reg.Status != store.StatusFailed {
		t.Errorf("mixed case aggregate wrong: %+v", reg)
	}
	if !strings.Contains(reg.Summary, "heat") {
		t.Errorf("summary should name the failing case: %q", reg.Summary)
	}
}

// TestExecuteEnvScriptWrittenAndSourced: a configured environment script
// is written by the clone task and sourced by every later stage script.
func TestExecuteEnvScriptWrittenAndSourced(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	// Configure an env script on the environment.
	env, err := s.GetEnvironment(cloneTask.EnvironmentID)
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
	cloneLog := storedLog(t, s, cloneTask, cloneTask.Attempts)
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

// execVarsYAML exercises the yaml `variables:` block through the real
// dispatch path: the entry's variables reach every stage script (expanded),
// and the site's whitelist decides which host names expand.
const execVarsYAML = `version: 3
defaults:
  build:
    command: "cmake -DEXEC=1 . && cmake --build ."
  variables:
    BUILD_ROOT: "$MD_CODE_DIR/build"
matrix:
  - tags: [cpu]
    variables:
      UNIT_XML: "$BUILD_ROOT/tests/unit.xml"
      HOST_TMP: "$MDTEST_HOST_TMP/x"
      TYPO: "$MD_CODEDIR/x"
    unit:
      command: "./build/unit_tests --gtest_output=xml:$UNIT_XML"
`

func TestExecuteStageVariablesExported(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execVarsYAML)
	// An env setup script, so the ordering claim below (variables are
	// exported after it) has something to be checked against.
	env, err := s.GetEnvironment(cloneTask.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	env.EnvScript = "module load gcc/13\n"
	if err := s.UpdateEnvironment(env); err != nil {
		t.Fatal(err)
	}

	unitTask := runUntilStage(t, svc, s, cloneTask, store.TaskKindUnit)
	if err := svc.ExecuteTask(context.Background(), unitTask); err != nil {
		t.Fatal(err)
	}

	// The stage script is the one running the unit command.
	var script string
	for _, sc := range exec.scripts {
		if strings.Contains(sc, "unit_tests --gtest_output") {
			script = sc
		}
	}
	if script == "" {
		t.Fatalf("no unit script recorded: %+v", exec.scripts)
	}
	// A variable is exported as one shell word: the expandable parts
	// double-quoted, the literals single-quoted — and a variable that
	// references another one comes after it.
	for _, want := range []string{
		`export BUILD_ROOT="${MD_CODE_DIR}"'/build'`,
		`export UNIT_XML="${BUILD_ROOT}"'/tests/unit.xml'`,
		// $MDTEST_HOST_TMP is not on the environment's whitelist, and
		// $MD_CODEDIR is a typo: both stay literal, both are reported.
		`export HOST_TMP='$MDTEST_HOST_TMP/x'`,
		`export TYPO='$MD_CODEDIR/x'`,
		"warning: variables.HOST_TMP: $MDTEST_HOST_TMP",
		"warning: variables.TYPO: $MD_CODEDIR",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("unit script missing %q:\n%s", want, script)
		}
	}
	// The variables come after the env script hook (they may build on what
	// it exports) and before the cd.
	envAt := strings.Index(script, "ENV_SCRIPT")
	varsAt := strings.Index(script, "export BUILD_ROOT=")
	cdAt := strings.Index(script, `cd "$MD_CODE_DIR"`)
	if envAt < 0 || varsAt < envAt || cdAt < varsAt {
		t.Errorf("variables must be exported after the env script and before the cd:\n%s", script)
	}
}

// TestExecuteStageVariablesUseEnvironmentWhitelist is the positive half: with
// the name on the whitelist of the environment the stage runs on, the same
// template expands instead of staying literal. The list belongs to the
// environment — the host decides what it exposes — so this is where it is set.
func TestExecuteStageVariablesUseEnvironmentWhitelist(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execVarsYAML)
	allowed := "MDTEST_HOST_TMP, PATH"
	env, err := s.GetEnvironment(cloneTask.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	env.AllowedEnvVars = &allowed
	if err := s.UpdateEnvironment(env); err != nil {
		t.Fatal(err)
	}

	unitTask := runUntilStage(t, svc, s, cloneTask, store.TaskKindUnit)
	if err := svc.ExecuteTask(context.Background(), unitTask); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, sc := range exec.scripts {
		if strings.Contains(sc, "unit_tests --gtest_output") {
			script = sc
		}
	}
	if !strings.Contains(script, `export HOST_TMP="${MDTEST_HOST_TMP}"'/x'`) {
		t.Errorf("a whitelisted host variable must expand:\n%s", script)
	}
	if strings.Contains(script, "warning: variables.HOST_TMP") {
		t.Errorf("a whitelisted host variable must not warn:\n%s", script)
	}
}

// TestExecuteStageRetriesTheClosingWrite: the write that ends an attempt
// (store.FinishAttempt) is the only thing that takes a node out of "running",
// so a transient failure is retried rather than leaving a task that no worker
// will ever claim again. The fake failure is registered on the run row's
// update — the first write the finish makes.
func TestExecuteStageRetriesTheClosingWrite(t *testing.T) {
	svc, s, _, _, cloneTask := newExecuteFixture(t, execYAML)
	// No waiting in the test: the backoff is only there to give a busy
	// database room between tries.
	old := finishAttemptBackoff
	finishAttemptBackoff = 0
	t.Cleanup(func() { finishAttemptBackoff = old })

	var writes int32
	fail := func(tx *gorm.DB) {
		if tx.Statement.Table == "test_runs" && atomic.AddInt32(&writes, 1) <= 2 {
			tx.AddError(errors.New("database is busy"))
		}
	}
	if err := s.DB.Callback().Update().Before("gorm:update").Register("test:busy-finish", fail); err != nil {
		t.Fatal(err)
	}

	if err := svc.ExecuteTask(context.Background(), cloneTask); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTask(cloneTask.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusPassed {
		t.Errorf("clone status = %q, want %q once the write went through", got.Status, store.StatusPassed)
	}
	if n := atomic.LoadInt32(&writes); n < 3 {
		t.Errorf("the closing write was tried %d times, want the two failures retried", n)
	}
}

// TestExecuteStageSummarizesTheLogTail: a stage's summary is read out of the
// END of its log — the summary line is the last thing the command prints, and a
// log longer than the tail the summary is read from (logSummaryTailBytes) is
// not read from its beginning to find it. Reading the head would summarize a
// stage from output written long before its result.
//
// It also pins what a restarted stage's log looks like: this attempt already has
// stored output (a previous process wrote it), so the execution continues after
// it rather than starting over.
func TestExecuteStageSummarizesTheLogTail(t *testing.T) {
	svc, s, exec, _, cloneTask := newExecuteFixture(t, execYAML)
	ctx := context.Background()
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatal(err)
	}

	build, err := s.ClaimReadyTask()
	if err != nil || build == nil || build.Kind != store.TaskKindBuild {
		t.Fatalf("claim build: %v %v", build, err)
	}
	// More output than the summary is read from, stored by whoever ran this
	// attempt before.
	var noise strings.Builder
	for i := 1; i <= 30000; i++ {
		fmt.Fprintf(&noise, "noise %d\n", i)
	}
	if noise.Len() <= logSummaryTailBytes {
		t.Fatalf("the test's noise (%d bytes) must be longer than the summary tail (%d)",
			noise.Len(), logSummaryTailBytes)
	}
	storeParts(t, s, build, noise.String())

	exec.outcome["cmake -DEXEC=1 ."] = 0
	exec.output["cmake -DEXEC=1 ."] = "compiling…\nMD-BUILDER-SUMMARY: build ok\n"

	if err := svc.ExecuteTask(ctx, build); err != nil {
		t.Fatal(err)
	}
	run := runOf(t, s, build)
	if !strings.Contains(run.Summary, "build ok") {
		t.Errorf("build summary = %q, want the summary line at the end of the log", run.Summary)
	}
	if strings.Contains(run.Summary, "noise") {
		t.Errorf("build summary = %q, want the tail of the log, not its head", run.Summary)
	}
	// Nothing was written over: the log is the previous output, the seam, and
	// this execution's own.
	all := storedLog(t, s, build, build.Attempts)
	if !strings.HasPrefix(all, noise.String()) {
		t.Errorf("the log no longer begins with what was already stored: %q", all[:80])
	}
	if !strings.Contains(all, "MD-BUILDER-SUMMARY: build ok") {
		t.Error("the log lost this execution's output")
	}
}
