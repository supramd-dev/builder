// Package api implements the HTTP API for md-builder authentication.
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/auth"
	"md-builder/server/runner"
	"md-builder/server/store"

	"gorm.io/gorm"
)

const (
	sessionCookie = "md_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// Server holds dependencies shared across API handlers.
type Server struct {
	Store *store.Store

	// Version is the served build's source revision (git commit id), shown
	// in the frontend footer. Injected by main; empty = unknown.
	Version string

	// Runner, when non-nil, creates task graphs for pushed commits
	// (webhook) and manual triggers. Injected by main so API tests can run
	// without it or with a fake executor.
	Runner *runner.Service

	// PublicURL is the address users reach this site at (config
	// server.publicURL, no trailing slash), used to build the GitLab OAuth
	// callback address. Injected by main. Empty disables GitLab sign-in: the
	// redirect URI has to be absolute and exact, so it is configured rather
	// than read from the request's Host header.
	PublicURL string
}

// New returns a configured *Server.
func New(s *store.Store) *Server {
	return &Server{Store: s}
}

// SetRunner wires the runner service used by the webhook and the manual
// trigger endpoint.
func (s *Server) SetRunner(svc *runner.Service) {
	s.Runner = svc
}

// SetPublicURL wires the site's public address, used to build the GitLab
// OAuth callback address. Empty (the default) disables GitLab sign-in.
func (s *Server) SetPublicURL(u string) {
	s.PublicURL = strings.TrimRight(strings.TrimSpace(u), "/")
}

// Register mounts the auth + environment API on the given mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	// GitLab sign-in. All three are unauthenticated by necessity: they run
	// before there is an account to authenticate. /enabled is the login
	// page's probe, /start and /callback are the two legs of the OAuth flow.
	mux.HandleFunc("/api/auth/gitlab/enabled", s.handleGitLabEnabled)
	mux.HandleFunc("/api/auth/gitlab/start", s.handleGitLabStart)
	mux.HandleFunc("/api/auth/gitlab/callback", s.handleGitLabCallback)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/health", s.handleHealth)
	// First-run setup (unauthenticated: it runs before any account exists,
	// and stops doing anything the moment one does — see handleSetup).
	mux.HandleFunc("/api/setup", s.handleSetup)
	// Deep health: the site-status board (git repo reachability, future
	// object storage). Authenticated — the probes read site configuration.
	mux.HandleFunc("/api/health/deep", s.requireAuth(s.handleHealthDeep))

	// Site configuration (requires an authenticated user).
	mux.HandleFunc("/api/site-config", s.requireAuth(s.handleSiteConfig))
	// Rotating the webhook secret: administrators only, since the value it
	// replaces is shown to them alone.
	mux.HandleFunc("/api/site-config/webhook-token", s.requireAdmin(s.handleWebhookTokenRotate))

	// Accounts. The list is administrators-only; editing is permitted per
	// target (yourself, or anybody for an administrator), so the item route
	// authenticates and decides inside the handler.
	mux.HandleFunc("/api/users", s.requireAdmin(s.handleUsers))
	mux.HandleFunc("/api/users/", s.requireAuth(s.handleUserItem))

	// GitLab webhook receiver (unauthenticated: called by the GitLab server).
	mux.HandleFunc("/api/webhooks/gitlab", s.handleGitLabWebhook)

	// Environment management (requires an authenticated user).
	mux.HandleFunc("/api/environments", s.requireAuth(s.handleEnvironments))
	mux.HandleFunc("/api/environments/", s.requireAuth(s.handleEnvironmentItem))

	// Test dashboard: matrix view, result reporting, run details
	// (require an authenticated user).
	mux.HandleFunc("/api/dashboard/", s.requireAuth(s.handleDashboard))
	mux.HandleFunc("/api/test-runs", s.requireAuth(s.handleTestRuns))
	mux.HandleFunc("/api/test-runs/", s.requireAuth(s.handleTestRunItem))
	mux.HandleFunc("/api/test-artifacts/", s.requireAuth(s.handleTestArtifact))

	// Job scheduling: manual trigger and monitoring (requires an
	// authenticated user).
	mux.HandleFunc("/api/jobs", s.requireAuth(s.handleJobs))
	mux.HandleFunc("/api/jobs/", s.requireAuth(s.handleJobs))

	// Task graphs: detail and incremental logs of the runner component.
	mux.HandleFunc("/api/tasks/", s.requireAuth(s.handleTaskItem))
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req loginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Username == "" || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password are required"})
		return
	}

	user, err := s.Store.GetUserByUsername(req.Username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
			return
		}
		log.Printf("login: lookup user %q: %v", req.Username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := auth.CheckPassword(user.PasswordHash, req.Password); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid username or password"})
		return
	}
	// The password was right, so an account that still cannot get in is
	// refused with its own message explaining why. The rules are shared with
	// the GitLab sign-in (loginRefusal) so the two cannot drift apart.
	if _, msg := loginRefusal(user); msg != "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": msg})
		return
	}

	if err := s.startSession(w, user); err != nil {
		log.Printf("login: create session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toMeJSON(user))
}

// startSession opens a login session for user and writes the session cookie.
// The login handler and the first-run setup (which signs the administrator it
// just created straight in) share it, so both hand out the same cookie.
func (s *Server) startSession(w http.ResponseWriter, user *store.User) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	now := time.Now()
	sess := &store.Session{
		Token:     token,
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: auth.SessionExpiry(now),
	}
	if err := s.Store.CreateSession(sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   false, // set true behind TLS in production
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

// toMeJSON is the signed-in account as the frontend knows it. The password
// hash is never part of it, and role is reported, not accepted.
func toMeJSON(user *store.User) map[string]any {
	return map[string]any{
		"id":       user.ID,
		"username": user.Username,
		"email":    user.Email,
		"role":     user.Role,
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cookie, err := r.Cookie(sessionCookie)
	if err == nil && cookie.Value != "" {
		if err := s.Store.DeleteSession(cookie.Value); err != nil {
			log.Printf("logout: delete session: %v", err)
		}
	}
	// Clear the cookie regardless of whether a session existed.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user, ok := s.currentUser(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	writeJSON(w, http.StatusOK, toMeJSON(user))
}

// currentUser resolves the authenticated user from the session cookie, if any.
// A disabled account has no valid session: disabling deletes the stored
// sessions, and this check covers the row that slipped through.
// currentUser resolves the session cookie to an account, and refuses one that
// is not allowed in — the same two rules loginRefusal applies at sign-in, so a
// session cannot outlive a decision an administrator made about the account.
// An account that is disabled or un-approved loses access at once rather than
// keeping it until its session expires.
func (s *Server) currentUser(r *http.Request) (*store.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	_, user, err := s.Store.GetSessionByToken(cookie.Value)
	if err != nil || user.Disabled || !user.Approved {
		return nil, false
	}
	return user, true
}

// AuthMiddleware protects routes that require a logged-in user.
func (s *Server) AuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := s.currentUser(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// requireAuth wraps a handler that needs an authenticated user and passes the
// user into the request context.
func (s *Server) requireAuth(next func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := s.currentUser(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)), user)
	}
}

// requireAdmin wraps a handler that only an administrator may call. Hiding
// the panel in the UI is presentation; this is the permission.
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request, *store.User)) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request, user *store.User) {
		if !user.IsAdmin() {
			log.Printf("admin required: %q (id %d) denied %s %s", user.Username, user.ID, r.Method, r.URL.Path)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
			return
		}
		next(w, r, user)
	})
}

// userFromRequest extracts the authenticated user injected by requireAuth.
func userFromRequest(r *http.Request) *store.User {
	user, _ := r.Context().Value(userContextKey{}).(*store.User)
	return user
}

type userContextKey struct{}
