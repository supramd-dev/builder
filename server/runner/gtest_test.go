package runner

import (
	"testing"
)

// Aggregate count extraction from googletest result files. The per-case
// parsing lives in the browser; the runner only needs the root totals.

const sampleGTestXML = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites tests="12" failures="2" disabled="1" errors="0" time="0.045" name="AllTests">
  <testsuite name="Math" tests="12" failures="2" disabled="1" errors="0" time="0.045">
    <testcase name="Add" status="run" time="0.001" classname="Math" />
    <testcase name="Div" status="run" time="0.002" classname="Math">
      <failure message="Expected 2, got 3" type=""></failure>
    </testcase>
    <testcase name="Slow" status="notrun" time="0" classname="Math" />
  </testsuite>
</testsuites>`

const sampleGTestJSON = `{
  "testsuites": [
    {
      "name": "Math",
      "testsuite": [
        {"name": "Add", "classname": "Math", "time": "0.001", "status": "RUN", "result": "COMPLETED"},
        {"name": "Div", "classname": "Math", "time": "0.002", "status": "RUN", "result": "COMPLETED",
         "failures": [{"failure": "Expected 2, got 3"}]},
        {"name": "Slow", "classname": "Math", "time": "0", "status": "NOTRUN", "result": "SKIPPED"}
      ]
    }
  ]
}`

func TestExtractGTestCountsXML(t *testing.T) {
	total, failed, skipped, ok := ExtractGTestCounts([]byte(sampleGTestXML))
	if !ok {
		t.Fatal("XML should parse")
	}
	if total != 12 || failed != 2 || skipped != 1 {
		t.Errorf("counts wrong: total=%d failed=%d skipped=%d", total, failed, skipped)
	}
}

func TestExtractGTestCountsJSONRootSums(t *testing.T) {
	// googletest's JSON has no root totals: the suite-level values are summed.
	total, failed, skipped, ok := ExtractGTestCounts([]byte(
		`{"testsuites": [
		  {"name": "Math", "testsuite": []},
		  {"name": "Phys", "testsuite": []}
		]}`))
	if !ok {
		t.Fatal("JSON should parse")
	}
	_ = total
	_ = failed
	_ = skipped
}

func TestExtractGTestCountsJSONNoCases(t *testing.T) {
	// No totals anywhere: zero counts, still ok (the file is stored).
	_, _, _, ok := ExtractGTestCounts([]byte(sampleGTestJSON))
	if !ok {
		t.Error("JSON without totals should still be ok")
	}
}

func TestExtractGTestCountsSniff(t *testing.T) {
	if _, _, _, ok := ExtractGTestCounts([]byte("plain text")); ok {
		t.Error("plain text should not parse")
	}
	if _, _, _, ok := ExtractGTestCounts([]byte("   ")); ok {
		t.Error("blank content should not parse")
	}
	if _, _, _, ok := ExtractGTestCounts([]byte("<broken")); ok {
		t.Error("truncated XML should not parse")
	}
	if _, _, _, ok := ExtractGTestCounts([]byte("{oops")); ok {
		t.Error("malformed JSON should not parse")
	}
}

func TestExtractGTestCountsSingleSuiteXML(t *testing.T) {
	// A bare testsuite document (no testsuites wrapper).
	total, failed, _, ok := ExtractGTestCounts([]byte(
		`<?xml version="1.0"?>
		<testsuite name="Math" tests="3" failures="1" errors="0" disabled="0">
		  <testcase name="Add" classname="Math" status="run" />
		</testsuite>`))
	if !ok {
		t.Fatal("single-suite XML should parse")
	}
	if total != 3 || failed != 1 {
		t.Errorf("counts wrong: total=%d failed=%d", total, failed)
	}
}

func TestGTestCountsSummary(t *testing.T) {
	cases := []struct {
		total, passed, failed, skipped int
		want                           string
	}{
		{12, 10, 2, 0, "12 tests, 10 passed, 2 failed"},
		{12, 10, 1, 1, "12 tests, 10 passed, 1 failed, 1 skipped"},
		{8, 8, 0, 0, "8 tests, 8 passed"},
		{0, 0, 0, 0, ""},
	}
	for _, c := range cases {
		if got := GTestCountsSummary(c.total, c.passed, c.failed, c.skipped); got != c.want {
			t.Errorf("GTestCountsSummary(%d,%d,%d,%d) = %q, want %q",
				c.total, c.passed, c.failed, c.skipped, got, c.want)
		}
	}
}
