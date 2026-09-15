package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

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
// GET lists recent root tasks. /api/jobs/manual and /api/jobs/manual-yaml
// are the two user-facing dispatches (custom stage commands; the yaml matrix
// of a chosen ref).
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, user *store.User) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/jobs")
	switch strings.Trim(rest, "/") {
	case "manual":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.triggerManual(w, r, user)
		return
	case "manual-yaml":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		s.triggerManualYAML(w, r, user)
		return
	}
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

// manualTestInput is the body of POST /api/jobs/manual: one repository
// (empty = site config default), an optional ref (empty = HEAD), the three
// stage commands (an empty stage is skipped; at least one is required) and
// the environments to run on.
type manualTestInput struct {
	Repo                string               `json:"repo"`
	Ref                 string               `json:"ref"`
	BuildCommand        string               `json:"buildCommand"`
	UnitCommand         string               `json:"unitCommand"`
	UnitArtifacts       runner.ArtifactPaths `json:"unitArtifacts"` // optional artifact path(s) the unit command produces
	RegressionCommand   string               `json:"regressionCommand"`
	RegressionArtifacts runner.ArtifactPaths `json:"regressionArtifacts"` // optional artifact path(s) the regression command produces
	EnvironmentIDs      []int64              `json:"environmentIds"`
}

// manualTestRoot is one created graph of the manual trigger response.
type manualTestRoot struct {
	TaskID        int64 `json:"taskId"`
	EnvironmentID int64 `json:"environmentId"`
}

// triggerManual handles POST /api/jobs/manual — create task graphs for a
// user-submitted test (repository + stage commands + environments) and let
// the scheduler run them.
func (s *Server) triggerManual(w http.ResponseWriter, r *http.Request, user *store.User) {
	if s.Runner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "task dispatch is not configured"})
		return
	}
	var in manualTestInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if len(in.EnvironmentIDs) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "select at least one environment"})
		return
	}
	if strings.TrimSpace(in.BuildCommand) == "" &&
		strings.TrimSpace(in.UnitCommand) == "" &&
		strings.TrimSpace(in.RegressionCommand) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "at least one stage command is required"})
		return
	}

	roots, err := s.Runner.DispatchManual(runner.ManualDispatch{
		Repo:                in.Repo,
		Ref:                 in.Ref,
		BuildCommand:        in.BuildCommand,
		UnitCommand:         in.UnitCommand,
		UnitArtifacts:       in.UnitArtifacts,
		RegressionCommand:   in.RegressionCommand,
		RegressionArtifacts: in.RegressionArtifacts,
		EnvironmentIDs:      in.EnvironmentIDs,
		Username:            user.Username,
	})
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	out := make([]manualTestRoot, 0, len(roots))
	for _, root := range roots {
		out = append(out, manualTestRoot{TaskID: root.ID, EnvironmentID: root.EnvironmentID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": out})
}

// manualYAMLInput is the body of POST /api/jobs/manual-yaml: a ref
// (branch, tag, short/full SHA; empty = HEAD) of the site-configured code
// repository whose md-builder.yaml matrix should be dispatched — the same
// flow a webhook push takes, started by hand.
type manualYAMLInput struct {
	Ref string `json:"ref"`
}

// triggerManualYAML handles POST /api/jobs/manual-yaml — resolve the ref,
// record the commit (deduplicated like a webhook push) and dispatch the
// yaml matrix at it: clone, yaml parse, environment matching, graphs.
func (s *Server) triggerManualYAML(w http.ResponseWriter, r *http.Request, user *store.User) {
	if s.Runner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "task dispatch is not configured"})
		return
	}
	var in manualYAMLInput
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	commit, res := s.Runner.DispatchForRef(r.Context(), in.Ref)
	resp := map[string]any{
		"commitId":       commit.ID,
		"commitSha":      commit.SHA,
		"commitCreated":  res.CommitCreated,
		"jobsCreated":    res.TasksCreated,
		"entriesSkipped": res.EntriesSkipped,
	}
	if res.Err != nil {
		resp["dispatchError"] = res.Err.Error()
		// Mirror the webhook: the commit (when resolved) is recorded, the
		// dispatch failure is surfaced in the body. 422 when nothing was
		// dispatched at all, 200 when at least one graph was created.
		code := http.StatusUnprocessableEntity
		if res.TasksCreated > 0 {
			code = http.StatusOK
		}
		writeJSON(w, code, resp)
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
