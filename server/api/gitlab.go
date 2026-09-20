package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// The GitLab sign-in integration: an OAuth 2 authorization-code flow against
// the GitLab instance named in the site configuration.
//
// A user who authorizes gets an account here, created in the "awaiting
// approval" state; an administrator has to admit it before it can sign in.
// The account is keyed by the GitLab user id, so later sign-ins land on the
// same account.
//
// Three routes:
//
//	GET /api/auth/gitlab/enabled   unauthenticated; may we offer the button?
//	GET /api/auth/gitlab/start     begin the flow (redirect to GitLab)
//	GET /api/auth/gitlab/callback  finish it (GitLab redirects back here)
const (
	// gitlabStateCookie carries the anti-CSRF state between the two legs of
	// the flow. HttpOnly and short-lived: it is only ever compared, never
	// read by the frontend.
	gitlabStateCookie = "md_gitlab_state"
	// gitlabStateTTL bounds how long a half-finished sign-in stays valid.
	gitlabStateTTL = 10 * time.Minute
	// gitlabHTTPTimeout bounds every call to GitLab, so an unresponsive
	// instance cannot hold a request goroutine open.
	gitlabHTTPTimeout = 15 * time.Second
	// gitlabScope is the only scope needed: it lets /api/v4/user report the
	// account's own id, username and email. Requesting more would ask the
	// user to grant more than this integration uses.
	gitlabScope = "read_user"
)

// gitlabClient is used for every outbound call to GitLab. A dedicated client
// keeps the timeout from leaking into other parts of the server.
var gitlabClient = &http.Client{Timeout: gitlabHTTPTimeout}

// gitlabUser is the subset of GET /api/v4/user this integration reads.
type gitlabUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

// gitlabTokenResponse is the successful body of POST /oauth/token.
type gitlabTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// gitlabConfig loads the site configuration and reports whether the sign-in
// integration can run. The second return is a reason for the user when it
// cannot; it is deliberately free of secrets and of configuration values, so
// it is safe to show on the login page.
func (s *Server) gitlabConfig() (*store.SiteConfig, string) {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("gitlab sign-in: load site config: %v", err)
		return nil, "the site configuration could not be read"
	}
	if !cfg.GitLabLoginEnabled {
		return nil, "GitLab sign-in is not enabled on this site"
	}
	// The redirect URI has to be absolute and configured, never derived from
	// the request's Host header (which the caller controls).
	if s.PublicURL == "" {
		return nil, "this server has no public URL configured (server.publicURL), so GitLab sign-in cannot run"
	}
	if !isAbsoluteHTTPURL(s.PublicURL) {
		return nil, "the configured public URL (server.publicURL) must include http:// or https://"
	}
	if msg := validateGitLabReady(cfg); msg != "" {
		return nil, msg
	}
	return cfg, ""
}

// gitlabRedirectURI is the callback address handed to GitLab. It must match
// the application's registered redirect URI exactly, which is why the public
// URL is configuration rather than something read off the request.
func (s *Server) gitlabRedirectURI() string {
	return s.PublicURL + "/api/auth/gitlab/callback"
}

// gitlabBase normalizes the configured instance address for building API
// paths (no trailing slash, so joins do not double up).
func gitlabBase(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// isAbsoluteHTTPURL reports whether raw is an http(s) URL with a host — the
// only shape the OAuth flow can build a request or a redirect from. A bare
// host ("md.example.com") parses without error but yields a relative
// redirect_uri, which GitLab rejects with a message that says nothing about
// the cause, so the shape is checked where it is configured.
func isAbsoluteHTTPURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// handleGitLabEnabled answers GET /api/auth/gitlab/enabled. It is
// unauthenticated on purpose: the login page asks before anyone has signed
// in, to decide whether to offer the button. It reports only a boolean.
func (s *Server) handleGitLabEnabled(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	_, msg := s.gitlabConfig()
	writeJSON(w, http.StatusOK, map[string]any{"enabled": msg == ""})
}

// handleGitLabStart begins the flow: it mints a state value, remembers it in
// a cookie and sends the browser to GitLab's authorization page.
func (s *Server) handleGitLabStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cfg, msg := s.gitlabConfig()
	if msg != "" {
		// The button is only offered when the integration is ready, so
		// arriving here means the configuration changed under the user (or
		// the endpoint was called directly). Send them back to the login
		// page with the reason rather than a bare JSON error.
		if s.PublicURL == "" {
			http.Error(w, msg, http.StatusServiceUnavailable)
			return
		}
		s.redirectToLogin(w, r, "unavailable")
		return
	}

	state, err := auth.NewToken()
	if err != nil {
		log.Printf("gitlab sign-in: generate state: %v", err)
		s.redirectToLogin(w, r, "error")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     gitlabStateCookie,
		Value:    state,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false, // set true behind TLS in production, like the session cookie
		MaxAge:   int(gitlabStateTTL.Seconds()),
	})

	q := url.Values{
		"client_id":     {cfg.GitLabClientID},
		"redirect_uri":  {s.gitlabRedirectURI()},
		"response_type": {"code"},
		"scope":         {gitlabScope},
		"state":         {state},
	}
	http.Redirect(w, r, gitlabBase(cfg.GitLabURL)+"/oauth/authorize?"+q.Encode(),
		http.StatusFound)
}

// handleGitLabCallback finishes the flow. Every outcome ends in a redirect
// back to the login page carrying a short status word — never a token, a
// code, or a message from GitLab.
func (s *Server) handleGitLabCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cfg, msg := s.gitlabConfig()
	if msg != "" {
		s.redirectToLogin(w, r, "unavailable")
		return
	}

	// The state cookie is single-use: clear it whatever happens next, so a
	// replayed callback cannot be matched against a still-valid state.
	stateCookie, err := r.Cookie(gitlabStateCookie)
	s.clearStateCookie(w)
	if err != nil || stateCookie.Value == "" {
		s.redirectToLogin(w, r, "error")
		return
	}
	if subtle.ConstantTimeCompare([]byte(stateCookie.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		log.Printf("gitlab sign-in: state mismatch from %s", r.RemoteAddr)
		s.redirectToLogin(w, r, "error")
		return
	}

	// GitLab reports a denied authorization by redirecting back with an
	// error instead of a code; that is a user decision, not a failure.
	if e := r.URL.Query().Get("error"); e != "" {
		s.redirectToLogin(w, r, "denied")
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		s.redirectToLogin(w, r, "error")
		return
	}

	token, err := s.gitlabExchangeCode(r, cfg, code)
	if err != nil {
		log.Printf("gitlab sign-in: exchange code: %v", err)
		s.redirectToLogin(w, r, "error")
		return
	}
	gu, err := s.gitlabFetchUser(r, cfg, token)
	if err != nil {
		log.Printf("gitlab sign-in: fetch user: %v", err)
		s.redirectToLogin(w, r, "error")
		return
	}

	status := s.gitlabSignIn(w, gu)
	if status == "ok" {
		// Signed in: land on the dashboard. There is nothing to tell the
		// user, so the login page is not in the path and no status word is
		// left in the URL.
		http.Redirect(w, r, s.PublicURL+"/#/", http.StatusFound)
		return
	}
	s.redirectToLogin(w, r, status)
}

// gitlabExchangeCode trades the authorization code for an access token.
func (s *Server) gitlabExchangeCode(r *http.Request, cfg *store.SiteConfig, code string) (string, error) {
	form := url.Values{
		"client_id":     {cfg.GitLabClientID},
		"client_secret": {cfg.GitLabClientSecret},
		"code":          {code},
		"grant_type":    {"authorization_code"},
		"redirect_uri":  {s.gitlabRedirectURI()},
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost,
		gitlabBase(cfg.GitLabURL)+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	res, err := gitlabClient.Do(req)
	if err != nil {
		// The error can carry the request URL; the body (which holds the
		// secret) is never included.
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if res.StatusCode != http.StatusOK {
		// GitLab's error body is a description of the failure (invalid
		// client, expired code); it carries no secret of ours, but it is
		// still the remote's text, so only the status is logged by callers.
		return "", fmt.Errorf("token endpoint returned %d", res.StatusCode)
	}
	var out gitlabTokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", errors.New("token response carried no access token")
	}
	return out.AccessToken, nil
}

// gitlabFetchUser reads the authorizing account from GitLab.
func (s *Server) gitlabFetchUser(r *http.Request, cfg *store.SiteConfig, token string) (*gitlabUser, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet,
		gitlabBase(cfg.GitLabURL)+"/api/v4/user", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	res, err := gitlabClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("user endpoint returned %d", res.StatusCode)
	}
	var out gitlabUser
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode user response: %w", err)
	}
	if out.ID == 0 || strings.TrimSpace(out.Username) == "" {
		return nil, errors.New("user response had no id or username")
	}
	return &out, nil
}

// gitlabSignIn turns an authorizing GitLab account into a session, and
// reports what happened as a status word for the login page.
//
// It never returns "ok" for an account that is not allowed in, and it never
// creates an administrator: a GitLab registration is always a regular user,
// and always starts out awaiting approval.
func (s *Server) gitlabSignIn(w http.ResponseWriter, gu *gitlabUser) string {
	existing, err := s.Store.GetUserByGitLabID(gu.ID)
	switch {
	case err == nil:
		// A returning account: the same rules as a password login.
		return s.finishGitLabLogin(w, existing)
	case !errors.Is(err, store.ErrNotFound):
		log.Printf("gitlab sign-in: look up gitlab id %d: %v", gu.ID, err)
		return "error"
	}

	// First sign-in from this GitLab account. GitLab only reports an email
	// when the account has one and the read_user scope was granted; without
	// it there is nothing to build an account on.
	email := strings.TrimSpace(gu.Email)
	if email == "" {
		log.Printf("gitlab sign-in: gitlab user %d (%s) has no visible email", gu.ID, gu.Username)
		return "no_email"
	}

	// An address that already belongs to an account here is never taken
	// over. GitLab's idea of who owns an address is not something this site
	// can verify, so linking the two would hand an existing account to
	// whoever controls that address on the GitLab instance.
	taken, err := s.Store.EmailTaken(email, 0)
	if err != nil {
		log.Printf("gitlab sign-in: check email %q: %v", email, err)
		return "error"
	}
	if taken {
		log.Printf("gitlab sign-in: gitlab user %d (%s) has email %q, which already belongs to a local account",
			gu.ID, gu.Username, email)
		return "email_taken"
	}

	username, err := s.uniqueUsername(gu.Username)
	if err != nil {
		log.Printf("gitlab sign-in: choose username for %q: %v", gu.Username, err)
		return "error"
	}

	// No password: the account can only ever be entered through GitLab, and
	// an empty hash cannot match any password (bcrypt rejects it), so the
	// password login path stays shut for it.
	gitlabID := gu.ID
	user := &store.User{
		Username: username,
		Email:    email,
		Role:     store.RoleUser, // never an administrator, whatever GitLab says
		Source:   store.SourceGitLab,
		GitLabID: &gitlabID,
	}
	if err := s.Store.CreatePendingUser(user); err != nil {
		log.Printf("gitlab sign-in: create pending user for gitlab id %d: %v", gu.ID, err)
		return "error"
	}
	log.Printf("gitlab sign-in: registered %q (id %d) from gitlab user %d — awaiting approval",
		user.Username, user.ID, gu.ID)
	return "pending"
}

// finishGitLabLogin applies the shared sign-in rules to an existing account
// and opens a session when it is allowed in.
func (s *Server) finishGitLabLogin(w http.ResponseWriter, user *store.User) string {
	switch status, _ := loginRefusal(user); status {
	case "disabled":
		return "disabled"
	case "pending":
		return "pending"
	}
	if err := s.startSession(w, user); err != nil {
		log.Printf("gitlab sign-in: create session for %q (id %d): %v", user.Username, user.ID, err)
		return "error"
	}
	log.Printf("gitlab sign-in: %q (id %d) signed in", user.Username, user.ID)
	return "ok"
}

// uniqueUsername returns a username based on the GitLab one that no account
// here has taken, appending a numeric suffix if needed. Two GitLab accounts
// may share a username across a rename, and a GitLab username may collide
// with a local one, so the account's identity is its GitLab id — the name is
// only a label.
func (s *Server) uniqueUsername(base string) (string, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "gitlab-user"
	}
	// The username column is unique and validated elsewhere; keep the
	// candidate within the same shape so it could be edited later.
	if len(base) > 32 {
		base = base[:32]
	}
	for i := 0; i < 50; i++ {
		candidate := base
		if i > 0 {
			candidate = base + "-" + strconv.Itoa(i+1)
		}
		taken, err := s.Store.UsernameTaken(candidate, 0)
		if err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", errors.New("no free username based on " + base)
}

// redirectToLogin sends the browser back to the login page with a status the
// page turns into a message. The status is a fixed word from this file, so
// nothing a remote server said can reach the URL.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request, status string) {
	http.Redirect(w, r, s.PublicURL+"/#/?gitlab="+url.QueryEscape(status), http.StatusFound)
}

// clearStateCookie expires the state cookie at the end of a flow.
func (s *Server) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     gitlabStateCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// loginRefusal applies the rules that keep an account out, shared by the
// password login and the GitLab sign-in so the two cannot drift apart. It
// returns a short status ("", "disabled", "pending") and the message to show.
func loginRefusal(user *store.User) (string, string) {
	switch {
	case user.Disabled:
		return "disabled", "this account has been disabled"
	case !user.Approved:
		return "pending", "this account is awaiting administrator approval"
	}
	return "", ""
}
