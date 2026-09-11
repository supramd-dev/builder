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
	Tags        string `json:"tags"`
	Enabled     bool   `json:"enabled"`
}

// commitJSON is a commit row of the dashboard matrix.
type commitJSON struct {
	ID       int64  `json:"id"`
	SHA      string `json:"sha"`
	ShortSHA string `json:"shortSha"`
	Repo     string `json:"repo"`
	RepoURL  string `json:"repoUrl,omitempty"` // web URL of the repository, when derivable
	Ref      string `json:"ref"`
	Author   string `json:"author"`
	Message  string `json:"message"`
	PushedAt string `json:"pushedAt"`
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
	jobs, err := s.Store.FindRootTasksByCommits(envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard: find root tasks: %v", err)
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
			Tags:        env.Tags,
			Enabled:     env.Enabled,
		})
	}

	// Commit rows, newest first; cells align with the environment columns.
	for i := range commits {
		commit := &commits[i]
		row := dashboardRowJSON{
			Commit: s.toCommitJSON(commit),
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

	repoFilter := ""
	if cfg, err := s.Store.GetSiteConfig(); err == nil && cfg.CodeRepo != "" {
		repoFilter = store.RepoPath(cfg.CodeRepo)
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

	for i := range commits {
		commit := &commits[i]
		row := fullRowJSON{
			Commit:   s.toCommitJSON(commit),
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
					if graph.Subs[i].Kind == taskKind {
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
						row.Stages[envID] = append(row.Stages[envID], st)
						break
					}
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

// caseInput is one test case in a report.
type caseInput struct {
	Name       string  `json:"name"`
	Status     string  `json:"status"`     // "passed" or "failed"
	ErrorValue float64 `json:"errorValue"` // regression error metric
	Message    string  `json:"message"`    // short note / failure reason
}

// runInputJSON is the request body of POST /api/test-runs. When cases are
// present the run status is derived from them; without cases the explicit
// status/summary are stored directly (the build runs' simplified path).
type runInputJSON struct {
	EnvironmentID int64       `json:"environmentId"`
	CommitID      int64       `json:"commitId"`
	CommitSHA     string      `json:"commitSha"` // alternative to commitId: repo+sha lookup
	CommitRepo    string      `json:"commitRepo"`
	Kind          string      `json:"kind"`    // "regression", "unit" or "build"
	Status        string      `json:"status"`  // used only when cases is empty
	Summary       string      `json:"summary"` // used only when cases is empty
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
		Status:        in.Status,
		Summary:       in.Summary,
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
		PushedAt: c.PushedAt.UTC().Format(time.RFC3339),
	}
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
// "running". The root's state and id are only a fallback for graphs where
// the stage sub-task is missing. When the stage failed before any report,
// the sub-task errors hint at what broke.
func taskCellJSON(kind string, root *store.Task, subs []store.Task) *runCellJSON {
	cell := &runCellJSON{RunID: 0, TaskID: root.ID, Trigger: root.Trigger}
	// The sub-task whose kind matches this view (build/unit/regression).
	stageKind := map[string]string{
		store.RunKindBuild:      store.TaskKindBuild,
		store.RunKindUnit:       store.TaskKindUnit,
		store.RunKindRegression: store.TaskKindRegression,
	}[kind]
	status := root.Status
	errMsg := root.Error
	for i := range subs {
		if subs[i].Kind != stageKind {
			continue
		}
		// The stage exists: mirror its own state (pending stays pending,
		// running stays running, failed stays failed). The root aggregate
		// must not bleed into a per-stage cell. The cell links to the
		// stage sub-task itself (its detail page shows the log).
		cell.TaskID = subs[i].ID
		status = subs[i].Status
		errMsg = subs[i].Error
		break
	}
	switch status {
	case store.TaskPending:
		cell.Status = "pending"
	case store.TaskRunning:
		cell.Status = "running"
	case store.TaskSkipped:
		// The stage never ran: an upstream task failed (the runner's
		// skip-dependents path). Displayed like a run-level skipped.
		cell.Status = "skipped"
		cell.Error = errMsg
	default: // stage/root failed (or finished without a report)
		cell.Status = store.StatusFailed
		cell.Error = errMsg
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
