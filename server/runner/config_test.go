package runner

import (
	"encoding/json"
	"testing"
)

// md-builder.yaml parsing: the results field accepts one path or a list.

func TestParseConfigResultsScalar(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 1
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
      results: "build/test_detail.xml"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Unit.Results; len(got) != 1 || got[0] != "build/test_detail.xml" {
		t.Errorf("scalar results: %+v", got)
	}
}

func TestParseConfigResultsList(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 1
defaults:
  unit:
    results: ["build/test_detail.xml"]
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
      results:
        - "build/test_detail.xml"
        - "build/extra_results.json"
  - tags: [gpu]
    regression:
      command: "python3 run.py"
      results: "reg/results.json"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Unit.Results; len(got) != 2 ||
		got[0] != "build/test_detail.xml" || got[1] != "build/extra_results.json" {
		t.Errorf("list results: %+v", got)
	}
	if got := entries[1].Regression.Results; len(got) != 1 || got[0] != "reg/results.json" {
		t.Errorf("scalar regression results: %+v", got)
	}
}

func TestParseConfigResultsOmitted(t *testing.T) {
	entries, err := ParseConfig([]byte(`version: 1
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
`))
	if err != nil {
		t.Fatal(err)
	}
	if got := entries[0].Unit.Results; len(got) != 0 {
		t.Errorf("omitted results should be empty: %+v", got)
	}
}

func TestParseConfigResultsInvalid(t *testing.T) {
	if _, err := ParseConfig([]byte(`version: 1
matrix:
  - tags: [cpu]
    unit:
      command: "ctest"
      results: {a: b}
`)); err == nil {
		t.Error("a mapping should not parse as results")
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
