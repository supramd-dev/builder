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

// --- stage helpers (shared by both dashboard views) ---

// dashboardStageKinds are the matrix columns of one (commit, environment)
// cell, in display order. clone has no column of its own (a clone failure
// shows up as every later stage being skipped).
var dashboardStageKinds = []string{store.RunKindBuild, store.RunKindUnit, store.RunKindRegression}

// stageTaskKind maps a dashboard column (a run kind) to the kind of the task
// node that carries it. The regression column is the virtual stage container:
// its status and counts are the cases' aggregate, while each case is a node of
// its own with its own attempt and log.
func stageTaskKind(kind string) string {
	switch kind {
	case store.RunKindBuild:
		return store.TaskKindBuild
	case store.RunKindUnit:
		return store.TaskKindUnit
	case store.RunKindRegression:
		return store.TaskKindRegressionStage
	}
	return ""
}

// countKind counts the nodes of one kind.
func countKind(nodes []store.Task, kind string) int {
	n := 0
	for i := range nodes {
		if nodes[i].Kind == kind {
			n++
		}
	}
	return n
}

// stageNode picks the node of a graph that stands for one stage column. Nil
// when the graph does not define the stage: a dispatch of an entry without a
// build command has no build node, one without presets no regression
// container — the matrix shows "—" for a stage that was never requested
// rather than inventing a failure.
func stageNode(kind string, nodes []store.Task) *store.Task {
	want := stageTaskKind(kind)
	if want == "" {
		return nil
	}
	for i := range nodes {
		if nodes[i].Kind == want {
			return &nodes[i]
		}
	}
	return nil
}

// stageCell renders one matrix cell from a stage node: the node carries the
// status, counts and timestamps of the stage's latest attempt (FinishAttempt
// writes the node and its run from the same values), and the attempt's run
// gives the cell its run link. The regression container has no run of its own,
// so its cell links to the task detail instead.
func stageCell(node *store.Task, run *store.TestRun) *runCellJSON {
	cell := &runCellJSON{
		TaskID:  node.ID,
		Status:  node.Status,
		Error:   node.Error,
		Summary: node.Summary,
		Total:   node.Total,
		Passed:  node.Passed,
		Failed:  node.Failed,
		Skipped: node.Skipped,
	}
	if run != nil {
		cell.RunID = run.ID
	}
	if node.StartedAt != nil {
		cell.StartedAt = node.StartedAt.UTC().Format(time.RFC3339)
	}
	if node.FinishedAt != nil {
		cell.FinishedAt = node.FinishedAt.UTC().Format(time.RFC3339)
	}
	return cell
}

// graphNodeIDs collects the ids of every active node of a set of graphs, so
// one query can fetch their latest runs.
func graphNodeIDs(graphs map[store.EnvCommit]store.RootTaskSummary) []int64 {
	var ids []int64
	for _, g := range graphs {
		for i := range g.Subs {
			ids = append(ids, g.Subs[i].ID)
		}
	}
	return ids
}

// runOf resolves a task's latest run, or nil when it has none: a virtual node
// records no run, and a node whose attempt was never opened has none either.
func runOf(runs map[int64]store.TestRun, taskID int64) *store.TestRun {
	run, ok := runs[taskID]
	if !ok {
		return nil
	}
	return &run
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
	// Owner is the username of the account that manages the environment. The
	// matrix is site-wide — a column may belong to someone else, and dispatch
	// matches yaml entries against every enabled environment — so the column
	// has to say whose machine it is.
	Owner string `json:"owner,omitempty"`
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
	Superseded bool   `json:"superseded,omitempty"` // an older row of the same SHA exists (manual re-dispatch, or the fork policies)
	// Live reports that a task graph of this row is still unfinished. It is
	// what an older row is judged by: under the default policy the newest row
	// of a revision is the only one that runs, but the fork policies let the
	// earlier ones keep running, so the matrix must not dim work that is still
	// moving.
	Live bool `json:"live,omitempty"`
	// Why the dispatch of this commit produced no task graph — the webhook's
	// dispatchError, stored at dispatch time (fetch/parse failure, no entry
	// matching an environment, no code repo configured). Empty when a graph
	// was created. The matrix shows it on the cells that have no graph.
	DispatchError string `json:"dispatchError,omitempty"`
}

// runCellJSON is one cell of the matrix: the state of the stage's task node,
// aligned with an environment column. Null when the (commit, environment) has
// no task graph, or when the graph does not define that stage. runId is 0 for
// a node without a run of its own (the regression container); taskId is always
// set, so the cell is clickable either way.
type runCellJSON struct {
	RunID      int64  `json:"runId"`
	TaskID     int64  `json:"taskId,omitempty"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Total      int    `json:"total"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
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
	// The matrix is the task graph, not a run table: every (commit,
	// environment) that was dispatched has a root task with one node per
	// stage, and each node's state is the state of the cell.
	graphs, err := s.Store.FindRootGraphsByCommits(envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard: find root graphs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	runs, err := s.Store.LatestRunsByTaskIDs(graphNodeIDs(graphs))
	if err != nil {
		log.Printf("dashboard: latest runs: %v", err)
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
	owners := s.environmentOwnerNames(envs)
	for i := range envs {
		env := &envs[i]
		out.Environments = append(out.Environments, dashboardEnvJSON{
			ID:          env.ID,
			Name:        env.Name,
			Description: env.Description,
			Tags:        env.Tags,
			Enabled:     env.Enabled,
			Owner:       owners[env.OwnerID],
		})
	}

	// Commit rows, newest first; cells align with the environment columns.
	superseded := supersededCommits(commits)
	for i := range commits {
		commit := &commits[i]
		cj := s.toCommitJSON(commit)
		cj.Superseded = superseded[commit.ID]
		cj.Live = commitLive(graphs, commit.ID)
		row := dashboardRowJSON{
			Commit: cj,
			Cells:  make([]*runCellJSON, len(envs)),
		}
		for j := range envs {
			graph, ok := graphs[store.EnvCommit{Env: envs[j].ID, Commit: commit.ID}]
			if !ok {
				continue // never dispatched on this environment: "—"
			}
			// The cell shows THIS view's stage, not the root: while a graph is
			// mid-build its unit stage is still queued, even though the root
			// already reports running.
			node := stageNode(kind, graph.Subs)
			if node == nil {
				continue // the graph does not define this stage
			}
			cell := stageCell(node, runOf(runs, node.ID))
			cell.Trigger = graph.Root.Trigger
			row.Cells[j] = cell
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
	graphs, err := s.Store.FindRootGraphsByCommits(envIDs, commitIDs)
	if err != nil {
		log.Printf("dashboard full: find root graphs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	runs, err := s.Store.LatestRunsByTaskIDs(graphNodeIDs(graphs))
	if err != nil {
		log.Printf("dashboard full: latest runs: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	out := fullDashboardJSON{
		RepoFilter:   repoFilter,
		RepoURL:      repoURL,
		Environments: make([]dashboardEnvJSON, 0, len(envs)),
		Rows:         make([]fullRowJSON, 0, len(commits)),
	}
	owners := s.environmentOwnerNames(envs)
	for i := range envs {
		env := &envs[i]
		out.Environments = append(out.Environments, dashboardEnvJSON{
			ID:          env.ID,
			Name:        env.Name,
			Description: env.Description,
			Tags:        env.Tags,
			Enabled:     env.Enabled,
			Owner:       owners[env.OwnerID],
		})
	}

	superseded := supersededCommits(commits)
	for i := range commits {
		commit := &commits[i]
		cj := s.toCommitJSON(commit)
		cj.Superseded = superseded[commit.ID]
		cj.Live = commitLive(graphs, commit.ID)
		row := fullRowJSON{
			Commit:   cj,
			Stages:   map[int64][]fullStageJSON{},
			TaskIDs:  map[int64]int64{},
			Triggers: map[int64]int{},
		}
		for j := range envs {
			envID := envs[j].ID

			// A graph exists for this (commit, environment): expose the graph
			// link and the state of each of its stage nodes.
			graph, hasGraph := graphs[store.EnvCommit{Env: envID, Commit: commit.ID}]
			if !hasGraph {
				continue
			}
			row.TaskIDs[envID] = graph.Root.ID
			row.Triggers[envID] = graph.Root.Trigger

			for _, kind := range dashboardStageKinds {
				node := stageNode(kind, graph.Subs)
				if node == nil {
					continue // stage not part of this graph
				}
				// The regression column is the virtual container: its status
				// and counts are the cases' aggregate, and it summarizes how
				// many cases the stage has.
				cell := stageCell(node, runOf(runs, node.ID))
				st := fullStageJSON{
					Kind:       kind,
					RunID:      cell.RunID,
					TaskID:     cell.TaskID,
					Status:     cell.Status,
					Error:      cell.Error,
					Summary:    cell.Summary,
					StartedAt:  cell.StartedAt,
					FinishedAt: cell.FinishedAt,
				}
				if kind == store.RunKindRegression {
					st.Summary = fmt.Sprintf("%d cases", countKind(graph.Subs, store.TaskKindRegressionCase))
				}
				row.Stages[envID] = append(row.Stages[envID], st)
			}
		}
		out.Rows = append(out.Rows, row)
	}

	writeJSON(w, http.StatusOK, out)
}

// --- POST /api/test-runs (result reporting) ---

// runInputJSON is the request body of POST /api/test-runs: the outcome of one
// task's current attempt. The task identifies the test — the environment,
// commit and run kind are derived from it, and a task that is not one's own to
// manage is refused — and its attempt is the one the store has in flight, so a
// report for an attempt that already ended opens the next one (the retry path)
// instead of overwriting the record.
//
// Status may be left empty to derive it from the counts (failed when any case
// failed, else passed).
type runInputJSON struct {
	TaskID         int64   `json:"taskId"` // required: the task (test) the attempt belongs to
	Status         string  `json:"status"` // optional: passed | failed | skipped
	Summary        string  `json:"summary"`
	Total          int     `json:"total"`
	Passed         int     `json:"passed"`
	Failed         int     `json:"failed"`
	Skipped        int     `json:"skipped"`
	DurationMillis float64 `json:"durationMillis"`
	StartedAt      string  `json:"startedAt"`  // optional RFC3339
	FinishedAt     string  `json:"finishedAt"` // optional RFC3339
}

// runJSON is the wire representation of a stored run: one attempt of one task.
type runJSON struct {
	ID             int64   `json:"id"`
	TaskID         int64   `json:"taskId"`
	Attempt        int     `json:"attempt"`
	Kind           string  `json:"kind"`
	Status         string  `json:"status"`
	Summary        string  `json:"summary"`
	Total          int     `json:"total"`
	Passed         int     `json:"passed"`
	Failed         int     `json:"failed"`
	Skipped        int     `json:"skipped"`
	DurationMillis float64 `json:"durationMillis"`
	EnvironmentID  int64   `json:"environmentId"`
	CommitID       int64   `json:"commitId"`
	StartedAt      string  `json:"startedAt"`
	FinishedAt     string  `json:"finishedAt"`
}

// handleTestRuns routes POST /api/test-runs — record the outcome of one task's
// current attempt (the external-tester entry point; the runner reports through
// store.FinishAttempt directly).
//
// The task is what the report is about, and the report must come from someone
// entitled to that task: the owner (or an administrator) of the environment it
// runs on. Anything else would let any authenticated account overwrite any
// test's result.
func (s *Server) handleTestRuns(w http.ResponseWriter, r *http.Request, user *store.User) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var in runInputJSON
	if err := decodeJSON(r, &in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	if in.TaskID == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId is required"})
		return
	}

	task, err := s.Store.GetTask(in.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		log.Printf("test-runs: lookup task: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if task.Virtual {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "virtual tasks do not record runs; report against their children",
		})
		return
	}
	env, err := s.Store.GetEnvironment(task.EnvironmentID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "environment not found"})
			return
		}
		log.Printf("test-runs: lookup environment: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if !canManageEnvironment(user, env) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "this environment belongs to another account",
		})
		return
	}

	res := store.AttemptResult{
		Status:         in.Status,
		Summary:        in.Summary,
		Total:          in.Total,
		Passed:         in.Passed,
		Failed:         in.Failed,
		Skipped:        in.Skipped,
		DurationMillis: in.DurationMillis,
	}
	if t, err := parseOptionalTime(in.StartedAt); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "startedAt must be RFC3339"})
		return
	} else if !t.IsZero() {
		res.StartedAt = t
	}
	if t, err := parseOptionalTime(in.FinishedAt); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "finishedAt must be RFC3339"})
		return
	} else if !t.IsZero() {
		res.FinishedAt = t
	}

	run, err := s.Store.FinishAttempt(task.ID, res)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidRunStatus):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status must be passed, failed or skipped"})
		case errors.Is(err, store.ErrVirtualTask):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "virtual tasks do not record runs"})
		case errors.Is(err, store.ErrTaskNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
		default:
			log.Printf("test-runs: finish attempt: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		}
		return
	}
	writeJSON(w, http.StatusCreated, toRunJSON(run))
}

// handleTestRunItem routes GET /api/test-runs/{id} — the detail view of one
// attempt of one task, with the task's other attempts and this attempt's
// artifacts — and GET /api/test-runs/{id}/artifacts/zip, a download bundle of
// that attempt's artifacts. A run owns no other task's files: the subtree
// bundle is the task one, GET /api/tasks/{id}/artifacts/zip.
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
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, store.ErrTestRunNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "test run not found"})
			return
		}
		log.Printf("test-run get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	env, err := s.Store.GetEnvironment(run.EnvironmentID)
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
	// The task the attempt belongs to: the run page is a view of the test, its
	// latest attempt, and every attempt before it.
	task, err := s.Store.GetTask(run.TaskID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			task = nil // task deleted after the run; detail stays viewable
		} else {
			log.Printf("test-run task: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
			return
		}
	}
	attempts, err := s.Store.ListTaskRuns(run.TaskID)
	if err != nil {
		log.Printf("test-run attempts: %v", err)
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
		Attempts:  make([]runJSON, 0, len(attempts)),
		Artifacts: make([]artifactRefJSON, 0, len(artifacts)),
	}
	if task != nil {
		name, key, kind := task.Name, task.NodeKey, task.Kind
		detail.TaskName = &name
		detail.TaskKey = &key
		detail.TaskKind = &kind
		detail.RootTaskID = task.RootID
		if task.Description != "" {
			desc := task.Description
			detail.TaskDescription = &desc
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
	for i := range attempts {
		detail.Attempts = append(detail.Attempts, toRunJSON(&attempts[i]))
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

// artifactRefJSON references one stored artifact in the run detail (the
// content itself comes from GET /api/test-artifacts/{id}).
type artifactRefJSON struct {
	ID   int64  `json:"id"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	Size int    `json:"size"`
}

// runDetailJSON is GET /api/test-runs/{id}'s response: one attempt of one task,
// the task's identity, its other attempts and the attempt's artifacts. Pointer
// fields are null when the referenced record was deleted.
type runDetailJSON struct {
	runJSON
	TaskName        *string           `json:"taskName"`        // the test's name ("unit tests", "regression: smoke-a")
	TaskDescription *string           `json:"taskDescription"` // human label from md-builder.yaml, when set
	TaskKey         *string           `json:"taskKey"`         // the node key (stable across dispatches)
	TaskKind        *string           `json:"taskKind"`
	RootTaskID      int64             `json:"rootTaskId"` // root of the graph; the graph-page link
	EnvironmentName *string           `json:"environmentName"`
	CommitSHA       *string           `json:"commitSha"`
	CommitShortSHA  *string           `json:"commitShortSha"`
	CommitMessage   *string           `json:"commitMessage"`
	CommitAuthor    *string           `json:"commitAuthor"`
	CommitRepo      *string           `json:"commitRepo"`    // repository location, e.g. "group/code"
	CommitRepoURL   *string           `json:"commitRepoUrl"` // web URL of the repository, when derivable
	Attempts        []runJSON         `json:"attempts"`      // every attempt of the task, newest first
	Artifacts       []artifactRefJSON `json:"artifacts"`
}

// handleTestArtifact routes GET /api/test-artifacts/{id} — one stored
// artifact's raw content as JSON (the run detail lists references only; this
// is the fetch entry point for the browser-side results parsing and the
// future regression "analyze" view) — GET /api/test-artifacts/{id}/download,
// the same bytes as a file download, and GET /api/test-artifacts/{id}/raw,
// the same bytes again for viewing: a type the browser renders instead of
// saving, with HTML held in a sandbox (see serveArtifactInline).
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
	if sub == "raw" {
		s.serveArtifactInline(w, r, id)
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
	a, ok := s.artifactOrError(w, id, "artifact download")
	if !ok {
		return
	}
	s.streamArtifact(w, r, a, "artifact download", artifactDownloadHeaders(a))
}

// artifactDownloadHeaders is the response of an artifact nobody can render
// in place: opaque bytes, saved under its own name.
func artifactDownloadHeaders(a *store.TestArtifact) http.Header {
	return http.Header{
		"Content-Type":        []string{"application/octet-stream"},
		"Content-Disposition": []string{fmt.Sprintf("attachment; filename=%q", artifactDownloadName(a, a.ID))},
	}
}

// artifactSandboxPolicy is the policy an inline HTML artifact is rendered
// under: the scripts and their pop-ups work, the document itself gets an
// opaque origin. The list matches the run page's <iframe sandbox> attribute,
// so a page behaves the same framed in the app and opened on its own.
const artifactSandboxPolicy = "sandbox allow-scripts allow-popups allow-downloads allow-forms allow-modals"

// artifactInlineTypes maps the file extensions the preview endpoint serves
// in place to the type it serves them as. HTML is the reason the endpoint
// exists — the one format a browser renders as a page — and it is the only
// entry that gets a sandbox. The text formats are there so a click opens a
// readable tab instead of a download; anything unlisted (an archive, a
// binary, and deliberately SVG, which is script-capable too) stays a
// download, where the browser applies its own file handling.
var artifactInlineTypes = map[string]string{
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".json": "text/plain; charset=utf-8",
	".xml":  "text/plain; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".log":  "text/plain; charset=utf-8",
	".csv":  "text/plain; charset=utf-8",
	".md":   "text/plain; charset=utf-8",
	".yaml": "text/plain; charset=utf-8",
	".yml":  "text/plain; charset=utf-8",
}

// serveArtifactInline streams one artifact for viewing rather than saving:
// the same object as the download, with a type the browser renders and, for
// HTML, a sandbox around what it renders.
//
// An artifact is a build product — its content comes from the code under
// test, not from this app — so it is not trusted the way the app's own pages
// are. Rendered from this origin, a page's scripts could call the API with
// the visitor's session. `sandbox` without `allow-same-origin` puts the
// document in an opaque origin instead: its scripts still run (a Plotly HTML
// export works, CDN and all) but it reads no cookie or storage and its
// requests carry no credentials. A page that needs more than that can still
// be downloaded and opened locally.
func (s *Server) serveArtifactInline(w http.ResponseWriter, r *http.Request, id int64) {
	a, ok := s.artifactOrError(w, id, "artifact preview")
	if !ok {
		return
	}
	contentType, inline := artifactInlineTypes[strings.ToLower(path.Ext(strings.TrimSpace(a.Name)))]
	if !inline {
		s.streamArtifact(w, r, a, "artifact preview", artifactDownloadHeaders(a))
		return
	}
	headers := http.Header{
		"Content-Type":           []string{contentType},
		"Content-Disposition":    []string{fmt.Sprintf("inline; filename=%q", artifactDownloadName(a, id))},
		"X-Content-Type-Options": []string{"nosniff"},
	}
	if strings.HasPrefix(contentType, "text/html") {
		headers.Set("Content-Security-Policy", artifactSandboxPolicy)
	}
	s.streamArtifact(w, r, a, "artifact preview", headers)
}

// artifactOrError loads one artifact, answering the 404/500 itself and
// reporting whether the caller should go on.
func (s *Server) artifactOrError(w http.ResponseWriter, id int64, what string) (*store.TestArtifact, bool) {
	a, err := s.Store.GetArtifact(id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "artifact not found"})
			return nil, false
		}
		log.Printf("%s: %v", what, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	return a, true
}

// streamArtifact copies one artifact's object into the response under the
// headers its caller decided on. Content-Length is set only when the backend
// knows the size, so a backend that cannot tell still streams.
func (s *Server) streamArtifact(w http.ResponseWriter, r *http.Request, a *store.TestArtifact, what string, headers http.Header) {
	rc, size, err := s.Store.OpenArtifact(r.Context(), a)
	if err != nil {
		s.artifactReadError(w, what, a, err)
		return
	}
	defer rc.Close()

	for k, v := range headers {
		w.Header()[k] = v
	}
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	if r.Method == http.MethodGet {
		if _, err := io.Copy(w, rc); err != nil {
			// The client is usually gone by now; nothing can be
			// reported in the response, so log and stop.
			log.Printf("%s: artifact %d: stream: %v", what, a.ID, err)
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

// zipEntry is one file of an artifact bundle: the path inside the archive and
// the artifact to stream into it.
type zipEntry struct {
	name string
	art  *store.TestArtifact
}

// downloadRunArtifactsZip streams one zip of a run's own artifacts. A run with
// no artifacts gets a 404 JSON error (an empty archive would look like
// success). For a whole test — every stage and case under a task — see
// downloadTaskArtifactsZip.
func (s *Server) downloadRunArtifactsZip(w http.ResponseWriter, r *http.Request, runID int64) {
	if _, err := s.Store.GetTestRun(runID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || errors.Is(err, store.ErrTestRunNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "test run not found"})
			return
		}
		log.Printf("run artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	artifacts, err := s.Store.ListRunArtifacts(runID)
	if err != nil {
		log.Printf("run artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	entries := make([]zipEntry, 0, len(artifacts))
	for i := range artifacts {
		a := &artifacts[i]
		entries = append(entries, zipEntry{name: artifactDownloadName(a, a.ID), art: a})
	}
	s.writeArtifactsZip(w, r, fmt.Sprintf("run-%d-artifacts.zip", runID), "run artifacts zip", entries)
}

// downloadTaskArtifactsZip streams one zip of a task subtree's artifacts: the
// task's own attempt at the archive root, every descendant's under a directory
// named after it (so a regression stage downloads as one bundle of its cases).
func (s *Server) downloadTaskArtifactsZip(w http.ResponseWriter, r *http.Request, taskID int64) {
	task, err := s.Store.GetTask(taskID)
	if err != nil {
		if errors.Is(err, store.ErrTaskNotFound) || errors.Is(err, gorm.ErrRecordNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
			return
		}
		log.Printf("task artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	refs, err := s.Store.ListSubtreeArtifacts(task.ID)
	if err != nil {
		log.Printf("task artifacts zip: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	entries := make([]zipEntry, 0, len(refs))
	for i := range refs {
		ref := refs[i]
		name := artifactDownloadName(&ref.Artifact, ref.Artifact.ID)
		if ref.TaskID != task.ID {
			name = path.Join(zipDirName(ref.TaskKey, ref.TaskName), name)
		}
		entries = append(entries, zipEntry{name: name, art: &ref.Artifact})
	}
	s.writeArtifactsZip(w, r, fmt.Sprintf("task-%d-artifacts.zip", task.ID), "task artifacts zip", entries)
}

// zipDirName names a descendant task's directory inside a bundle: its node key
// when it has one (stable across dispatches), else its display name.
func zipDirName(key, name string) string {
	if strings.TrimSpace(key) != "" {
		return sanitizeZipSegment(strings.ReplaceAll(key, ":", "-"))
	}
	return sanitizeZipSegment(name)
}

// writeArtifactsZip streams the entries as a zip download named filename. An
// empty bundle is a 404, never an empty archive (which would look like
// success). A failing read before the archive header is written is still
// reportable as a status code; one that fails after streaming has begun is
// dropped and the archive left unfinished, see below.
func (s *Server) writeArtifactsZip(w http.ResponseWriter, r *http.Request, filename, what string, entries []zipEntry) {
	if len(entries) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no artifacts"})
		return
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
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	zw := zip.NewWriter(w)
	used := map[string]int{}
	for i := range entries {
		if failErr != nil {
			break
		}
		name, a := entries[i].name, entries[i].art
		// Read the object before writing the entry header: a header with no
		// bytes behind it would look like an empty file, not a failure.
		// One artifact at a time, so a large bundle is never buffered whole.
		rc, _, err := s.Store.OpenArtifact(r.Context(), a)
		if err != nil {
			if !started {
				failErr, failArt = err, a
				break
			}
			// The archive is already streaming: the only honest outcome is
			// to drop the entry and leave the zip truncated (an incomplete
			// archive is detectable; a zero-byte entry is not).
			log.Printf("%s: artifact %d: %v", what, a.ID, err)
			dropped = true
			continue
		}
		// Two artifacts with the same path (the same file name in two stage
		// directories that sanitized alike) must not overwrite each other:
		// suffix " (2)", " (3)".
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
			log.Printf("%s: artifact %d: entry: %v", what, a.ID, err)
			rc.Close()
			continue
		}
		if _, err := io.Copy(fw, rc); err != nil {
			log.Printf("%s: artifact %d: stream: %v", what, a.ID, err)
		}
		rc.Close()
	}
	if failErr != nil {
		// Nothing was written yet, so the caller still gets a proper status.
		s.artifactReadError(w, what, failArt, failErr)
		return
	}
	if dropped {
		// Deliberately leave the archive unclosed. Flushing hands over what
		// the writers have (the entries so far), and the missing central
		// directory leaves the download visibly broken — which beats a valid
		// zip quietly missing a file, the failure a partial backend outage
		// would otherwise hide behind.
		if err := zw.Flush(); err != nil {
			log.Printf("%s: flush: %v", what, err)
		}
		return
	}
	if err := zw.Close(); err != nil {
		log.Printf("%s: close: %v", what, err) // headers already sent; the client sees a truncated zip
	}
}

// sanitizeZipSegment makes a task name safe as one zip path segment:
// separators and empty/dotted segments collapse to "task".
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
		return "task"
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
		ID:            c.ID,
		SHA:           c.SHA,
		ShortSHA:      shortSHA(c.SHA),
		Repo:          c.Repo,
		RepoURL:       s.repoWebURL(c.Repo),
		Ref:           c.Ref,
		Author:        c.Author,
		Message:       c.Message,
		Event:         c.Event,
		PushedAt:      c.PushedAt.UTC().Format(time.RFC3339),
		DispatchError: c.DispatchError,
	}
}

// commitLive reports whether any task graph of a commit row is still
// unfinished: its root task is not terminal, so something in it may still
// change. A row with no graph at all (never dispatched, or the dispatch
// failed) is not live.
func commitLive(graphs map[store.EnvCommit]store.RootTaskSummary, commitID int64) bool {
	for key, graph := range graphs {
		if key.Commit != commitID || graph.Root == nil {
			continue
		}
		if !store.TaskStatusTerminal(graph.Root.Status) {
			return true
		}
	}
	return false
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

func toRunJSON(run *store.TestRun) runJSON {
	out := runJSON{
		ID:             run.ID,
		TaskID:         run.TaskID,
		Attempt:        run.Attempt,
		Kind:           run.Kind,
		Status:         run.Status,
		Summary:        run.Summary,
		Total:          run.Total,
		Passed:         run.Passed,
		Failed:         run.Failed,
		Skipped:        run.Skipped,
		DurationMillis: run.DurationMillis,
		EnvironmentID:  run.EnvironmentID,
		CommitID:       run.CommitID,
	}
	if !run.StartedAt.IsZero() {
		out.StartedAt = run.StartedAt.UTC().Format(time.RFC3339)
	}
	if !run.FinishedAt.IsZero() {
		out.FinishedAt = run.FinishedAt.UTC().Format(time.RFC3339)
	}
	return out
}
