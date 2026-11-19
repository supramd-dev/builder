package performance

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseReportReadsTheDocumentedShape(t *testing.T) {
	data := `{
	  "schema": 1,
	  "date": "2026-09-01T08:12:44+08:00",
	  "host": "cpu-node-1",
	  "cases": [
	    {"name": "nvt-1000", "metrics": {
	      "throughput": {"value": 1523.4, "unit": "step/s"},
	      "wall_time":  {"value": 12.4, "unit": "s", "better": "lower"}}},
	    {"name": "poisson", "metrics": {
	      "iterations": {"value": 88, "unit": "1", "better": "HIGHER"}}}
	  ]
	}`

	r, err := ParseReport([]byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Schema != 1 {
		t.Fatalf("schema = %d, want 1", r.Schema)
	}
	wantDate := time.Date(2026, 9, 1, 0, 12, 44, 0, time.UTC)
	if !r.Date.Equal(wantDate) {
		t.Fatalf("date = %v, want %v", r.Date, wantDate)
	}
	if len(r.Cases) != 2 {
		t.Fatalf("got %d cases, want 2", len(r.Cases))
	}

	c := r.Cases[0]
	if c.Name != "nvt-1000" {
		t.Fatalf("case[0].name = %q", c.Name)
	}
	// Metrics come back sorted by name, whatever order the file wrote them in.
	if len(c.Metrics) != 2 || c.Metrics[0].Name != "throughput" || c.Metrics[1].Name != "wall_time" {
		t.Fatalf("case[0].metrics = %+v", c.Metrics)
	}
	if m := c.Metrics[0]; m.Value != 1523.4 || m.Unit != "step/s" || m.Better != BetterHigher {
		t.Fatalf("throughput = %+v", m)
	}
	if m := c.Metrics[1]; m.Value != 12.4 || m.Unit != "s" || m.Better != BetterLower {
		t.Fatalf("wall_time = %+v", m)
	}
	// "better" is case-insensitive; anything else is refused (below).
	if m := r.Cases[1].Metrics[0]; m.Better != BetterHigher {
		t.Fatalf("iterations.better = %q, want %q", m.Better, BetterHigher)
	}
}

func TestParseReportRejectsMalformedReports(t *testing.T) {
	const valid = `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[
		{"name":"case","metrics":{"m":{"value":1,"unit":"s"}}}]}`

	cases := []struct {
		name string
		data string
		want string // substring of the error
	}{
		{"missing date", `{"schema":1,"cases":[{"name":"c","metrics":{"m":{"value":1,"unit":"s"}}}]}`, "date is required"},
		{"unparsable date", `{"schema":1,"date":"2026-09-01","cases":[{"name":"c","metrics":{"m":{"value":1,"unit":"s"}}}]}`, "is not RFC 3339"},
		{"non-string date", `{"schema":1,"date":1756713600,"cases":[]}`, "date must be an RFC 3339 string"},
		{"unsupported schema", `{"schema":2,"date":"2026-09-01T08:00:00Z","cases":[]}`, "unsupported schema 2"},
		{"non-numeric schema", `{"schema":"1"}`, "schema must be the number 1"},
		{"missing cases", `{"schema":1,"date":"2026-09-01T08:00:00Z"}`, "cases is required"},
		{"cases not an array", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":{}}`, "cases must be an array"},
		{"empty cases", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[]}`, "cases must not be empty"},
		{"case without name", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"metrics":{"m":{"value":1,"unit":"s"}}}]}`, "has no name"},
		{"blank case name", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":" ","metrics":{"m":{"value":1,"unit":"s"}}}]}`, "name must not be empty"},
		{"duplicate case", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[
			{"name":"c","metrics":{"m":{"value":1,"unit":"s"}}},
			{"name":"c","metrics":{"m":{"value":2,"unit":"s"}}}]}`, "appears twice"},
		{"missing metrics", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c"}]}`, "metrics is required"},
		{"empty metrics", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{}}]}`, "metrics must not be empty"},
		{"bare number metric", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":1.5}}]}`, "must be an object"},
		{"missing value", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":{"unit":"s"}}}]}`, "has no value"},
		{"non-numeric value", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":{"value":"fast","unit":"s"}}}]}`, "value must be a number"},
		{"missing unit", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":{"value":1.5}}}]}`, "has no unit"},
		{"blank unit", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":{"value":1.5,"unit":" "}}}]}`, "unit must not be empty"},
		{"bad better", `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[{"name":"c","metrics":{"m":{"value":1.5,"unit":"s","better":"faster"}}}]}`, "better must be"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseReport([]byte(tc.data))
			if err == nil {
				t.Fatalf("expected an error")
			}
			if errors.Is(err, ErrNotReport) {
				t.Fatalf("a file with schema 1 is a report, malformed or not; got ErrNotReport")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}

	// The valid document still parses: the table above only fails because of
	// what it changes, not because of the shape.
	if _, err := ParseReport([]byte(valid)); err != nil {
		t.Fatalf("valid report: %v", err)
	}
}

func TestParseReportIgnoresFilesThatAreNotReports(t *testing.T) {
	notReports := []string{
		"",
		"benchmark finished in 12.4s\n",
		`[{"data":[]}]`,
		`{"data":[{"x":[1],"y":[2]}],"layout":{}}`, // a *.plot.json figure document
		`{"host":"cpu-node-1","kernel":"6.1"}`,     // a benchmark's own config dump
		`null`,
	}
	for _, data := range notReports {
		if _, err := ParseReport([]byte(data)); !errors.Is(err, ErrNotReport) {
			t.Fatalf("ParseReport(%.40q) = %v, want ErrNotReport", data, err)
		}
	}
}

func TestParseReportUnknownFieldsAreIgnored(t *testing.T) {
	// Forward compatibility: a newer benchmark may write fields this server
	// does not know, and they must not make the report unreadable.
	_, err := ParseReport([]byte(`{
	  "schema": 1,
	  "date": "2026-09-01T08:00:00Z",
	  "compiler": "gcc 14",
	  "cases": [{"name": "c", "host": "x", "metrics": {"m": {"value": 1, "unit": "s", "note": "gpu"}}}]
	}`))
	if err != nil {
		t.Fatalf("unknown fields: %v", err)
	}
}
