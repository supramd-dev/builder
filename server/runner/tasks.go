package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
// created; the error is surfaced to the caller and stored on the commit row
// (see recordDispatchOutcome).
//
// The work runs on its own context, not the request's: the caller here is a
// GitLab webhook, and GitLab gives up on the HTTP response after ten seconds.
// A dispatch that outlives that must still finish — the commit's columns would
// otherwise stay empty for a run that did nothing wrong. Only the fetch is
// bounded (Service.FetchTimeout).
func (s *Service) DispatchForCommit(commit *store.Commit) DispatchResult {
	return s.dispatchYAML(context.Background(), commit, store.TaskTriggerWebhook)
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
	sha, err := s.resolveRefBounded(ctx, repo, ref, creds)
	if err != nil {
		return commit, DispatchResult{Err: err}
	}

	commit = store.Commit{
		Repo:     store.RepoPath(repo),
		SHA:      sha,
		Ref:      strings.TrimSpace(ref),
		Author:   "manual",
		Message:  "manual yaml dispatch " + time.Now().UTC().Format("2006-01-02 15:04"),
		Event:    store.CommitEventManualYAML,
		PushedAt: time.Now(),
	}
	created, err := s.Store.GetOrCreateCommit(&commit)
	if err != nil {
		return commit, DispatchResult{Err: err}
	}
	res := s.dispatchYAML(ctx, &commit, store.TaskTriggerManualYAML)
	res.CommitCreated = created
	return commit, res
}

// dispatchYAML is the shared yaml-matrix dispatch: fetch md-builder.yaml at
// the commit, parse it, match entries to enabled environments and create
// one graph per entry, with the given trigger source.
func (s *Service) dispatchYAML(ctx context.Context, commit *store.Commit, trigger int) DispatchResult {
	res := DispatchResult{}
	// Whatever the outcome, it lands on the commit row: the dashboard has no
	// other way to say why a commit's columns are empty, and a re-dispatch
	// that succeeds must clear the message a previous attempt left.
	defer s.recordDispatchOutcome(commit, &res)

	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		res.Err = err
		return res
	}
	if cfg.CodeRepo == "" {
		res.Err = errors.New("site config has no code repository set")
		return res
	}

	// The fetch is the only network call in this path. Bound it so an
	// unreachable repository server fails with a recorded reason instead of
	// holding the dispatching goroutine indefinitely; a caller that already
	// has an earlier deadline keeps it.
	ctx, cancel := context.WithTimeout(ctx, s.fetchTimeout())
	defer cancel()

	creds := &GitCredentials{AccessToken: cfg.AccessToken}
	yamlBytes, err := s.FetchYAML(ctx, cfg.CodeRepo, commit.SHA, creds)
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
		if err := s.createGraph(commit, &entry, env, trigger); err != nil {
			res.Err = err
			return res
		}
		res.TasksCreated++
	}
	return res
}

// recordDispatchOutcome stores the dispatch result on the commit row: the
// error when one occurred, the reason when the yaml matched nothing, and an
// empty message when graphs were created (clearing an earlier attempt's).
// Storing is bookkeeping — a failure to store is logged, not returned: the
// dispatch result already carries the real error, and losing the note must
// not turn into a second failure.
func (s *Service) recordDispatchOutcome(commit *store.Commit, res *DispatchResult) {
	if commit == nil || commit.ID == 0 {
		return
	}
	msg := ""
	switch {
	case res.Err != nil:
		msg = res.Err.Error()
	case res.TasksCreated == 0 && res.EntriesSkipped > 0:
		// Not a failure — the yaml is fine — but it leaves the same empty
		// cells, and "no entry matched" is the answer to "why?".
		msg = fmt.Sprintf("md-builder.yaml: no entry matched an enabled environment "+
			"(all %d %s skipped)", res.EntriesSkipped, plural(res.EntriesSkipped, "entry", "entries"))
	}
	if err := s.Store.SetCommitDispatchError(commit.ID, msg); err != nil {
		log.Printf("runner: commit %d: record dispatch outcome: %v", commit.ID, err)
		return
	}
	commit.DispatchError = msg
}

// plural picks the singular or plural form of a word for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// DispatchManual creates one task graph per requested environment for a
// user-submitted test: the repository is resolved to a commit (recorded in
// the commits table like a webhook push) and the manual stage commands are
// stored as the entry snapshot. Roots are marked TaskTriggerManual. The
// returned roots are ordered like the requested environments.
//
// A failure part-way through (environment 3 of 4 disabled, a graph that
// cannot be built) leaves the graphs already created and is recorded on the
// commit row, like the yaml path's errors: the environments that got no
// graph are exactly the ones the matrix cannot otherwise explain.
func (s *Service) DispatchManual(in ManualDispatch) (roots []*store.Task, err error) {
	// The commit is nil until one is recorded, so the failures before that
	// (no repository, unresolvable ref) leave nothing to annotate.
	var commit *store.Commit
	defer func() {
		s.recordDispatchOutcome(commit, &DispatchResult{Err: err, TasksCreated: len(roots)})
	}()

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

	// The caller chooses repo; the site's token is only attached when that
	// repository is on the configured host (see CredsForRepo). The lookup is
	// bounded like the matrix read: the repository is the caller's, and this
	// runs on a request handler's goroutine.
	creds := CredsForRepo(cfg.CodeRepo, repo, cfg.AccessToken)
	sha, err := s.resolveRefBounded(context.Background(), repo, in.Ref, creds)
	if err != nil {
		return nil, err
	}

	// Record a fresh commit row per dispatch (unlike webhook pushes, which
	// deduplicate on (repo, sha)): each manual trigger gets its own matrix
	// row, so re-running the same SHA shows each attempt. The message
	// carries the time so same-SHA rows are distinguishable.
	now := time.Now()
	commit = &store.Commit{
		Repo:     store.RepoPath(repo),
		SHA:      sha,
		Ref:      strings.TrimSpace(in.Ref),
		Author:   in.Username,
		Message:  "manual test " + now.UTC().Format("2006-01-02 15:04"),
		Event:    store.CommitEventManual,
		PushedAt: now,
	}
	if err := s.Store.CreateCommit(commit); err != nil {
		return nil, err
	}

	roots = make([]*store.Task, 0, len(in.EnvironmentIDs))
	for _, envID := range in.EnvironmentIDs {
		env, err := s.Store.GetEnvironment(envID)
		if err != nil {
			return roots, fmt.Errorf("environment %d: %w", envID, err)
		}
		if !env.Enabled {
			return roots, fmt.Errorf("environment %s is disabled", env.Name)
		}
		root, err := s.createManualGraph(commit, env, in)
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
// An existing root for the (commit, environment) pair is re-dispatched,
// mirroring the webhook requeue path.
func (s *Service) createManualGraph(commit *store.Commit, env *store.TestEnvironment, in ManualDispatch) (*store.Task, error) {
	entry := MergedEntry{
		Tags:    env.TagList(),
		Timeout: DefaultTimeoutSeconds,
		Build: BuildConfig{
			Command: CommandList{strings.TrimSpace(in.BuildCommand)},
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

	return s.persistGraph(commit, &entry, env, store.TaskTriggerManual)
}

// createGraph persists one yaml entry's task graph (root + nodes) for the
// (commit, environment) pair with the given trigger source.
func (s *Service) createGraph(commit *store.Commit, entry *MergedEntry, env *store.TestEnvironment, trigger int) error {
	_, err := s.persistGraph(commit, entry, env, trigger)
	return err
}

// persistGraph turns one merged entry into the task graph of one (commit,
// environment) pair and persists it.
//
// A re-dispatch reuses the pair's root and matches nodes by node key: the
// stages and cases the entry still defines are refreshed and re-armed (a new
// attempt each), the ones it no longer defines are retired with their runs,
// logs and artifacts kept as history (store.UpsertTaskGraph). Nothing is
// deleted, and every real node comes out of it with the attempt's pending
// run, so the matrix cell and the run page exist from dispatch time on.
func (s *Service) persistGraph(commit *store.Commit, entry *MergedEntry, env *store.TestEnvironment, trigger int) (*store.Task, error) {
	graph, err := BuildTaskGraph(entry)
	if err != nil {
		return nil, err
	}
	entryJSON, err := json.Marshal(RootConfig{Entry: *entry})
	if err != nil {
		return nil, err
	}

	root := &store.Task{
		Kind:          store.TaskKindRoot,
		NodeKey:       store.TaskKindRoot,
		Name:          "test " + shortSHA(commit.SHA),
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          SortedTagString(entry.Tags),
		Trigger:       trigger,
		Config:        string(entryJSON),
	}
	nodes := make([]store.TaskNode, len(graph))
	for i := range graph {
		g := graph[i]
		nodes[i] = store.TaskNode{
			Task: &store.Task{
				Kind:        g.Kind,
				NodeKey:     g.NodeKey,
				Name:        g.Name,
				Description: g.Description,
				Config:      g.Config,
			},
			Deps:      g.Deps,
			ParentKey: g.ParentKey,
		}
	}
	if _, err := s.Store.UpsertTaskGraph(root, nodes); err != nil {
		return nil, err
	}
	return root, nil
}

// shortSHA abbreviates a commit id for display.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
