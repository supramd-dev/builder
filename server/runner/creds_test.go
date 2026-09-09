package runner

import (
	"strings"
	"testing"
)

func TestGitCredentialsEmpty(t *testing.T) {
	var nilCreds *GitCredentials
	if !nilCreds.Empty() {
		t.Error("nil credentials should be empty")
	}
	if nilCreds.HasToken() || nilCreds.HasKey() {
		t.Error("nil credentials should have neither token nor key")
	}
	c := &GitCredentials{}
	if !c.Empty() {
		t.Error("zero credentials should be empty")
	}
	c = &GitCredentials{DeployToken: "tok"}
	if c.Empty() {
		t.Error("token-only credentials should not be empty")
	}
}

func TestTokenUser(t *testing.T) {
	c := &GitCredentials{}
	if c.TokenUser() != "oauth2" {
		t.Errorf("default token user: want oauth2, got %q", c.TokenUser())
	}
	c = &GitCredentials{DeployTokenUser: "gitlab+deploy-token-42"}
	if c.TokenUser() != "gitlab+deploy-token-42" {
		t.Errorf("token user: got %q", c.TokenUser())
	}
}

func TestAuthenticatedURL(t *testing.T) {
	c := &GitCredentials{DeployToken: "glpat-secret", DeployTokenUser: "gitlab+deploy-token-7"}

	cases := []struct {
		in, want string
	}{
		// "+" is a sub-delim and stays unescaped in userinfo (git and Go's
		// HTTP client both parse it back correctly).
		{"https://gitlab.com/group/code", "https://gitlab+deploy-token-7:glpat-secret@gitlab.com/group/code"},
		{"https://gitlab.com/group/code.git", "https://gitlab+deploy-token-7:glpat-secret@gitlab.com/group/code.git"},
		{"http://gitlab.example.com/g/code", "http://gitlab+deploy-token-7:glpat-secret@gitlab.example.com/g/code"},
		// Non-http locations pass through unchanged (an SSH remote is
		// handled by SSHURL instead).
		{"git@gitlab.com:group/code.git", "git@gitlab.com:group/code.git"},
		{"file:///tmp/local-repo", "file:///tmp/local-repo"},
	}
	for _, tc := range cases {
		if got := c.AuthenticatedURL(tc.in); got != tc.want {
			t.Errorf("AuthenticatedURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// No token: unchanged.
	none := &GitCredentials{}
	if got := none.AuthenticatedURL("https://gitlab.com/group/code"); got != "https://gitlab.com/group/code" {
		t.Errorf("no token: got %q", got)
	}
}

func TestSSHURL(t *testing.T) {
	c := &GitCredentials{DeployKey: "-----BEGIN OPENSSH PRIVATE KEY-----"}

	cases := []struct {
		in, wantHost, wantURL string
		wantOK                bool
	}{
		{"https://gitlab.com/group/code.git", "gitlab.com", "ssh://git@gitlab.com/group/code.git", true},
		{"https://gitlab.example.com:8443/g/code", "gitlab.example.com:8443", "ssh://git@gitlab.example.com:8443/g/code.git", true},
		{"git@gitlab.com:group/code.git", "gitlab.com", "ssh://git@gitlab.com/group/code.git", true},
		{"ssh://git@gitlab.com:2222/group/code.git", "gitlab.com:2222", "ssh://git@gitlab.com:2222/group/code.git", true},
		{"gitlab.com/group/code", "gitlab.com", "ssh://git@gitlab.com/group/code.git", true},
		// Local paths make no sense over SSH.
		{"file:///tmp/local-repo", "", "", false},
		{"/tmp/local-repo", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range cases {
		gotURL, ok := c.SSHURL(tc.in)
		if ok != tc.wantOK {
			t.Errorf("SSHURL(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && gotURL != tc.wantURL {
			t.Errorf("SSHURL(%q) = %q, want %q", tc.in, gotURL, tc.wantURL)
		}
		gotHost, ok := c.SSHHost(tc.in)
		if ok != tc.wantOK {
			t.Errorf("SSHHost(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if ok && gotHost != tc.wantHost {
			t.Errorf("SSHHost(%q) = %q, want %q", tc.in, gotHost, tc.wantHost)
		}
	}

	// No key: never applies.
	none := &GitCredentials{}
	if _, ok := none.SSHURL("https://gitlab.com/group/code"); ok {
		t.Error("no key: SSHURL should not apply")
	}
}

func TestRedact(t *testing.T) {
	c := &GitCredentials{DeployToken: "glpat-secret"}
	authed := c.AuthenticatedURL("https://gitlab.com/group/code")

	msg := "fatal: could not read from remote repository: " + authed
	got := Redact(msg, "glpat-secret")
	if strings.Contains(got, "glpat-secret") {
		t.Errorf("token leaked: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("redaction marker missing: %q", got)
	}
	if got := Redact("plain message", ""); got != "plain message" {
		t.Errorf("empty secret should not alter the message: %q", got)
	}
}
