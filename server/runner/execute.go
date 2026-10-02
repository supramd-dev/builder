package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"md-builder/server/store"
)

// Sub-task execution: one claimed Task row is executed to a terminal state
// according to its Kind. The scheduler (scheduler.go) drives this; tests
// inject fakes for the transport (Execer) and source acquisition
// (RepoCloner).

// stageTimeoutSlack is added to a stage's configured timeout for the SSH
// session overall bound (script upload, session setup).
const stageTimeoutSlack = 5 * time.Minute

// fetchArtifactTimeout bounds the short session that reads an artifact file
// back from the remote host.
const fetchArtifactTimeout = 2 * time.Minute

// maxArtifactBytes caps a stored results/log/series artifact (the same cap
// as the task log; anything larger is truncated at fetch time).
const maxArtifactBytes = 8 * 1024 * 1024

// cloneTimeout is the overall bound of the clone sub-task when the entry
// does not set one.
const cloneTimeoutSecs = 3600

// ExecuteTask runs one claimed sub-task to a terminal state. It never
// returns an execution error — the task row records the outcome — only
// unexpected store failures surface as errors.
func (s *Service) ExecuteTask(ctx context.Context, task *store.Task) error {
	switch task.Kind {
	case store.TaskKindClone:
		s.executeClone(ctx, task)
	case store.TaskKindBuild:
		s.executeBuild(ctx, task)
	case store.TaskKindUnit:
		s.executeUnit(ctx, task)
	case store.TaskKindRegressionCase:
		s.executeCase(ctx, task)
	default:
		s.failEarly(task, fmt.Sprintf("unknown task kind %q", task.Kind))
	}
	return nil
}

// rootContext gathers what every sub-task needs: the root's entry snapshot,
// the environment and the site config credentials.
type rootContext struct {
	root  *store.Task
	entry *MergedEntry
	sha   string
	env   *store.TestEnvironment
	cfg   *store.SiteConfig
}

// loadRootContext loads the sub-task's root snapshot, environment and site
// config, failing the task when any lookup breaks.
func (s *Service) loadRootContext(task *store.Task) (*rootContext, bool) {
	rc := &rootContext{}
	root, err := s.Store.GetTask(task.RootID)
	if err != nil {
		s.failEarly(task, fmt.Sprintf("root task lookup failed: %v", err))
		return nil, false
	}
	rc.root = root
	var rootCfg RootConfig
	if err := json.Unmarshal([]byte(root.Config), &rootCfg); err != nil {
		s.failEarly(task, fmt.Sprintf("root config snapshot is invalid: %v", err))
		return nil, false
	}
	rc.entry = &rootCfg.Entry
	rc.sha = s.Store.CommitSHA(root.CommitID)
	if rc.sha == "" {
		s.failEarly(task, "commit lookup failed")
		return nil, false
	}
	env, err := s.Store.GetEnvironment(task.EnvironmentID)
	if err != nil {
		s.failEarly(task, fmt.Sprintf("environment lookup failed: %v", err))
		return nil, false
	}
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		s.failEarly(task, fmt.Sprintf("site config lookup failed: %v", err))
		return nil, false
	}
	rc.cfg = cfg
	rc.env = env
	return rc, true
}

// failEarly marks a task failed for a reason that surfaced before any
// command ran (invalid snapshot, broken lookups) and appends the reason to
// the task log directly — the LogWriter-based failTaskLogged needs a root
// context that may not exist yet. The attempt's run is failed with it, so the
// matrix cell carries the outcome (and a runId to click through) instead of
// only the graph showing the failure.
func (s *Service) failEarly(task *store.Task, msg string) {
	if cfg, err := s.Store.GetSiteConfig(); err == nil {
		msg = Redact(msg, cfg.AccessToken)
		msg = Redact(msg, cfg.SecretToken)
	}
	logw := NewLogWriter(s.Store, task)
	fmt.Fprintf(logw, "task failed: %s\n", msg)
	logw.Close()
	s.finishStage(task, store.StatusFailed, truncateSummary(msg), msg, stageCounts{}, nil)
}

// creds builds the git credentials from the site config.
func (rc *rootContext) creds() *GitCredentials {
	return &GitCredentials{AccessToken: rc.cfg.AccessToken}
}

// stageLogWriter returns the task's log writer with the site's secrets
// (access token, MD_SECRET_TOKEN) armed for scrubbing — a stage command
// echoing its environment would otherwise persist them in the log.
func (s *Service) stageLogWriter(rc *rootContext, task *store.Task) *LogWriter {
	logw := NewLogWriter(s.Store, task)
	logw.SetSecrets(rc.cfg.AccessToken, rc.cfg.SecretToken)
	return logw
}

// scriptInput assembles the shared ScriptInput for sub-task scripts.
// workdir/case/timeout are the stage-specific fields.
func (rc *rootContext) scriptInput(stageCommand CommandList, workdir, caseName string, timeout int) *ScriptInput {
	return &ScriptInput{
		CommitSHA:     rc.sha,
		EnvName:       rc.env.Name,
		EnvTags:       rc.env.Tags,
		TaskDir:       RemoteTaskDir(rc.sha),
		CodeDir:       rc.remoteCodeDir(),
		EnvScriptName: rc.env.EnvScriptName(),
		Entry:         rc.entry,
		AllowedEnv:    rc.env.AllowedEnvList(),
		StageCommand:  stageCommand,
		Workdir:       workdir,
		CaseName:      caseName,
		SecretToken:   rc.cfg.SecretToken,
		Timeout:       timeout,
	}
}

// remoteCodeDir is the remote path of the code checkout (clone extracts it
// under the task dir).
func (rc *rootContext) remoteCodeDir() string {
	return RemoteTaskDir(rc.sha) + "/code"
}

// executeClone implements the clone sub-task: server-side clone of the code
// repository, tar stream upload to the remote workspace, then (when the
// environment carries one) the env setup script written next to the code so
// every later stage can source it.
func (s *Service) executeClone(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	logw := NewLogWriter(s.Store, task)
	defer logw.Close()

	if rc.cfg.CodeRepo == "" {
		s.failTaskLogged(task, logw, "site config has no code repository set")
		return
	}

	fmt.Fprintf(logw, "cloning %s at %s on the server\n", rc.cfg.CodeRepo, rc.sha)

	h := envToSSHHost(rc.env)
	remoteDir := RemoteTaskDir(rc.sha)
	uploaded, err := s.Clone.CloneAndUpload(
		ctx, h,
		rc.cfg.CodeRepo, rc.sha,
		rc.creds(), remoteDir,
		slackTimeout(cloneTimeoutSecs, stageTimeoutSlack),
		logw,
	)
	if err != nil {
		s.failTaskLogged(task, logw, fmt.Sprintf("clone/upload failed: %v", err))
		return
	}
	fmt.Fprintf(logw, "uploaded %.1f MiB to %s:%s\n", float64(uploaded)/(1024*1024), rc.env.Host, remoteDir)

	if err := s.writeEnvScript(ctx, rc, logw); err != nil {
		s.failTaskLogged(task, logw, fmt.Sprintf("env script upload failed: %v", err))
		return
	}

	s.finishStage(task, store.StatusPassed, fmt.Sprintf("cloned %s", shortSHA(rc.sha)), "", stageCounts{}, nil)
}

// writeEnvScript materializes the environment's setup script on the remote
// host at $TASK_DIR/md-builder-env-<hash>.sh. An environment without a
// script is fine — a warning names it so the log explains why stages run
// without environment setup.
func (s *Service) writeEnvScript(ctx context.Context, rc *rootContext, logw *LogWriter) error {
	if rc.env.EnvScriptName() == "" {
		fmt.Fprintf(logw, "warning: environment %s has no env script; stages will run without one\n", rc.env.Name)
		return nil
	}
	name := rc.env.EnvScriptName()
	var script strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&script, format+"\n", args...) }
	w("#!/usr/bin/env bash")
	w("# environment setup script of %s (written by md-builder)", commentSafe(rc.env.Name))
	w("%s", rc.env.EnvScript)

	// The task dir carries a $HOME reference — double-quote so the remote
	// shell expands it (single quotes would create a literal "$HOME" path).
	dir := RemoteTaskDir(rc.sha)
	remote := fmt.Sprintf("mkdir -p %s && cat > %s && chmod +x %s",
		shellExpand(dir), shellExpand(dir+"/"+name), shellExpand(dir+"/"+name))

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, remote, script.String(), fetchArtifactTimeout, logw, logw)
	if res.ExitCode != 0 {
		return fmt.Errorf("write %s failed (exit %d)", name, res.ExitCode)
	}
	fmt.Fprintf(logw, "wrote env script %s\n", name)
	return nil
}

// executeBuild implements the build sub-task: generate the build script and
// run it in the configured workdir on the remote host. The outcome is
// recorded as a "build" test run so the dashboard can show per-environment
// build results next to the unit/regression kinds.
func (s *Service) executeBuild(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	var stage BuildStageConfig
	if err := json.Unmarshal([]byte(task.Config), &stage); err != nil {
		s.failEarly(task, fmt.Sprintf("build config snapshot is invalid: %v", err))
		return
	}

	logw := s.stageLogWriter(rc, task)
	defer logw.Close()

	script, err := BuildStageScript(rc.scriptInput(stage.Command, stage.Workdir, "", stage.Timeout))
	if err != nil {
		s.failTaskLogged(task, logw, "stage script is invalid: "+err.Error())
		return
	}

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, "bash -s", script, slackTimeout(stage.Timeout, stageTimeoutSlack), logw, logw)

	// Build artifacts (when configured) are fetched back verbatim as file
	// artifacts — no results parsing, the build verdict is the exit code.
	_, artifacts := s.fetchStageArtifacts(ctx, h, rc, task, store.ArtifactKindFile, stage.Workdir, stage.Artifacts, logw, res.ExitCode)
	logw.Flush() // the build run's summary is derived from the persisted log

	// Record the dashboard build run from the log tail (no per-case results).
	output := s.readLogTail(task)
	summary := ExtractSummary(output, res.ExitCode)
	if res.ExitCode < 0 {
		summary = truncateSummary(fmt.Sprintf("ssh execution failed: %s; log tail: %s", res.Stderr, tailLine(output, 3)))
	}
	s.finishCommand(task, logw, res.ExitCode, res.Stderr, summary, stageCounts{}, artifacts)
}

// executeUnit implements the unit test sub-task: run the stage command in
// its workdir, fetch the configured artifact files back (aggregate counts +
// raw artifacts), then record the TestRun for the dashboard.
func (s *Service) executeUnit(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	var stage StageConfig
	if err := json.Unmarshal([]byte(task.Config), &stage); err != nil {
		s.failEarly(task, fmt.Sprintf("stage config snapshot is invalid: %v", err))
		return
	}

	logw := s.stageLogWriter(rc, task)
	defer logw.Close()

	script, err := BuildStageScript(rc.scriptInput(stage.Command, stage.Workdir, "", stage.Timeout))
	if err != nil {
		s.failTaskLogged(task, logw, "stage script is invalid: "+err.Error())
		return
	}

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, "bash -s", script, slackTimeout(stage.Timeout, stageTimeoutSlack), logw, logw)

	counts, artifacts := s.fetchStageArtifacts(ctx, h, rc, task, store.ArtifactKindResults, stage.Workdir, stage.Artifacts, logw, res.ExitCode)

	logw.Flush() // the summary is derived from the persisted log

	// The log holds the full output; the summary is derived from it.
	output := s.readLogTail(task)
	summary := ExtractSummary(output, res.ExitCode)
	if res.ExitCode < 0 {
		// Session-level failure (dial, timeout): surface the transport error.
		summary = truncateSummary(fmt.Sprintf("ssh execution failed: %s; log tail: %s", res.Stderr, tailLine(output, 3)))
	} else if !hasSummaryLine(output) {
		// No MD-BUILDER-SUMMARY line: default to the parsed counts when the
		// results files yielded them.
		passed := counts.total - counts.failed - counts.skipped
		if s := GTestCountsSummary(counts.total, passed, counts.failed, counts.skipped); s != "" {
			summary = s
		}
	}
	s.finishCommand(task, logw, res.ExitCode, res.Stderr, summary, counts, artifacts)
}

// executeCase implements one regression case sub-task: run the preset's
// command in its workdir, fetch the artifact files, then record the case
// row (and its artifacts) into the run's aggregated regression result.
func (s *Service) executeCase(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	var stage CaseStageConfig
	if err := json.Unmarshal([]byte(task.Config), &stage); err != nil {
		s.failEarly(task, fmt.Sprintf("case config snapshot is invalid: %v", err))
		return
	}

	logw := s.stageLogWriter(rc, task)
	defer logw.Close()

	script, err := BuildStageScript(rc.scriptInput(stage.Command, stage.Workdir, stage.Case, stage.Timeout))
	if err != nil {
		s.failTaskLogged(task, logw, "case script is invalid: "+err.Error())
		return
	}

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, "bash -s", script, slackTimeout(stage.Timeout, stageTimeoutSlack), logw, logw)

	// The case's artifact files (when configured) are fetched back and
	// attached to the case's own run — the browser parses them for the case's
	// detail. Their tallies are not the case's result: a case's verdict is
	// its command's exit status alone (docs/test-matrix.md), so the counts
	// parsed here are dropped.
	_, artifacts := s.fetchStageArtifacts(ctx, h, rc, task, store.ArtifactKindResults, stage.Workdir, stage.Artifacts, logw, res.ExitCode)

	logw.Flush() // the case's summary is derived from the persisted log
	output := s.readLogTail(task)

	summary := ExtractSummary(output, res.ExitCode)
	if res.ExitCode < 0 {
		summary = truncateSummary(fmt.Sprintf("ssh execution failed: %s; log tail: %s", res.Stderr, tailLine(output, 3)))
	}

	// A case is one command and one test, so its run counts as that one test
	// (0/1 when the command failed) — which is how the demo seeds a case and
	// what the run page shows. Leaving the counts empty would print "0/0
	// passed" on a case whose single test ran fine.
	counts := stageCounts{total: 1, failed: 1}
	if res.ExitCode == 0 {
		counts.failed = 0
	}
	s.finishCommand(task, logw, res.ExitCode, res.Stderr, summary, counts, artifacts)
}

// stageCounts is the aggregate a stage's results files yielded.
type stageCounts struct{ total, failed, skipped int }

// fetchStageArtifacts reads the configured artifact files back from the
// remote host and returns the summed aggregate counts plus the artifact
// inputs (run-scoped for unit; the caller links them to the case). Relative
// paths resolve against the stage's workdir. Nothing runs when the session
// never started (exitCode < 0). kind selects the stored artifact kind:
// results files feed the aggregate counts, file artifacts (build) are stored
// as-is and never parsed.
func (s *Service) fetchStageArtifacts(ctx context.Context, h SSHHost, rc *rootContext, task *store.Task,
	kind string, workdir string, artifacts ArtifactPaths, logw *LogWriter, exitCode int) (stageCounts, []store.ArtifactInput) {
	var counts stageCounts
	var out []store.ArtifactInput
	for _, path := range artifacts.Clean() {
		if exitCode < 0 {
			break // the session never ran; nothing to fetch
		}
		content, fetchErr := s.fetchArtifactFile(ctx, h, rc, path, workdir)
		switch {
		case fetchErr != nil:
			fmt.Fprintf(logw, "artifact %s: %v; skipping counts\n", path, fetchErr)
		case content == "":
			fmt.Fprintf(logw, "artifact %s: not found or empty; skipping counts\n", path)
		default:
			out = append(out, store.ArtifactInput{
				Kind:    kind,
				Name:    path,
				Content: content,
			})
			if kind != store.ArtifactKindResults {
				break // file artifacts are stored verbatim; no counts parsing
			}
			if total, failed, skipped, ok := ExtractGTestCounts([]byte(content)); ok {
				counts.total += total
				counts.failed += failed
				counts.skipped += skipped
			} else {
				fmt.Fprintf(logw, "artifact %s: unrecognized format; storing file without counts\n", path)
			}
		}
	}
	return counts, out
}

// fetchArtifactFile cats the configured artifact file on the remote host
// (relative paths resolve against the stage's workdir, falling back to the
// code directory), capped at maxArtifactBytes. The fetched bytes are
// written to the returned string; a scratch writer swallows the
// (unexpected) stderr without polluting the stage log.
func (s *Service) fetchArtifactFile(ctx context.Context, h SSHHost, rc *rootContext, path, workdir string) (string, error) {
	var script strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&script, format+"\n", args...) }
	w("#!/usr/bin/env bash")
	w("set -uo pipefail")
	if !strings.HasPrefix(path, "/") {
		wd := strings.TrimSpace(workdir)
		switch {
		case wd == "":
			w("cd %s || exit 1", shellExpand(rc.remoteCodeDir()))
		case strings.HasPrefix(wd, "/"):
			w("cd %s || exit 1", shellExpand(wd))
		default:
			w("cd %s || exit 1", shellExpand(rc.remoteCodeDir()+"/"+strings.TrimPrefix(wd, "/")))
		}
	}
	w("head -c %d %s", maxArtifactBytes, shq(path))
	var out, errOut strings.Builder
	res := s.SSH.RunScript(ctx, h, "bash -s", script.String(), fetchArtifactTimeout, &out, &errOut)
	if res.ExitCode != 0 {
		return "", fmt.Errorf("remote read failed (exit %d)", res.ExitCode)
	}
	// Artifacts are served verbatim to every authenticated user and the site's
	// secrets are exported into the stage scripts, so a command that dumps its
	// environment would otherwise publish them here. The stage log writer
	// scrubs the same two values (see stageLogWriter); this path stores its
	// bytes in the object store instead of the log table, so it needs its own
	// pass.
	content := out.String()
	for _, secret := range []string{rc.cfg.AccessToken, rc.cfg.SecretToken} {
		content = Redact(content, secret)
	}
	return content, nil
}

// readLogTail re-reads the persisted log tail of the task's current attempt
// (the LogWriter already closed by defer ordering — read what landed) for
// summary extraction. It reads the end of the log, not its beginning: the
// summary line is the last thing a stage prints, and a long stage's output
// runs past the page size ReadTaskLogs returns, so reading from zero would
// derive the summary from the head of a log whose outcome is at its tail.
func (s *Service) readLogTail(task *store.Task) string {
	logs, err := s.Store.ReadTaskLogTail(task.ID, task.Attempts)
	if err != nil {
		return ""
	}
	var all string
	for _, l := range logs {
		all += l.Content
	}
	return all
}

// finishStage records one real stage's attempt: it writes the attempt's run
// (status, summary, counts, artifacts) and refreshes the task's cache from
// the same values, then rolls the virtual nodes back up
// (store.FinishAttempt) — the one write path a stage outcome takes, so the
// node, the run and the containers can never disagree.
//
// counts/artifacts carry the fetched results files' summed aggregates and raw
// contents (zero/nil when not configured or unfetchable). A stage whose counts
// report failed cases is failed even when its command exited zero: a ctest
// wrapper can swallow the test binary's result.
func (s *Service) finishStage(task *store.Task, status, summary, errMsg string,
	counts stageCounts, artifacts []store.ArtifactInput) {
	if counts.failed > 0 {
		status = store.StatusFailed
	}
	res := store.AttemptResult{
		Status:     status,
		Summary:    truncateSummary(summary),
		Error:      errMsg,
		Attempt:    task.Attempts, // the attempt this execution is; a re-dispatch must not take its result
		Total:      counts.total,
		Passed:     counts.total - counts.failed - counts.skipped,
		Failed:     counts.failed,
		Skipped:    counts.skipped,
		StartedAt:  taskStart(task),
		FinishedAt: time.Now(),
		Artifacts:  artifacts,
	}
	if err := s.finishAttempt(task, res); err != nil {
		log.Printf("runner: task %d: finish %s attempt %d gave up after %d tries: %v "+
			"(the node stays running; restart the service or re-dispatch its commit)",
			task.ID, task.Kind, task.Attempts, finishAttemptTries, err)
	}
}

// finishAttemptTries bounds the retries of the attempt's closing write, and
// finishAttemptBackoff is the first wait between them (it doubles).
var (
	finishAttemptTries   = 4
	finishAttemptBackoff = 100 * time.Millisecond
)

// finishAttempt writes the attempt's outcome through the one write path that
// ends it, retrying a failure that looks transient (a busy SQLite file, a
// connection that dropped). Retrying matters because this write is the only
// thing that ends the node: a single failed call leaves the task "running"
// with no worker behind it, and nothing ever claims a running node again — it
// would sit on the dashboard as running until the next restart resets it (the
// retries are bounded so a database that is really down still lets the worker
// go; the caller logs the give-up).
func (s *Service) finishAttempt(task *store.Task, res store.AttemptResult) error {
	delay := finishAttemptBackoff
	var err error
	for try := 1; try <= finishAttemptTries; try++ {
		if _, err = s.Store.FinishAttempt(task.ID, res); err == nil {
			return nil
		}
		if try == finishAttemptTries {
			break
		}
		if errors.Is(err, store.ErrVirtualTask) || errors.Is(err, store.ErrInvalidRunStatus) ||
			errors.Is(err, store.ErrTestRunNotFound) {
			// A refusal, not a flake: the store rejected the report itself
			// (wrong kind of node, unknown status, attempt row gone), so
			// repeating it changes nothing.
			return err
		}
		log.Printf("runner: task %d: finish %s attempt %d: %v; retrying in %s",
			task.ID, task.Kind, task.Attempts, err, delay)
		time.Sleep(delay)
		delay *= 2
	}
	return err
}

// finishCommand closes a stage whose command ran: exit 0 passes it, anything
// else fails it with the redacted reason, which also lands in the log.
func (s *Service) finishCommand(task *store.Task, logw *LogWriter, exitCode int, stderr, summary string,
	counts stageCounts, artifacts []store.ArtifactInput) {
	if exitCode == 0 {
		s.finishStage(task, store.StatusPassed, summary, "", counts, artifacts)
		return
	}
	msg := stderr
	if msg == "" {
		msg = fmt.Sprintf("command exited with %d (see log)", exitCode)
	}
	msg = fmt.Sprintf("exit %d: %s", exitCode, tailLine(msg, 3))
	s.writeFailure(task, logw, msg)
	s.finishStage(task, store.StatusFailed, summary, truncateSummary(msg), counts, artifacts)
}

// failTaskLogged marks a task failed for a reason that surfaced outside the
// command's exit code (clone/upload failure, env script upload, an invalid
// stage script) and appends the reason to its log.
func (s *Service) failTaskLogged(task *store.Task, logw *LogWriter, msg string) {
	msg = s.writeFailure(task, logw, msg)
	s.finishStage(task, store.StatusFailed, truncateSummary(msg), truncateSummary(msg), stageCounts{}, nil)
}

// writeFailure redacts the site's secrets out of msg, appends it to the task's
// log and returns the redacted text (what gets stored on the task's error
// column).
func (s *Service) writeFailure(task *store.Task, logw *LogWriter, msg string) string {
	if cfg, err := s.Store.GetSiteConfig(); err == nil {
		msg = Redact(msg, cfg.AccessToken)
		msg = Redact(msg, cfg.SecretToken)
	}
	fmt.Fprintf(logw, "task failed: %s\n", msg)
	return msg
}

func taskStart(task *store.Task) time.Time {
	if task.StartedAt != nil {
		return *task.StartedAt
	}
	return time.Time{}
}
