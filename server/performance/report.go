// Package performance reads the benchmark reports a performance stage stores
// and turns them into the history the performance page plots.
//
// A `performance:` stage stores each of its artifact files as a series
// artifact, verbatim: the runner does not interpret them. What makes one of
// those files a *report* is its own content — the schema documented here and
// in frontend/docs/test-matrix.md:
//
//	{
//	  "schema": 1,
//	  "date": "2026-09-01T08:12:44+08:00",
//	  "cases": [
//	    {"name": "nvt-1000",
//	     "metrics": {"throughput": {"value": 1523.4, "unit": "step/s"},
//	                 "wall_time":  {"value": 12.4, "unit": "s", "better": "lower"}}}
//	  ]
//	}
//
// The module is self-contained on purpose: schema, parsing, aggregation and
// the HTTP route all live in this package, and the host server only wires it
// up (`performance.New(store).Register(mux, api.AuthMiddleware)`). Its one
// dependency on the rest of the server is the narrow Loader interface below.
package performance

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ReportSchema is the version of the report format this package reads. A file
// whose `schema` is anything else is refused by name (and listed on the page
// as unreadable) rather than guessed at; a file with no `schema` key at all is
// not a report — see ErrNotReport.
const ReportSchema = 1

// Metric directions: whether a larger value is a better result.
const (
	BetterHigher = "higher" // the default
	BetterLower  = "lower"
)

// Report-size limits. A report is written by the benchmark itself, so these
// only have to be generous enough for real benchmarks and small enough that
// one file cannot flood the page: 200 cases of 50 metrics is far past what a
// chart is worth reading, and the limits are reported (not silently applied).
const (
	maxCases         = 200
	maxMetrics       = 50
	maxCaseNameLen   = 128
	maxMetricNameLen = 64
	maxUnitLen       = 32
)

// ErrNotReport is returned for files that do not claim to be a performance
// report at all: plain text (a log), a JSON document without a `schema` key
// (a *.plotly.json figure, a benchmark's own config dump). The caller ignores
// them silently — an artifact of a performance run is not necessarily a
// measurement. A file that *does* claim to be a report and is malformed
// returns an ordinary error, which is surfaced.
var ErrNotReport = errors.New("not a performance report")

// Metric is one measurement of one case: its value, the unit the value is in
// (the y-axis label of the chart it ends up on) and which way is better.
type Metric struct {
	Name   string
	Value  float64
	Unit   string
	Better string
}

// Case is one named benchmark case and its metrics. Cases within a report are
// kept in the order the file wrote them; metrics are sorted by name.
type Case struct {
	Name    string
	Metrics []Metric
}

// Report is one parsed benchmark report: the measurements of one run, stamped
// with the time the benchmark itself recorded.
type Report struct {
	Schema int
	// Date is when the measurement was taken, taken from the file. It is what
	// the history plots on its x axis: it comes from the benchmark, so reruns
	// of one commit are distinguishable and the report stands on its own
	// without referring to the commit it was produced from.
	Date  time.Time
	Cases []Case
}

// ParseReport reads one benchmark report. A file that is not a report is
// reported as ErrNotReport; anything else that goes wrong is a specific error
// naming the case or metric at fault, and the caller lists it on the page.
func ParseReport(data []byte) (*Report, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		// Not a JSON object: plain text, an array, a bare scalar. Nothing here
		// claims to be a report.
		return nil, ErrNotReport
	}
	rawSchema, ok := doc["schema"]
	if !ok {
		// Valid JSON, but not a report (a figure document, a config dump, a
		// JSON log). Only `schema` marks the format, deliberately: any JSON
		// file of a performance run is otherwise just data.
		return nil, ErrNotReport
	}

	var schema int
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return nil, fmt.Errorf("schema must be the number %d", ReportSchema)
	}
	if schema != ReportSchema {
		return nil, fmt.Errorf("unsupported schema %d (this site reads %d)", schema, ReportSchema)
	}

	rawDate, ok := doc["date"]
	if !ok {
		return nil, errors.New(`date is required: when the measurement was taken, as RFC 3339 ("2026-09-01T08:12:44+08:00")`)
	}
	var dateText string
	if err := json.Unmarshal(rawDate, &dateText); err != nil {
		return nil, errors.New("date must be an RFC 3339 string")
	}
	date, err := time.Parse(time.RFC3339, strings.TrimSpace(dateText))
	if err != nil {
		return nil, fmt.Errorf("date %q is not RFC 3339 (e.g. 2026-09-01T08:12:44+08:00)", dateText)
	}

	rawCases, ok := doc["cases"]
	if !ok {
		return nil, errors.New("cases is required")
	}
	var caseList []json.RawMessage
	if err := json.Unmarshal(rawCases, &caseList); err != nil {
		return nil, errors.New("cases must be an array")
	}
	if len(caseList) == 0 {
		return nil, errors.New("cases must not be empty")
	}
	if len(caseList) > maxCases {
		return nil, fmt.Errorf("too many cases: %d, at most %d", len(caseList), maxCases)
	}

	report := &Report{Schema: schema, Date: date, Cases: make([]Case, 0, len(caseList))}
	seen := make(map[string]bool, len(caseList))
	for i, rawCase := range caseList {
		c, err := parseCase(rawCase, i)
		if err != nil {
			return nil, err
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("case %q appears twice", c.Name)
		}
		seen[c.Name] = true
		report.Cases = append(report.Cases, c)
	}
	return report, nil
}

// parseCase reads one entry of the cases array. i is its position, used to say
// which entry is malformed when it carries no usable name.
func parseCase(raw json.RawMessage, i int) (Case, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Case{}, fmt.Errorf("case %d is not an object", i)
	}

	rawName, ok := fields["name"]
	if !ok {
		return Case{}, fmt.Errorf("case %d has no name", i)
	}
	var name string
	if err := json.Unmarshal(rawName, &name); err != nil {
		return Case{}, fmt.Errorf("case %d: name must be a string", i)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Case{}, fmt.Errorf("case %d: name must not be empty", i)
	}
	if len(name) > maxCaseNameLen {
		return Case{}, fmt.Errorf("case %q: name is longer than %d characters", name, maxCaseNameLen)
	}

	rawMetrics, ok := fields["metrics"]
	if !ok {
		return Case{}, fmt.Errorf("case %q: metrics is required", name)
	}
	var metricFields map[string]json.RawMessage
	if err := json.Unmarshal(rawMetrics, &metricFields); err != nil {
		return Case{}, fmt.Errorf("case %q: metrics must be an object of metric name -> {value, unit}", name)
	}
	if len(metricFields) == 0 {
		return Case{}, fmt.Errorf("case %q: metrics must not be empty", name)
	}
	if len(metricFields) > maxMetrics {
		return Case{}, fmt.Errorf("case %q: too many metrics: %d, at most %d", name, len(metricFields), maxMetrics)
	}

	// Sorted, so two files that list the same metrics in a different order
	// produce the same charts (and the same API response) byte for byte.
	names := make([]string, 0, len(metricFields))
	for n := range metricFields {
		names = append(names, n)
	}
	sort.Strings(names)

	c := Case{Name: name, Metrics: make([]Metric, 0, len(names))}
	for _, metricName := range names {
		m, err := parseMetric(name, metricName, metricFields[metricName])
		if err != nil {
			return Case{}, err
		}
		c.Metrics = append(c.Metrics, m)
	}
	return c, nil
}

// parseMetric reads one metric of a case. Every metric is an object: a bare
// number would carry no unit, and a chart's y axis has to say what it is
// measuring.
func parseMetric(caseName, name string, raw json.RawMessage) (Metric, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Metric{}, fmt.Errorf(`case %q: metric %q must be an object like {"value": 1.2, "unit": "s"}`, caseName, name)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return Metric{}, fmt.Errorf("case %q: a metric name is empty", caseName)
	}
	if len(name) > maxMetricNameLen {
		return Metric{}, fmt.Errorf("case %q: metric name %q is longer than %d characters", caseName, name, maxMetricNameLen)
	}

	rawValue, ok := fields["value"]
	if !ok {
		return Metric{}, fmt.Errorf("case %q: metric %q has no value", caseName, name)
	}
	var value float64
	if err := json.Unmarshal(rawValue, &value); err != nil {
		return Metric{}, fmt.Errorf("case %q: metric %q: value must be a number", caseName, name)
	}
	if !isFinite(value) {
		return Metric{}, fmt.Errorf("case %q: metric %q: value must be a finite number", caseName, name)
	}

	rawUnit, ok := fields["unit"]
	if !ok {
		return Metric{}, fmt.Errorf(`case %q: metric %q has no unit ("1" for a plain count, "%%" for a percentage)`, caseName, name)
	}
	var unit string
	if err := json.Unmarshal(rawUnit, &unit); err != nil {
		return Metric{}, fmt.Errorf("case %q: metric %q: unit must be a string", caseName, name)
	}
	unit = strings.TrimSpace(unit)
	if unit == "" {
		return Metric{}, fmt.Errorf("case %q: metric %q: unit must not be empty", caseName, name)
	}
	if len(unit) > maxUnitLen {
		return Metric{}, fmt.Errorf("case %q: metric %q: unit is longer than %d characters", caseName, name, maxUnitLen)
	}

	// Which way is better is a property of the metric, so the report states it
	// once here rather than the page guessing from the metric's name.
	better := BetterHigher
	if rawBetter, ok := fields["better"]; ok {
		var text string
		if err := json.Unmarshal(rawBetter, &text); err != nil {
			return Metric{}, fmt.Errorf(`case %q: metric %q: better must be "higher" or "lower"`, caseName, name)
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case BetterHigher:
			better = BetterHigher
		case BetterLower:
			better = BetterLower
		default:
			return Metric{}, fmt.Errorf(`case %q: metric %q: better must be "higher" or "lower", not %q`, caseName, name, text)
		}
	}

	return Metric{Name: name, Value: value, Unit: unit, Better: better}, nil
}

// isFinite reports whether f is a real number. JSON cannot spell NaN or an
// infinity, but a value too large for a float64 can arrive as one (+Inf), and
// it would poison every median it entered.
func isFinite(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0)
}
