package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"md-builder/server/sshcheck"
	"md-builder/server/store"

	"gorm.io/gorm"
)

// environmentJSON is the wire representation of a test environment. The
// private key is accepted on create/update but never returned in full.
type environmentJSON struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Username    string   `json:"username"`
	PrivateKey  string   `json:"privateKey,omitempty"` // write-only; masked on read
	Tags        []string `json:"tags"`                 // lowercased labels used for job matching
	Description string   `json:"description"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   string   `json:"createdAt"`
	UpdatedAt   string   `json:"updatedAt"`
}

// environmentInput is the request body for create/update.
type environmentInput struct {
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Username    string   `json:"username"`
	PrivateKey  string   `json:"privateKey"`
	Tags        []string `json:"tags"`
	Description string   `json:"description"`
	Enabled     *bool    `json:"enabled"` // pointer so omitted means "keep current" on update
}

// handleEnvironments routes /api/environments (list, create).
func (s *Server) handleEnvironments(w http.ResponseWriter, r *http.Request, user *store.User) {
	switch r.Method {
	case http.MethodGet:
		s.listEnvironments(w, r, user)
	case http.MethodPost:
		s.createEnvironment(w, r, user)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// handleEnvironmentItem routes /api/environments/{id} (get, update, delete)
// and /api/environments/{id}/test (connectivity check).
func (s *Server) handleEnvironmentItem(w http.ResponseWriter, r *http.Request, user *store.User) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/environments/")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid environment id"})
		return
	}

	// /api/environments/{id}/test
	if len(parts) == 2 && parts[1] == "test" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.testEnvironment(w, r, user, id)
		return
	}
	// /api/environments/{id}/enabled — toggle enable/disable.
	if len(parts) == 2 && parts[1] == "enabled" {
		if r.Method != http.MethodPut && r.Method != http.MethodPatch {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.toggleEnvironment(w, r, user, id)
		return
	}
	// /api/environments/{id}/exec — run a command on the remote host.
	if len(parts) == 2 && parts[1] == "exec" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.execEnvironment(w, r, user, id)
		return
	}
	// /api/environments/{id}/script — upload and run a script.
	if len(parts) == 2 && parts[1] == "script" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.scriptEnvironment(w, r, user, id)
		return
	}
	// /api/environments/{id}/build-test — clone the code repository and run
	// a build command (ad-hoc; reuses the job script generator).
	if len(parts) == 2 && parts[1] == "build-test" {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.handleBuildTest(w, r, user, id)
		return
	}
	if len(parts) != 1 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.getEnvironment(w, r, user, id)
	case http.MethodPut, http.MethodPatch:
		s.updateEnvironment(w, r, user, id)
	case http.MethodDelete:
		s.deleteEnvironment(w, r, user, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// --- /api/environments ---

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request, user *store.User) {
	envs, err := s.Store.ListEnvironments(user.ID)
	if err != nil {
		log.Printf("list environments: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]environmentJSON, 0, len(envs))
	for i := range envs {
		out = append(out, toEnvironmentJSON(&envs[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"environments": out})
}

func (s *Server) createEnvironment(w http.ResponseWriter, r *http.Request, user *store.User) {
	var in environmentInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if msg := validateEnvironmentInput(&in); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	env := &store.TestEnvironment{
		OwnerID:     user.ID,
		Name:        in.Name,
		Host:        in.Host,
		Username:    in.Username,
		PrivateKey:  in.PrivateKey,
		Tags:        strings.Join(in.Tags, ","),
		Description: in.Description,
		Enabled:     true, // new environments start enabled unless overridden below
	}
	if in.Enabled != nil {
		env.Enabled = *in.Enabled
	}
	if err := s.Store.CreateEnvironment(env); err != nil {
		log.Printf("create environment: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, toEnvironmentJSON(env))
}

// --- /api/environments/{id} ---

func (s *Server) getEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toEnvironmentJSON(env))
}

func (s *Server) updateEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	var in environmentInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	// Allow updating fields individually? Keep it simple: all fields are
	// provided by the form; an empty private key means "keep the existing one".
	keepKey := strings.TrimSpace(in.PrivateKey) == ""
	if keepKey {
		in.PrivateKey = env.PrivateKey
	}
	if msg := validateEnvironmentInput(&in); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": msg})
		return
	}

	env.Name = in.Name
	env.Host = in.Host
	env.Username = in.Username
	env.PrivateKey = in.PrivateKey
	env.Tags = strings.Join(in.Tags, ",")
	env.Description = in.Description
	if in.Enabled != nil {
		env.Enabled = *in.Enabled
	}
	if err := s.Store.UpdateEnvironment(env); err != nil {
		log.Printf("update environment %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, toEnvironmentJSON(env))
}

// toggleEnvironment handles PUT/PATCH /api/environments/{id}/enabled with
// body {"enabled": true|false}.
func (s *Server) toggleEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if req.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "enabled field is required"})
		return
	}
	env, err := s.Store.SetEnvironmentEnabled(user.ID, id, *req.Enabled)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toEnvironmentJSON(env))
}

func (s *Server) deleteEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	if err := s.Store.DeleteEnvironment(user.ID, id); err != nil {
		log.Printf("delete environment %d: %v", id, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// --- /api/environments/{id}/test ---

func (s *Server) testEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	res := sshcheck.Check(env.Host, env.Username, env.PrivateKey)
	writeJSON(w, http.StatusOK, res)
}

// --- /api/environments/{id}/exec ---

// execEnvironment runs a shell command on the remote host of the environment.
// Only enabled environments accept commands.
func (s *Server) execEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	if !env.Enabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "environment is disabled; enable it before running commands",
		})
		return
	}

	var req struct {
		Command string `json:"command"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command is required"})
		return
	}

	res := sshcheck.Exec(env.Host, env.Username, env.PrivateKey, req.Command)
	writeJSON(w, http.StatusOK, res)
}

// --- /api/environments/{id}/script ---

// scriptEnvironment uploads and runs a script on the remote host of the
// environment. The script body is streamed over stdin; the remote command
// is derived from the first-line comment, which names the interpreter:
//
//	#!/usr/bin/env bash    (or "# bash", default for language "bash")
//	#!/usr/bin/env python3 (or "# python3", default for language "python")
//
// Only enabled environments accept scripts.
func (s *Server) scriptEnvironment(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	if !env.Enabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "environment is disabled; enable it before running commands",
		})
		return
	}

	var req struct {
		Language string `json:"language"` // "bash" or "python"
		Script   string `json:"script"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if strings.TrimSpace(req.Script) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "script is required"})
		return
	}

	cmd, err := scriptCommand(req.Language, req.Script)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	res := sshcheck.Script(env.Host, env.Username, env.PrivateKey, cmd, req.Script)
	writeJSON(w, http.StatusOK, res)
}

// scriptCommand derives the remote command that consumes the script from
// stdin, based on the declared language and the first-line interpreter
// comment in the script itself.
func scriptCommand(language, script string) (string, error) {
	first := script
	if i := strings.IndexByte(script, '\n'); i >= 0 {
		first = script[:i]
	}
	first = strings.TrimSpace(first)

	// Explicit interpreter comment: "#!anything" or "# <name>".
	if strings.HasPrefix(first, "#") {
		name := strings.TrimPrefix(first, "#!")
		name = strings.TrimSpace(strings.TrimPrefix(name, "#"))
		name = strings.TrimSpace(name)
		// Keep the basename of a shebang path (e.g. /usr/bin/env bash).
		if i := strings.LastIndexAny(name, "/ "); i >= 0 {
			name = name[i+1:]
		}
		switch name {
		case "bash", "sh", "python", "python3":
			return name + " -", nil
		case "":
			// fall through to language default
		default:
			return "", fmt.Errorf("unsupported interpreter %q on first line; use bash, sh, python or python3", name)
		}
	}

	// Language default. The interpreter reads the program from stdin.
	switch language {
	case "bash":
		return "bash -", nil
	case "python":
		return "python3 -", nil
	default:
		return "", fmt.Errorf("unsupported language %q; use bash or python", language)
	}
}

// --- helpers ---

// validateEnvironmentInput returns a human-readable error message, or "".
func validateEnvironmentInput(in *environmentInput) string {
	if strings.TrimSpace(in.Name) == "" {
		return "name is required"
	}
	if strings.TrimSpace(in.Host) == "" {
		return "host is required"
	}
	if strings.TrimSpace(in.Username) == "" {
		return "username is required"
	}
	if strings.TrimSpace(in.PrivateKey) == "" {
		return "private key is required"
	}
	if !isPEMKey(in.PrivateKey) {
		return "private key must be a PEM-encoded SSH key"
	}
	return ""
}

// isPEMKey performs a cheap check that the blob looks like PEM.
func isPEMKey(key string) bool {
	t := strings.TrimSpace(key)
	return strings.HasPrefix(t, "-----BEGIN") && strings.Contains(t, "-----END")
}

// toEnvironmentJSON converts a stored environment for the wire. The private
// key is never sent back in full.
func toEnvironmentJSON(env *store.TestEnvironment) environmentJSON {
	return environmentJSON{
		ID:          env.ID,
		Name:        env.Name,
		Host:        env.Host,
		Username:    env.Username,
		Tags:        env.TagList(),
		Description: env.Description,
		Enabled:     env.Enabled,
		CreatedAt:   env.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
		UpdatedAt:   env.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"),
	}
}

// respondEnvironmentError maps store errors to HTTP responses.
func respondEnvironmentError(w http.ResponseWriter, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment not found"})
		return
	}
	log.Printf("environment lookup: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
}
