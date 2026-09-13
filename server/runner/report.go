package runner

import (
	"fmt"
	"strings"
)

// Summary extraction. The former line-protocol report (REPORT-BEGIN/END
// markers) is gone: each sub-task streams its full output into the task
// log, and its outcome is derived from the exit code. What remains is the
// one-paragraph summary convention: a stage command may print a line
// starting with MD-BUILDER-SUMMARY: which becomes the stored test-run
// summary; without it the summary is the exit code plus the log tail.

// SummaryPrefix is the line prefix a stage command prints to declare its
// own one-line conclusion.
const SummaryPrefix = "MD-BUILDER-SUMMARY: "

// maxSummaryLen caps a stored summary.
const maxSummaryLen = 500

// ExtractSummary pulls the test-run summary from a stage's log output: the
// first MD-BUILDER-SUMMARY: line when present, otherwise "exit N" plus the
// last lines of the output flattened to one line.
func ExtractSummary(output string, exitCode int) string {
	if line, ok := summaryLine(output); ok {
		return truncateSummary(line)
	}
	return truncateSummary(fmt.Sprintf("exit %d; %s", exitCode, tailLine(output, 5)))
}

// hasSummaryLine reports whether the output carries a MD-BUILDER-SUMMARY
// line (the stage's own conclusion, preferred over derived counts).
func hasSummaryLine(output string) bool {
	_, ok := summaryLine(output)
	return ok
}

// summaryLine returns the text of the first MD-BUILDER-SUMMARY line.
func summaryLine(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, SummaryPrefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, SummaryPrefix)), true
		}
	}
	return "", false
}

// TailLine returns the last n lines of s, flattened to one line (used for
// failure summaries and error messages).
func TailLine(s string, n int) string { return tailLine(s, n) }

func tailLine(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	start := 0
	if len(lines) > n {
		start = len(lines) - n
	}
	joined := strings.Join(strings.Fields(strings.Join(lines[start:], " ")), " ")
	return joined
}

func truncateSummary(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > maxSummaryLen {
		return s[:maxSummaryLen]
	}
	return s
}
