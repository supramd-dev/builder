package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The `variables:` block of a matrix entry: rendered as shell words, expanded
// against the built-in MD_* variables, the entry's other variables and the
// site's allow-listed host variables — and inert for everything else.

// varsEntry builds a minimal entry carrying the given variables.
func varsEntry(vars map[string]string) *MergedEntry {
	return &MergedEntry{
		Tags:      []string{"cpu"},
		Timeout:   30,
		Env:       map[string]string{"CC": "gcc"},
		Variables: vars,
		Build:     BuildConfig{Command: CommandList{"true"}},
	}
}

// renderVars renders one entry's variables block as the lines it produces.
func renderVars(in *ScriptInput) string {
	var b strings.Builder
	ExportVariables(func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }, in)
	return b.String()
}

func TestVariableRendering(t *testing.T) {
	got := renderVars(&ScriptInput{
		CommitSHA:  "abc1234",
		TaskDir:    "/tmp/mdtest",
		CodeDir:    "/tmp/mdtest/code",
		AllowedEnv: []string{"SCRATCH"},
		Entry: varsEntry(map[string]string{
			"SUPRAMD_PATH": "$BUILD_ROOT/bin/supramd", // depends on BUILD_ROOT
			"BUILD_ROOT":   "$MD_CODE_DIR/cmake-build",
			"RUN_DIR":      "$SCRATCH/runs-$MD_COMMIT",
			"PRICE":        "5$$ per run",
			"STRAY":        "$NOT_ALLOWED/stray",
			"EMPTY":        "",
		}),
	})

	// A dependency is exported before the variable that references it, so the
	// shell expands it with the value already in place. Built-ins and allowed
	// host variables expand (double-quoted), literal runs are single-quoted.
	want := []string{
		`export BUILD_ROOT="${MD_CODE_DIR}"'/cmake-build'`,
		`export SUPRAMD_PATH="${BUILD_ROOT}"'/bin/supramd'`,
		`export STRAY='$NOT_ALLOWED/stray'`,
		`export EMPTY=''`,
		`export PRICE='5$ per run'`,
		`export RUN_DIR="${SCRATCH}"'/runs-'"${MD_COMMIT}"`,
		"echo 'warning: variables.STRAY: $NOT_ALLOWED is not a built-in variable, a variables entry or an allowed environment variable; kept literally' >&2",
	}
	for _, line := range want {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("rendered variables missing %q; got:\n%s", line, got)
		}
	}
	// The warning goes to stderr (the task log) *before* the export it is
	// about, so reading the log top-down explains the value below it.
	if strings.Index(got, "warning: variables.STRAY") > strings.Index(got, "export STRAY=") {
		t.Errorf("the warning must precede the export it explains:\n%s", got)
	}
	if strings.Contains(got, "`") {
		t.Errorf("rendering leaked shell syntax:\n%s", got)
	}
}

// TestVariableOrderingAndEscapes covers the corners that need no shell.
func TestVariableOrderingAndEscapes(t *testing.T) {
	// A chain three deep, declared in reverse order.
	got := renderVars(&ScriptInput{Entry: varsEntry(map[string]string{
		"C": "$B/c",
		"B": "$A/b",
		"A": "$MD_TASK_DIR/a",
	})})
	iA, iB, iC := strings.Index(got, "export A="), strings.Index(got, "export B="), strings.Index(got, "export C=")
	if iA < 0 || iB < iA || iC < iB {
		t.Errorf("variables must be exported in dependency order, got:\n%s", got)
	}

	// One reference repeated, a quote next to a reference, a reference that is
	// the whole value: all render as one shell word holding the value as
	// written.
	got = renderVars(&ScriptInput{CodeDir: "/tmp/mdtest/code", Entry: varsEntry(map[string]string{
		"ONLY":   "$MD_CODE_DIR",
		"DUP":    "$MD_CODE_DIR/$MD_CODE_DIR",
		"QUOTED": "it's $MD_CODE_DIR's",
	})})
	for _, line := range []string{
		`export ONLY="${MD_CODE_DIR}"`,
		`export DUP="${MD_CODE_DIR}"'/'"${MD_CODE_DIR}"`,
	} {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("rendered variables missing %q; got:\n%s", line, got)
		}
	}
	if !strings.Contains(got, `'it'\''s '"${MD_CODE_DIR}"''\''s'`) {
		t.Errorf("a quote in a literal run must be escaped for the shell:\n%s", got)
	}

	// Malformed and non-reference dollars stay text; an escaped "$$" is one.
	got = renderVars(&ScriptInput{Entry: varsEntry(map[string]string{"M": "50$ ${ } $$HOME"})})
	if !strings.Contains(got, `export M='50$ ${ } $HOME'`+"\n") {
		t.Errorf("dollar handling wrong:\n%s", got)
	}
}

// TestVariablesExpandInBash runs the generated script through real bash: the
// promise is about what the remote shell ends up with, not about the text.
func TestVariablesExpandInBash(t *testing.T) {
	// The site's whitelist names this one, and the host has it in its
	// environment (the login shell of a real build host plays this part).
	t.Setenv("MDTEST_HOST_ROOT", "/opt/supramd")

	in := &ScriptInput{
		CommitSHA:  "abc1234",
		EnvName:    "cpu-node-1",
		TaskDir:    "/tmp/mdtest",
		CodeDir:    "/tmp/mdtest/code",
		AllowedEnv: []string{"MDTEST_HOST_ROOT"},
		Entry: varsEntry(map[string]string{
			"BUILD_ROOT":   "$MD_CODE_DIR/build",
			"SUPRAMD_PATH": "$BUILD_ROOT/bin/supramd", // chained
			"HOST_PATH":    "$MDTEST_HOST_ROOT/bin/supramd",
			"BLOCKED":      "$NOT_ALLOWED/x",
		}),
		StageCommand: CommandList{`echo "$SUPRAMD_PATH|$HOST_PATH|$BLOCKED"`},
		Timeout:      30,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	res := bashRun(t, script)
	if res.exit != 0 {
		t.Fatalf("script failed (exit %d):\n%s\n--- script ---\n%s", res.exit, res.out, script)
	}
	want := "/tmp/mdtest/code/build/bin/supramd|/opt/supramd/bin/supramd|$NOT_ALLOWED/x"
	if !strings.Contains(res.out, want) {
		t.Errorf("expanded variables wrong:\nwant line %q\ngot:\n%s", want, res.out)
	}
	// The unresolved reference is reported, so a literal "$" in a path is
	// never silent again.
	if !strings.Contains(res.out, "warning: variables.BLOCKED: $NOT_ALLOWED") {
		t.Errorf("missing warning for the unexpandable reference:\n%s", res.out)
	}
}

// TestVariableValueNotWhitelistedIsLiteral pins the negative: with the same
// config and the name missing from the whitelist, the host value does not
// leak into the script.
func TestVariableValueNotWhitelistedIsLiteral(t *testing.T) {
	t.Setenv("MDTEST_HOST_ROOT", "/opt/supramd")
	in := &ScriptInput{
		TaskDir:      "/tmp/mdtest",
		CodeDir:      "/tmp/mdtest/code",
		Entry:        varsEntry(map[string]string{"HOST_PATH": "$MDTEST_HOST_ROOT/bin"}),
		StageCommand: CommandList{`echo "[$HOST_PATH]"`},
		Timeout:      30,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	res := bashRun(t, script)
	if !strings.Contains(res.out, "[$MDTEST_HOST_ROOT/bin]") {
		t.Errorf("an unwhitelisted name must stay literal, got: %s\n--- script ---\n%s", res.out, script)
	}
	if strings.Contains(res.out, "/opt/supramd") {
		t.Errorf("an unwhitelisted host variable leaked into the script:\n%s", res.out)
	}
	if !strings.Contains(res.out, "warning: variables.HOST_PATH: $MDTEST_HOST_ROOT") {
		t.Errorf("the kept-literal reference must be reported:\n%s", res.out)
	}
}

// TestVariableValuesCannotInjectShell checks the other half of the contract:
// a value is data. Command substitution, backticks and quote-breaking text in
// a template must never run, however they are spliced around a real
// reference — which is where a quoting bug would show.
func TestVariableValuesCannotInjectShell(t *testing.T) {
	dir := t.TempDir()
	sub, tick, brk := filepath.Join(dir, "SUB"), filepath.Join(dir, "TICK"), filepath.Join(dir, "BRK")

	in := &ScriptInput{
		CommitSHA: "abc1234",
		TaskDir:   "/tmp/mdtest",
		CodeDir:   "/tmp/mdtest/code",
		Entry: varsEntry(map[string]string{
			"SUBSTITUTION": "$MD_CODE_DIR$(touch " + sub + ")",
			"BACKTICK":     "$MD_CODE_DIR`touch " + tick + "`",
			"QUOTEBREAK":   "$MD_CODE_DIR'; touch " + brk + "; echo '",
		}),
		StageCommand: CommandList{`printf '%s\n' "$SUBSTITUTION" "$BACKTICK" "$QUOTEBREAK"`},
		Timeout:      30,
	}
	script, err := BuildStageScript(in)
	if err != nil {
		t.Fatal(err)
	}
	res := bashRun(t, script)
	if res.exit != 0 {
		t.Fatalf("script failed (exit %d):\n%s\n--- script ---\n%s", res.exit, res.out, script)
	}
	for _, p := range []string{sub, tick, brk} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a variable value executed a command: %s exists", p)
		}
	}
	// The values arrive as written: the reference inside them expanded, the
	// command substitution left alone.
	if !strings.Contains(res.out, "/tmp/mdtest/code$(touch "+sub+")") ||
		!strings.Contains(res.out, "/tmp/mdtest/code`touch "+tick+"`") ||
		!strings.Contains(res.out, "/tmp/mdtest/code'; touch "+brk+"; echo '") {
		t.Errorf("values were not kept literal:\n%s", res.out)
	}
}

// TestConfigVariablesValidation covers the parse-time rules: an entry whose
// variables cannot be rendered into a sane script is refused before dispatch.
func TestConfigVariablesValidation(t *testing.T) {
	parse := func(entryBody string) ([]MergedEntry, error) {
		return ParseConfig([]byte(`version: 2
matrix:
  - tags: [cpu]
` + entryBody + `    build:
      command: "make"
    unit:
      command: "ctest"
`))
	}

	entries, err := parse("    variables:\n      SUPRAMD: \"$MD_CODE_DIR/bin/supramd\"\n")
	if err != nil {
		t.Fatalf("a valid variables block must parse: %v", err)
	}
	if got := entries[0].Variables["SUPRAMD"]; got != "$MD_CODE_DIR/bin/supramd" {
		t.Errorf("variables not carried into the merged entry: %q", got)
	}

	cases := []struct {
		name, body, want string
	}{
		{"identifier", "    variables:\n      \"not a name\": \"x\"\n", "not a valid shell identifier"},
		{"reserved", "    variables:\n      MD_CODE_DIR: \"x\"\n", "reserved"},
		{"env clash", "    env:\n      CC: \"gcc\"\n    variables:\n      CC: \"x\"\n", "in both env and variables"},
		{"self cycle", "    variables:\n      A: \"$A/x\"\n", "cycle"},
		{"cycle", "    variables:\n      A: \"$B/x\"\n      B: \"$A/y\"\n", "cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.body)
			if err == nil {
				t.Fatalf("want a parse error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestConfigVariablesDefaultsMerge covers the defaults/entry split: an entry
// inherits the shared variables and may override one of them.
func TestConfigVariablesDefaultsMerge(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
defaults:
  variables:
    BUILD_ROOT: "$MD_CODE_DIR/build"
    TOOLCHAIN: "gcc"
matrix:
  - tags: [cpu]
    build:
      command: "make"
    unit:
      command: "ctest"
  - tags: [gpu]
    variables:
      TOOLCHAIN: "nvcc"
      SUPRAMD: "$BUILD_ROOT/bin/supramd"
    build:
      command: "make"
    unit:
      command: "ctest"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Variables; len(got) != 2 || got["TOOLCHAIN"] != "gcc" || got["BUILD_ROOT"] != "$MD_CODE_DIR/build" {
		t.Errorf("entry 0 must inherit both defaults, got %v", got)
	}
	got := entries[1].Variables
	if len(got) != 3 || got["TOOLCHAIN"] != "nvcc" || got["SUPRAMD"] != "$BUILD_ROOT/bin/supramd" {
		t.Errorf("entry 1 must override TOOLCHAIN and keep the rest, got %v", got)
	}
	// Entries without variables stay nil, so no exports are emitted for them.
	if entries, err := ParseConfig([]byte(`version: 2
matrix:
  - tags: [cpu]
    build:
      command: "make"
    unit:
      command: "ctest"
`)); err != nil || entries[0].Variables != nil {
		t.Errorf("an entry without variables must keep a nil map (err=%v, got %v)", err, entries[0].Variables)
	}
}
