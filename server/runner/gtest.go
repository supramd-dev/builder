package runner

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"
)

// Aggregate googletest count extraction. The runner deliberately does not
// parse per-case results: the results file is stored verbatim as a run
// artifact and the browser renders the per-case list. What the backend needs
// for the matrix cell are the root totals only, read from the file's
// top-level attributes (or summed over suites when absent).

// gtestCounts is the aggregate a googletest report carries at its root.
type gtestCounts struct {
	Total   int
	Failed  int // failures + errors
	Skipped int // disabled / not run
}

// ExtractGTestCounts pulls the aggregate totals from a googletest results
// file (XML or JSON, sniffed by the first non-space byte). ok is false when
// the content is neither parseable nor recognizable — the caller then keeps
// its previous counts (zero) and still stores the file as an artifact.
func ExtractGTestCounts(data []byte) (total, failed, skipped int, ok bool) {
	trimmed := strings.TrimLeft(string(data), " \t\r\n")
	if trimmed == "" {
		return 0, 0, 0, false
	}
	var c gtestCounts
	var err error
	switch trimmed[0] {
	case '<':
		c, err = gtestCountsXML(trimmed)
	case '{':
		c, err = gtestCountsJSON(trimmed)
	default:
		return 0, 0, 0, false
	}
	if err != nil {
		return 0, 0, 0, false
	}
	return c.Total, c.Failed, c.Skipped, true
}

// gtestRootAttrs is the attribute set both XML and JSON roots carry.
type gtestRootAttrs struct {
	Tests    int `xml:"tests,attr" json:"tests"`
	Failures int `xml:"failures,attr" json:"failures"`
	Errors   int `xml:"errors,attr" json:"errors"`
	Disabled int `xml:"disabled,attr" json:"disabled"`
}

// gtestCountsXML reads the totals off the first element (testsuites, or a
// single testsuite document). Only the root start element is consumed —
// per-case nodes are the browser's job.
func gtestCountsXML(data string) (gtestCounts, error) {
	dec := xml.NewDecoder(strings.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return gtestCounts{}, err
		}
		if start, isStart := tok.(xml.StartElement); isStart {
			var attrs gtestRootAttrs
			if err := dec.DecodeElement(&attrs, &start); err != nil {
				// DecodeElement walks the whole subtree; the attributes were
				// already read into attrs by the unmarshaler before any child
				// issue — fall back to manual attr scan on malformed children.
				attrs = gtestRootAttrsFromXML(start)
			}
			return countsFromAttrs(attrs), nil
		}
	}
}

// gtestRootAttrsFromXML scans a start element's attributes by name (used
// when the element has malformed children the struct decoder trips over).
func gtestRootAttrsFromXML(start xml.StartElement) gtestRootAttrs {
	var attrs gtestRootAttrs
	for _, a := range start.Attr {
		switch a.Name.Local {
		case "tests":
			attrs.Tests = atoiSafe(a.Value)
		case "failures":
			attrs.Failures = atoiSafe(a.Value)
		case "errors":
			attrs.Errors = atoiSafe(a.Value)
		case "disabled":
			attrs.Disabled = atoiSafe(a.Value)
		}
	}
	return attrs
}

// gtestJSONDocument mirrors the googletest JSON shape far enough to read the
// root totals; when the root omits them (a bare testsuites array wrapper),
// the suite-level values are summed.
type gtestJSONDocument struct {
	gtestRootAttrs
	Suites []struct {
		gtestRootAttrs
	} `json:"testsuites"`
}

// gtestCountsJSON reads the totals from the JSON report: root-level when
// present, else summed over the suites.
func gtestCountsJSON(data string) (gtestCounts, error) {
	var doc gtestJSONDocument
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		return gtestCounts{}, err
	}
	if doc.Tests > 0 || doc.Failures > 0 || doc.Errors > 0 || doc.Disabled > 0 {
		return countsFromAttrs(doc.gtestRootAttrs), nil
	}
	total := gtestRootAttrs{}
	for i := range doc.Suites {
		total.Tests += doc.Suites[i].Tests
		total.Failures += doc.Suites[i].Failures
		total.Errors += doc.Suites[i].Errors
		total.Disabled += doc.Suites[i].Disabled
	}
	return countsFromAttrs(total), nil
}

func countsFromAttrs(a gtestRootAttrs) gtestCounts {
	return gtestCounts{Total: a.Tests, Failed: a.Failures + a.Errors, Skipped: a.Disabled}
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range strings.TrimSpace(s) {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// GTestCountsSummary renders the default summary line from aggregate counts
// ("12 tests: 10 passed, 2 failed, 1 skipped" with zero parts dropped). It
// is used only when the stage printed no MD-BUILDER-SUMMARY line.
func GTestCountsSummary(total, passed, failed, skipped int) string {
	if total == 0 && failed == 0 && skipped == 0 {
		return ""
	}
	parts := []string{fmt.Sprintf("%d tests", total)}
	if passed > 0 {
		parts = append(parts, fmt.Sprintf("%d passed", passed))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failed))
	}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	return strings.Join(parts, ", ")
}
