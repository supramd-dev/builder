package runner

import (
	"strings"
	"testing"
)

func sampleEntry() *MergedEntry {
	return &MergedEntry{
		Tags:    []string{"cpu"},
		Timeout: 600,
		Build: BuildConfig{
			Generator:  GeneratorCMake,
			CMakeFlags: "-DCMAKE_BUILD_TYPE=Release",
			Threads:    4,
		},
		Env:        map[string]string{"CC": "gcc", "WITH-SPACE": "a b"},
		Unit:       &EnvConfig{Command: "ctest -L unit", Timeout: 300},
		Regression: &EnvConfig{Command: "python3 run.py --suite full"},
	}
}

func TestBuildScriptCMake(t *testing.T) {
	script, err := BuildScript(&ScriptInput{
		CommitSHA:     "0123456789abcdef",
		CodeRepoURL:   "https://gitlab.example.com/group/code",
		TestInputRepo: "https://gitlab.example.com/group/tests",
		TestInputRef:  "main",
		EnvName:       "cpu-node-1",
		EnvTags:       "cpu",
		Entry:         sampleEntry(),
	})
	if err != nil {
		t.Fatalf("build script: %v", err)
	}

	for _, want := range []string{
		"set -uo pipefail",
		"git clone --quiet --depth 1 --branch 'main' 'https://gitlab.example.com/group/tests' \"$TESTS\"",
		"git clone --quiet 'https://gitlab.example.com/group/code' \"$CODE\"",
		"git checkout --quiet '0123456789abcdef'",
		"export MD_COMMIT='0123456789abcdef'",
		"export MD_ENV_NAME='cpu-node-1'",
		"export MD_CODE_DIR=\"$CODE\"",
		"export MD_TEST_INPUT_DIR=\"$TESTS\"",
		"export CC='gcc'",
		"export WITH-SPACE='a b'",
		"timeout 600 cmake -DCMAKE_BUILD_TYPE=Release .",
		"timeout 600 cmake --build . -j4",
		"timeout 300 bash -c 'ctest -L unit'",
		"timeout 600 bash -c 'python3 run.py --suite full'", // falls back to entry timeout
		"===MD-BUILDER-REPORT-BEGIN===",
		"===MD-BUILDER-REPORT-END===",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q\n--- script ---\n%s", want, script)
		}
	}
	// Single quotes in values must be escaped.
	if strings.Contains(script, "'a b'") && strings.Contains(script, "'\\''") {
		t.Error("unexpected double escaping")
	}
}

func TestBuildScriptScriptGeneratorAndNoTestInput(t *testing.T) {
	entry := sampleEntry()
	entry.Build = BuildConfig{Generator: GeneratorScript, Command: "./build.sh --cuda"}
	script, err := BuildScript(&ScriptInput{
		CommitSHA:   "abc",
		CodeRepoURL: "https://gitlab.example.com/group/code",
		EnvName:     "gpu",
		EnvTags:     "gpu,cuda",
		Entry:       entry,
	})
	if err != nil {
		t.Fatalf("build script: %v", err)
	}
	if !strings.Contains(script, "timeout 600 bash -c './build.sh --cuda'") {
		t.Errorf("script generator command missing:\n%s", script)
	}
	if strings.Contains(script, "git clone --quiet --depth 1") {
		t.Errorf("no test-input repo configured; tests clone should be omitted:\n%s", script)
	}
	if !strings.Contains(script, "mkdir -p \"$TESTS\"") {
		t.Errorf("TESTS should be created empty:\n%s", script)
	}
}

func TestTotalScriptTimeout(t *testing.T) {
	entry := sampleEntry()
	total, err := TotalScriptTimeout(entry)
	if err != nil {
		t.Fatal(err)
	}
	// unit 300 + regression 600 (entry fallback) + build 600 + slack 900.
	if total != 300+600+600+900 {
		t.Errorf("unexpected total: %d", total)
	}
}

func TestParseReport(t *testing.T) {
	out := `some noise
===MD-BUILDER-REPORT-BEGIN===
unit-status passed
unit-summary all 12 tests passed
regression-status failed
regression-summary max relative error 3.2e-7 exceeds tolerance 1e-8
===MD-BUILDER-REPORT-END===
trailing noise
`
	rep, err := ParseReport(out)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Unit == nil || rep.Unit.Status != "passed" || rep.Unit.Summary != "all 12 tests passed" {
		t.Errorf("unit outcome wrong: %+v", rep.Unit)
	}
	if rep.Regression == nil || rep.Regression.Status != "failed" {
		t.Errorf("regression outcome wrong: %+v", rep.Regression)
	}
	if !strings.Contains(rep.Regression.Summary, "3.2e-7") {
		t.Errorf("regression summary wrong: %q", rep.Regression.Summary)
	}
}

func TestParseReportPartialAndMissing(t *testing.T) {
	// Only unit reported.
	rep, err := ParseReport("===MD-BUILDER-REPORT-BEGIN===\nunit-status failed\nunit-summary timeout\n===MD-BUILDER-REPORT-END===\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Unit == nil || rep.Unit.Status != "failed" {
		t.Errorf("unit wrong: %+v", rep.Unit)
	}
	if rep.Regression != nil {
		t.Errorf("regression should be nil: %+v", rep.Regression)
	}

	// No markers at all: the script died before reporting.
	if _, err := ParseReport("clone: fatal: repository not found"); err == nil {
		t.Error("expected error for missing markers")
	}
}

func TestParseReportTruncatesAndNormalizes(t *testing.T) {
	long := strings.Repeat("x", 600)
	rep, err := ParseReport("===MD-BUILDER-REPORT-BEGIN===\nunit-status weird\nunit-summary " + long + "\n===MD-BUILDER-REPORT-END===\n")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Unit.Status != "failed" {
		t.Errorf("unknown status should normalize to failed: %q", rep.Unit.Status)
	}
	if len(rep.Unit.Summary) != 500 {
		t.Errorf("summary should truncate to 500, got %d", len(rep.Unit.Summary))
	}
}
