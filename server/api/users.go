package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"md-builder/server/auth"
	"md-builder/server/store"
)

// userJSON is the wire form of an account. The password hash is never
// included, and role is read-only: the CLI is the only thing that sets one.
type userJSON struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	Disabled  bool   `json:"disabled"`
	CreatedAt string `json:"createdAt"`
	// Source is where the account came from ("local" or "gitlab"). It is
	// reported, never accepted: an account's origin is a fact about it, not
	// a setting.
	Source string `json:"source"`
	// Approved is false while the account waits for an administrator to
	// admit it (every GitLab self-registration starts here).
	Approved bool `json:"approved"`
	// GitLabID is the account's user id on the GitLab instance it
	// registered through; 0 for a local account.
	GitLabID int64 `json:"gitlabId"`
}

// userInput is the request body for updating an account. An empty password
// keeps the stored one; a nil disabled or approved keeps the current state.
type userInput struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Disabled *bool  `json:"disabled"`
	// Approved admits a self-registered account. Administrators only, and
	// never on themselves (see handleUserItem).
	Approved *bool `json:"approved"`
}

// handleUsers routes GET /api/users — the account list, administrators only.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request, user *store.User) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	_ = user // an administrator, checked by requireAdmin
	users, err := s.Store.ListUsers()
	if err != nil {
		log.Printf("list users: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]userJSON, 0, len(users))
	for i := range users {
		out = append(out, toUserJSON(&users[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

// handleUserItem routes PUT /api/users/{id} — edit an account. Anyone may edit
// their own; an administrator may edit anyone's. Disabling and approving are
// narrower: only an administrator may do either, only to a regular user, never
// to themselves. The role is not editable here at all.
func (s *Server) handleUserItem(w http.ResponseWriter, r *http.Request, actor *store.User) {
	id, err := userIDFromPath(r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid user id"})
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var in userInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Permission: yourself, or anybody if you are an administrator. It is
	// decided before the target is loaded, so a regular user cannot tell an
	// existing id from a missing one.
	if actor.ID != id && !actor.IsAdmin() {
		log.Printf("user edit denied: %q (id %d) is not an administrator and tried to edit user %d",
			actor.Username, actor.ID, id)
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
		return
	}

	target, err := s.Store.GetUserByID(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "user not found"})
			return
		}
		log.Printf("update user %d: lookup: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	username := strings.TrimSpace(in.Username)
	email := strings.TrimSpace(in.Email)
	if msg := auth.ValidateUsername(username); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	if msg := auth.ValidateEmail(email); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}
	password := in.Password
	if password != "" {
		if msg := auth.ValidatePassword(password); msg != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
			return
		}
	}

	disabled := target.Disabled
	if in.Disabled != nil {
		if !actor.IsAdmin() {
			log.Printf("user edit denied: %q (id %d) tried to change the disabled state of %q (id %d)",
				actor.Username, actor.ID, target.Username, target.ID)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
			return
		}
		if actor.ID == target.ID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "you cannot disable your own account"})
			return
		}
		if target.IsAdmin() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "an administrator account cannot be disabled"})
			return
		}
		disabled = *in.Disabled
	}

	// Approval is the administrator's decision about a self-registered
	// account, so it follows the same shape as disabling: administrators
	// only, never on themselves, and never on another administrator (an
	// administrator account is approved by construction and has nothing to
	// approve).
	approved := target.Approved
	if in.Approved != nil {
		if !actor.IsAdmin() {
			log.Printf("user edit denied: %q (id %d) tried to change the approval state of %q (id %d)",
				actor.Username, actor.ID, target.Username, target.ID)
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "administrator permission required"})
			return
		}
		if actor.ID == target.ID {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "you cannot change your own approval state"})
			return
		}
		if target.IsAdmin() {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "an administrator account is always approved"})
			return
		}
		approved = *in.Approved
	}

	// The unique indexes are the real guarantee; this turns the common
	// mistake into a message instead of a constraint violation.
	if taken, err := s.Store.UsernameTaken(username, target.ID); err != nil {
		log.Printf("update user %d: username check: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	} else if taken {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "username " + username + " is already taken"})
		return
	}
	if taken, err := s.Store.EmailTaken(email, target.ID); err != nil {
		log.Printf("update user %d: email check: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	} else if taken {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "email " + email + " is already in use"})
		return
	}

	hash := ""
	if password != "" {
		h, err := auth.HashPassword(password)
		if err != nil {
			log.Printf("update user %d: hash password: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		hash = h
	}

	if err := s.Store.UpdateUser(target.ID, store.UserUpdate{
		Username:     username,
		Email:        email,
		PasswordHash: hash,
		Disabled:     disabled,
		Approved:     approved,
	}); err != nil {
		log.Printf("update user %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// A disabled account must lose its sessions at once, and a new password
	// must invalidate the sessions it was meant to replace — all of them
	// except the caller's own, so editing your own account does not log you
	// out of the browser you are using.
	//
	// Withdrawing an approval counts as a refusal for the same reason
	// disabling does: the account must lose access when the decision is made,
	// not when its session happens to expire.
	if (disabled && !target.Disabled) || (!approved && target.Approved) {
		if err := s.Store.DeleteSessionsForUser(target.ID, ""); err != nil {
			log.Printf("update user %d: drop sessions: %v", id, err)
		}
	} else if hash != "" {
		if err := s.Store.DeleteSessionsForUser(target.ID, sessionToken(r)); err != nil {
			log.Printf("update user %d: drop other sessions: %v", id, err)
		}
	}

	// Admitting an account is worth an audit line: it is the step that turns
	// a self-registration into a usable account.
	if approved && !target.Approved {
		log.Printf("user approved: %q (id %d) admitted by %q (id %d)",
			target.Username, target.ID, actor.Username, actor.ID)
	}

	target.Username = username
	target.Email = email
	target.Disabled = disabled
	target.Approved = approved
	writeJSON(w, http.StatusOK, toUserJSON(target))
}

// userIDFromPath extracts the id from /api/users/{id}.
func userIDFromPath(path string) (int64, error) {
	rest := strings.TrimPrefix(path, "/api/users/")
	rest = strings.TrimSuffix(rest, "/")
	return strconv.ParseInt(rest, 10, 64)
}

// sessionToken returns the caller's session token, or "" when the request
// carries none.
func sessionToken(r *http.Request) string {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func toUserJSON(u *store.User) userJSON {
	out := userJSON{
		ID:        u.ID,
		Username:  u.Username,
		Email:     u.Email,
		Role:      u.Role,
		Disabled:  u.Disabled,
		CreatedAt: u.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		Source:    u.Source,
		Approved:  u.Approved,
	}
	if u.GitLabID != nil {
		out.GitLabID = *u.GitLabID
	}
	return out
}
