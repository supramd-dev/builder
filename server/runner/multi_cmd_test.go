package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// bashRun runs a generated script through real bash (the chaining logic
// must hold in the shell that will execute it remotely). The MD_CODE_DIR
// of the sample entry is materialized so the cd succeeds.
func bashRun(t *testing.T, script string) struct {
	exit int
	out  string
} {
	t.Helper()
	if err := os.MkdirAll("/tmp/mdtest/code", 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll("/tmp/mdtest") })
	// macOS test machines lack GNU timeout: shim it on the PATH (the
	// remote environments are Linux and have it).
	shimDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shimDir, "timeout"),
		[]byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+shimDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	res := struct {
		exit int
		out  string
	}{0, string(out)}
	if ee, ok := err.(*exec.ExitError); ok {
		res.exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("bash: %v", err)
	}
	return res
}

// The multi-command script shape: the commands chain with `&&` under one
// timeout each, and a failure short-circuits the rest.
func TestMultiCommandScriptShape(t *testing.T) {
	in := &ScriptInput{
		TaskDir:      "/tmp/mdtest",
		CodeDir:      "/tmp/mdtest/code",
		Entry:        sampleEntry(),
		StageCommand: CommandList{"echo one", "exit 3", "echo three", "exit 0"},
		Timeout:      30,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("script:\n%s", script)
	if !strings.Contains(script, "timeout 30 bash -c 'echo one' && ") {
		t.Error("commands must chain with &&")
	}
	if strings.Contains(script, "STAGE_STATUS") {
		t.Error("no status bookkeeping needed for fail-fast chaining")
	}
	if !strings.HasSuffix(strings.TrimSpace(script), "exit $?") {
		t.Error("missing exit mapping")
	}
}

func TestMultiCommandScriptBash(t *testing.T) {
	for _, tc := range []struct {
		name      string
		cmds      CommandList
		wantExit  int
		wantOut   []string
		notIn     []string
	}{
		{"all pass", CommandList{"echo one", "echo two"}, 0,
			[]string{"one", "two"}, nil},
		{"first fails, rest skipped", CommandList{"exit 3", "echo two"}, 3,
			nil, []string{"two"}},
		{"first failure short-circuits", CommandList{"exit 5", "exit 7", "echo ok"}, 5,
			nil, []string{"ok"}},
		{"mid failure stops the tail", CommandList{"echo a", "exit 9", "echo b"}, 9,
			[]string{"a"}, []string{"b"}},
		{"single command stays inline", CommandList{"echo only"}, 0,
			[]string{"only"}, nil},
	} {
		in := &ScriptInput{
			TaskDir:      "/tmp/mdtest",
			CodeDir:      "/tmp/mdtest/code",
			Entry:        sampleEntry(),
			StageCommand: tc.cmds,
			Timeout:      30,
		}
		script, err := BuildStageScript(in)
		if err != nil {
			t.Fatal(err)
		}
		res := bashRun(t, script)
		if res.exit != tc.wantExit {
			t.Errorf("%s: exit %d, want %d\nscript:\n%s\noutput:\n%s",
				tc.name, res.exit, tc.wantExit, script, res.out)
		}
		for _, want := range tc.wantOut {
			if !strings.Contains(res.out, want) {
				t.Errorf("%s: output missing %q: %s", tc.name, want, res.out)
			}
		}
		for _, banned := range tc.notIn {
			if strings.Contains(res.out, banned) {
				t.Errorf("%s: output must not contain %q (later commands must not run): %s",
					tc.name, banned, res.out)
			}
		}
	}
}

// The list form flows through the whole chain: yaml → merged entry →
// graph snapshot → generated script.
func TestCommandListThroughGraph(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
defaults:
  build:
    command: "cmake ."
presets:
  heat:
    command: ["make prepare", "mpirun ./run_heat"]
matrix:
  - tags: [cpu]
    unit:
      command: ["make data", "ctest -L unit"]
    regression:
      use: [heat]
`))
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := BuildTaskGraph(&entries[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range tasks {
		switch sub.Kind {
		case "unit":
			var sc StageConfig
			if err := json.Unmarshal([]byte(sub.Config), &sc); err != nil {
				t.Fatal(err)
			}
			if len(sc.Command) != 2 || sc.Command[0] != "make data" {
				t.Errorf("unit snapshot wrong: %+v", sc.Command)
			}
			script, err := BuildStageScript(&ScriptInput{
				TaskDir: "/tmp/mdtest", CodeDir: "/tmp/mdtest/code",
				Entry: &entries[0], StageCommand: sc.Command, Timeout: sc.Timeout,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(script, "timeout 3600 bash -c 'make data' && ") ||
				!strings.Contains(script, "timeout 3600 bash -c 'ctest -L unit'") {
				t.Errorf("unit script wrong:\n%s", script)
			}
		case "regression":
			var cc CaseStageConfig
			if err := json.Unmarshal([]byte(sub.Config), &cc); err != nil {
				t.Fatal(err)
			}
			if len(cc.Command) != 2 || cc.Command[1] != "mpirun ./run_heat" {
				t.Errorf("case snapshot wrong: %+v", cc.Command)
			}
		}
	}
}
