package runner

import (
	"fmt"
	"sort"
	"strings"
)

// ScriptInput bundles everything the generated remote script needs.
type ScriptInput struct {
	CommitSHA     string
	CodeRepoURL   string
	TestInputRepo string // may be empty: script skips the clone
	TestInputRef  string // branch or commit of the test input repo
	EnvName       string
	EnvTags       string // comma-joined
	Entry         *MergedEntry
}

// TotalScriptTimeout returns the overall SSH session timeout for a job:
// the sum of stage timeouts plus a 15-minute slack (clone etc.).
func TotalScriptTimeout(entry *MergedEntry) (int, error) {
	if entry == nil {
		return 0, fmt.Errorf("no entry config")
	}
	total := 0
	for _, stage := range []*EnvConfig{entry.Unit, entry.Regression} {
		if stage != nil {
			total += resolveStageTimeout(stage, entry)
		}
	}
	// Build stage gets the entry timeout too; add slack for clones.
	total += entry.Timeout
	total += 15 * 60
	return total, nil
}

// BuildScript renders the remote bash script for a job. The script clones
// both repositories, builds and runs the test stages, and finally prints a
// line-protocol report between BEGIN/END markers.
func BuildScript(in *ScriptInput) (string, error) {
	entry := in.Entry
	if entry == nil {
		return "", fmt.Errorf("no entry config")
	}
	short := in.CommitSHA
	if len(short) > 12 {
		short = short[:12]
	}

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("#!/usr/bin/env bash")
	w("set -uo pipefail")
	w("")
	w("# md-builder job: commit %s on environment %q (tags: %s)", in.CommitSHA, in.EnvName, in.EnvTags)
	w("")
	w("WORK=\"$HOME/.md-builder/jobs/%s\"", short)
	w("rm -rf \"$WORK\"")
	w("mkdir -p \"$WORK\"")
	w("CODE=\"$WORK/code\"")
	w("TESTS=\"$WORK/tests\"")
	w("")
	w("# Stages report their status via these variables.")
	w("BUILD_STATUS=skipped")
	w("UNIT_STATUS=skipped")
	w("REGRESSION_STATUS=skipped")
	w("UNIT_SUMMARY=\"not configured\"")
	w("REGRESSION_SUMMARY=\"not configured\"")
	w("BUILD_LOG=\"$WORK/build.log\"")
	w("UNIT_LOG=\"$WORK/unit.log\"")
	w("REGRESSION_LOG=\"$WORK/regression.log\"")
	w("")
	w("capture_tail() {")
	w("  # capture_tail <logfile>: last lines of a log, flattened to one line")
	w("  tail -n 5 \"$1\" 2>/dev/null | tr '\\n' ' ' | cut -c1-500")
	w("}")
	w("")
	w("emit_report() {")
	w("  echo \"===MD-BUILDER-REPORT-BEGIN===\"")
	w("  echo \"unit-status $UNIT_STATUS\"")
	w("  echo \"unit-summary $UNIT_SUMMARY\"")
	w("  echo \"regression-status $REGRESSION_STATUS\"")
	w("  echo \"regression-summary $REGRESSION_SUMMARY\"")
	w("  echo \"===MD-BUILDER-REPORT-END===\"")
	w("}")
	w("")

	// Stage 1: clone the test input repository (the test cases).
	if strings.TrimSpace(in.TestInputRepo) != "" {
		ref := strings.TrimSpace(in.TestInputRef)
		if ref == "" {
			ref = "HEAD"
		}
		w("# Clone the test input repository (test cases).")
		w("if git clone --quiet --depth 1 --branch %s %s \"$TESTS\" >\"$TESTS.clone.log\" 2>&1 ||", shq(ref), shq(in.TestInputRepo))
		w("   git clone --quiet %s \"$TESTS\" >\"$TESTS.clone.log\" 2>&1; then", shq(in.TestInputRepo))
		w("  :")
		w("else")
		w("  UNIT_STATUS=failed")
		w("  UNIT_SUMMARY=\"failed to clone test input repository: $(capture_tail \"$TESTS.clone.log\")\"")
		w("  REGRESSION_STATUS=failed")
		w("  REGRESSION_SUMMARY=\"failed to clone test input repository\"")
		w("  emit_report")
		w("  exit 0")
		w("fi")
		w("")
	} else {
		w("# No test input repository configured; $TESTS is created empty.")
		w("mkdir -p \"$TESTS\"")
		w("")
	}

	// Stage 2: clone the code repository at the pushed SHA.
	w("# Clone the code repository at the pushed commit.")
	w("if git clone --quiet %s \"$CODE\" >\"$CODE.clone.log\" 2>&1 &&", shq(in.CodeRepoURL))
	w("   (cd \"$CODE\" && git checkout --quiet %s >>\"$CODE.clone.log\" 2>&1); then", shq(in.CommitSHA))
	w("  :")
	w("else")
	w("  UNIT_STATUS=failed")
	w("  UNIT_SUMMARY=\"failed to clone code repository at %s: $(capture_tail \"$CODE.clone.log\")\"", in.CommitSHA)
	w("  REGRESSION_STATUS=failed")
	w("  REGRESSION_SUMMARY=\"failed to clone code repository\"")
	w("  emit_report")
	w("  exit 0")
	w("fi")
	w("")

	// Exported environment for the stages.
	w("export MD_COMMIT=%s", shq(in.CommitSHA))
	w("export MD_ENV_NAME=%s", shq(in.EnvName))
	w("export MD_ENV_TAGS=%s", shq(in.EnvTags))
	w("export MD_CODE_DIR=\"$CODE\"")
	w("export MD_TEST_INPUT_DIR=\"$TESTS\"")
	keys := make([]string, 0, len(entry.Env))
	for k := range entry.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		w("export %s=%s", k, shq(entry.Env[k]))
	}
	w("")

	// Stage 3: build.
	buildTimeout := entry.Timeout
	if buildTimeout <= 0 {
		buildTimeout = DefaultTimeoutSeconds
	}
	w("# Build stage.")
	if entry.Build.Generator == GeneratorScript {
		w("if (cd \"$CODE\" && timeout %d bash -c %s >\"$BUILD_LOG\" 2>&1); then", buildTimeout, shq(entry.Build.Command))
	} else {
		flags := strings.TrimSpace(entry.Build.CMakeFlags)
		threads := entry.Build.Threads
		if threads <= 0 {
			threads = DefaultBuildThreads
		}
		w("if (cd \"$CODE\" && timeout %d cmake %s . >\"$BUILD_LOG\" 2>&1 && ", buildTimeout, flags)
		w("     timeout %d cmake --build . -j%d >>\"$BUILD_LOG\" 2>&1); then", buildTimeout, threads)
	}
	w("  BUILD_STATUS=passed")
	w("else")
	w("  BUILD_STATUS=failed")
	w("fi")
	w("")

	// Stage 4: unit / regression (only when build passed).
	unitT := 0
	if entry.Unit != nil {
		unitT = resolveStageTimeout(entry.Unit, entry)
	} else {
		unitT = entry.Timeout
	}
	regT := 0
	if entry.Regression != nil {
		regT = resolveStageTimeout(entry.Regression, entry)
	} else {
		regT = entry.Timeout
	}

	w("# Test stages (skipped when the build failed).")
	w("if [ \"$BUILD_STATUS\" != passed ]; then")
	if entry.Unit != nil {
		w("  UNIT_STATUS=failed")
		w("  UNIT_SUMMARY=\"build failed; unit tests skipped: $(capture_tail \"$BUILD_LOG\")\"")
	}
	if entry.Regression != nil {
		w("  REGRESSION_STATUS=failed")
		w("  REGRESSION_SUMMARY=\"build failed; regression tests skipped: $(capture_tail \"$BUILD_LOG\")\"")
	}
	w("  emit_report")
	w("  exit 0")
	w("fi")
	w("")
	if entry.Unit != nil {
		w("# Unit test stage.")
		w("if (cd \"$CODE\" && timeout %d bash -c %s >\"$UNIT_LOG\" 2>&1); then", unitT, shq(entry.Unit.Command))
		w("  UNIT_STATUS=passed")
		w("  UNIT_SUMMARY=\"$(grep -m1 '^MD-BUILDER-SUMMARY: ' \"$UNIT_LOG\" | cut -d' ' -f3-)\"")
		w("  if [ -z \"$UNIT_SUMMARY\" ]; then")
		w("    UNIT_SUMMARY=\"exit 0; $(capture_tail \"$UNIT_LOG\")\"")
		w("  fi")
		w("else")
		w("  rc=$?")
		w("  UNIT_STATUS=failed")
		w("  UNIT_SUMMARY=\"exit $rc; $(capture_tail \"$UNIT_LOG\")\"")
		w("fi")
		w("")
	}
	if entry.Regression != nil {
		w("# Regression test stage.")
		w("if (cd \"$CODE\" && timeout %d bash -c %s >\"$REGRESSION_LOG\" 2>&1); then", regT, shq(entry.Regression.Command))
		w("  REGRESSION_STATUS=passed")
		w("  REGRESSION_SUMMARY=\"$(grep -m1 '^MD-BUILDER-SUMMARY: ' \"$REGRESSION_LOG\" | cut -d' ' -f3-)\"")
		w("  if [ -z \"$REGRESSION_SUMMARY\" ]; then")
		w("    REGRESSION_SUMMARY=\"exit 0; $(capture_tail \"$REGRESSION_LOG\")\"")
		w("  fi")
		w("else")
		w("  rc=$?")
		w("  REGRESSION_STATUS=failed")
		w("  REGRESSION_SUMMARY=\"exit $rc; $(capture_tail \"$REGRESSION_LOG\")\"")
		w("fi")
		w("")
	}
	w("emit_report")
	return b.String(), nil
}

// shq single-quotes a string for safe use in bash.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
