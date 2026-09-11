package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
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
	case store.TaskKindUnit, store.TaskKindRegression:
		s.executeStage(ctx, task)
	default:
		s.failTask(task, fmt.Sprintf("unknown task kind %q", task.Kind))
	}
	return nil
}

// rootContext gathers what every sub-task needs: the root's entry snapshot,
// the environment and the site config credentials.
type rootContext struct {
	root     *store.Task
	entry    *MergedEntry
	sha      string
	inputRef string
	env      *store.TestEnvironment
	cfg      *store.SiteConfig
}

// loadRootContext loads the sub-task's root snapshot, environment and site
// config, failing the task when any lookup breaks.
func (s *Service) loadRootContext(task *store.Task) (*rootContext, bool) {
	rc := &rootContext{}
	root, err := s.Store.GetTask(task.RootID)
	if err != nil {
		s.failTask(task, fmt.Sprintf("root task lookup failed: %v", err))
		return nil, false
	}
	rc.root = root
	var rootCfg RootConfig
	if err := json.Unmarshal([]byte(root.Config), &rootCfg); err != nil {
		s.failTask(task, fmt.Sprintf("root config snapshot is invalid: %v", err))
		return nil, false
	}
	rc.entry = &rootCfg.Entry
	rc.inputRef = rootCfg.TestInputRef
	rc.sha = s.Store.CommitSHA(root.CommitID)
	if rc.sha == "" {
		s.failTask(task, "commit lookup failed")
		return nil, false
	}
	env, err := s.Store.GetEnvironmentAny(task.EnvironmentID)
	if err != nil {
		s.failTask(task, fmt.Sprintf("environment lookup failed: %v", err))
		return nil, false
	}
	rc.env = env
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		s.failTask(task, fmt.Sprintf("site config lookup failed: %v", err))
		return nil, false
	}
	rc.cfg = cfg
	return rc, true
}

// creds builds the git credentials from the site config.
func (rc *rootContext) creds() *GitCredentials {
	return &GitCredentials{
		DeployKey:       rc.cfg.DeployKey,
		DeployToken:     rc.cfg.DeployToken,
		DeployTokenUser: rc.cfg.DeployTokenUser,
	}
}

// scriptInput assembles the shared ScriptInput for sub-task scripts.
func (rc *rootContext) scriptInput(task *store.Task, stageCommand string, timeout int) *ScriptInput {
	return &ScriptInput{
		CommitSHA:    rc.sha,
		EnvName:      rc.env.Name,
		EnvTags:      rc.env.Tags,
		CodeDir:      rc.remoteCodeDir(),
		Entry:        rc.entry,
		StageCommand: stageCommand,
		Timeout:      timeout,
	}
}

// remoteCodeDir is the remote path of the code checkout (clone extracts
// "code" and "tests" side by side under the task dir).
func (rc *rootContext) remoteCodeDir() string {
	return RemoteTaskDir(rc.sha) + "/code"
}

// executeClone implements the clone sub-task: server-side clone of both
// repositories, tar stream upload to the remote workspace.
func (s *Service) executeClone(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	logw := NewLogWriter(s.Store, task.ID)
	defer logw.Close()

	if rc.cfg.CodeRepo == "" {
		s.failTaskLogged(task, logw, "site config has no code repository set")
		return
	}

	testInputRef := rc.inputRef
	fmt.Fprintf(logw, "cloning %s at %s on the server\n", rc.cfg.CodeRepo, rc.sha)

	h := envToSSHHost(rc.env)
	remoteDir := RemoteTaskDir(rc.sha)
	uploaded, err := s.Clone.CloneAndUpload(
		ctx, h,
		rc.cfg.CodeRepo, rc.sha,
		rc.cfg.TestInputRepo, testInputRef,
		rc.creds(), remoteDir,
		slackTimeout(cloneTimeoutSecs, stageTimeoutSlack),
		logw,
	)
	if err != nil {
		s.failTaskLogged(task, logw, fmt.Sprintf("clone/upload failed: %v", err))
		return
	}
	fmt.Fprintf(logw, "uploaded %.1f MiB to %s:%s\n", float64(uploaded)/(1024*1024), rc.env.Host, remoteDir)

	if err := s.Store.FinishTask(task.ID, store.TaskDone, ""); err != nil {
		log.Printf("runner: task %d: finish: %v", task.ID, err)
	}
}

// executeBuild implements the build sub-task: generate the build script and
// run it in the code directory on the remote host. The outcome is recorded
// as a "build" test run so the dashboard can show per-environment build
// results next to the unit/regression kinds.
func (s *Service) executeBuild(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	var stage BuildStageConfig
	if err := json.Unmarshal([]byte(task.Config), &stage); err != nil {
		s.failTask(task, fmt.Sprintf("build config snapshot is invalid: %v", err))
		return
	}

	logw := NewLogWriter(s.Store, task.ID)
	defer logw.Close()

	script, err := BuildBuildScript(rc.scriptInput(task, "", stage.Timeout))
	if err != nil {
		s.failTaskLogged(task, logw, err.Error())
		return
	}

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, "bash -s", script, slackTimeout(stage.Timeout, stageTimeoutSlack), logw, logw)
	logw.Flush() // the build run's summary is derived from the persisted log

	// Record the dashboard build run from the log tail (no per-case results).
	output := s.readLogTail(task.ID)
	status := store.StatusFailed
	if res.ExitCode == 0 {
		status = store.StatusPassed
	}
	summary := ExtractSummary(output, res.ExitCode)
	if res.ExitCode < 0 {
		summary = truncateSummary(fmt.Sprintf("ssh execution failed: %s; log tail: %s", res.Stderr, tailLine(output, 3)))
	}
	s.recordStageRun(task, rc, status, summary)

	s.finishCommandTask(task, logw, res.ExitCode, res.Stderr)
}

// executeStage implements a test sub-task (unit / regression): run the
// stage command remotely, then record the TestRun for the dashboard.
func (s *Service) executeStage(ctx context.Context, task *store.Task) {
	rc, ok := s.loadRootContext(task)
	if !ok {
		return
	}
	var stage StageConfig
	if err := json.Unmarshal([]byte(task.Config), &stage); err != nil {
		s.failTask(task, fmt.Sprintf("stage config snapshot is invalid: %v", err))
		return
	}

	logw := NewLogWriter(s.Store, task.ID)
	defer logw.Close()

	script, err := BuildStageScript(rc.scriptInput(task, stage.Command, stage.Timeout))
	if err != nil {
		s.failTaskLogged(task, logw, err.Error())
		return
	}

	h := envToSSHHost(rc.env)
	res := s.SSH.RunScript(ctx, h, "bash -s", script, slackTimeout(stage.Timeout, stageTimeoutSlack), logw, logw)
	logw.Flush() // the summary is derived from the persisted log

	// The log holds the full output; the summary is derived from it.
	output := s.readLogTail(task.ID)
	status := store.StatusFailed
	if res.ExitCode == 0 {
		status = store.StatusPassed
	}
	summary := ExtractSummary(output, res.ExitCode)
	if res.ExitCode < 0 {
		// Session-level failure (dial, timeout): surface the transport error.
		summary = truncateSummary(fmt.Sprintf("ssh execution failed: %s; log tail: %s", res.Stderr, tailLine(output, 3)))
	}
	s.recordStageRun(task, rc, status, summary)
	s.finishCommandTask(task, logw, res.ExitCode, res.Stderr)
}

// readLogTail re-reads the persisted log tail (the LogWriter already closed
// by defer ordering — read what landed) for summary extraction.
func (s *Service) readLogTail(taskID int64) string {
	logs, err := s.Store.ReadTaskLogs(taskID, 0)
	if err != nil {
		return ""
	}
	var all string
	for _, l := range logs {
		all += l.Content
	}
	return all
}

// recordStageRun upserts the dashboard TestRun for a finished (or failed to
// even start) test stage.
func (s *Service) recordStageRun(task *store.Task, rc *rootContext, status, summary string) {
	input := &store.RunInput{
		EnvironmentID: task.EnvironmentID,
		CommitID:      task.CommitID,
		Kind:          task.Kind, // unit/regression match the run kinds
		Status:        status,
		Summary:       summary,
		StartedAt:     taskStart(task),
		FinishedAt:    time.Now(),
	}
	if _, err := s.Store.UpsertTestRun(input); err != nil {
		log.Printf("runner: task %d: record %s run: %v", task.ID, task.Kind, err)
	}
}

// finishCommandTask maps an execution result to the task's terminal state:
// exit 0 → done, else failed with a redacted error.
func (s *Service) finishCommandTask(task *store.Task, logw *LogWriter, exitCode int, stderr string) {
	if exitCode != 0 {
		msg := stderr
		if msg == "" {
			msg = fmt.Sprintf("command exited with %d (see log)", exitCode)
		}
		s.failTaskLogged(task, logw, fmt.Sprintf("exit %d: %s", exitCode, tailLine(msg, 3)))
		return
	}
	if err := s.Store.FinishTask(task.ID, store.TaskDone, ""); err != nil {
		log.Printf("runner: task %d: finish: %v", task.ID, err)
	}
}

// failTask marks a task failed with a redacted message.
func (s *Service) failTask(task *store.Task, msg string) {
	if cfg, err := s.Store.GetSiteConfig(); err == nil {
		msg = Redact(msg, cfg.DeployToken)
	}
	if err := s.Store.FinishTask(task.ID, store.TaskFailed, msg); err != nil {
		log.Printf("runner: task %d: finish failed: %v", task.ID, err)
	}
}

// failTaskLogged marks a task failed and appends the reason to its log.
func (s *Service) failTaskLogged(task *store.Task, logw *LogWriter, msg string) {
	if cfg, err := s.Store.GetSiteConfig(); err == nil {
		msg = Redact(msg, cfg.DeployToken)
	}
	fmt.Fprintf(logw, "task failed: %s\n", msg)
	if err := s.Store.FinishTask(task.ID, store.TaskFailed, msg); err != nil {
		log.Printf("runner: task %d: finish failed: %v", task.ID, err)
	}
}

func taskStart(task *store.Task) time.Time {
	if task.StartedAt != nil {
		return *task.StartedAt
	}
	return time.Time{}
}
