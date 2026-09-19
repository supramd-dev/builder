package api

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// setupStateJSON answers GET /api/setup: whether the site still needs its
// first-run setup. Nothing else is reported — the endpoint is
// unauthenticated, so it says as little as the frontend needs to choose
// between the setup page and the login page.
type setupStateJSON struct {
	Required bool `json:"required"`
}

// setupInput is the body of POST /api/setup: the code repository, the first
// administrator, and nothing about the webhook — the site generates its own
// webhook token, and the setup page only shows where to copy it from.
type setupInput struct {
	CodeRepo    string `json:"codeRepo"`
	AccessToken string `json:"accessToken"` // optional: a public repository needs none
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
}

// handleSetup routes GET/POST /api/setup — the first-run setup page.
//
// Both are unauthenticated by necessity: a site with no account has no
// session to authenticate with, and no administrator to create one. What
// keeps them safe is the gate behind them — the store refuses to write as
// soon as any account exists — so the window in which they do anything is
// exactly "the database is empty", and it closes on the first account,
// whether that came from here, from adduser or from seed.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.getSetupState(w)
	case http.MethodPost:
		s.completeSetup(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) getSetupState(w http.ResponseWriter) {
	required, err := s.Store.SetupRequired()
	if err != nil {
		log.Printf("setup: count accounts: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, setupStateJSON{Required: required})
}

// completeSetup handles POST /api/setup: create the first account as an
// administrator, store the code repository, and sign the new administrator in
// — the setup page hands the browser straight to the dashboard.
func (s *Server) completeSetup(w http.ResponseWriter, r *http.Request) {
	var in setupInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	in.CodeRepo = strings.TrimSpace(in.CodeRepo)
	in.AccessToken = strings.TrimSpace(in.AccessToken)
	in.Username = strings.TrimSpace(in.Username)
	in.Email = strings.TrimSpace(in.Email)

	if in.CodeRepo == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code repository is required"})
		return
	}
	// The same rules the CLI and the account API apply, so the first
	// administrator is an account the others could not be edited into. The
	// repository location is not host-validated: a self-hosted GitLab lives
	// on an arbitrary host.
	for _, msg := range []string{
		auth.ValidateUsername(in.Username),
		auth.ValidateEmail(in.Email),
		auth.ValidatePassword(in.Password),
	} {
		if msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
	}

	hash, err := auth.HashPassword(in.Password)
	if err != nil {
		log.Printf("setup: hash password: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	user := &store.User{Username: in.Username, Email: in.Email, PasswordHash: hash}
	switch err := s.Store.CompleteSetup(user, in.CodeRepo, in.AccessToken); {
	case errors.Is(err, store.ErrSetupDone):
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "this site is already set up — log in with an existing account",
		})
		return
	case err != nil:
		log.Printf("setup: create the first administrator: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	if err := s.startSession(w, user); err != nil {
		// The account is in the database; a failed sign-in is not a reason to
		// report the setup as failed, only to say the login has to be redone.
		log.Printf("setup: create session for %q: %v", user.Username, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "the account was created, but signing in failed — log in with it",
		})
		return
	}
	// Who was created and from where; never the password or the access token.
	log.Printf("setup: first administrator %q (id %d) created from %s",
		user.Username, user.ID, r.RemoteAddr)
	writeJSON(w, http.StatusCreated, toMeJSON(user))
}
