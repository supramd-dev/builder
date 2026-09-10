package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"md-builder/server/runner"
	"md-builder/server/store"

	"gorm.io/gorm"
)

// jobJSON is the wire representation of a scheduled root task. The shape
// matches the former Job rows so existing clients keep working.
type jobJSON struct {
	ID            int64  `json:"id"`
	CommitID      int64  `json:"commitId"`
	EnvironmentID int64  `json:"environmentId"`
	Tags          string `json:"tags"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	Attempts      int    `json:"attempts"`
	StartedAt     string `json:"startedAt"`
	FinishedAt    string `json:"finishedAt"`
}

// handleJobs routes /api/jobs: POST re-dispatches a commit (manual trigger),
// GET lists recent root tasks.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	switch r.Method {
	case http.MethodGet:
		s.listJobs(w, r)
	case http.MethodPost:
		s.triggerJobs(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// listJobs handles GET /api/jobs?limit=20 — recent root tasks for monitoring.
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "limit must be an integer between 1 and 100"})
			return
		}
		limit = n
	}
	tasks, err := s.Store.ListRootTasks(limit)
	if err != nil {
		log.Printf("jobs list: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]jobJSON, 0, len(tasks))
	for i := range tasks {
		out = append(out, toJobJSON(&tasks[i]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// triggerInput is the request body of POST /api/jobs.
type triggerInput struct {
	CommitID   int64  `json:"commitId"`
	CommitSHA  string `json:"commitSha"`
	CommitRepo string `json:"commitRepo"`
}

// triggerJobs handles POST /api/jobs — re-run the dispatch for a commit:
// read the md-builder.yaml at that commit, match entries to environments
// (picking up tag/config changes since the push), rebuild the task graphs.
func (s *Server) triggerJobs(w http.ResponseWriter, r *http.Request) {
	if s.Runner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "task dispatch is not configured"})
		return
	}
	var in triggerInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	commitID := in.CommitID
	if commitID == 0 && in.CommitSHA != "" {
		c := &store.Commit{Repo: in.CommitRepo, SHA: in.CommitSHA}
		if _, err := s.Store.GetOrCreateCommit(c); err != nil {
			log.Printf("jobs trigger: create commit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		commitID = c.ID
	}
	if commitID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "commitId or commitSha is required"})
		return
	}

	commit, err := s.Store.GetCommitByID(commitID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "commit not found"})
			return
		}
		log.Printf("jobs trigger: lookup commit: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	d := s.Runner.DispatchForCommit(commit)
	if d.Err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":          d.Err.Error(),
			"jobsCreated":    d.TasksCreated,
			"entriesSkipped": d.EntriesSkipped,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobsCreated":    d.TasksCreated,
		"entriesSkipped": d.EntriesSkipped,
	})
}

func toJobJSON(t *store.Task) jobJSON {
	out := jobJSON{
		ID:            t.ID,
		CommitID:      t.CommitID,
		EnvironmentID: t.EnvironmentID,
		Tags:          t.Tags,
		Status:        t.Status,
		Error:         t.Error,
		Attempts:      t.Attempts,
	}
	if t.StartedAt != nil {
		out.StartedAt = t.StartedAt.UTC().Format(timeFormat)
	}
	if t.FinishedAt != nil {
		out.FinishedAt = t.FinishedAt.UTC().Format(timeFormat)
	}
	return out
}

// timeFormat is the shared RFC3339 layout.
const timeFormat = "2006-01-02T15:04:05Z"

// Compile-time interface shape check for the dispatch surface used here.
var _ = runner.DispatchResult{}
