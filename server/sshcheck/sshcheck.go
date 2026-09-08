// Package sshcheck verifies connectivity to a test environment host over SSH
// using a private key, and reports basic host information.
package sshcheck

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// defaultTimeout bounds each phase (TCP dial, handshake, auth).
const defaultTimeout = 8 * time.Second

// Result reports the outcome of a connectivity check.
type Result struct {
	Success  bool          `json:"success"`
	Message  string        `json:"message"`              // human-readable summary
	Banner   string        `json:"banner,omitempty"`     // remote SSH version banner
	Duration time.Duration `json:"durationMilliSeconds"` // total elapsed time
}

// Check attempts to dial and authenticate to host as username with keyPEM.
// host may be "hostname" or "hostname:port" (port 22 assumed when omitted).
func Check(host, username, keyPEM string) Result {
	start := time.Now()

	addr := host
	if !strings.Contains(addr, ":") {
		addr = net.JoinHostPort(host, "22")
	}

	signer, err := ssh.ParsePrivateKey([]byte(keyPEM))
	if err != nil {
		return fail(start, fmt.Sprintf("invalid private key: %v", err))
	}

	cfg := &ssh.ClientConfig{
		User: username,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // TODO: pin host keys per environment
		Timeout:         defaultTimeout,
	}

	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return fail(start, fmt.Sprintf("connection failed: %v", err))
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
		Message:  fmt.Sprintf("connected to %s as %s; %s", addr, username, strings.TrimSpace(out)),
		Banner:   banner,
		Duration: time.Since(start),
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

// ErrInvalidKey is returned when a PEM blob cannot be parsed as an SSH key.
var ErrInvalidKey = errors.New("sshcheck: invalid private key")
