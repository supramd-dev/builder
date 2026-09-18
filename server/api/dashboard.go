package api

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"md-builder/server/storage"
	"md-builder/server/store"

	"gorm.io/gorm"
)

// countKind counts the sub-tasks of one kind (the regression case count).
func countKind(subs []store.Task, kind string) int {
	n := 0
	for i := range subs {
		if subs[i].Kind == kind {
			n++
		}
	}
	return n
}

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
	Tags        string `json:"tags"`
	Enabled     bool   `json:"enabled"`
}

// commitJSON is a commit row of the dashboard matrix.
type commitJSON struct {
	ID         int64  `json:"id"`
	SHA        string `json:"sha"`
	ShortSHA   string `json:"shortSha"`
	Repo       string `json:"repo"`
	RepoURL    string `json:"repoUrl,omitempty"` // web URL of the repository, when derivable
	Ref        string `json:"ref"`
	Author     string `json:"author"`
	Message    string `json:"message"`
	Event      string `json:"event,omitempty"` // what created the row: push | tag_push | merge_request | manual | manual_yaml
	PushedAt   string `json:"pushedAt"`
	Superseded bool   `json:"superseded,omitempty"` // a newer attempt of the same SHA exists (manual re-dispatch)
}

// runCellJSON is one cell of the matrix: a run's summary, aligned with an
// environment column. Null when neither a run nor a task graph exists for
// that (commit, environment). When only a task graph exists (queued/running,
// or failed before any report), the cell carries the root task's status with
// runId 0 and taskId set (the frontend links to the task detail).
type runCellJSON struct {
	RunID      int64  `json:"runId"`
	TaskID     int64  `json:"taskId,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Total      int    `json:"total"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Trigger    int    `json:"trigger,omitempty"` // the root graph's trigger (0 = webhook)
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
	RepoURL      string             `json:"repoUrl,omitempty"`    // web URL of that repo, when derivable
	Environments []dashboardEnvJSON `json:"environments"`
	Rows         []dashboardRowJSON `json:"rows"` // one row per commit, newest first
}

// handleDashboard routes GET /api/dashboard/{kind}. kind: regression|unit|build
// (single-kind matrices) or "full" (every commit row carries, per environment,
// the build/unit/regression stages plus the task graph link).
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	rest := strings.TrimPrefix(r.URL.Path, "/api/dashboard/")
	kind := strings.Trim(rest, "/")
	if kind == "full" {
		s.dashboardFull(w, r)
		return
	}
	if !store.RunKindValid(kind) {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "unknown dashboard kind; use regression, unit, build or full",
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
	repoFilter, repoURL := "", ""
	if cfg, err := s.Store.GetSiteConfig(); err == nil && cfg.CodeRepo != "" {
		repoFilter = store.RepoPath(cfg.CodeRepo)
		repoURL = s.repoWebURL(cfg.CodeRepo)
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
	jobs, err := s.Store.FindRootTasksByCommits(envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard: find root tasks: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := dashboardJSON{
		Kind:         kind,
		RepoFilter:   repoFilter,
		RepoURL:      repoURL,
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
			Tags:        env.Tags,
			Enabled:     env.Enabled,
		})
	}

	// Commit rows, newest first; cells align with the environment columns.
	superseded := supersededCommits(commits)
	for i := range commits {
		commit := &commits[i]
		cj := s.toCommitJSON(commit)
		cj.Superseded = superseded[commit.ID]
		row := dashboardRowJSON{
			Commit: cj,
			Cells:  make([]*runCellJSON, len(envs)),
		}
		for j := range envs {
			env := &envs[j]
			key := store.EnvCommit{Env: env.ID, Commit: commit.ID}
			if run, ok := runs[key]; ok {
				row.Cells[j] = toRunCellJSON(&run, runStatusForDisplay(&run))
				continue
			}
			// No run yet: overlay the live task state when a graph exists.
			// The cell shows THIS view's stage (kind), not the root: while a
			// graph is mid-build its unit stage is still queued, even though
			// the root reports running.
			if summary, ok := jobs[key]; ok {
				row.Cells[j] = taskCellJSON(kind, summary.Root, summary.Subs)
			}
		}
		out.Rows = append(out.Rows, row)
	}

	writeJSON(w, http.StatusOK, out)
}

// fullStageJSON is one stage cell of the full matrix: the recorded test run
// (build/unit/regression) for that (commit, environment), or the live task
// state (runId 0, taskId set) when the run has not landed yet. A null entry
// means the commit has no graph on that environment.
type fullStageJSON struct {
	Kind       string `json:"kind"` // build | unit | regression
	RunID      int64  `json:"runId"`
	TaskID     int64  `json:"taskId,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Summary    string `json:"summary,omitempty"`
	StartedAt  string `json:"startedAt"`
	FinishedAt string `json:"finishedAt"`
}

// fullRowJSON is one row of the full matrix: the commit, per environment the
// pipeline stages in display order (build, unit, regression) and the task
// graph link (to the dependency-graph page).
type fullRowJSON struct {
	Commit   commitJSON                `json:"commit"`
	Stages   map[int64][]fullStageJSON `json:"stages"`             // environment id → stages
	TaskIDs  map[int64]int64           `json:"taskIds"`            // environment id → root task id (graph link)
	Triggers map[int64]int             `json:"triggers,omitempty"` // environment id → root trigger (0 = webhook)
}

// fullDashboardJSON is the full matrix response.
type fullDashboardJSON struct {
	RepoFilter   string             `json:"repoFilter,omitempty"`
	RepoURL      string             `json:"repoUrl,omitempty"` // web URL of the filtered repo, when derivable
	Environments []dashboardEnvJSON `json:"environments"`
	Rows         []fullRowJSON      `json:"rows"`
}

// dashboardFull serves GET /api/dashboard/full: every commit row carries, for
// every environment, the state of each pipeline stage — the recorded runs of
// build/unit/regression, or the live task state while the graph runs.
func (s *Server) dashboardFull(w http.ResponseWriter, r *http.Request) {
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

	repoFilter, repoURL := "", ""
	if cfg, err := s.Store.GetSiteConfig(); err == nil && cfg.CodeRepo != "" {
		repoFilter = store.RepoPath(cfg.CodeRepo)
		repoURL = s.repoWebURL(cfg.CodeRepo)
	}

	envs, err := s.Store.ListAllEnvironments()
	if err != nil {
		log.Printf("dashboard full: list environments: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	commits, err := s.Store.ListCommits(repoFilter, nCommits)
	if err != nil {
		log.Printf("dashboard full: list commits: %v", err)
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
	runsByKind := map[string]map[store.EnvCommit]store.TestRun{}
	for _, kind := range []string{store.RunKindBuild, store.RunKindUnit, store.RunKindRegression} {
		runs, err := s.Store.FindRunsByCommits(kind, envIDs, commitIDs)
		if err != nil {
			log.Printf("dashboard full: find %s runs: %v", kind, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
		runsByKind[kind] = runs
	}
	graphs, err := s.Store.FindRootGraphsByCommits(envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard full: find root graphs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := fullDashboardJSON{
		RepoFilter:   repoFilter,
		RepoURL:      repoURL,
		Environments: make([]dashboardEnvJSON, 0, len(envs)),
		Rows:         make([]fullRowJSON, 0, len(commits)),
	}
	for i := range envs {
		env := &envs[i]
		out.Environments = append(out.Environments, dashboardEnvJSON{
			ID:          env.ID,
			Name:        env.Name,
			Description: env.Description,
			Tags:        env.Tags,
			Enabled:     env.Enabled,
		})
	}

	superseded := supersededCommits(commits)
	for i := range commits {
		commit := &commits[i]
		cj := s.toCommitJSON(commit)
		cj.Superseded = superseded[commit.ID]
		row := fullRowJSON{
			Commit:   cj,
			Stages:   map[int64][]fullStageJSON{},
			TaskIDs:  map[int64]int64{},
			Triggers: map[int64]int{},
		}
		for j := range envs {
			envID := envs[j].ID
			key := store.EnvCommit{Env: envID, Commit: commit.ID}

			// A graph exists (or existed) for this (commit, environment):
			// expose the graph link and overlay live states for stages whose
			// run has not landed.
			graph, hasGraph := graphs[key]
			if hasGraph {
				row.TaskIDs[envID] = graph.Root.ID
				row.Triggers[envID] = graph.Root.Trigger
			}

			for _, kind := range []string{store.RunKindBuild, store.RunKindUnit, store.RunKindRegression} {
				if run, ok := runsByKind[kind][key]; ok {
					row.Stages[envID] = append(row.Stages[envID], fullStageJSON{
						Kind:       kind,
						RunID:      run.ID,
						Status:     runStatusForDisplay(&run),
						Summary:    run.Summary,
						StartedAt:  run.StartedAt.UTC().Format(time.RFC3339),
						FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339),
					})
					continue
				}
				// No run: show the live sub-task state when the graph is here.
				if !hasGraph {
					continue
				}
				taskKind := map[string]string{
					store.RunKindBuild:      store.TaskKindBuild,
					store.RunKindUnit:       store.TaskKindUnit,
					store.RunKindRegression: store.TaskKindRegression,
				}[kind]
				for i := range graph.Subs {
					if graph.Subs[i].Kind != taskKind {
						continue
					}
					sub := &graph.Subs[i]
					st := fullStageJSON{
						Kind:   kind,
						TaskID: sub.ID, // the stage sub-task: its detail page has the log
						Status: liveSubStatus(sub),
					}
					if st.Status == "" {
						continue // stage not part of this graph (no run, not queued)
					}
					if sub.Status == store.TaskFailed {
						st.Error = sub.Error
					}
					// Multiple regression case sub-tasks: keep the most
					// severe state (first failing, else first active) and
					// summarize the case count.
					if existing := row.Stages[envID]; kind == store.RunKindRegression && len(existing) > 0 {
						prev := &existing[len(existing)-1]
						if st.Status == store.StatusFailed || prev.Status != store.StatusFailed {
							if st.Status == store.StatusFailed || prev.Status == "" {
								prev.TaskID = st.TaskID
								prev.Status = st.Status
								prev.Error = st.Error
							}
						}
						prev.Summary = fmt.Sprintf("%d cases", countKind(graph.Subs, taskKind))
						continue
					}
					if kind == store.RunKindRegression {
						st.Summary = fmt.Sprintf("%d cases", countKind(graph.Subs, taskKind))
					}
					row.Stages[envID] = append(row.Stages[envID], st)
				}
			}
		}
		out.Rows = append(out.Rows, row)
	}

	writeJSON(w, http.StatusOK, out)
}

// runStatusForDisplay translates a stored run status for the dashboards: a
// failed run whose summary starts with "skipped:" is the runner's
// recordSkippedRuns artifact — the stage never ran because an upstream task
// failed — so it surfaces as "skipped" instead of a hard failure.
func runStatusForDisplay(run *store.TestRun) string {
	if run.Status == store.StatusFailed && strings.HasPrefix(run.Summary, "skipped:") {
		return "skipped"
	}
	return run.Status
}

// liveSubStatus renders a not-yet-reported stage's dashboard status from its
// sub-task: queued/running while pending/running, failed/skipped verbatim
// (skipped = an upstream stage failed before this one could run; skipped
// stages that already got their recordSkippedRuns row surface through the
// run path instead), and "" when the stage finished but no run was reported
// — the cell then stays empty instead of showing a misleading failure.
func liveSubStatus(sub *store.Task) string {
	switch sub.Status {
	case store.TaskPending:
		return "pending"
	case store.TaskRunning:
		return "running"
	case store.TaskFailed:
		return store.StatusFailed
	case store.TaskSkipped:
		return "skipped"
	default:
		// done: a finished stage without a reported run — neither success
		// nor failure is known, so the cell shows nothing for this stage.
		return ""
	}
}

// --- POST /api/test-runs (result reporting) ---

// caseInput is one test case in a report — it becomes a child TestRun under
// the reported run.
type caseInput struct {
	Name           string  `json:"name"`
	Description    string  `json:"description"` // the case's human label (md-builder.yaml preset description)
	Status         string  `json:"status"`      // "passed", "failed" or "skipped"
	Message        string  `json:"message"`     // short note / failure reason
	DurationMillis float64 `json:"durationMillis"`
	TaskID         int64   `json:"taskId"` // the case's own sub-task, when known
}

// runInputJSON is the request body of POST /api/test-runs. When cases are
// present the run status is derived from them; without cases the explicit
// counts and status/summary are stored directly (the aggregate unit path).
type runInputJSON struct {
	EnvironmentID int64       `json:"environmentId"`
	CommitID      int64       `json:"commitId"`
	CommitSHA     string      `json:"commitSha"` // alternative to commitId: repo+sha lookup
	CommitRepo    string      `json:"commitRepo"`
	Kind          string      `json:"kind"`        // "regression", "unit" or "build"
	Description   string      `json:"description"` // human label from md-builder.yaml (optional)
	Status        string      `json:"status"`      // used only when cases is empty
	Summary       string      `json:"summary"`     // used only when cases is empty
	Total         int         `json:"total"`       // aggregate counts, used only when cases is empty
	Passed        int         `json:"passed"`
	Failed        int         `json:"failed"`
	Skipped       int         `json:"skipped"`
	Cases         []caseInput `json:"cases"`
	StartedAt     string      `json:"startedAt"`  // optional RFC3339
	FinishedAt    string      `json:"finishedAt"` // optional RFC3339
}

// runJSON is the wire representation of a stored run (summary form).
type runJSON struct {
	ID            int64  `json:"id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Summary       string `json:"summary"`
	Description   string `json:"description,omitempty"` // human label from md-builder.yaml, stored at dispatch/report time
	Name          string `json:"name"`                  // child runs: the preset/case name; empty on top-level runs
	Message       string `json:"message"`
	Total         int    `json:"total"`
	Passed        int    `json:"passed"`
	Failed        int    `json:"failed"`
	Skipped       int    `json:"skipped"`
	TaskID        int64  `json:"taskId"` // stage sub-task that produced the run (0 = external report)
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
		Description:   in.Description,
		Status:        in.Status,
		Summary:       in.Summary,
		Total:         in.Total,
		Passed:        in.Passed,
		Failed:        in.Failed,
		Skipped:       in.Skipped,
	}
	for i := range in.Cases {
		input.Cases = append(input.Cases, store.CaseInput{
			Name:           strings.TrimSpace(in.Cases[i].Name),
			Description:    in.Cases[i].Description,
			Status:         in.Cases[i].Status,
			Message:        in.Cases[i].Message,
			DurationMillis: in.Cases[i].DurationMillis,
			TaskID:         in.Cases[i].TaskID,
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
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be regression, unit or build"})
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
// run, including its case results — and GET /api/test-runs/{id}/artifacts/zip,
// a download bundle of the run's artifacts (children included).
func (s *Server) handleTestRunItem(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	rest := strings.TrimPrefix(r.URL.Path, "/api/test-runs/")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	idStr, sub := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		idStr, sub = rest[:i], rest[i+1:]
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid run id"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if sub == "artifacts/zip" {
		s.downloadRunArtifactsZip(w, r, id)
		return
	}
	if sub != "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
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
	// Keep the detail view consistent with the dashboard: a failed run whose
	// summary starts with "skipped:" is the runner's recordSkippedRuns
	// artifact — the stage never ran, an upstream stage failed.
	if run.Status == store.StatusFailed && strings.HasPrefix(run.Summary, "skipped:") {
		run.Status = "skipped"
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
	cases, err := s.Store.ListChildRuns(run.ID)
	if err != nil {
		log.Printf("test-run cases: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	artifacts, err := s.Store.ListRunArtifacts(run.ID)
	if err != nil {
		log.Printf("test-run artifacts: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	detail := runDetailJSON{
		runJSON:   toRunJSON(run),
		Cases:     make([]caseJSON, 0, len(cases)),
		Artifacts: make([]artifactRefJSON, 0, len(artifacts)),
	}
	// A child run links back to its parent regression run (the breadcrumb).
	if run.ParentID != 0 {
		detail.ParentRunID = run.ParentID
		if parent, err := s.Store.GetTestRun(run.ParentID); err == nil && parent.Name != "" {
			detail.ParentName = &parent.Name
		}
	}
	// Root task of the producing stage sub-task (for the breadcrumb link to
	// the graph page); 0 when the run came from an external report or the
	// task row was deleted.
	if run.TaskID != 0 {
		if task, err := s.Store.GetTask(run.TaskID); err == nil {
			detail.RootTaskID = task.RootID
		}
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
		repo := commit.Repo
		repoURL := s.repoWebURL(commit.Repo)
		detail.CommitSHA = &sha
		detail.CommitShortSHA = &short
		detail.CommitMessage = &msg
		detail.CommitAuthor = &author
		detail.CommitRepo = &repo
		detail.CommitRepoURL = &repoURL
	}
	for i := range cases {
		detail.Cases = append(detail.Cases, toCaseJSON(&cases[i]))
	}
	for i := range artifacts {
		detail.Artifacts = append(detail.Artifacts, artifactRefJSON{
			ID:   artifacts[i].ID,
			Kind: artifacts[i].Kind,
			Name: artifacts[i].Name,
			Size: int(artifacts[i].Size),
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

// caseJSON is one case in the run detail: a summary of the case's own
// (child) TestRun — id doubles as the runId the UI links into the case page.
type caseJSON struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	Description    string  `json:"description,omitempty"` // the case's human label (preset description)
	Status         string  `json:"status"`
	Message        string  `json:"message"`
	DurationMillis float64 `json:"durationMillis"`
}

// artifactRefJSON references one stored artifact in the run detail (the
// content itself comes from GET /api/test-artifacts/{id}).
type artifactRefJSON struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Size int    `json:"size"`
}

// runDetailJSON is GET /api/test-runs/{id}'s response: the run summary plus
// environment/commit context, the case list and the artifact references.
// Pointer fields are null when the referenced record was deleted. Child runs
// carry ParentRunID/ParentName so their detail page can link back up.
type runDetailJSON struct {
	runJSON
	RootTaskID      int64             `json:"rootTaskId"` // root of the producing stage task (0 = external report); the graph-page link
	ParentRunID     int64             `json:"parentRunId"`
	ParentName      *string           `json:"parentName"` // set only for child runs (the preset name)
	EnvironmentName *string           `json:"environmentName"`
	CommitSHA       *string           `json:"commitSha"`
	CommitShortSHA  *string           `json:"commitShortSha"`
	CommitMessage   *string           `json:"commitMessage"`
	CommitAuthor    *string           `json:"commitAuthor"`
	CommitRepo      *string           `json:"commitRepo"`    // repository location, e.g. "group/code"
	CommitRepoURL   *string           `json:"commitRepoUrl"` // web URL of the repository, when derivable
	Cases           []caseJSON        `json:"cases"`
	Artifacts       []artifactRefJSON `json:"artifacts"`
}

// handleTestArtifact routes GET /api/test-artifacts/{id} — one stored
// artifact's raw content (the run detail lists references only; this is the
// fetch entry point for the browser-side results parsing and the future
// regression "analyze" view) — and GET /api/test-artifacts/{id}/download,
// the same bytes as a file download.
func (s *Server) handleTestArtifact(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/test-artifacts/")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	idStr, sub := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		idStr, sub = rest[:i], rest[i+1:]
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid artifact id"})
		return
	}
	if sub == "download" {
		s.downloadArtifact(w, r, id)
		return
	}
	if sub != "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	a, err := s.Store.GetArtifact(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact not found"})
			return
		}
		log.Printf("artifact get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	content, err := s.Store.ArtifactContent(r.Context(), a)
	if err != nil {
		s.artifactReadError(w, "artifact get", a, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      a.ID,
		"runId":   a.RunID,
		"kind":    a.Kind,
		"name":    a.Name,
		"content": string(content),
	})
}

// artifactReadError answers a failed artifact read: 404 when the object is
// missing from the backend, 502 when the backend itself failed (the artifact
// exists in the database, so the caller's request was fine). The detail is
// logged, never returned — it names the endpoint and the key, not the
// credentials.
func (s *Server) artifactReadError(w http.ResponseWriter, what string, a *store.TestArtifact, err error) {
	if errors.Is(err, storage.ErrNotFound) {
		log.Printf("%s: artifact %d: %v", what, a.ID, err)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact content is missing from object storage"})
		return
	}
	log.Printf("%s: artifact %d: %v", what, a.ID, err)
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "object storage unavailable"})
}

// downloadArtifact streams one artifact as a file download. The name is the
// artifact's path basename (artifact names are remote paths like
// "build/test_detail.xml"); an empty or dotted name falls back to a
// kind-based default so the browser always gets a sensible filename.
func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request, id int64) {
	a, err := s.Store.GetArtifact(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact not found"})
			return
		}
		log.Printf("artifact download: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	rc, size, err := s.Store.OpenArtifact(r.Context(), a)
	if err != nil {
		s.artifactReadError(w, "artifact download", a, err)
		return
	}
	defer rc.Close()

	name := artifactDownloadName(a, id)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodGet {
		if _, err := io.Copy(w, rc); err != nil {
			// The client is usually gone by now; nothing can be
			// reported in the response, so log and stop.
			log.Printf("artifact download: artifact %d: stream: %v", id, err)
		}
	}
}

// artifactDownloadName derives a safe download filename: the source path's
// basename, ASCII-only, never "." / ".." / empty.
func artifactDownloadName(a *store.TestArtifact, id int64) string {
	base := path.Base(strings.TrimSpace(a.Name))
	base = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_' || r == '+':
			return r
		default:
			return '-'
		}
	}, base)
	if base == "" || base == "." || base == ".." || strings.Trim(base, ".-") == "" {
		return fmt.Sprintf("artifact-%d.%s.txt", id, a.Kind)
	}
	return base
}

// downloadRunArtifactsZip streams one zip of the run's artifacts: the run's
// own at the archive root, regression child runs' under cases/<case name>/.
// Runs with no artifacts anywhere get a 404 JSON error (an empty archive
// would look like success).
func (s *Server) downloadRunArtifactsZip(w http.ResponseWriter, r *http.Request, runID int64) {
	if _, err := s.Store.GetTestRun(runID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "test run not found"})
			return
		}
		log.Printf("run artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	byRun, err := s.Store.ListRunArtifactsDeep(runID)
	if err != nil {
		log.Printf("run artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	children, err := s.Store.ListChildRuns(runID)
	if err != nil {
		log.Printf("run artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	total := 0
	for _, list := range byRun {
		total += len(list)
	}
	if total == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run has no artifacts"})
		return
	}

	// Archive layout: the run's own files at the root, each child run's under
	// cases/<case name>/ (the names children carry are the preset names).
	nameOf := make(map[int64]string, len(children))
	for i := range children {
		nameOf[children[i].ID] = children[i].Name
	}

	// started records whether the archive header has been written. Until it
	// has, a failing read is still reportable as a status code instead of a
	// download that quietly contains nothing.
	started := false
	var failErr error
	var failArt *store.TestArtifact
	// dropped records an artifact that could not be read once the stream had
	// begun: the archive is left unfinished, see below.
	dropped := false

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("run-%d-artifacts.zip", runID)))
	zw := zip.NewWriter(w)
	used := map[string]int{}
	add := func(name string, a *store.TestArtifact) {
		if failErr != nil {
			return
		}
		// Read the object before writing the entry header: a header with no
		// bytes behind it would look like an empty file, not a failure.
		// One artifact at a time, so a large run is never buffered whole.
		rc, _, err := s.Store.OpenArtifact(r.Context(), a)
		if err != nil {
			if !started {
				failErr, failArt = err, a
				return
			}
			// The archive is already streaming: the only honest outcome is
			// to drop the entry and leave the zip truncated (an incomplete
			// archive is detectable; a zero-byte entry is not).
			log.Printf("run artifacts zip: artifact %d: %v", a.ID, err)
			dropped = true
			return
		}
		defer rc.Close()
		// Two artifacts with the same basename (the path prefix was the only
		// difference) must not overwrite each other: suffix " (2)", " (3)".
		key := strings.ToLower(name)
		n := used[key]
		used[key] = n + 1
		if n > 0 {
			ext := path.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			name = fmt.Sprintf("%s (%d)%s", stem, n+1, ext)
		}
		started = true // zw.Create writes the local header from here on
		fw, err := zw.Create(name)
		if err != nil {
			log.Printf("run artifacts zip: artifact %d: entry: %v", a.ID, err)
			return
		}
		if _, err := io.Copy(fw, rc); err != nil {
			log.Printf("run artifacts zip: artifact %d: stream: %v", a.ID, err)
		}
	}
	for i := range byRun[runID] {
		add(artifactDownloadName(&byRun[runID][i], byRun[runID][i].ID), &byRun[runID][i])
	}
	for i := range children {
		c := children[i]
		for j := range byRun[c.ID] {
			a := &byRun[c.ID][j]
			add(path.Join("cases", sanitizeZipSegment(c.Name), artifactDownloadName(a, a.ID)), a)
		}
	}
	if failErr != nil {
		// Nothing was written yet, so the caller still gets a proper status.
		s.artifactReadError(w, "run artifacts zip", failArt, failErr)
		return
	}
	if dropped {
		// Deliberately leave the archive unclosed. Flushing hands over what
		// the writers have (the entries so far), and the missing central
		// directory leaves the download visibly broken — which beats a valid
		// zip quietly missing a file, the failure a partial backend outage
		// would otherwise hide behind.
		if err := zw.Flush(); err != nil {
			log.Printf("run artifacts zip: flush: %v", err)
		}
		return
	}
	if err := zw.Close(); err != nil {
		log.Printf("run artifacts zip: close: %v", err) // headers already sent; the client sees a truncated zip
	}
}

// sanitizeZipSegment makes a child-run (case) name safe as one zip path
// segment: separators and empty/dotted segments collapse to "case".
func sanitizeZipSegment(name string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '.' || r == '-' || r == '_' || r == '+' || r == ' ':
			return r
		default:
			return '-'
		}
	}, strings.TrimSpace(name))
	if s == "" || s == "." || s == ".." || strings.Trim(s, ".- ") == "" {
		return "case"
	}
	return s
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

func (s *Server) toCommitJSON(c *store.Commit) commitJSON {
	return commitJSON{
		ID:       c.ID,
		SHA:      c.SHA,
		ShortSHA: shortSHA(c.SHA),
		Repo:     c.Repo,
		RepoURL:  s.repoWebURL(c.Repo),
		Ref:      c.Ref,
		Author:   c.Author,
		Message:  c.Message,
		Event:    c.Event,
		PushedAt: c.PushedAt.UTC().Format(time.RFC3339),
	}
}

// supersededCommits returns the ids of every commit row but the newest of
// each (repo, sha) group: manual re-dispatches of the same SHA insert one
// row per attempt, and only the latest attempt is the live result. commits
// must be newest-first (ListCommits order).
func supersededCommits(commits []store.Commit) map[int64]bool {
	seen := map[string]bool{}
	superseded := map[int64]bool{}
	for i := range commits {
		key := commits[i].Repo + "\x00" + commits[i].SHA
		if seen[key] {
			superseded[commits[i].ID] = true
		}
		seen[key] = true
	}
	return superseded
}

// repoWebURL turns a repository location — the site config's codeRepo or a
// webhook's path_with_namespace — into the web URL hosting it, so dashboard
// commit cells can link to the actual repository. Falls back to "" when the
// host is unknown (a bare "group/project" path).
func (s *Server) repoWebURL(repo string) string {
	loc := strings.TrimSpace(repo)
	if loc == "" {
		return ""
	}
	// A full http(s) URL: drop a trailing .git and use it as-is.
	if strings.HasPrefix(loc, "http://") || strings.HasPrefix(loc, "https://") {
		return strings.TrimSuffix(strings.TrimSuffix(loc, "/"), ".git")
	}
	// Otherwise resolve the host from the site config's codeRepo, if it
	// names the same repository. The webhook stores the bare
	// "group/project" path (RepoPath of a URL), so compare the config's
	// path both against loc itself and against RepoPath(loc).
	if cfg, err := s.Store.GetSiteConfig(); err == nil && cfg.CodeRepo != "" {
		cfgPath := store.RepoPath(cfg.CodeRepo)
		if cfgPath == loc || cfgPath == store.RepoPath(loc) {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(cfg.CodeRepo), "/"), ".git")
		}
	}
	// Try to at least keep an scp-style host: git@host:group/project.
	if i := strings.IndexByte(loc, '@'); i >= 0 {
		if j := strings.IndexByte(loc[i:], ':'); j > 0 {
			return "https://" + loc[i+1:i+j] + "/" + strings.TrimSuffix(loc[i+j+1:], ".git")
		}
	}
	return ""
}

// toRunCellJSON renders a recorded run as a matrix cell. status is the
// display status (runStatusForDisplay), which may differ from the stored one
// for the skipped-artifact runs.
func toRunCellJSON(run *store.TestRun, status string) *runCellJSON {
	return &runCellJSON{
		RunID:      run.ID,
		Status:     status,
		Total:      run.Total,
		Passed:     run.Passed,
		Failed:     run.Failed,
		StartedAt:  run.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt: run.FinishedAt.UTC().Format(time.RFC3339),
	}
}

// taskCellJSON renders a live task graph as a matrix cell (runId 0,
// taskId set: clickable through to the stage's task detail). kind selects
// the stage this dashboard view is about (build/unit/regression): the cell
// mirrors that sub-task's own state — a running root with a queued unit
// stage shows "pending" on the unit dashboard, a running build stage shows
// "running". Regression expands to one sub-task per case; their states are
// aggregated (failed > running > pending > skipped > done). When the graph
// has no sub-task of that kind (the graph simply does not include the
// stage — e.g. a manual dispatch with only a build command), the cell is
// nil: the stage was never requested, so the matrix shows "—" rather than
// inventing a failure. When the stage failed before any report, the
// sub-task errors hint at what broke.
func taskCellJSON(kind string, root *store.Task, subs []store.Task) *runCellJSON {
	// The sub-tasks whose kind matches this view (build/unit/regression).
	stageKind := map[string]string{
		store.RunKindBuild:      store.TaskKindBuild,
		store.RunKindUnit:       store.TaskKindUnit,
		store.RunKindRegression: store.TaskKindRegression,
	}[kind]
	cell := &runCellJSON{RunID: 0, TaskID: root.ID, Trigger: root.Trigger}
	var matched []store.Task
	for i := range subs {
		if subs[i].Kind == stageKind {
			matched = append(matched, subs[i])
		}
	}
	if len(matched) == 0 {
		return nil // the graph has no such stage: nothing to show
	}

	// Aggregate the matched sub-tasks: any failure fails, any activity runs,
	// all-queued stays pending, all-skipped surfaces as skipped.
	rank := func(status string) int {
		switch status {
		case store.TaskFailed:
			return 4
		case store.TaskRunning:
			return 3
		case store.TaskPending:
			return 2
		case store.TaskSkipped:
			return 1
		default:
			return 0 // done
		}
	}
	best := 0
	for i := range matched {
		if r := rank(matched[i].Status); r > best {
			best = r
		}
	}
	if best == 0 {
		best = 1 // every stage finished but no run landed yet
	}
	// The cell links to the first sub-task of this stage whose own state
	// decides the aggregate (a failing case, the running one, ...), so the
	// click lands on the most relevant log.
	pick := &matched[0]
	for i := range matched {
		if rank(matched[i].Status) == best {
			pick = &matched[i]
			break
		}
	}
	cell.TaskID = pick.ID
	status := pick.Status
	switch best {
	case 4:
		cell.Status = store.StatusFailed
		cell.Error = pick.Error
		if cell.Error == "" {
			// Prefer the first failed sub-task's error (clone/build failures
			// are more actionable than the root's derived status).
			for i := range subs {
				if subs[i].Status == store.TaskFailed || subs[i].Status == store.TaskSkipped {
					if subs[i].Error != "" {
						cell.Error = subs[i].Error
					}
					break
				}
			}
		}
	case 3:
		cell.Status = "running"
	case 2:
		cell.Status = "pending"
	default:
		if status == store.TaskSkipped {
			cell.Status = "skipped"
		} else {
			cell.Status = "pending" // finished, run not landed yet
		}
		cell.Error = pick.Error
	}
	if root.StartedAt != nil {
		cell.StartedAt = root.StartedAt.UTC().Format(time.RFC3339)
	}
	if root.FinishedAt != nil {
		cell.FinishedAt = root.FinishedAt.UTC().Format(time.RFC3339)
	}
	return cell
}

func toRunJSON(run *store.TestRun) runJSON {
	return runJSON{
		ID:            run.ID,
		Kind:          run.Kind,
		Status:        run.Status,
		Summary:       run.Summary,
		Description:   run.Description,
		Name:          run.Name,
		Message:       run.Message,
		Total:         run.Total,
		Passed:        run.Passed,
		Failed:        run.Failed,
		Skipped:       run.Skipped,
		TaskID:        run.TaskID,
		EnvironmentID: run.EnvironmentID,
		CommitID:      run.CommitID,
		StartedAt:     run.StartedAt.UTC().Format(time.RFC3339),
		FinishedAt:    run.FinishedAt.UTC().Format(time.RFC3339),
	}
}

func toCaseJSON(c *store.TestRun) caseJSON {
	return caseJSON{
		ID:             c.ID,
		Name:           c.Name,
		Description:    c.Description,
		Status:         c.Status,
		Message:        c.Message,
		DurationMillis: c.DurationMillis,
	}
}
