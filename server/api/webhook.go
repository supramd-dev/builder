package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// handleGitLabWebhook receives GitLab webhook events (POST /api/webhooks/gitlab).
//
// For now a git push event is only logged — triggering test runs is future
// work. The endpoint is unauthenticated by design: GitLab servers cannot
// hold a session cookie. When a webhook secret is configured it should be
// verified here (X-Gitlab-Token header).
func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	event := r.Header.Get("X-Gitlab-Event")

	// GitLab payloads have a loosely defined schema; parse defensively.
	var payload struct {
		ObjectKind string `json:"object_kind"`
		Project    struct {
			Name              string `json:"name"`
			PathWithNamespace string `json:"path_with_namespace"`
			WebURL            string `json:"web_url"`
		} `json:"project"`
		Ref      string `json:"ref"`
		Before   string `json:"before"`
		After    string `json:"after"`
		UserName string `json:"user_name"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("gitlab webhook: bad payload: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	kind := payload.ObjectKind
	if kind == "" {
		kind = event // fall back to the X-Gitlab-Event header
	}

	switch {
	case kind == "push":
		ref := strings.TrimPrefix(payload.Ref, "refs/heads/")
		log.Printf("gitlab webhook: push event on %s (%s -> %s) by %s; test triggering not implemented yet",
			payload.Project.PathWithNamespace, ref, payload.After, payload.UserName)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "received",
			"event":   "push",
			"project": payload.Project.PathWithNamespace,
			"ref":     ref,
			"message": "push event received; test triggering not implemented yet",
		})
	default:
		log.Printf("gitlab webhook: ignored event %q", kind)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ignored",
			"event":   kind,
			"message": "event type ignored; only push events are handled",
		})
	}
}
