package api

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/store"
)

// gitlabEventPayload holds the fields of the GitLab events we consume: push,
// tag push and merge request. GitLab payloads have a loosely defined schema;
// parse defensively — every event kind carries project, object_kind and user
// information, the kind-specific fields are layered on top.
type gitlabEventPayload struct {
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

	// Merge request events: the MR payload nests its state under
	// object_attributes.
	ObjectAttributes struct {
		Action       string `json:"action"` // open | update | close | merge | reopen | approved | ...
		Title        string `json:"title"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
		State        string `json:"state"`
		LastCommit   struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
}

// handleGitLabWebhook receives GitLab webhook events (POST /api/webhooks/gitlab).
//
// Push, tag push and merge request events are recorded in the commits table
// — the dashboard's columns — and dispatched: the md-builder.yaml matrix at
// the event's commit is read, its entries matched to enabled environments by
// tags, and one job per entry is created for the worker pool.
//
// It cannot use a session cookie (the caller is the GitLab server), so it is
// authenticated with the site's webhook token instead: the value shown in
// Settings → Webhook, echoed back by GitLab in the X-Gitlab-Token header.
// That token is unrelated to the site's secret token, which is exported to
// the build scripts as MD_SECRET_TOKEN.
func (s *Server) handleGitLabWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	// Verify before reading the body: a forged event must not be parsed, and
	// the cheaper the rejection the better.
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("gitlab webhook: load site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !webhookTokenMatches(cfg.WebhookToken, r.Header.Get("X-Gitlab-Token")) {
		// The token itself is never logged — only that it did not match.
		log.Printf("gitlab webhook: rejected %s: missing or wrong X-Gitlab-Token "+
			"(copy the token from Settings → Webhook into the GitLab webhook)", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid webhook token"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1 MiB cap
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "failed to read request body"})
		return
	}

	event := r.Header.Get("X-Gitlab-Event")

	var payload gitlabEventPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("gitlab webhook: bad payload: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON payload"})
		return
	}

	kind := payload.ObjectKind
	if kind == "" {
		kind = event // fall back to the X-Gitlab-Event header
	}

	switch kind {
	case "push":
		s.recordPush(w, payload, store.CommitEventPush)
	case "tag_push":
		s.recordPush(w, payload, store.CommitEventTagPush)
	case "merge_request":
		s.recordMergeRequest(w, payload)
	default:
		log.Printf("gitlab webhook: ignored event %q", kind)
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "ignored",
			"event":   kind,
			"message": "event type ignored; push, tag_push and merge_request events are handled",
		})
	}
}

// webhookTokenMatches reports whether the X-Gitlab-Token header carries the
// site's webhook token. The comparison is constant-time, so a caller cannot
// recover the token byte by byte from the response times. An empty token on
// either side never matches: GetSiteConfig generates one, so an empty stored
// token means the row was emptied by hand.
func webhookTokenMatches(want, got string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// recordPush stores a push or tag push event as a dashboard commit column.
// The head commit (the last of the pushed list) is the tested revision; tag
// pushes carry the tag name in ref and the tagged SHA in after.
func (s *Server) recordPush(w http.ResponseWriter, payload gitlabEventPayload, event string) {
	ref := strings.TrimPrefix(payload.Ref, "refs/heads/")
	if event == store.CommitEventTagPush {
		// refs/tags/v1.2.3 → v1.2.3 (keep the full name: tags are the
		// interesting part of a tag push).
		ref = strings.TrimPrefix(payload.Ref, "refs/tags/")
	}
	// The head of a push is the last entry in the commits array; its message
	// makes a useful column label. Tag pushes carry no commits list — the
	// message stays empty and the tag name in ref identifies the column.
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
		Event:    event,
		PushedAt: time.Now(),
	}
	created, err := s.Store.GetOrCreateCommit(commit)
	if err != nil {
		log.Printf("gitlab webhook: record commit %s/%s: %v", commit.Repo, commit.SHA, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	log.Printf("gitlab webhook: %s event on %s (%s) by %s recorded as commit %d",
		event, payload.Project.PathWithNamespace, commit.SHA, payload.UserName, commit.ID)

	// Dispatch: only for pushes to the configured code repository.
	resp := map[string]any{
		"status":   "received",
		"event":    event,
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

// recordMergeRequest stores a merge request event as a dashboard commit
// column and dispatches it like a push. Only the actions that change what
// should be tested trigger a dispatch: open (a fresh source branch state),
// reopen and merge (the merged result). Updates and approvals leave the
// tested SHA alone; close is recorded but not dispatched.
func (s *Server) recordMergeRequest(w http.ResponseWriter, payload gitlabEventPayload) {
	oa := payload.ObjectAttributes
	if oa.SourceBranch == "" && oa.LastCommit.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "merge request event missing object_attributes (source_branch / last_commit)",
		})
		return
	}

	// The tested revision is the MR's last commit on the source branch.
	sha := oa.LastCommit.ID
	ref := oa.SourceBranch
	message := firstLine(oa.LastCommit.Message)
	if message == "" {
		message = firstLine(oa.Title)
	}

	commit := &store.Commit{
		Repo:     payload.Project.PathWithNamespace,
		SHA:      sha,
		Ref:      ref,
		Author:   payload.UserName,
		Message:  message,
		Event:    store.CommitEventMergeRequest,
		PushedAt: time.Now(),
	}
	created, err := s.Store.GetOrCreateCommit(commit)
	if err != nil {
		log.Printf("gitlab webhook: record commit %s/%s: %v", commit.Repo, commit.SHA, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	dispatch := oa.Action == "open" || oa.Action == "reopen" || oa.Action == "merge"
	log.Printf("gitlab webhook: merge request event on %s (%s, action %q) by %s recorded as commit %d",
		payload.Project.PathWithNamespace, commit.SHA, oa.Action, payload.UserName, commit.ID)

	resp := map[string]any{
		"status":   "received",
		"event":    "merge_request",
		"action":   oa.Action,
		"project":  payload.Project.PathWithNamespace,
		"ref":      ref,
		"commitId": commit.ID,
		"created":  created,
	}
	if !dispatch {
		resp["message"] = "action " + oa.Action + " recorded; not dispatched (only open, reopen and merge trigger tests)"
	}
	if dispatch && s.Runner != nil {
		if s.shouldDispatch(payload) {
			d := s.Runner.DispatchForCommit(commit)
			resp["jobsCreated"] = d.TasksCreated
			resp["entriesSkipped"] = d.EntriesSkipped
			if d.Err != nil {
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
func (s *Server) shouldDispatch(payload gitlabEventPayload) bool {
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
