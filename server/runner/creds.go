package runner

import (
	"fmt"
	"net/url"
	"strings"
)

// GitCredentials bundles the optional GitLab deploy key / deploy token from
// the site configuration. The zero value clones without credentials (public
// repositories). Both may be set; which one applies depends on the
// repository URL scheme (see AuthenticatedURL / SSHKey).
type GitCredentials struct {
	// DeployKey is a PEM-encoded SSH private key of a GitLab deploy key.
	DeployKey string
	// DeployToken is a GitLab deploy token or access token.
	DeployToken string
	// DeployTokenUser is the username issued alongside the token (empty
	// means "oauth2", the GitLab default for access tokens).
	DeployTokenUser string
}

// Empty reports whether no credentials are configured.
func (c *GitCredentials) Empty() bool {
	return c == nil || (strings.TrimSpace(c.DeployKey) == "" && strings.TrimSpace(c.DeployToken) == "")
}

// TokenUser returns the username to pair with the deploy token.
func (c *GitCredentials) TokenUser() string {
	if u := strings.TrimSpace(c.DeployTokenUser); u != "" {
		return u
	}
	return "oauth2"
}

// HasToken reports whether a deploy token is configured.
func (c *GitCredentials) HasToken() bool {
	return c != nil && strings.TrimSpace(c.DeployToken) != ""
}

// HasKey reports whether a deploy key is configured.
func (c *GitCredentials) HasKey() bool {
	return c != nil && strings.TrimSpace(c.DeployKey) != ""
}

// AuthenticatedURL returns a repository URL with the deploy token embedded
// for git over HTTPS: https://<user>:<token>@host/group/repo.git. When no
// token is configured (or the URL is not http/https), the input is returned
// unchanged. The returned URL is a secret — never log or store it.
func (c *GitCredentials) AuthenticatedURL(repoURL string) string {
	if !c.HasToken() {
		return repoURL
	}
	u, ok := parseHTTPURL(repoURL)
	if !ok {
		return repoURL
	}
	u.User = url.UserPassword(c.TokenUser(), strings.TrimSpace(c.DeployToken))
	return u.String()
}

// SSHURL converts a repository location to the SSH form
// ssh://git@host[:port]/group/repo.git when a deploy key is configured.
// It returns ("", false) when the key does not apply (not configured, or
// the location cannot be interpreted as a hosted git repository).
func (c *GitCredentials) SSHURL(repoURL string) (string, bool) {
	if !c.HasKey() {
		return "", false
	}
	host, path, ok := splitRepoLocation(repoURL)
	if !ok || host == "" || path == "" {
		return "", false
	}
	return fmt.Sprintf("ssh://git@%s/%s.git", host, path), true
}

// SSHHost returns the host of a repository location's SSH form (see
// SSHURL), for host-key verification.
func (c *GitCredentials) SSHHost(repoURL string) (string, bool) {
	if !c.HasKey() {
		return "", false
	}
	host, _, ok := splitRepoLocation(repoURL)
	if !ok || host == "" {
		return "", false
	}
	return host, true
}

// parseHTTPURL parses an http(s) repository URL.
func parseHTTPURL(repoURL string) (*url.URL, bool) {
	u, err := url.Parse(strings.TrimSpace(repoURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, false
	}
	return u, true
}

// splitRepoLocation splits a repository location into host and
// "group/project" path. It understands https URLs, scp-style remotes
// (git@host:group/repo), ssh:// URLs and bare "host/group/repo" strings.
// The path is normalized without a trailing ".git".
func splitRepoLocation(repoURL string) (host, path string, ok bool) {
	loc := strings.TrimSpace(repoURL)
	if loc == "" {
		return "", "", false
	}
	switch {
	case strings.Contains(loc, "://"):
		u, err := url.Parse(loc)
		if err != nil || u.Host == "" {
			return "", "", false
		}
		host = u.Hostname()
		if p := u.Port(); p != "" {
			host = host + ":" + p
		}
		path = strings.Trim(u.Path, "/")
	case strings.Contains(loc, "@") && strings.Contains(loc, ":"):
		// scp-style: git@host:group/repo(.git)
		rest := loc[strings.LastIndexByte(loc, '@')+1:]
		i := strings.IndexByte(rest, ':')
		if i < 0 {
			return "", "", false
		}
		host = rest[:i]
		path = strings.Trim(rest[i+1:], "/")
	default:
		// bare host/group/repo or host:group/repo
		i := strings.IndexAny(loc, "/:")
		if i < 0 {
			return "", "", false
		}
		if loc[i] == ':' && !strings.Contains(loc[:i], "/") {
			host = loc[:i]
			path = strings.Trim(loc[i+1:], "/")
		} else {
			host = loc[:i]
			path = strings.Trim(loc[i+1:], "/")
		}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || path == "" || !strings.Contains(path, "/") {
		return "", "", false
	}
	return host, path, true
}

// Redact replaces a secret's occurrences in s with a fixed placeholder so
// git error messages (which often echo the failing URL) can be stored or
// returned safely. The token and any token-embedded URL are both redacted.
func Redact(s, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "REDACTED")
}
