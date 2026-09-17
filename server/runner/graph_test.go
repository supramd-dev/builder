package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"md-builder/server/store"
)

func sampleEntry() *MergedEntry {
	return &MergedEntry{
		Tags:    []string{"cpu"},
		Timeout: 300,
		Env:     map[string]string{"CC": "gcc"},
		Build:   BuildConfig{Command: "cmake -DX=1 . && cmake --build . -j4"},
		Unit:    &EnvConfig{Command: CommandList{"ctest -L unit"}, Timeout: 100},
		Regression: []RegressionCase{
			{Name: "heat", Command: CommandList{"python3 run_heat.py"}, Timeout: 200},
			{Name: "poisson", Command: CommandList{"python3 run_poisson.py"}, Timeout: 0},
		},
	}
}

func TestBuildTaskGraphShape(t *testing.T) {
	tasks, err := BuildTaskGraph(sampleEntry())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 5 { // clone, build, unit, regression:heat, regression:poisson
		t.Fatalf("want 5 nodes (clone, build, unit, 2 cases), got %d", len(tasks))
	}
	wantKinds := []string{
		store.TaskKindClone, store.TaskKindBuild, store.TaskKindUnit,
		store.TaskKindRegression, store.TaskKindRegression,
	}
	for i, kind := range wantKinds {
		if tasks[i].Kind != kind {
			t.Errorf("node %d kind: want %s, got %s", i, kind, tasks[i].Kind)
		}
	}
	// Case nodes are named after their presets.
	if tasks[3].Name != "regression: heat" || tasks[4].Name != "regression: poisson" {
		t.Errorf("case node names: %q %q", tasks[3].Name, tasks[4].Name)
	}
	// clone has no dependencies: the root is a container, not a gate (it
	// only reaches a terminal state once every sub-task has).
	if len(tasks[0].Deps) != 0 {
		t.Errorf("clone deps: %v", tasks[0].Deps)
	}
	// build depends on clone (index 0).
	if len(tasks[1].Deps) != 1 || tasks[1].Deps[0] != store.TaskSubPlaceholderBase+0 {
		t.Errorf("build deps: %v", tasks[1].Deps)
	}
	// unit and both cases depend on build (index 1).
	for _, i := range []int{2, 3, 4} {
		if len(tasks[i].Deps) != 1 || tasks[i].Deps[0] != store.TaskSubPlaceholderBase+1 {
			t.Errorf("node %d deps: %v", i, tasks[i].Deps)
		}
	}

	// The build snapshot carries the command and env.
	var build BuildStageConfig
	if err := json.Unmarshal([]byte(tasks[1].Config), &build); err != nil {
		t.Fatal(err)
	}
	if build.Command != "cmake -DX=1 . && cmake --build . -j4" {
		t.Errorf("build snapshot wrong: %+v", build)
	}
	if build.Timeout != 300 || build.Env["CC"] != "gcc" {
		t.Errorf("build snapshot timeout/env wrong: %+v", build)
	}

	// The unit snapshot carries the command, its own timeout and workdir.
	var unit StageConfig
	if err := json.Unmarshal([]byte(tasks[2].Config), &unit); err != nil {
		t.Fatal(err)
	}
	if unit.Command.String() != "ctest -L unit" || unit.Timeout != 100 || unit.Workdir != "" {
		t.Errorf("unit snapshot wrong: %+v", unit)
	}

	// The case snapshots carry the preset name, command and timeout.
	var heat CaseStageConfig
	if err := json.Unmarshal([]byte(tasks[3].Config), &heat); err != nil {
		t.Fatal(err)
	}
	if heat.Case != "heat" || heat.Command.String() != "python3 run_heat.py" || heat.Timeout != 200 {
		t.Errorf("heat case snapshot wrong: %+v", heat)
	}
	var poisson CaseStageConfig
	if err := json.Unmarshal([]byte(tasks[4].Config), &poisson); err != nil {
		t.Fatal(err)
	}
	// Timeout 0 on the case falls back to the entry timeout (300).
	if poisson.Timeout != 300 {
		t.Errorf("poisson timeout should default to entry timeout: %d", poisson.Timeout)
	}
}

func TestBuildTaskGraphArtifactsPassthrough(t *testing.T) {
	entry := sampleEntry()
	entry.Unit.Artifacts = ArtifactPaths{"build/test_detail.xml", "build/extra.json"}
	entry.Regression[0].Artifacts = ArtifactPaths{"reg/results.json"}
	entry.Regression[0].Workdir = "regression/heat"
	tasks, err := BuildTaskGraph(entry)
	if err != nil {
		t.Fatal(err)
	}
	var unit StageConfig
	if err := json.Unmarshal([]byte(tasks[2].Config), &unit); err != nil {
		t.Fatal(err)
	}
	if len(unit.Artifacts) != 2 || unit.Artifacts[0] != "build/test_detail.xml" || unit.Artifacts[1] != "build/extra.json" {
		t.Errorf("unit artifacts not passed through: %+v", unit)
	}
	var heat CaseStageConfig
	if err := json.Unmarshal([]byte(tasks[3].Config), &heat); err != nil {
		t.Fatal(err)
	}
	if len(heat.Artifacts) != 1 || heat.Artifacts[0] != "reg/results.json" || heat.Workdir != "regression/heat" {
		t.Errorf("case artifacts/workdir not passed through: %+v", heat)
	}
}

// A build command is whatever the yaml author chose — the snapshot passes
// it through verbatim; no recipe is generated server-side.
func TestBuildTaskGraphBuildCommandPassthrough(t *testing.T) {
	entry := sampleEntry()
	entry.Build = BuildConfig{Command: "./build.sh --cuda"}
	entry.Regression = nil
	tasks, err := BuildTaskGraph(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 { // clone, build, unit
		t.Fatalf("want 3 nodes, got %d", len(tasks))
	}
	var build BuildStageConfig
	if err := json.Unmarshal([]byte(tasks[1].Config), &build); err != nil {
		t.Fatal(err)
	}
	if build.Command != "./build.sh --cuda" {
		t.Errorf("build snapshot wrong: %+v", build)
	}
}

// Without a build command the graph omits the build node and the test
// stages depend on the clone directly.
func TestBuildTaskGraphNoBuild(t *testing.T) {
	entry := sampleEntry()
	entry.Build = BuildConfig{}
	entry.Regression = nil
	tasks, err := BuildTaskGraph(entry)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 { // clone, unit
		t.Fatalf("want 2 nodes (clone, unit), got %d", len(tasks))
	}
	if tasks[1].Kind != store.TaskKindUnit {
		t.Fatalf("node 1 kind: %s", tasks[1].Kind)
	}
	if len(tasks[1].Deps) != 1 || tasks[1].Deps[0] != store.TaskSubPlaceholderBase+0 {
		t.Errorf("unit must depend on clone: %v", tasks[1].Deps)
	}
}

func TestBuildTaskGraphNil(t *testing.T) {
	if _, err := BuildTaskGraph(nil); err == nil {
		t.Error("nil entry should error")
	}
}

// The build command runs verbatim under timeout, in the workdir like any
// other stage (an out-of-source cmake is just `cmake <src>` written by hand).
func TestBuildScript(t *testing.T) {
	in := &ScriptInput{
		CommitSHA: "abcdef123456",
		EnvName:   "cpu-node",
		EnvTags:   "cpu",
		TaskDir:   "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:   "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:     sampleEntry(),
		Timeout:   120,
	}
	script, err := BuildScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -uo pipefail",
		`export MD_TASK_DIR="$HOME/.md-builder/tasks/abcdef123456"`,
		`export MD_CODE_DIR="$HOME/.md-builder/tasks/abcdef123456/code"`,
		`cd "$MD_CODE_DIR" || exit 1`,
		"timeout 120 bash -c 'cmake -DX=1 . && cmake --build . -j4'",
		"export CC='gcc'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// A build command with a workdir runs there — the same workdir semantics
// as unit/regression stages.
func TestBuildScriptWorkdir(t *testing.T) {
	in := &ScriptInput{
		TaskDir: "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir: "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry: &MergedEntry{
			Tags:  []string{"cpu"},
			Build: BuildConfig{Command: `cmake "$MD_CODE_DIR" && cmake --build .`},
		},
		Workdir: "build",
		Timeout: 120,
	}
	script, err := BuildScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`cd "$MD_CODE_DIR/build" || exit 1`,
		`timeout 120 bash -c 'cmake "$MD_CODE_DIR" && cmake --build .'`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("workdir build script missing %q:\n%s", want, script)
		}
	}
}

func TestBuildStageScriptAndExports(t *testing.T) {
	in := &ScriptInput{
		CommitSHA:    "abcdef123456",
		EnvName:      "cpu-node",
		EnvTags:      "cpu,mpi",
		TaskDir:      "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:      "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:        sampleEntry(),
		StageCommand: CommandList{"ctest -L unit"},
		Workdir:      "tests/unit",
		Timeout:      90,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`export MD_TASK_DIR="$HOME/.md-builder/tasks/abcdef123456"`,
		`export MD_CODE_DIR="$HOME/.md-builder/tasks/abcdef123456/code"`,
		`cd "$MD_CODE_DIR/tests/unit" || exit 1`,
		"timeout 90 bash -c 'ctest -L unit'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// The MD_* exports render deterministically (sorted env keys); the case
	// script additionally exports MD_CASE.
	caseIn := &ScriptInput{
		CommitSHA:    "abcdef123456",
		EnvName:      "cpu-node",
		EnvTags:      "cpu,mpi",
		TaskDir:      "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:      "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:        sampleEntry(),
		StageCommand: CommandList{"python3 run_heat.py"},
		CaseName:     "heat",
		Timeout:      90,
	}
	caseScript, err := BuildStageScript(caseIn)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"export MD_COMMIT='abcdef123456'",
		"export MD_ENV_NAME='cpu-node'",
		"export MD_ENV_TAGS='cpu,mpi'",
		`export MD_TASK_DIR="$HOME/.md-builder/tasks/abcdef123456"`,
		`export MD_CODE_DIR="$HOME/.md-builder/tasks/abcdef123456/code"`,
		"export MD_CASE='heat'",
		"export CC='gcc'",
	} {
		if !strings.Contains(caseScript, want) {
			t.Errorf("case script exports missing %q:\n%s", want, caseScript)
		}
	}
}

func TestBuildStageScriptEnvScriptSource(t *testing.T) {
	in := &ScriptInput{
		TaskDir:       "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:       "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:         sampleEntry(),
		StageCommand:  CommandList{"ctest -L unit"},
		EnvScriptName: "md-builder-env-abc123def456.sh",
		Timeout:       90,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`ENV_SCRIPT="$MD_TASK_DIR/md-builder-env-abc123def456.sh"`,
		`if [ -f "$ENV_SCRIPT" ]; then . "$ENV_SCRIPT"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("env-script block missing %q:\n%s", want, script)
		}
	}

	// Without an env script the block is absent.
	in.EnvScriptName = ""
	script, err = BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "ENV_SCRIPT") {
		t.Errorf("no env script configured: block should be absent:\n%s", script)
	}
}

// ExportEnv keeps its exported signature for tests (the preamble helper).
func TestExportEnvShape(t *testing.T) {
	in := &ScriptInput{
		CommitSHA: "abcdef123456",
		EnvName:   "cpu-node",
		EnvTags:   "cpu,mpi",
		TaskDir:   "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:   "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:     sampleEntry(),
	}
	var b strings.Builder
	ExportEnv(func(f string, args ...any) { fmt.Fprintf(&b, f, args...) }, in)
	out := b.String()
	for _, want := range []string{
		"export MD_COMMIT='abcdef123456'",
		"export MD_ENV_NAME='cpu-node'",
		"export MD_ENV_TAGS='cpu,mpi'",
		`export MD_TASK_DIR="$HOME/.md-builder/tasks/abcdef123456"`,
		`export MD_CODE_DIR="$HOME/.md-builder/tasks/abcdef123456/code"`,
		"export CC='gcc'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exports missing %q:\n%s", want, out)
		}
	}
}

// TestScriptSecretTokenExport: the site's secret token, when set, is
// exported as MD_SECRET_TOKEN in every stage script — quoted so
// metacharacters stay inert; unset leaves no trace of the variable.
func TestScriptSecretTokenExport(t *testing.T) {
	in := &ScriptInput{
		CommitSHA:    "abcdef123456",
		EnvName:      "cpu-node",
		EnvTags:      "cpu",
		TaskDir:      "$HOME/.md-builder/tasks/abcdef123456",
		CodeDir:      "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:        sampleEntry(),
		StageCommand: CommandList{"curl -H \"Authorization: Bearer $MD_SECRET_TOKEN\" https://mirror.internal/dataset"},
		Timeout:      90,
	}

	// Without a secret: no export line, the command reference stays.
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "MD_SECRET_TOKEN=") {
		t.Errorf("unset secret must not be exported:\n%s", script)
	}
	if !strings.Contains(script, "$MD_SECRET_TOKEN") {
		t.Errorf("command's reference should survive verbatim:\n%s", script)
	}

	// With a secret containing metacharacters: exported single-quoted.
	in.SecretToken = "s3cr't-$(rm -rf /)"
	script, err = BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `export MD_SECRET_TOKEN='s3cr'\''t-$(rm -rf /)'`; !strings.Contains(script, want) {
		t.Errorf("secret export should be shell-quoted, want %q in:\n%s", want, script)
	}
}
