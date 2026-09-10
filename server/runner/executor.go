package runner

import (
	"context"
	"io"
	"time"

	"md-builder/server/store"
)

// Execer is the runner's transport abstraction: everything task execution
// does to a remote environment. Tests inject fakes; production uses the
// SSH implementation below. (The former sshcheck package is absorbed into
// this component; ssh.go carries its functions.)
type Execer interface {
	// RunScript executes cmd (usually "bash -s") with script on stdin,
	// bounded by timeout, streaming output into stdout/stderr.
	RunScript(ctx context.Context, h SSHHost, cmd, script string, timeout time.Duration, stdout, stderr io.Writer) ExecResult
	// ExecHost runs a short command, buffering its output (interactive
	// checks, remote presence probes).
	ExecHost(h SSHHost, cmd string) ExecResult
	// CheckHost verifies connectivity (environment test endpoint).
	CheckHost(h SSHHost) Result
	// ExtractTarTo streams a gzipped tar from r into destDir on the host.
	ExtractTarTo(ctx context.Context, h SSHHost, r io.Reader, destDir string, timeout time.Duration) error
}

// RepoCloner is the server-side source acquisition abstraction.
type RepoCloner interface {
	// CloneAndUpload clones the repositories on the server and extracts
	// them into remoteWorkDir on the host (see repo.go).
	CloneAndUpload(ctx context.Context, h SSHHost, codeRepoURL, codeRef, testInputRepo, testInputRef string, creds *GitCredentials, remoteWorkDir string, timeout time.Duration, logw io.Writer) (int64, error)
}

// SSHExecer is the production Execer/RepoCloner over the ssh.go functions.
type SSHExecer struct{}

// Compile-time interface conformance.
var (
	_ Execer     = SSHExecer{}
	_ RepoCloner = SSHExecer{}
)

// RunScript implements Execer.
func (SSHExecer) RunScript(ctx context.Context, h SSHHost, cmd, script string, timeout time.Duration, stdout, stderr io.Writer) ExecResult {
	return RunSSH(ctx, h, cmd, script, timeout, stdout, stderr)
}

// ExecHost implements Execer.
func (SSHExecer) ExecHost(h SSHHost, cmd string) ExecResult {
	return ExecSSH(h, cmd)
}

// CheckHost implements Execer.
func (SSHExecer) CheckHost(h SSHHost) Result {
	return CheckSSH(h)
}

// ExtractTarTo implements Execer.
func (SSHExecer) ExtractTarTo(ctx context.Context, h SSHHost, r io.Reader, destDir string, timeout time.Duration) error {
	return ExtractTarTo(ctx, h, r, destDir, timeout)
}

// CloneAndUpload implements RepoCloner.
func (SSHExecer) CloneAndUpload(ctx context.Context, h SSHHost, codeRepoURL, codeRef, testInputRepo, testInputRef string, creds *GitCredentials, remoteWorkDir string, timeout time.Duration, logw io.Writer) (int64, error) {
	return CloneAndUpload(ctx, h, codeRepoURL, codeRef, testInputRepo, testInputRef, creds, remoteWorkDir, timeout, logw)
}

// envToSSHHost builds the SSH endpoint from a stored environment row.
func envToSSHHost(env *store.TestEnvironment) SSHHost {
	return SSHHostFromEnv(env)
}

// slackTimeout adds execution slack to a stage timeout (clone and command
// stages get a fixed overhead for session setup and teardown).
func slackTimeout(stageSecs int, slack time.Duration) time.Duration {
	t := time.Duration(stageSecs) * time.Second
	if t <= 0 {
		t = DefaultStageTimeoutSeconds * time.Second
	}
	return t + slack
}
