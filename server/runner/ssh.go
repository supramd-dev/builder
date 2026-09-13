package runner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"md-builder/server/store"
)

// This file absorbs the former sshcheck package: connectivity check,
// command execution and script execution over SSH. On top of the old
// surface it adds streaming execution (output forwarded to io.Writers so
// task logs can be persisted incrementally) and tar extraction over stdin
// (the server-side clone uploads the source tree this way).

// defaultTimeout bounds connection phases (TCP dial, handshake, auth).
const defaultSSHTimeout = 8 * time.Second

// execTimeout bounds a remote command run.
const execTimeout = 60 * time.Second

// scriptExecTimeout bounds a remote script run without an explicit timeout.
const scriptExecTimeout = 10 * time.Minute

// Result reports the outcome of a connectivity check.
type Result struct {
	Success        bool   `json:"success"`
	Message        string `json:"message"`          // human-readable summary
	Banner         string `json:"banner,omitempty"` // remote SSH version banner
	DurationMillis int64  `json:"durationMilliSeconds"`
}

// ExecResult reports the outcome of a remote command execution.
type ExecResult struct {
	Success        bool   `json:"success"`
	Stdout         string `json:"stdout"`
	Stderr         string `json:"stderr"`
	ExitCode       int    `json:"exitCode"`
	DurationMillis int64  `json:"durationMilliSeconds"`
}

// SSHHost identifies the remote endpoint of an execution: the fields of a
// stored test environment that SSH needs. The Execer interface (see
// executor.go) is defined against this struct so tests can inject fakes
// without building full environment rows.
type SSHHost struct {
	Host       string
	Username   string
	PrivateKey string
}

// SSHHostFromEnv extracts the SSH endpoint from a stored environment.
func SSHHostFromEnv(env *store.TestEnvironment) SSHHost {
	return SSHHost{Host: env.Host, Username: env.Username, PrivateKey: env.PrivateKey}
}

// dial connects and authenticates to host as username with keyPEM.
// host may be "hostname" or "hostname:port" (port 22 assumed when omitted).
func dialSSH(host, username, keyPEM string) (*ssh.Client, error) {
	addr := host
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(addr, "22")
	}
	signer, err := ssh.ParsePrivateKey([]byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}
	cfg := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: pin host keys per environment
		Timeout:         defaultSSHTimeout,
	}
	return ssh.Dial("tcp", addr, cfg)
}

// Check verifies connectivity and reports basic host information.
func CheckSSH(h SSHHost) Result {
	start := time.Now()

	client, err := dialSSH(h.Host, h.Username, h.PrivateKey)
	if err != nil {
		return failSSH(start, err.Error())
	}
	defer client.Close()

	banner := string(client.ServerVersion())

	// Run a trivial command to confirm a working shell session.
	out, err := runCommandOn(client, "uname -sr")
	if err != nil {
		return failSSH(start, fmt.Sprintf("connected, but command execution failed: %v", err))
	}

	return Result{
		Success:        true,
		Message:        fmt.Sprintf("connected to %s as %s; %s", h.Host, h.Username, strings.TrimSpace(out)),
		Banner:         banner,
		DurationMillis: time.Since(start).Milliseconds(),
	}
}

// ExecSSH runs cmd on the remote host and returns stdout, stderr and exit
// code (buffered; used by the interactive Run-command feature).
func ExecSSH(h SSHHost, cmd string) ExecResult {
	var stdout, stderr strings.Builder
	res := RunSSH(context.Background(), h, cmd, "", execTimeout, &stdout, &stderr)
	return bufferStreamResult(res, stdout.String(), stderr.String())
}

// ScriptSSH runs script on the remote host, feeding it on stdin, and
// returns stdout, stderr and exit code (buffered). The remote command cmd
// is typically an interpreter reading the program from stdin (e.g. "bash
// -s"). Uses the default script timeout.
func ScriptSSH(h SSHHost, cmd, script string) ExecResult {
	return ScriptSSHWithTimeout(h, cmd, script, scriptExecTimeout)
}

// ScriptSSHWithTimeout is ScriptSSH with a caller-provided overall timeout.
// Long jobs (clone + build + test) need more than the default.
func ScriptSSHWithTimeout(h SSHHost, cmd, script string, timeout time.Duration) ExecResult {
	if timeout <= 0 {
		timeout = scriptExecTimeout
	}
	var stdout, stderr strings.Builder
	res := RunSSH(context.Background(), h, cmd, script, timeout, &stdout, &stderr)
	return bufferStreamResult(res, stdout.String(), stderr.String())
}

// bufferStreamResult merges a streaming result with its buffered writers:
// when the session never produced output through the writers (dial failure,
// timeout, run error), the result's own Stderr carries the reason and wins.
func bufferStreamResult(res ExecResult, stdout, stderr string) ExecResult {
	res.Stdout = stdout
	if stderr != "" {
		res.Stderr = stderr
	}
	return res
}

// RunSSH executes cmd remotely with stdinData ("" means no stdin), bounded
// by timeout, streaming stdout/stderr into the given writers (they receive
// output as it arrives, before completion). ctx cancellation force-closes
// the session. The returned ExecResult carries timings and the exit code;
// Stdout/Stderr are left empty (the caller owns the writers).
func RunSSH(ctx context.Context, h SSHHost, cmd, stdinData string, timeout time.Duration, stdout, stderr io.Writer) ExecResult {
	start := time.Now()

	client, err := dialSSH(h.Host, h.Username, h.PrivateKey)
	if err != nil {
		return ExecResult{Success: false, Stderr: err.Error(), ExitCode: -1, DurationMillis: time.Since(start).Milliseconds()}
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{Success: false, Stderr: err.Error(), ExitCode: -1, DurationMillis: time.Since(start).Milliseconds()}
	}
	defer sess.Close()

	if stdout != nil {
		sess.Stdout = stdout
	}
	if stderr != nil {
		sess.Stderr = stderr
	}
	if stdinData != "" {
		sess.Stdin = strings.NewReader(stdinData)
	}

	// ctx or timeout force-closes the client, which kills the session.
	if deadline, ok := ctx.Deadline(); ok && (timeout <= 0 || deadline.Before(time.Now().Add(timeout))) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	} else if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()

	select {
	case err := <-done:
		exit := 0
		if err != nil {
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				exit = ee.ExitStatus()
			} else {
				return ExecResult{
					Success:        false,
					Stderr:         fmt.Sprintf("run error: %v", err),
					ExitCode:       -1,
					DurationMillis: time.Since(start).Milliseconds(),
				}
			}
		}
		return ExecResult{Success: exit == 0, ExitCode: exit, DurationMillis: time.Since(start).Milliseconds()}
	case <-ctx.Done():
		_ = client.Close()
		reason := ctx.Err()
		if timeout > 0 && errors.Is(reason, context.DeadlineExceeded) {
			reason = fmt.Errorf("command timed out after %s", timeout)
		}
		return ExecResult{
			Success:        false,
			Stderr:         fmt.Sprintf("%v", reason),
			ExitCode:       -1,
			DurationMillis: time.Since(start).Milliseconds(),
		}
	}
}

// ExtractTarTo streams a gzipped tar archive from r into destDir on the
// remote host (server-side clone upload): the remote side runs
// `mkdir -p && tar -xzf - -C dir`, the archive flows through the SSH stdin
// without a temp file on either end. destDir is wiped first (the clone task
// owns the workspace). destDir may reference $HOME — it is double-quoted so
// the remote shell expands it (single quotes would create a literal "~"
// directory).
func ExtractTarTo(ctx context.Context, h SSHHost, r io.Reader, destDir string, timeout time.Duration) error {
	remote := fmt.Sprintf("rm -rf %s && mkdir -p %s && tar -xzf - -C %s",
		shellExpand(destDir), shellExpand(destDir), shellExpand(destDir))
	client, err := dialSSH(h.Host, h.Username, h.PrivateKey)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer sess.Close()

	var stderr strings.Builder
	sess.Stderr = &stderr
	sess.Stdin = bufio.NewReader(r)

	done := make(chan error, 1)
	go func() { done <- sess.Run(remote) }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("remote tar: %v (%s)", err, strings.TrimSpace(stderr.String()))
		}
		return nil
	case <-ctx.Done():
		_ = client.Close()
		return fmt.Errorf("tar upload: %v", ctx.Err())
	case <-time.After(timeout):
		_ = client.Close()
		return fmt.Errorf("tar upload timed out after %s; last stderr: %s", timeout, strings.TrimSpace(stderr.String()))
	}
}

// RunCommandWithExitCode runs a short command remotely and returns its exit
// code, stdout and stderr buffered (used by the clone task to verify the
// upload and by interactive checks).
func RunCommandWithExitCode(h SSHHost, cmd string, timeout time.Duration) (int, string, string, error) {
	var stdout, stderr strings.Builder
	res := RunSSH(context.Background(), h, cmd, "", timeout, &stdout, &stderr)
	if res.ExitCode < 0 && !res.Success {
		return res.ExitCode, "", stderr.String(), errors.New(stderr.String())
	}
	return res.ExitCode, stdout.String(), stderr.String(), nil
}

func runCommandOn(client *ssh.Client, cmd string) (string, error) {
	sess, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer sess.Close()

	out, err := sess.CombinedOutput(cmd)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func failSSH(start time.Time, msg string) Result {
	return Result{Success: false, Message: msg, DurationMillis: time.Since(start).Milliseconds()}
}
