package runner

import (
	"fmt"
	"strings"
)

// Report markers delimit the line-protocol report in the script output.
const (
	ReportBegin = "===MD-BUILDER-REPORT-BEGIN==="
	ReportEnd   = "===MD-BUILDER-REPORT-END==="
)

// StageOutcome is the result of one test stage (unit / regression).
type StageOutcome struct {
	Status  string // "passed" or "failed"
	Summary string
}

// Report is the parsed outcome of a job's remote execution.
type Report struct {
	Unit       *StageOutcome
	Regression *StageOutcome
}

// ParseReport extracts the line-protocol report from script output. Lines
// between the BEGIN/END markers carry "<stage>-status <status>" and
// "<stage>-summary <text>". A missing marker pair is an error (the script did
// not run to completion); stages absent from the report are nil.
func ParseReport(output string) (*Report, error) {
	begin := strings.Index(output, ReportBegin)
	end := strings.LastIndex(output, ReportEnd)
	if begin < 0 || end < 0 || end < begin {
		return nil, fmt.Errorf("report markers not found in output")
	}
	body := output[begin+len(ReportBegin) : end]

	rep := &Report{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "unit-status ") {
			oc := stageOrNew(rep.Unit)
			oc.Status = strings.TrimSpace(strings.TrimPrefix(line, "unit-status "))
			rep.Unit = oc
		} else if strings.HasPrefix(line, "unit-summary ") {
			oc := stageOrNew(rep.Unit)
			oc.Summary = truncateSummary(strings.TrimSpace(strings.TrimPrefix(line, "unit-summary ")))
			rep.Unit = oc
		} else if strings.HasPrefix(line, "regression-status ") {
			oc := stageOrNew(rep.Regression)
			oc.Status = strings.TrimSpace(strings.TrimPrefix(line, "regression-status "))
			rep.Regression = oc
		} else if strings.HasPrefix(line, "regression-summary ") {
			oc := stageOrNew(rep.Regression)
			oc.Summary = truncateSummary(strings.TrimSpace(strings.TrimPrefix(line, "regression-summary ")))
			rep.Regression = oc
		}
	}
	// Normalize statuses to passed/failed; anything else counts as failed.
	for _, st := range []*StageOutcome{rep.Unit, rep.Regression} {
		if st != nil && st.Status != "passed" && st.Status != "failed" {
			st.Status = "failed"
		}
	}
	return rep, nil
}

func stageOrNew(st *StageOutcome) *StageOutcome {
	if st != nil {
		return st
	}
	return &StageOutcome{}
}

// maxSummaryLen caps a stored summary.
const maxSummaryLen = 500

func truncateSummary(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxSummaryLen {
		return s[:maxSummaryLen]
	}
	return s
}
