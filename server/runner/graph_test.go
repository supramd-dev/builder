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
		Build:   BuildConfig{Generator: GeneratorCMake, CMakeFlags: "-DX=1", Threads: 4},
		Unit:    &EnvConfig{Command: "ctest -L unit", Timeout: 100},
		Regression: &EnvConfig{
			Command: "python3 run.py",
			Timeout: 200,
		},
	}
}

func TestBuildTaskGraphShape(t *testing.T) {
	tasks, err := BuildTaskGraph(sampleEntry())
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("want 4 nodes (clone, build, unit, regression), got %d", len(tasks))
	}
	wantKinds := []string{store.TaskKindClone, store.TaskKindBuild, store.TaskKindUnit, store.TaskKindRegression}
	for i, kind := range wantKinds {
		if tasks[i].Kind != kind {
			t.Errorf("node %d kind: want %s, got %s", i, kind, tasks[i].Kind)
		}
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
	// unit/regression depend on build (index 1).
	for _, i := range []int{2, 3} {
		if len(tasks[i].Deps) != 1 || tasks[i].Deps[0] != store.TaskSubPlaceholderBase+1 {
			t.Errorf("node %d deps: %v", i, tasks[i].Deps)
		}
	}

	// The build snapshot carries the recipe and env.
	var build BuildStageConfig
	if err := json.Unmarshal([]byte(tasks[1].Config), &build); err != nil {
		t.Fatal(err)
	}
	if build.Generator != GeneratorCMake || build.CMakeFlags != "-DX=1" || build.Threads != 4 {
		t.Errorf("build snapshot wrong: %+v", build)
	}
	if build.Timeout != 300 || build.Env["CC"] != "gcc" {
		t.Errorf("build snapshot timeout/env wrong: %+v", build)
	}

	// The unit snapshot carries the command and its own timeout.
	var unit StageConfig
	if err := json.Unmarshal([]byte(tasks[2].Config), &unit); err != nil {
		t.Fatal(err)
	}
	if unit.Command != "ctest -L unit" || unit.Timeout != 100 {
		t.Errorf("unit snapshot wrong: %+v", unit)
	}
}

func TestBuildTaskGraphScriptBuild(t *testing.T) {
	entry := sampleEntry()
	entry.Build = BuildConfig{Generator: GeneratorScript, Command: "./build.sh --cuda"}
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
		t.Errorf("script build snapshot wrong: %+v", build)
	}
}

func TestBuildTaskGraphNil(t *testing.T) {
	if _, err := BuildTaskGraph(nil); err == nil {
		t.Error("nil entry should error")
	}
}

func TestBuildBuildScriptCMake(t *testing.T) {
	in := &ScriptInput{
		CommitSHA: "abcdef123456",
		EnvName:   "cpu-node",
		EnvTags:   "cpu",
		CodeDir:   "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:     sampleEntry(),
		Timeout:   120,
	}
	script, err := BuildBuildScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"set -uo pipefail",
		`CODE="$HOME/.md-builder/tasks/abcdef123456/code"`,
		`cd "$CODE" || exit 1`,
		"timeout 120 cmake -DX=1 . && timeout 120 cmake --build . -j4",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestBuildStageScriptAndExports(t *testing.T) {
	in := &ScriptInput{
		CommitSHA:    "abcdef123456",
		EnvName:      "cpu-node",
		EnvTags:      "cpu,mpi",
		CodeDir:      "$HOME/.md-builder/tasks/abcdef123456/code",
		Entry:        sampleEntry(),
		StageCommand: "ctest -L unit",
		Timeout:      90,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`CODE="$HOME/.md-builder/tasks/abcdef123456/code"`,
		`cd "$CODE" || exit 1`,
		"timeout 90 bash -c 'ctest -L unit'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}

	// The MD_* exports render deterministically (sorted env keys).
	var b strings.Builder
	ExportEnv(func(f string, args ...any) { fmt.Fprintf(&b, f, args...) }, in)
	out := b.String()
	for _, want := range []string{
		"export MD_COMMIT='abcdef123456'",
		"export MD_ENV_NAME='cpu-node'",
		"export MD_ENV_TAGS='cpu,mpi'",
		`export MD_TEST_INPUT_DIR="$HOME/.md-builder/tasks/abcdef123456/tests"`,
		"export CC='gcc'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("exports missing %q:\n%s", want, out)
		}
	}
}
