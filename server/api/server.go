// Package api implements the HTTP API for md-builder authentication.
package api

import (
	"context"
	"errors"
	"log"
	"net/http"
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

	// Runner, when non-nil, creates task graphs for pushed commits
	// (webhook) and manual triggers. Injected by main so API tests can run
	// without it or with a fake executor.
	Runner *runner.Service
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

// Register mounts the auth + environment API on the given mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/me", s.handleMe)
	mux.HandleFunc("/api/health", s.handleHealth)

	// Site configuration (requires an authenticated user).
	mux.HandleFunc("/api/site-config", s.requireAuth(s.handleSiteConfig))

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

	// Job scheduling: manual trigger and monitoring (requires an
	// authenticated user).
	mux.HandleFunc("/api/jobs", s.requireAuth(s.handleJobs))

	// Task graphs: detail and incremental logs of the runner component.
	mux.HandleFunc("/api/tasks/", s.requireAuth(s.handleTaskItem))
}

// --- handlers ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

	token, err := auth.NewToken()
	if err != nil {
		log.Printf("login: generate token: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	now := time.Now()
	sess := &store.Session{
		Token:     token,
		UserID:    user.ID,
		CreatedAt: now,
		ExpiresAt: auth.SessionExpiry(now),
	}
	if err := s.Store.CreateSession(sess); err != nil {
		log.Printf("login: create session: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
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
	writeJSON(w, http.StatusOK, map[string]any{
		"username": user.Username,
		"email":    user.Email,
	})
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
	writeJSON(w, http.StatusOK, map[string]any{
		"username": user.Username,
		"email":    user.Email,
	})
}

// currentUser resolves the authenticated user from the session cookie, if any.
func (s *Server) currentUser(r *http.Request) (*store.User, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	_, user, err := s.Store.GetSessionByToken(cookie.Value)
	if err != nil {
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

// userFromRequest extracts the authenticated user injected by requireAuth.
func userFromRequest(r *http.Request) *store.User {
	user, _ := r.Context().Value(userContextKey{}).(*store.User)
	return user
}

type userContextKey struct{}
