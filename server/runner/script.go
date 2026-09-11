package runner

import (
	"fmt"
	"sort"
	"strings"
)

// Per-sub-task remote script generation. Each sub-task runs one small bash
// script in its own SSH session (unlike the former single monolithic job
// script); the task graph provides the ordering.

// DefaultStageTimeoutSeconds applies when a stage config carries no timeout.
const DefaultStageTimeoutSeconds = 3600

// BuildScript renders the remote bash script for the build sub-task: export
// the MD_* environment plus the entry env, then run the build recipe
// (cmake or a custom command) under `timeout` in the code directory.
func BuildBuildScript(in *ScriptInput) (string, error) {
	if in == nil || in.Entry == nil {
		return "", fmt.Errorf("no entry config")
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("#!/usr/bin/env bash")
	w("set -uo pipefail")
	// CODE keeps $HOME as a reference: the assignment is double-quoted so it
	// expands on the remote host at assignment time (a single-quoted value
	// would stay literal in `cd "$CODE"`).
	w("CODE=%s", shellExpand(in.CodeDir))
	w("cd \"$CODE\" || exit 1")
	w("")

	envTimeout := in.Timeout
	if envTimeout <= 0 {
		envTimeout = DefaultStageTimeoutSeconds
	}
	if in.Entry.Build.Generator == GeneratorScript {
		w("# Build stage (custom command).")
		w("timeout %d bash -c %s", envTimeout, shq(in.Entry.Build.Command))
	} else {
		flags := strings.TrimSpace(in.Entry.Build.CMakeFlags)
		threads := in.Entry.Build.Threads
		if threads <= 0 {
			threads = DefaultBuildThreads
		}
		w("# Build stage (cmake).")
		w("timeout %d cmake %s . && timeout %d cmake --build . -j%d", envTimeout, flags, envTimeout, threads)
	}
	// The exit code decides passed/failed; the log carries the output.
	w("exit $?")
	return b.String(), nil
}

// BuildStageScript renders the remote bash script for a test sub-task
// (unit / regression / future kinds): export MD_* plus entry env, run the
// command under `timeout` in the code directory.
func BuildStageScript(in *ScriptInput) (string, error) {
	if in == nil || in.StageCommand == "" {
		return "", fmt.Errorf("no stage command")
	}
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("#!/usr/bin/env bash")
	w("set -uo pipefail")
	w("CODE=%s", shellExpand(in.CodeDir))
	w("cd \"$CODE\" || exit 1")
	w("timeout %d bash -c %s", in.TimeoutOr(DefaultStageTimeoutSeconds), shq(in.StageCommand))
	w("exit $?")
	return b.String(), nil
}

// ScriptInput bundles everything a sub-task script needs. The MD_* exports
// are emitted by ExportEnv (shared by both script kinds).
type ScriptInput struct {
	CommitSHA string
	EnvName   string
	EnvTags   string
	CodeDir   string // absolute remote path of the code checkout

	// Entry carries the root task's merged entry snapshot (build recipe +
	// env map). Build scripts use Entry.Build; stage scripts only use
	// Entry.Env.
	Entry *MergedEntry

	// StageCommand is the test command (stage scripts only).
	StageCommand string

	// Timeout is the stage timeout in seconds.
	Timeout int
}

// TimeoutOr returns Timeout, or fallback when unset.
func (in *ScriptInput) TimeoutOr(fallback int) int {
	if in.Timeout > 0 {
		return in.Timeout
	}
	return fallback
}

// ExportEnv renders the `export` lines shared by all sub-task scripts:
// MD_COMMIT / MD_ENV_NAME / MD_ENV_TAGS / MD_CODE_DIR / MD_TEST_INPUT_DIR
// plus the entry's env map (sorted for deterministic scripts).
func ExportEnv(w func(format string, args ...any), in *ScriptInput) {
	w("export MD_COMMIT=%s", shq(in.CommitSHA))
	w("export MD_ENV_NAME=%s", shq(in.EnvName))
	w("export MD_ENV_TAGS=%s", shq(in.EnvTags))
	// The directory exports carry $HOME references: double-quote so they
	// expand on the remote host instead of staying literal.
	w("export MD_CODE_DIR=%s", shellExpand(in.CodeDir))
	w("export MD_TEST_INPUT_DIR=%s", shellExpand(remoteTestsDir(in.CodeDir)))
	if in.Entry != nil {
		keys := make([]string, 0, len(in.Entry.Env))
		for k := range in.Entry.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			w("export %s=%s", k, shq(in.Entry.Env[k]))
		}
	}
	w("")
}

// remoteTestsDir derives the tests directory from the code dir (both are
// extracted as siblings by the clone task: .../code and .../tests).
func remoteTestsDir(codeDir string) string {
	return strings.TrimSuffix(codeDir, "/code") + "/tests"
}

// shq single-quotes a string for safe use in bash.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// shellExpand double-quotes a path for bash, letting $HOME (and other
// parameter expansions) resolve on the remote host. Backslashes, double
// quotes and dollars that are NOT part of $HOME-style references would
// need escaping; task dirs are plain "$HOME/..." so this stays simple.
func shellExpand(s string) string {
	return "\"" + s + "\""
}

// ShellQuote is the exported form of shq, for API handlers composing
// one-off remote commands.
func ShellQuote(s string) string { return shq(s) }
