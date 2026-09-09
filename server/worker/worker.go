// Package worker runs the job scheduling loop: claim pending jobs and execute
// them via the runner, with N concurrent workers.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"md-builder/server/runner"
	"md-builder/server/store"
)

// defaults.
const (
	defaultWorkers = 2
	pollInterval   = 2 * time.Second
)

// Dispatcher creates jobs for a commit: it fetches the md-builder.yaml at
// that commit, matches matrix entries to enabled environments (by tags) and
// requeues/creates one job per entry. It is shared by the webhook handler
// and the manual POST /api/jobs trigger.
type Dispatcher struct {
	Store     *store.Store
	FetchYAML runner.YAMLFetcher
}

// DispatchResult summarizes a dispatch run.
type DispatchResult struct {
	JobsCreated    int
	EntriesSkipped int
	Err            error
}

// DispatchForCommit creates or requeues jobs for the given commit. The
// md-builder.yaml is fetched at that commit of the configured code
// repository (site config). When the fetch or parse fails (unreachable repo,
// bad YAML), the commit stays recorded but no jobs are created; the error is
// surfaced to the caller.
func (d *Dispatcher) DispatchForCommit(commit *store.Commit) DispatchResult {
	res := DispatchResult{}

	cfg, err := d.Store.GetSiteConfig()
	if err != nil {
		res.Err = err
		return res
	}
	if cfg.CodeRepo == "" {
		res.Err = errors.New("site config has no code repository set")
		return res
	}

	yamlBytes, err := d.FetchYAML(cfg.CodeRepo, commit.SHA)
	if err != nil {
		res.Err = err
		return res
	}
	entries, err := runner.ParseConfig(yamlBytes)
	if err != nil {
		res.Err = err
		return res
	}

	envs, err := d.Store.ListEnabledEnvironments()
	if err != nil {
		res.Err = err
		return res
	}

	var jobs []*store.Job
	for i := range entries {
		entry := entries[i]
		env := runner.PickEnvironment(entry, envs)
		if env == nil {
			res.EntriesSkipped++
			continue
		}
		cfgJSON, err := json.Marshal(entry)
		if err != nil {
			res.Err = err
			return res
		}
		jobs = append(jobs, &store.Job{
			CommitID:      commit.ID,
			EnvironmentID: env.ID,
			Tags:          runner.SortedTagString(entry.Tags),
			Config:        string(cfgJSON),
			TestInputRef:  cfg.TestRepoRef,
		})
	}

	if len(jobs) > 0 {
		created, err := d.Store.CreateJobs(jobs)
		if err != nil {
			res.Err = err
			return res
		}
		res.JobsCreated = len(created)
	}
	return res
}

// Pool is the scheduling loop executing claimed jobs.
type Pool struct {
	Store  *store.Store
	Runner *runner.Runner
	N      int // worker goroutine count; default 2
}

// Start launches the pool; call once from main. It resets stale running jobs
// (crash recovery) and polls for pending work until ctx is cancelled.
func (p *Pool) Start(ctx context.Context) {
	if n, err := p.Store.ResetStaleRunning(); err != nil {
		log.Printf("worker: reset stale running jobs: %v", err)
	} else if n > 0 {
		log.Printf("worker: reset %d stale running job(s) to pending", n)
	}

	n := p.N
	if n <= 0 {
		n = defaultWorkers
	}
	for i := 0; i < n; i++ {
		go p.loop(ctx)
	}
	log.Printf("worker: started %d worker(s)", n)
}

func (p *Pool) loop(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				job, err := p.Store.ClaimNextJob(0)
				if err != nil {
					log.Printf("worker: claim: %v", err)
					break
				}
				if job == nil {
					break // queue empty
				}
				p.Runner.Run(job)
			}
		}
	}
}
