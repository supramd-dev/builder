package api

import (
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"md-builder/server/store"

	"gorm.io/gorm"
)

// defaultCommits is the number of recent commits (dashboard columns) returned
// when the client does not ask for a specific count.
const defaultCommits = 10

// maxCommits caps the ?commits= query parameter.
const maxCommits = 50

// dashboardEnvJSON is an environment column of the dashboard matrix.
type dashboardEnvJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// commitJSON is a commit row of the dashboard matrix.
type commitJSON struct {
	ID       int64  `json:"id"`
	SHA      string `json:"sha"`
	ShortSHA string `json:"shortSha"`
	Repo     string `json:"repo"`
	Ref      string `json:"ref"`
	Author   string `json:"author"`
	Message  string `json:"message"`
	PushedAt string `json:"pushedAt"`
}

// runCellJSON is one cell of the matrix: a run's summary, aligned with an
// environment column. Null when no run exists for that (commit, environment).
type runCellJSON struct {
	RunID      int64  `json:"runId"`
	Status     string `json:"status"`
	Total      int    `json:"total"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
}

// dashboardRowJSON is one matrix row: a commit plus its cells, aligned
// one-to-one with the environments array.
type dashboardRowJSON struct {
	Commit commitJSON     `json:"commit"`
	Cells  []*runCellJSON `json:"cells"` // null entries = no run
}

// dashboardJSON is the full matrix response.
type dashboardJSON struct {
	Kind         string             `json:"kind"`
	RepoFilter   string             `json:"repoFilter,omitempty"` // site-config codeRepo path, when set
	Environments []dashboardEnvJSON `json:"environments"`
	Rows         []dashboardRowJSON `json:"rows"` // one row per commit, newest first
}

// handleDashboard routes GET /api/dashboard/{kind} (kind: regression|unit).
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	rest := strings.TrimPrefix(r.URL.Path, "/api/dashboard/")
	kind := strings.Trim(rest, "/")
	if kind != store.RunKindRegression && kind != store.RunKindUnit {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "unknown dashboard kind; use regression or unit",
		})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	nCommits := defaultCommits
	if v := r.URL.Query().Get("commits"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxCommits {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "commits must be an integer between 1 and 50",
			})
			return
		}
		nCommits = n
	}

	// Column filter: when the site config names a code repository, only
	// pushes to that repository are shown; otherwise all pushes.
	repoFilter := ""
	if cfg, err := s.Store.GetSiteConfig(); err == nil && cfg.CodeRepo != "" {
		repoFilter = store.RepoPath(cfg.CodeRepo)
	}

	envs, err := s.Store.ListAllEnvironments()
	if err != nil {
		log.Printf("dashboard: list environments: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	commits, err := s.Store.ListCommits(repoFilter, nCommits)
	if err != nil {
		log.Printf("dashboard: list commits: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	envIDs := make([]int64, len(envs))
	for i, e := range envs {
		envIDs[i] = e.ID
	}
	commitIDs := make([]int64, len(commits))
	for i, c := range commits {
		commitIDs[i] = c.ID
	}
	runs, err := s.Store.FindRunsByCommits(kind, envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard: find runs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := dashboardJSON{
		Kind:         kind,
		RepoFilter:   repoFilter,
		Environments: make([]dashboardEnvJSON, 0, len(envs)),
		Rows:         make([]dashboardRowJSON, 0, len(commits)),
	}

	// Environment columns, name-ordered.
	for i := range envs {
		env := &envs[i]
		out.Environments = append(out.Environments, dashboardEnvJSON{
			ID:          env.ID,
			Name:        env.Name,
			Description: env.Description,
			Enabled:     env.Enabled,
		})
	}

	// Commit rows, newest first; cells align with the environment columns.
	for i := range commits {
		commit := &commits[i]
		row := dashboardRowJSON{
			Commit: toCommitJSON(commit),
			Cells:  make([]*runCellJSON, len(envs)),
		}
		for j := range envs {
			env := &envs[j]
			if run, ok := runs[store.EnvCommit{Env: env.ID, Commit: commit.ID}]; ok {
				row.Cells[j] = toRunCellJSON(&run)
			}
		}
		out.Rows = append(out.Rows, row)
	}

	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/test-runs (result reporting) ---

// caseInput is one test case in a report.
type caseInput struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`     // "passed" or "failed"
	ErrorValue float64 `json:"errorValue"` // regression error metric
	Message    string  `json:"message"`    // short note / failure reason
}

// runInputJSON is the request body of POST /api/test-runs.
type runInputJSON struct {
	EnvironmentID int64       `json:"environmentId"`
	CommitID      int64       `json:"commitId"`
	CommitSHA     string      `json:"commitSha"` // alternative to commitId: repo+sha lookup
	CommitRepo    string      `json:"commitRepo"`
	Kind          string      `json:"kind"` // "regression" or "unit"
	Cases         []caseInput `json:"cases"`
	StartedAt     string      `json:"startedAt"`  // optional RFC3339
	FinishedAt    string      `json:"finishedAt"` // optional RFC3339
}

// runJSON is the wire representation of a stored run (summary form).
type runJSON struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Total         int    `json:"total"`
	Passed        int    `json:"passed"`
	Failed        int    `json:"failed"`
	EnvironmentID int64  `json:"environmentId"`
	CommitID      int64  `json:"commitId"`
	StartedAt     string `json:"startedAt"`
	FinishedAt    string `json:"finishedAt"`
}

// handleTestRuns routes POST /api/test-runs — submit (or replace) the result
// of one test kind for one environment at one commit.
func (s *Server) handleTestRuns(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var in runInputJSON
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	// Resolve the commit: either the internal id or a repo+sha pair.
	commitID := in.CommitID
	if commitID == 0 && in.CommitSHA != "" {
		c := &store.Commit{Repo: in.CommitRepo, SHA: in.CommitSHA}
		if _, err := s.Store.GetOrCreateCommit(c); err != nil {
			log.Printf("test-runs: create commit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		commitID = c.ID
	}
	if commitID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "commitId or commitSha is required"})
		return
	}

	// Environment existence is validated via a lookup; the dashboard is a
	// site-wide view, but reports must reference a real environment.
	if _, err := s.Store.GetEnvironmentAny(in.EnvironmentID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment not found"})
			return
		}
		log.Printf("test-runs: lookup environment: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if _, err := s.Store.GetCommitByID(commitID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "commit not found"})
			return
		}
		log.Printf("test-runs: lookup commit: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	input := &store.RunInput{
		EnvironmentID: in.EnvironmentID,
		CommitID:      commitID,
		Kind:          in.Kind,
	}
	for i := range in.Cases {
		input.Cases = append(input.Cases, store.TestCaseResult{
			Name:       strings.TrimSpace(in.Cases[i].Name),
			Status:     in.Cases[i].Status,
			ErrorValue: in.Cases[i].ErrorValue,
			Message:    in.Cases[i].Message,
		})
	}
	if t, err := parseOptionalTime(in.StartedAt); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "startedAt must be RFC3339"})
		return
	} else if !t.IsZero() {
		input.StartedAt = t
	}
	if t, err := parseOptionalTime(in.FinishedAt); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "finishedAt must be RFC3339"})
		return
	} else if !t.IsZero() {
		input.FinishedAt = t
	}

	run, err := s.Store.UpsertTestRun(input)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidRunKind):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be regression or unit"})
		case errors.Is(err, store.ErrInvalidCaseStatus):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "case status must be passed or failed"})
		default:
			log.Printf("test-runs: upsert: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, toRunJSON(run))
}

// handleTestRunItem routes GET /api/test-runs/{id} — the detail view of one
// run, including its case results.
func (s *Server) handleTestRunItem(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	rest := strings.TrimPrefix(r.URL.Path, "/api/test-runs/")
	if rest == "" || strings.Contains(rest, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid run id"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	run, err := s.Store.GetTestRun(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "test run not found"})
			return
		}
		log.Printf("test-run get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	env, err := s.Store.GetEnvironmentAny(run.EnvironmentID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			env = nil // environment deleted after the run; detail stays viewable
		} else {
			log.Printf("test-run environment: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	commit, err := s.Store.GetCommitByID(run.CommitID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			commit = nil
		} else {
			log.Printf("test-run commit: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	cases, err := s.Store.ListCaseResults(run.ID)
	if err != nil {
		log.Printf("test-run cases: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	detail := runDetailJSON{
		runJSON: toRunJSON(run),
		Cases:   make([]caseJSON, 0, len(cases)),
	}
	if env != nil {
		name := env.Name
		detail.EnvironmentName = &name
	}
	if commit != nil {
		sha := commit.SHA
		short := shortSHA(commit.SHA)
		msg := commit.Message
		author := commit.Author
		detail.CommitSHA = &sha
		detail.CommitShortSHA = &short
		detail.CommitMessage = &msg
		detail.CommitAuthor = &author
	}
	for i := range cases {
		detail.Cases = append(detail.Cases, toCaseJSON(&cases[i]))
	}
	writeJSON(w, http.StatusOK, detail)
}

// caseJSON is one case result in the run detail.
type caseJSON struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	ErrorValue float64 `json:"errorValue"`
	Message    string  `json:"message"`
}

// runDetailJSON is GET /api/test-runs/{id}'s response: the run summary plus
// environment/commit context and the case list. Pointer fields are null when
// the referenced record was deleted.
type runDetailJSON struct {
	runJSON
	EnvironmentName *string    `json:"environmentName"`
	CommitSHA       *string    `json:"commitSha"`
	CommitShortSHA  *string    `json:"commitShortSha"`
	CommitMessage   *string    `json:"commitMessage"`
	CommitAuthor    *string    `json:"commitAuthor"`
	Cases           []caseJSON `json:"cases"`
}

// --- helpers ---

// parseOptionalTime parses an RFC3339 timestamp; empty means zero.
func parseOptionalTime(v string) (time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// shortSHA abbreviates a commit id for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func toCommitJSON(c *store.Commit) commitJSON {
	return commitJSON{
		ID:       c.ID,
		SHA:      c.SHA,
		ShortSHA: shortSHA(c.SHA),
		Repo:     c.Repo,
		Ref:      c.Ref,
		Author:   c.Author,
		Message:  c.Message,
		PushedAt: c.PushedAt.UTC().Format(time.RFC3339),
	}
}

func toRunCellJSON(run *store.TestRun) *runCellJSON {
	return &runCellJSON{
		RunID:      run.ID,
		Status:     run.Status,
		Total:      run.Total,
		Passed:     run.Passed,
		Failed:     run.Failed,
		StartedAt:  run.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339),
	}
}

func toRunJSON(run *store.TestRun) runJSON {
	return runJSON{
		ID:            run.ID,
		Kind:          run.Kind,
		Status:        run.Status,
		Total:         run.Total,
		Passed:        run.Passed,
		Failed:        run.Failed,
		EnvironmentID: run.EnvironmentID,
		CommitID:      run.CommitID,
		StartedAt:     run.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:    run.FinishedAt.UTC().Format(time.RFC3339),
	}
}

func toCaseJSON(c *store.TestCaseResult) caseJSON {
	return caseJSON{
		ID:         c.ID,
		Name:       c.Name,
		Status:     c.Status,
		ErrorValue: c.ErrorValue,
		Message:    c.Message,
	}
}
