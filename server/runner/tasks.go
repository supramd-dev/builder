package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"md-builder/server/store"
)

// Task creation: reading the md-builder.yaml matrix at a commit and turning
// each matched entry into a task graph (root + sub-tasks). Shared by the
// GitLab webhook and the manual POST /api/jobs trigger.

// DispatchResult summarizes a dispatch run.
type DispatchResult struct {
	TasksCreated   int  // number of task graphs (roots) created/requeued
	SubtasksTotal  int  // total sub-task nodes across graphs
	EntriesSkipped int  // entries with no matching environment
	CommitCreated  bool // the commit row was newly inserted (false = deduplicated)
	Err            error
}

// ManualDispatch is a user-submitted test request: one repository (site
// config default when empty), an optional ref (HEAD when empty) and the
// stage commands (an empty stage is skipped), to run on the given
// environments.
type ManualDispatch struct {
	Repo                string
	Ref                 string
	BuildCommand        string
	UnitCommand         string
	UnitArtifacts       ArtifactPaths // optional artifact paths for the unit command
	RegressionCommand   string
	RegressionArtifacts ArtifactPaths // optional artifact paths for the regression command
	EnvironmentIDs      []int64
	Username            string
}

// DispatchForCommit creates or requeues the task graphs for the given
// commit: the md-builder.yaml is fetched at that commit of the configured
// code repository, entries are matched to enabled environments by tags and
// one graph is created per entry. When the fetch or parse fails
// (unreachable repo, bad YAML), the commit stays recorded but no tasks are
// created; the error is surfaced to the caller.
func (s *Service) DispatchForCommit(commit *store.Commit) DispatchResult {
	return s.dispatchYAML(commit, store.TaskTriggerWebhook)
}

// DispatchForRef is the manual yaml-matrix trigger ("run the webhook flow
// on demand"): the ref (branch, tag, short/full SHA; empty = HEAD) is
// resolved against the site-configured code repository, recorded as a
// commit row (deduplicated like a webhook push), and the yaml matrix at
// that commit is dispatched exactly as the webhook would. Graphs are keyed
// by (commit, environment): re-triggering the same ref requeues the same
// graphs with fresh snapshots, so yaml/environment changes are picked up.
func (s *Service) DispatchForRef(ctx context.Context, ref string) (store.Commit, DispatchResult) {
	var commit store.Commit
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		return commit, DispatchResult{Err: err}
	}
	repo := strings.TrimSpace(cfg.CodeRepo)
	if repo == "" {
		return commit, DispatchResult{Err: errors.New("site config has no code repository set")}
	}

	creds := &GitCredentials{AccessToken: cfg.AccessToken}
	sha, err := s.resolveRef(ctx, repo, ref, creds)
	if err != nil {
		return commit, DispatchResult{Err: err}
	}

	commit = store.Commit{
		Repo:     store.RepoPath(repo),
		SHA:      sha,
		Ref:      strings.TrimSpace(ref),
		Author:   "manual",
		Message:  "manual yaml dispatch " + time.Now().UTC().Format("2006-01-02 15:04"),
		PushedAt: time.Now(),
	}
	created, err := s.Store.GetOrCreateCommit(&commit)
	if err != nil {
		return commit, DispatchResult{Err: err}
	}
	res := s.dispatchYAML(&commit, store.TaskTriggerManualYAML)
	res.CommitCreated = created
	return commit, res
}

// dispatchYAML is the shared yaml-matrix dispatch: fetch md-builder.yaml at
// the commit, parse it, match entries to enabled environments and create
// one graph per entry, with the given trigger source.
func (s *Service) dispatchYAML(commit *store.Commit, trigger int) DispatchResult {
	res := DispatchResult{}

	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		res.Err = err
		return res
	}
	if cfg.CodeRepo == "" {
		res.Err = errors.New("site config has no code repository set")
		return res
	}

	creds := &GitCredentials{AccessToken: cfg.AccessToken}
	yamlBytes, err := s.FetchYAML(cfg.CodeRepo, commit.SHA, creds)
	if err != nil {
		res.Err = err
		return res
	}
	entries, err := ParseConfig(yamlBytes)
	if err != nil {
		res.Err = err
		return res
	}

	envs, err := s.Store.ListEnabledEnvironments()
	if err != nil {
		res.Err = err
		return res
	}

	for i := range entries {
		entry := entries[i]
		env := PickEnvironment(entry, envs)
		if env == nil {
			res.EntriesSkipped++
			continue
		}
		if err := s.createGraph(commit, &entry, env, cfg, trigger); err != nil {
			res.Err = err
			return res
		}
		res.TasksCreated++
	}
	return res
}

// DispatchManual creates one task graph per requested environment for a
// user-submitted test: the repository is resolved to a commit (recorded in
// the commits table like a webhook push) and the manual stage commands are
// stored as the entry snapshot. Roots are marked TaskTriggerManual. The
// returned roots are ordered like the requested environments.
func (s *Service) DispatchManual(in ManualDispatch) ([]*store.Task, error) {
	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		return nil, err
	}
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(cfg.CodeRepo)
	}
	if repo == "" {
		return nil, errors.New("no repository given and the site config has no code repository set")
	}
	if len(in.EnvironmentIDs) == 0 {
		return nil, errors.New("no environment selected")
	}

	creds := &GitCredentials{AccessToken: cfg.AccessToken}
	sha, err := s.resolveRef(context.Background(), repo, in.Ref, creds)
	if err != nil {
		return nil, err
	}

	// Record a fresh commit row per dispatch (unlike webhook pushes, which
	// deduplicate on (repo, sha)): each manual trigger gets its own matrix
	// row, so re-running the same SHA shows each attempt. The message
	// carries the time so same-SHA rows are distinguishable.
	now := time.Now()
	commit := &store.Commit{
		Repo:     store.RepoPath(repo),
		SHA:      sha,
		Ref:      strings.TrimSpace(in.Ref),
		Author:   in.Username,
		Message:  "manual test " + now.UTC().Format("2006-01-02 15:04"),
		PushedAt: now,
	}
	if err := s.Store.CreateCommit(commit); err != nil {
		return nil, err
	}

	roots := make([]*store.Task, 0, len(in.EnvironmentIDs))
	for _, envID := range in.EnvironmentIDs {
		env, err := s.Store.GetEnvironmentAny(envID)
		if err != nil {
			return roots, fmt.Errorf("environment %d: %w", envID, err)
		}
		if !env.Enabled {
			return roots, fmt.Errorf("environment %s is disabled", env.Name)
		}
		root, err := s.createManualGraph(commit, env, cfg, in)
		if err != nil {
			return roots, err
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// createManualGraph builds and persists one environment's graph from the
// manual stage commands: build, then the requested test stages — an empty
// stage command means that stage is skipped (the graph omits its node).
// The manual regression command becomes a single case named "regression".
// An existing root for the (commit, environment) pair is redeployed,
// mirroring the webhook requeue path.
func (s *Service) createManualGraph(commit *store.Commit, env *store.TestEnvironment, cfg *store.SiteConfig, in ManualDispatch) (*store.Task, error) {
	entry := MergedEntry{
		Tags:    env.TagList(),
		Timeout: DefaultTimeoutSeconds,
		Build: BuildConfig{
			Command: strings.TrimSpace(in.BuildCommand),
		},
	}
	if cmd := strings.TrimSpace(in.UnitCommand); cmd != "" {
		entry.Unit = &EnvConfig{
			Command:   CommandList{cmd},
			Timeout:   DefaultTimeoutSeconds,
			Artifacts: in.UnitArtifacts.Clean(),
		}
	}
	if cmd := strings.TrimSpace(in.RegressionCommand); cmd != "" {
		entry.Regression = []RegressionCase{{
			Name:      "regression",
			Command:   CommandList{cmd},
			Timeout:   DefaultTimeoutSeconds,
			Artifacts: in.RegressionArtifacts.Clean(),
		}}
	}

	graph, err := BuildTaskGraph(&entry)
	if err != nil {
		return nil, err
	}
	entryJSON, err := json.Marshal(RootConfig{Entry: entry})
	if err != nil {
		return nil, err
	}

	root, err := s.Store.FindRootTaskByCommitEnv(commit.ID, env.ID)
	if err != nil && err != store.ErrTaskNotFound {
		return nil, err
	}
	if err == nil {
		// Requeue: refresh the snapshot, drop the old graph, reset state.
		root.Config = string(entryJSON)
		root.Tags = SortedTagString(entry.Tags)
		if err := s.Store.RedeployRootTask(root); err != nil {
			return nil, err
		}
		if err := s.Store.UpdateTaskConfig(root.ID, root.Config, root.Tags); err != nil {
			return nil, err
		}
		if err := s.dropStaleStageRuns(root, graph); err != nil {
			return nil, err
		}
		// The regression run aggregates case rows of the OLD dispatch; the
		// rebuilt graph re-records its own cases, so clear the stale ones.
		if err := s.Store.ResetRegressionRun(root.EnvironmentID, root.CommitID); err != nil {
			return nil, err
		}
		return root, s.createSubTasks(root, graph)
	}

	root = &store.Task{
		Kind:          store.TaskKindRoot,
		Name:          "test " + shortSHA(commit.SHA),
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          SortedTagString(entry.Tags),
		Trigger:       store.TaskTriggerManual,
		Config:        string(entryJSON),
	}
	if err := s.Store.CreateTask(root); err != nil {
		return nil, err
	}
	return root, s.createSubTasks(root, graph)
}

// dropStaleStageRuns removes the recorded test runs of stages the rebuilt
// graph no longer contains (e.g. a re-dispatch without the unit stage):
// without this, the matrix would keep showing the dropped stage's stale
// result forever.
func (s *Service) dropStaleStageRuns(root *store.Task, graph []GraphTask) error {
	present := map[string]bool{}
	for i := range graph {
		switch graph[i].Kind {
		case store.TaskKindBuild, store.TaskKindUnit, store.TaskKindRegression:
			present[graph[i].Kind] = true
		}
	}
	for _, kind := range []string{store.TaskKindBuild, store.TaskKindUnit, store.TaskKindRegression} {
		if present[kind] {
			continue
		}
		if err := s.Store.DeleteTestRun(root.EnvironmentID, root.CommitID, kind); err != nil {
			return fmt.Errorf("drop stale %s run: %w", kind, err)
		}
	}
	return nil
}

// createGraph persists one entry's task graph (root + sub-tasks) for the
// (commit, environment) pair with the given trigger source, requeueing an
// existing root (delete old sub-tasks, rebuild from the fresh snapshot).
func (s *Service) createGraph(commit *store.Commit, entry *MergedEntry, env *store.TestEnvironment, cfg *store.SiteConfig, trigger int) error {
	graph, err := BuildTaskGraph(entry)
	if err != nil {
		return err
	}

	entryJSON, err := json.Marshal(RootConfig{Entry: *entry})
	if err != nil {
		return err
	}

	root, err := s.Store.FindRootTaskByCommitEnv(commit.ID, env.ID)
	if err != nil && err != store.ErrTaskNotFound {
		return err
	}
	if err == nil {
		// Requeue: refresh the snapshot, drop the old graph, reset state.
		root.Config = string(entryJSON)
		root.Tags = SortedTagString(entry.Tags)
		if err := s.Store.RedeployRootTask(root); err != nil {
			return err
		}
		if err := s.Store.UpdateTaskConfig(root.ID, root.Config, root.Tags); err != nil {
			return err
		}
		if err := s.dropStaleStageRuns(root, graph); err != nil {
			return err
		}
		// The regression run aggregates case rows of the OLD dispatch; the
		// rebuilt graph re-records its own cases, so clear the stale ones.
		if err := s.Store.ResetRegressionRun(root.EnvironmentID, root.CommitID); err != nil {
			return err
		}
		return s.createSubTasks(root, graph)
	}

	root = &store.Task{
		Kind:          store.TaskKindRoot,
		Name:          "test " + shortSHA(commit.SHA),
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          SortedTagString(entry.Tags),
		Trigger:       trigger,
		Config:        string(entryJSON),
	}
	// A concurrent dispatch may have created the root meanwhile; treat a
	// unique-style failure by requeueing instead.
	if err := s.Store.CreateTask(root); err != nil {
		existing, ferr := s.Store.FindRootTaskByCommitEnv(commit.ID, env.ID)
		if ferr != nil {
			return err
		}
		root = existing
		root.Config = string(entryJSON)
		root.Tags = SortedTagString(entry.Tags)
		if err := s.Store.RedeployRootTask(root); err != nil {
			return err
		}
		if err := s.Store.UpdateTaskConfig(root.ID, root.Config, root.Tags); err != nil {
			return err
		}
		if err := s.Store.ResetRegressionRun(root.EnvironmentID, root.CommitID); err != nil {
			return err
		}
	}
	return s.createSubTasks(root, graph)
}

// createSubTasks materializes the graph nodes under the root and seeds a
// placeholder run per test stage so the dashboard cell and the run detail
// page exist from dispatch time on (status pending/running, following the
// stage live — the real outcome later replaces the placeholder).
func (s *Service) createSubTasks(root *store.Task, graph []GraphTask) error {
	subs := make([]*store.Task, len(graph))
	deps := make([][]int64, len(graph))
	for i, g := range graph {
		subs[i] = &store.Task{
			Kind:          g.Kind,
			Name:          g.Name,
			CommitID:      root.CommitID,
			EnvironmentID: root.EnvironmentID,
			Config:        g.Config,
		}
		deps[i] = g.Deps
	}
	stored, err := store.CreateTaskGraph(s.Store, root, subs, deps)
	if err != nil {
		return err
	}
	return s.seedStageRuns(root, stored[1:])
}

// seedStageRuns writes the dispatch-time placeholder runs for the graph's
// test stages: build/unit get one pending top-level run; regression gets a
// pending parent with one pending child run per case (the detail page lists
// every case from the start). TaskID links each run to the FIRST sub-task of
// its kind; the stage overwrites it (the case runs update their own child)
// when it executes.
func (s *Service) seedStageRuns(root *store.Task, subs []*store.Task) error {
	// Group by run kind: regression expands to several sub-tasks (one per
	// case) that all aggregate under ONE parent run.
	taskID := map[string]int64{}
	cases := map[string][]store.CaseInput{}
	for _, sub := range subs {
		var kind string
		switch sub.Kind {
		case store.TaskKindBuild:
			kind = store.RunKindBuild
		case store.TaskKindUnit:
			kind = store.RunKindUnit
		case store.TaskKindRegression:
			kind = store.RunKindRegression
			var stage CaseStageConfig
			if err := json.Unmarshal([]byte(sub.Config), &stage); err != nil {
				return fmt.Errorf("seed regression run: %w", err)
			}
			cases[kind] = append(cases[kind], store.CaseInput{Name: stage.Case})
		default:
			continue // clone has no run
		}
		if _, ok := taskID[kind]; !ok {
			taskID[kind] = sub.ID // first sub-task of the kind
		}
	}
	for kind, id := range taskID {
		_, err := s.Store.UpsertPlaceholderRun(&store.RunInput{
			EnvironmentID: root.EnvironmentID,
			CommitID:      root.CommitID,
			Kind:          kind,
			TaskID:        id,
			Status:        store.StatusPending,
			Cases:         cases[kind],
		})
		if err != nil {
			return fmt.Errorf("seed %s run: %w", kind, err)
		}
	}
	return nil
}

// shortSHA abbreviates a commit id for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
