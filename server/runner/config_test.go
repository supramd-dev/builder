package runner

import (
	"encoding/json"
	"strings"
	"testing"
)

// md-builder.yaml v2 parsing: presets, use/disable expansion, workdir and
// validation.

func TestParseConfigV2Presets(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
presets:
  heat:
    description: "heat equation"
    command: "mpirun ./run_heat"
    workdir: "regression/heat"
    timeout: 1800
    results: "regression/heat/out.xml"
  poisson:
    command: "./run_poisson"
    results: ["out/a.xml", "out/b.json"]
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
    regression:
      use: [heat, poisson]
  - tags: [gpu]
    regression: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}

	// Entry 1 selected both presets.
	reg := entries[0].Regression
	if len(reg) != 2 {
		t.Fatalf("entry 0: want 2 cases, got %d", len(reg))
	}
	if reg[0].Name != "heat" || reg[0].Command != "mpirun ./run_heat" ||
		reg[0].Workdir != "regression/heat" || reg[0].Timeout != 1800 ||
		len(reg[0].Results) != 1 || reg[0].Results[0] != "regression/heat/out.xml" {
		t.Errorf("heat case wrong: %+v", reg[0])
	}
	if reg[1].Name != "poisson" || reg[1].Workdir != "" || len(reg[1].Results) != 2 {
		t.Errorf("poisson case wrong: %+v", reg[1])
	}

	// Entry 2 used every preset (empty regression stanza).
	if len(entries[1].Regression) != 2 {
		t.Fatalf("entry 1: want 2 cases (all presets), got %d", len(entries[1].Regression))
	}
	if entries[1].Regression[0].Name != "heat" || entries[1].Regression[1].Name != "poisson" {
		t.Errorf("all-presets selection wrong: %+v", entries[1].Regression)
	}
}

func TestParseConfigV2UseDisable(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
presets:
  a: {command: "./a"}
  b: {command: "./b"}
  c: {command: "./c"}
matrix:
  - tags: [cpu]
    regression:
      use: [a, b]
      disable: [b]
  - tags: [gpu]
    regression:
      disable: [a]
`))
	if err != nil {
		t.Fatal(err)
	}
	if names := caseNames(entries[0].Regression); len(names) != 1 || names[0] != "a" {
		t.Errorf("use+disable wrong: %v", names)
	}
	// disable without use drops from the all-preset selection.
	if names := caseNames(entries[1].Regression); len(names) != 2 || names[0] != "b" || names[1] != "c" {
		t.Errorf("disable-from-all wrong: %v", names)
	}
}

func TestParseConfigV2UnknownPreset(t *testing.T) {
	if _, err := ParseConfig([]byte(`version: 2
presets:
  a: {command: "./a"}
matrix:
  - tags: [cpu]
    regression:
      use: [nope]
`)); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("unknown use preset should error: %v", err)
	}
	if _, err := ParseConfig([]byte(`version: 2
presets:
  a: {command: "./a"}
matrix:
  - tags: [cpu]
    regression:
      disable: [nope]
`)); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Errorf("unknown disable preset should error: %v", err)
	}
}

func TestParseConfigV2PresetNeedsCommand(t *testing.T) {
	if _, err := ParseConfig([]byte(`version: 2
presets:
  a: {workdir: "x"}
matrix:
  - tags: [cpu]
    regression: {}
`)); err == nil || !strings.Contains(err.Error(), "needs a command") {
		t.Errorf("preset without command should error: %v", err)
	}
}

func TestParseConfigV2NoStages(t *testing.T) {
	// Unit missing and regression resolving to nothing.
	if _, err := ParseConfig([]byte(`version: 2
presets:
  a: {command: "./a"}
matrix:
  - tags: [cpu]
    regression:
      use: [a]
      disable: [a]
`)); err == nil || !strings.Contains(err.Error(), "no stage") {
		t.Errorf("fully-disabled regression without unit should error: %v", err)
	}
}

func TestParseConfigV2VersionCheck(t *testing.T) {
	if _, err := ParseConfig([]byte(`version: 1
matrix:
  - tags: [cpu]
    unit: {command: "ctest"}
`)); err == nil || !strings.Contains(err.Error(), "version must be 2") {
		t.Errorf("version 1 should be rejected: %v", err)
	}
}

func TestParseConfigV2Workdirs(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
defaults:
  build:
    workdir: "build-default"
  unit:
    workdir: "unit-default"
matrix:
  - tags: [cpu]
    unit:
      command: "ctest"
    build:
      workdir: "build-entry"
`))
	if err != nil {
		t.Fatal(err)
	}
	e := entries[0]
	if e.Build.Workdir != "build-entry" {
		t.Errorf("entry build workdir should win: %q", e.Build.Workdir)
	}
	if e.Unit.Workdir != "unit-default" {
		t.Errorf("defaults unit workdir should apply: %q", e.Unit.Workdir)
	}
}

func TestParseConfigV2PresetTimeoutDefault(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 2
defaults:
  timeout: 900
presets:
  a: {command: "./a"}
matrix:
  - tags: [cpu]
    regression: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Regression[0].Timeout; got != 900 {
		t.Errorf("preset timeout should fall back to defaults.timeout: got %d", got)
	}
}

// ResultsPaths JSON: legacy snapshots stored a single string; new ones store
// a list. Both decode; encoding always emits the list form.
func TestResultsPathsJSONRoundTrip(t *testing.T) {
	var legacy ResultsPaths
	if err := json.Unmarshal([]byte(`"build/test_detail.xml"`), &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0] != "build/test_detail.xml" {
		t.Errorf("legacy scalar: %+v", legacy)
	}

	var list ResultsPaths
	if err := json.Unmarshal([]byte(`["a.xml","b.json"]`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0] != "a.xml" || list[1] != "b.json" {
		t.Errorf("list form: %+v", list)
	}

	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `["a.xml","b.json"]` {
		t.Errorf("marshal: %s", b)
	}

	var empty ResultsPaths
	b, err = json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Errorf("marshal empty: %s", b)
	}
	if err := json.Unmarshal([]byte(`null`), &empty); err != nil || len(empty) != 0 {
		t.Errorf("null should decode empty: %v %+v", err, empty)
	}
}

func TestResultsPathsClean(t *testing.T) {
	in := ResultsPaths{"  a.xml ", "", "a.xml", "b.json", " "}
	got := in.Clean()
	if len(got) != 2 || got[0] != "a.xml" || got[1] != "b.json" {
		t.Errorf("clean: %+v", got)
	}
}

func caseNames(cases []RegressionCase) []string {
	out := make([]string, len(cases))
	for i := range cases {
		out[i] = cases[i].Name
	}
	return out
}
