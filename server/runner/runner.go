package runner

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"md-builder/server/store"
)

// ExecFunc executes a generated script on an environment's remote host with
// an overall timeout. The real implementation wraps sshcheck; tests inject
// fakes.
type ExecFunc func(env *store.TestEnvironment, script string, timeoutSecs int) (stdout string, exitCode int, err error)

// Runner executes claimed jobs: config snapshot → script → SSH → report →
// database.
type Runner struct {
	Store *store.Store
	Exec  ExecFunc
}

// Run executes one job to a terminal state (done / failed), reporting the
// test-run results along the way. It never returns an error for a failed
// execution — the job row records it — only for unexpected store failures.
func (r *Runner) Run(job *store.Job) {
	env, err := r.Store.GetEnvironmentAny(job.EnvironmentID)
	if err != nil {
		r.failJob(job, fmt.Sprintf("environment lookup failed: %v", err))
		return
	}
	commit, err := r.Store.GetCommitByID(job.CommitID)
	if err != nil {
		r.failJob(job, fmt.Sprintf("commit lookup failed: %v", err))
		return
	}

	var entry MergedEntry
	if err := json.Unmarshal([]byte(job.Config), &entry); err != nil {
		r.failJob(job, fmt.Sprintf("job config snapshot is invalid: %v", err))
		return
	}

	cfg, err := r.Store.GetSiteConfig()
	if err != nil {
		r.failJob(job, fmt.Sprintf("site config lookup failed: %v", err))
		return
	}

	timeoutSecs, err := TotalScriptTimeout(&entry)
	if err != nil {
		r.failJob(job, err.Error())
		return
	}

	script, err := BuildScript(&ScriptInput{
		CommitSHA:     commit.SHA,
		CodeRepoURL:   cfg.CodeRepo,
		TestInputRepo: cfg.TestInputRepo,
		TestInputRef:  job.TestInputRef,
		EnvName:       env.Name,
		EnvTags:       env.Tags,
		Entry:         &entry,
	})
	if err != nil {
		r.failJob(job, err.Error())
		return
	}

	stdout, _, execErr := r.Exec(env, script, timeoutSecs)
	if execErr != nil {
		r.failJob(job, fmt.Sprintf("ssh execution failed: %v", execErr))
		return
	}

	report, err := ParseReport(stdout)
	if err != nil {
		r.failJob(job, fmt.Sprintf("%v; output tail: %s", err, tailString(stdout, 300)))
		return
	}

	// Record the stage outcomes as test runs (simplified: one-paragraph
	// summary, no per-case detail).
	now := time.Now()
	if report.Unit != nil {
		if _, err := r.Store.UpsertTestRun(&store.RunInput{
			EnvironmentID: env.ID,
			CommitID:      commit.ID,
			Kind:          store.RunKindUnit,
			Status:        report.Unit.Status,
			Summary:       report.Unit.Summary,
			StartedAt:     jobStarted(job),
			FinishedAt:    now,
		}); err != nil {
			log.Printf("runner: job %d: record unit run: %v", job.ID, err)
		}
	}
	if report.Regression != nil {
		if _, err := r.Store.UpsertTestRun(&store.RunInput{
			EnvironmentID: env.ID,
			CommitID:      commit.ID,
			Kind:          store.RunKindRegression,
			Status:        report.Regression.Status,
			Summary:       report.Regression.Summary,
			StartedAt:     jobStarted(job),
			FinishedAt:    now,
		}); err != nil {
			log.Printf("runner: job %d: record regression run: %v", job.ID, err)
		}
	}

	if err := r.Store.FinishJob(job.ID, store.JobDone, ""); err != nil {
		log.Printf("runner: job %d: finish: %v", job.ID, err)
	}
}

func (r *Runner) failJob(job *store.Job, msg string) {
	if err := r.Store.FinishJob(job.ID, store.JobFailed, msg); err != nil {
		log.Printf("runner: job %d: finish failed: %v", job.ID, err)
	}
}

func jobStarted(job *store.Job) time.Time {
	if job.StartedAt != nil {
		return *job.StartedAt
	}
	return time.Time{}
}

func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return s
}
