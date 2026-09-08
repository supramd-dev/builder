// Package sshcheck runs commands on remote hosts over SSH using a private
// key. It provides a connectivity check, arbitrary command execution and
// script execution (via stdin).
package sshcheck

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// defaultTimeout bounds connection phases (TCP dial, handshake, auth).
const defaultTimeout = 8 * time.Second

// execTimeout bounds a remote command run.
const execTimeout = 60 * time.Second

// scriptExecTimeout bounds a remote script run. Scripts (e.g. test jobs)
// are expected to take longer than single commands.
const scriptExecTimeout = 10 * time.Minute

// Result reports the outcome of a connectivity check.
type Result struct {
	Success  bool          `json:"success"`
	Message  string        `json:"message"`          // human-readable summary
	Banner   string        `json:"banner,omitempty"` // remote SSH version banner
	Duration time.Duration `json:"durationMilliSeconds"`
}

// ExecResult reports the outcome of a remote command execution.
type ExecResult struct {
	Success  bool          `json:"success"`
	Stdout   string        `json:"stdout"`
	Stderr   string        `json:"stderr"`
	ExitCode int           `json:"exitCode"`
	Duration time.Duration `json:"durationMilliSeconds"`
}

// dial connects and authenticates to host as username with keyPEM.
// host may be "hostname" or "hostname:port" (port 22 assumed when omitted).
func dial(host, username, keyPEM string) (*ssh.Client, error) {
	addr := host
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(host, "22")
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
		Timeout:         defaultTimeout,
	}
	return ssh.Dial("tcp", addr, cfg)
}

// Check verifies connectivity and reports basic host information.
func Check(host, username, keyPEM string) Result {
	start := time.Now()

	client, err := dial(host, username, keyPEM)
	if err != nil {
		return fail(start, err.Error())
	}
	defer client.Close()

	banner := string(client.ServerVersion())

	// Run a trivial command to confirm a working shell session.
	out, err := runCommand(client, "uname -sr")
	if err != nil {
		return fail(start, fmt.Sprintf("connected, but command execution failed: %v", err))
	}

	return Result{
		Success:  true,
		Message:  fmt.Sprintf("connected to %s as %s; %s", host, username, strings.TrimSpace(out)),
		Banner:   banner,
		Duration: time.Since(start),
	}
}

// Exec runs cmd on the remote host and returns stdout, stderr and exit code.
func Exec(host, username, keyPEM, cmd string) ExecResult {
	return run(host, username, keyPEM, cmd, "", execTimeout)
}

// Script runs script on the remote host, feeding it on stdin, and returns
// stdout, stderr and exit code. The remote command cmd is typically
// "interpreter" reading the program from stdin (e.g. "bash -s" or
// "python3 -").
func Script(host, username, keyPEM, cmd, script string) ExecResult {
	return run(host, username, keyPEM, cmd, script, scriptExecTimeout)
}

// run executes cmd remotely with stdin fed from stdinData ("" means no
// stdin), bounded by timeout.
func run(host, username, keyPEM, cmd, stdinData string, timeout time.Duration) ExecResult {
	start := time.Now()

	client, err := dial(host, username, keyPEM)
	if err != nil {
		return ExecResult{Success: false, Stderr: err.Error(), ExitCode: -1, Duration: time.Since(start)}
	}
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		return ExecResult{Success: false, Stderr: err.Error(), ExitCode: -1, Duration: time.Since(start)}
	}
	defer sess.Close()

	var stdout, stderr strings.Builder
	sess.Stdout = &stdout
	sess.Stderr = &stderr
	if stdinData != "" {
		sess.Stdin = io.NopCloser(strings.NewReader(stdinData))
	}

	// Bound total runtime; kill the session channel on timeout.
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
					Success:  false,
					Stdout:   stdout.String(),
					Stderr:   stderr.String() + fmt.Sprintf("\nrun error: %v", err),
					ExitCode: -1,
					Duration: time.Since(start),
				}
			}
		}
		return ExecResult{
			Success:  exit == 0,
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
			ExitCode: exit,
			Duration: time.Since(start),
		}
	case <-time.After(timeout):
		_ = client.Close() // force-close kills the session
		return ExecResult{
			Success:  false,
			Stdout:   stdout.String(),
			Stderr:   stderr.String() + fmt.Sprintf("\ncommand timed out after %s", timeout),
			ExitCode: -1,
			Duration: time.Since(start),
		}
	}
}

func runCommand(client *ssh.Client, cmd string) (string, error) {
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

func fail(start time.Time, msg string) Result {
	return Result{Success: false, Message: msg, Duration: time.Since(start)}
}
