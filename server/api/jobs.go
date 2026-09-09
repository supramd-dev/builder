package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"

	"md-builder/server/store"

	"gorm.io/gorm"
)

// jobJSON is the wire representation of a scheduled job.
type jobJSON struct {
	ID            int64  `json:"id"`
	CommitID      int64  `json:"commitId"`
	EnvironmentID int64  `json:"environmentId"`
	Tags          string `json:"tags"`
	Status        string `json:"status"`
	Error         string `json:"error"`
	Attempts      int    `json:"attempts"`
	TestInputRef  string `json:"testInputRef"`
	StartedAt     string `json:"startedAt"`
	FinishedAt    string `json:"finishedAt"`
}

// handleJobs routes /api/jobs: POST re-dispatches a commit (manual trigger),
// GET lists recent jobs.
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

// listJobs handles GET /api/jobs?limit=20 — recent jobs for monitoring.
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
	jobs, err := s.Store.ListJobs(limit)
	if err != nil {
		log.Printf("jobs list: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	out := make([]jobJSON, 0, len(jobs))
	for i := range jobs {
		out = append(out, toJobJSON(&jobs[i]))
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
// (picking up tag/config changes since the push), requeue the jobs.
func (s *Server) triggerJobs(w http.ResponseWriter, r *http.Request) {
	if s.Dispatch == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "job dispatch is not configured"})
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

	d := s.Dispatch.DispatchForCommit(commit)
	if d.Err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":          d.Err.Error(),
			"jobsCreated":    d.JobsCreated,
			"entriesSkipped": d.EntriesSkipped,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"jobsCreated":    d.JobsCreated,
		"entriesSkipped": d.EntriesSkipped,
	})
}

func toJobJSON(j *store.Job) jobJSON {
	out := jobJSON{
		ID:            j.ID,
		CommitID:      j.CommitID,
		EnvironmentID: j.EnvironmentID,
		Tags:          j.Tags,
		Status:        j.Status,
		Error:         j.Error,
		Attempts:      j.Attempts,
		TestInputRef:  j.TestInputRef,
	}
	if j.StartedAt != nil {
		out.StartedAt = j.StartedAt.UTC().Format(timeFormat)
	}
	if j.FinishedAt != nil {
		out.FinishedAt = j.FinishedAt.UTC().Format(timeFormat)
	}
	return out
}

// timeFormat is the shared RFC3339 layout.
const timeFormat = "2006-01-02T15:04:05Z"
