package runner

import (
	"net/url"
	"strings"
)

// GitCredentials is the single repository credential: a GitLab Project
// Access Token (read_repository scope) applied over HTTPS Basic auth.
// The zero value / nil clones without authentication (public
// repositories).
type GitCredentials struct {
	// AccessToken is the token value (e.g. "glpat-…").
	AccessToken string
}

// Token returns the configured access token ("" when none).
func (c *GitCredentials) Token() string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.AccessToken)
}

// Empty reports whether no credentials are configured.
func (c *GitCredentials) Empty() bool {
	return c.Token() == ""
}

// httpBasicAuth returns the Basic-auth transport credential pair for the
// token: Project Access Tokens authenticate as "oauth2", the GitLab
// convention for token-over-HTTPS clones.
func (c *GitCredentials) httpBasicAuth() (username, password string, ok bool) {
	if c.Empty() {
		return "", "", false
	}
	return "oauth2", c.Token(), true
}

// HTTPURL converts a repository location into the https URL go-git
// clones from: https URLs pass through, scp-style (git@host:group/repo)
// and ssh:// locations are rewritten to their https form, and a bare
// "group/repo" path is resolved against no host (returned unchanged —
// the caller reports the unsupported form). When a token is configured
// it is NOT embedded in the URL; go-git receives it through the
// http.BasicAuth transport instead.
func HTTPURL(repoURL string) string {
	loc := strings.TrimSpace(repoURL)
	// scp-style remotes (git@host:group/repo) first: url.Parse misreads
	// them as opaque URLs with a bogus scheme ("git@gitlab.com:…"), so
	// they must be rewritten before the scheme-based handling.
	if i := strings.IndexByte(loc, '@'); i >= 0 && !strings.Contains(loc, "://") {
		rest := loc[i+1:]
		if j := strings.IndexByte(rest, ':'); j > 0 && !strings.Contains(rest[:j], "/") {
			return "https://" + rest[:j] + "/" + rest[j+1:]
		}
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	switch u.Scheme {
	case "http", "https":
		// Strip any user info: the token goes through the transport, and a
		// stale user:pass in the URL would shadow it.
		u.User = nil
		return u.String()
	case "ssh":
		// ssh://git@host[:sshPort]/group/repo(.git) → https://host/group/repo(.git).
		// The ssh port (e.g. :2222) is dropped: it is not the https port.
		if u.Hostname() != "" && u.Path != "" {
			return "https://" + u.Hostname() + u.Path
		}
	}
	return loc
}

// Redact replaces a secret's occurrences in s with a fixed placeholder so
// error messages (which may echo the failing credential) can be stored or
// returned safely.
func Redact(s, secret string) string {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "REDACTED")
}
