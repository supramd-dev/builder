package runner

import (
	"fmt"
	"sort"
	"strings"
)

// Per-sub-task remote script generation. Each sub-task runs one small bash
// script in its own SSH session; the task graph provides the ordering.
// Every script follows the same shape:
//
//	#!/usr/bin/env bash
//	set -uo pipefail
//	export MD_COMMIT=... MD_ENV_NAME=... MD_ENV_TAGS=... MD_TASK_DIR=... MD_CODE_DIR=...
//	export MD_SECRET_TOKEN=...                                                # when the site has one
//	export <entry env, sorted>
//	[ -f "$MD_TASK_DIR/md-builder-env-<hash>.sh" ] && . ... || warn   # when the env has one
//	cd "<workdir>" || exit 1
//	timeout N bash -c '<command>'
//	exit $?                                                              # single command
//	timeout N bash -c '<c1>' &&                                          # command list:
//	timeout N bash -c '<c2>'                                             # stops at the first
//	exit $?                                                              # failure
//
// The MD_* exports and the env script land in every stage script because
// each stage is its own SSH session: state set by one never leaks to the
// next.

// DefaultStageTimeoutSeconds applies when a stage config carries no timeout.
const DefaultStageTimeoutSeconds = 3600

// ScriptInput bundles everything a sub-task script needs.
type ScriptInput struct {
	CommitSHA string
	EnvName   string
	EnvTags   string
	TaskDir   string // absolute remote path of the task workspace (~/.md-builder/tasks/<sha12>)
	CodeDir   string // absolute remote path of the code checkout

	// EnvScriptName is the on-host file name of the environment's setup
	// script (store.TestEnvironment.EnvScriptName); empty = the environment
	// has none.
	EnvScriptName string

	// Entry carries the root task's merged entry snapshot (build recipe +
	// env map). The env map is exported by every script kind.
	Entry *MergedEntry

	// StageCommand is the command list to run (all script kinds): one or
	// several commands, each under its own `timeout` wrapper. They run in
	// order and stop at the first failure — a non-zero exit skips the
	// remaining commands and fails the stage with that exit code.
	StageCommand CommandList

	// Workdir is the working directory of the command, relative to the code
	// dir; empty = the code dir itself. Absolute paths pass through.
	Workdir string

	// CaseName names the regression case (empty for build/unit); exported
	// as MD_CASE so commands can tell which preset they are running.
	CaseName string

	// SecretToken is the site's write-only secret (Settings → Repository);
	// when set it is exported as MD_SECRET_TOKEN so yaml commands can
	// authenticate against internal services without hardcoding
	// credentials in the repository. Empty = not configured, the variable
	// stays unset.
	SecretToken string

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

// BuildStageScript renders the remote bash script for a stage sub-task
// (build, unit, regression case): the shared preamble plus the command list
// in the configured workdir. Each command runs under its own `timeout`
// wrapper, chained with `&&`: a failing command stops the stage right
// there (the remaining ones do not run) and its exit code becomes the
// stage's exit code.
func BuildStageScript(in *ScriptInput) (string, error) {
	if in == nil || in.StageCommand.IsEmpty() {
		return "", fmt.Errorf("no stage command")
	}
	cmds := in.StageCommand.Clean()
	secs := in.TimeoutOr(DefaultStageTimeoutSeconds)

	// Chain the commands with `&&`: order is kept, the first failure
	// short-circuits the rest, and $? carries its exit code to `exit $?`.
	chain := make([]string, len(cmds))
	for i, cmd := range cmds {
		chain[i] = fmt.Sprintf("timeout %d bash -c %s", secs, shq(cmd))
	}
	return renderScript(in, scriptBody{cmds: []string{strings.Join(chain, " && \\\n")}})
}

// scriptBody is the stage-specific part of a rendered script.
type scriptBody struct {
	cmds []string // lines between the cd and the exit; exit code decides pass/fail
}

// renderScript emits the shared preamble (bash header, MD_* + entry env
// exports, env-script source, cd into the workdir), the body lines and the
// exit mapping.
func renderScript(in *ScriptInput, body scriptBody) (string, error) {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	w("#!/usr/bin/env bash")
	w("set -uo pipefail")
	exportEnv(w, in)
	sourceEnvScript(w, in)
	if err := writeCD(w, in); err != nil {
		return "", err
	}
	for _, line := range body.cmds {
		w("%s", line)
	}
	// The exit code decides passed/failed; the log carries the output.
	w("exit $?")
	return b.String(), nil
}

// exportEnv renders the `export` lines shared by all sub-task scripts:
// MD_COMMIT / MD_ENV_NAME / MD_ENV_TAGS / MD_TASK_DIR / MD_CODE_DIR
// (MD_CASE for regression case scripts) plus the entry's env map (sorted
// for deterministic scripts). The MD_* variables are usable in commands,
// workdirs and results paths — they expand inside double quotes on the
// remote host.
func exportEnv(w func(format string, args ...any), in *ScriptInput) {
	w("export MD_COMMIT=%s", shq(in.CommitSHA))
	w("export MD_ENV_NAME=%s", shq(in.EnvName))
	w("export MD_ENV_TAGS=%s", shq(in.EnvTags))
	// The directory exports carry $HOME references: double-quote so they
	// expand on the remote host instead of staying literal.
	w("export MD_TASK_DIR=%s", shellExpand(in.TaskDir))
	w("export MD_CODE_DIR=%s", shellExpand(in.CodeDir))
	if in.CaseName != "" {
		w("export MD_CASE=%s", shq(in.CaseName))
	}
	if in.SecretToken != "" {
		// Single-quoted: the token may contain shell metacharacters, and
		// unlike the directory exports nothing in it should ever expand.
		w("export MD_SECRET_TOKEN=%s", shq(in.SecretToken))
	}
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

// sourceEnvScript renders the environment setup hook: source the
// environment's script when it exists, warn and continue when it does not
// (the clone task may have failed to write it; stages still run). Emitted
// only when the environment has a script configured.
func sourceEnvScript(w func(format string, args ...any), in *ScriptInput) {
	if in.EnvScriptName == "" {
		return
	}
	w("ENV_SCRIPT=\"$MD_TASK_DIR/%s\"", in.EnvScriptName)
	w("if [ -f \"$ENV_SCRIPT\" ]; then . \"$ENV_SCRIPT\"")
	w("else echo \"warning: env script $ENV_SCRIPT not found; continuing without it\" >&2; fi")
	w("")
}

// writeCD emits the cd into the stage's working directory. A relative
// workdir resolves against MD_CODE_DIR (so $MD_* references in the yaml
// work); an absolute workdir passes through; empty = the code dir.
func writeCD(w func(format string, args ...any), in *ScriptInput) error {
	wd := strings.TrimSpace(in.Workdir)
	switch {
	case wd == "":
		w("cd \"$MD_CODE_DIR\" || exit 1")
	case strings.HasPrefix(wd, "/"):
		w("cd %s || exit 1", shellExpand(wd))
	default:
		w("cd \"$MD_CODE_DIR/%s\" || exit 1", strings.TrimPrefix(wd, "/"))
	}
	w("")
	return nil
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

// ExportEnv renders the `export` lines shared by all sub-task scripts (the
// exported form used by tests).
func ExportEnv(w func(format string, args ...any), in *ScriptInput) {
	exportEnv(w, in)
}
