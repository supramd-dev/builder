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
	TasksCreated   int // number of task graphs (roots) created/requeued
	SubtasksTotal  int // total sub-task nodes across graphs
	EntriesSkipped int
	Err            error
}

// ManualDispatch is a user-submitted test request: one repository (site
// config default when empty), an optional ref (HEAD when empty) and the
// stage commands (an empty stage is skipped), to run on the given
// environments.
type ManualDispatch struct {
	Repo              string
	Ref               string
	BuildCommand      string
	UnitCommand       string
	RegressionCommand string
	EnvironmentIDs    []int64
	Username          string
}

// DispatchForCommit creates or requeues the task graphs for the given
// commit: the md-builder.yaml is fetched at that commit of the configured
// code repository, entries are matched to enabled environments by tags and
// one graph is created per entry. When the fetch or parse fails
// (unreachable repo, bad YAML), the commit stays recorded but no tasks are
// created; the error is surfaced to the caller.
func (s *Service) DispatchForCommit(commit *store.Commit) DispatchResult {
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

	creds := &GitCredentials{
		DeployKey:       cfg.DeployKey,
		DeployToken:     cfg.DeployToken,
		DeployTokenUser: cfg.DeployTokenUser,
	}
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
		if err := s.createGraph(commit, &entry, env, cfg); err != nil {
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

	creds := &GitCredentials{
		DeployKey:       cfg.DeployKey,
		DeployToken:     cfg.DeployToken,
		DeployTokenUser: cfg.DeployTokenUser,
	}
	sha, err := s.resolveRef(context.Background(), repo, in.Ref, creds)
	if err != nil {
		return nil, err
	}

	// Record the commit like a webhook push so the dashboard lists it.
	commit := &store.Commit{
		Repo:     store.RepoPath(repo),
		SHA:      sha,
		Ref:      strings.TrimSpace(in.Ref),
		Author:   in.Username,
		Message:  "manual test",
		PushedAt: time.Now(),
	}
	if _, err := s.Store.GetOrCreateCommit(commit); err != nil {
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
// manual stage commands: build (default cmake recipe when empty), then the
// requested test stages. An existing root for the (commit, environment)
// pair is redeployed, mirroring the webhook requeue path.
func (s *Service) createManualGraph(commit *store.Commit, env *store.TestEnvironment, cfg *store.SiteConfig, in ManualDispatch) (*store.Task, error) {
	entry := MergedEntry{
		Tags:    env.TagList(),
		Timeout: DefaultTimeoutSeconds,
		Build: BuildConfig{
			Generator: GeneratorScript,
			Command:   strings.TrimSpace(in.BuildCommand),
		},
	}
	if entry.Build.Command == "" {
		entry.Build.Command = "cmake . && cmake --build . -j8"
	}
	if cmd := strings.TrimSpace(in.UnitCommand); cmd != "" {
		entry.Unit = &EnvConfig{Command: cmd, Timeout: DefaultTimeoutSeconds}
	}
	if cmd := strings.TrimSpace(in.RegressionCommand); cmd != "" {
		entry.Regression = &EnvConfig{Command: cmd, Timeout: DefaultTimeoutSeconds}
	}

	graph, err := BuildTaskGraph(&entry)
	if err != nil {
		return nil, err
	}
	entryJSON, err := json.Marshal(RootConfig{
		Entry:        entry,
		TestInputRef: cfg.TestRepoRef,
	})
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

// createGraph persists one entry's task graph (root + sub-tasks) for the
// (commit, environment) pair, requeueing an existing root (delete old
// sub-tasks, rebuild from the fresh snapshot).
func (s *Service) createGraph(commit *store.Commit, entry *MergedEntry, env *store.TestEnvironment, cfg *store.SiteConfig) error {
	graph, err := BuildTaskGraph(entry)
	if err != nil {
		return err
	}

	entryJSON, err := json.Marshal(RootConfig{
		Entry:        *entry,
		TestInputRef: cfg.TestRepoRef,
	})
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
		return s.createSubTasks(root, graph)
	}

	root = &store.Task{
		Kind:          store.TaskKindRoot,
		Name:          "test " + shortSHA(commit.SHA),
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          SortedTagString(entry.Tags),
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
	}
	return s.createSubTasks(root, graph)
}

// createSubTasks materializes the graph nodes under the root.
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
	_, err := store.CreateTaskGraph(s.Store, root, subs, deps)
	return err
}

// shortSHA abbreviates a commit id for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
