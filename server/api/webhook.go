package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/store"
)

// gitlabPushPayload holds the fields of a GitLab push event we consume.
// GitLab payloads have a loosely defined schema; parse defensively.
type gitlabPushPayload struct {
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
	Commits  []struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	} `json:"commits"`
}

// handleGitLabWebhook receives GitLab webhook events (POST /api/webhooks/gitlab).
//
// Push events are recorded in the commits table — the dashboard's columns —
// and dispatched: the md-builder.yaml matrix at the pushed commit is read,
// its entries matched to enabled environments by tags, and one job per entry
// is created for the worker pool. The endpoint is unauthenticated by design:
// GitLab servers cannot hold a session cookie. When a webhook secret is
// configured it should be verified here (X-Gitlab-Token header).
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

	var payload gitlabPushPayload
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
		s.recordPush(w, payload)
	default:
		log.Printf("gitlab webhook: ignored event %q", kind)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ignored",
			"event":   kind,
			"message": "event type ignored; only push events are handled",
		})
	}
}

// recordPush stores a push event as a dashboard commit column. The head
// commit (the last of the pushed list) is the tested revision.
func (s *Server) recordPush(w http.ResponseWriter, payload gitlabPushPayload) {
	ref := strings.TrimPrefix(payload.Ref, "refs/heads/")
	// The head of a push is the last entry in the commits array; its message
	// makes a useful column label.
	message := ""
	if n := len(payload.Commits); n > 0 {
		message = firstLine(payload.Commits[n-1].Message)
	}

	commit := &store.Commit{
		Repo:     payload.Project.PathWithNamespace,
		SHA:      payload.After,
		Ref:      ref,
		Author:   payload.UserName,
		Message:  message,
		PushedAt: time.Now(),
	}
	created, err := s.Store.GetOrCreateCommit(commit)
	if err != nil {
		log.Printf("gitlab webhook: record commit %s/%s: %v", commit.Repo, commit.SHA, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	log.Printf("gitlab webhook: push event on %s (%s) by %s recorded as commit %d",
		payload.Project.PathWithNamespace, commit.SHA, payload.UserName, commit.ID)

	// Dispatch: only for pushes to the configured code repository.
	resp := map[string]any{
		"status":   "received",
		"event":    "push",
		"project":  payload.Project.PathWithNamespace,
		"ref":      ref,
		"commitId": commit.ID,
		"created":  created,
	}
	if s.Runner != nil {
		if s.shouldDispatch(payload) {
			d := s.Runner.DispatchForCommit(commit)
			resp["jobsCreated"] = d.TasksCreated
			resp["entriesSkipped"] = d.EntriesSkipped
			if d.Err != nil {
				// The commit is recorded; the dispatch failure is surfaced but
				// is not a webhook-level error (GitLab would retry pointlessly).
				resp["dispatchError"] = d.Err.Error()
				log.Printf("gitlab webhook: dispatch for commit %d failed: %v", commit.ID, d.Err)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// shouldDispatch reports whether a push should trigger jobs: the pushed
// repository must match the configured code repository (when one is set).
// The payload carries path_with_namespace ("group/code"), while the config
// may be a full URL — compare on the extracted repo path, falling back to a
// suffix comparison for bare-path configs.
func (s *Server) shouldDispatch(payload gitlabPushPayload) bool {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil || cfg.CodeRepo == "" {
		return false
	}
	pushed := strings.TrimSuffix(strings.Trim(payload.Project.PathWithNamespace, "/"), ".git")
	configured := store.RepoPath(cfg.CodeRepo)
	if configured == "" || configured == pushed {
		return configured == pushed
	}
	// Configured as a bare path (e.g. "group/code"): match the tail.
	return strings.HasSuffix(pushed, "/"+configured) || pushed == configured
}

// firstLine returns the first line of a commit message (its title).
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
