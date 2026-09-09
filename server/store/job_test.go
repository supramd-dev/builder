package store

import (
	"testing"
	"time"
)

func seedJobFixture(t *testing.T, s *Store) (user *User, env *TestEnvironment, commit *Commit) {
	t.Helper()
	user = &User{Username: "jobuser-" + t.Name(), Email: "job@example.com", PasswordHash: "x"}
	if err := s.CreateUser(user); err != nil {
		t.Fatal(err)
	}
	env = &TestEnvironment{OwnerID: user.ID, Name: "cpu-node-" + t.Name(), Host: "h", Username: "u", PrivateKey: "k", Tags: "cpu"}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	commit = &Commit{Repo: "group/code", SHA: "aaabbbccc" + t.Name(), PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(commit); err != nil {
		t.Fatal(err)
	}
	return
}

func TestCreateJobsAndRequeue(t *testing.T) {
	s := newTestStore(t)
	_, env, commit := seedJobFixture(t, s)

	created, err := s.CreateJobs([]*Job{{
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          "cpu",
		Config:        `{"tags":["cpu"]}`,
		TestInputRef:  "main",
	}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(created) != 1 || created[0].Status != JobPending || created[0].ID == 0 {
		t.Fatalf("unexpected created job: %+v", created[0])
	}

	// Requeue: same (commit, env) refreshes and bumps attempts.
	requeued, err := s.CreateJobs([]*Job{{
		CommitID:      commit.ID,
		EnvironmentID: env.ID,
		Tags:          "cpu",
		Config:        `{"tags":["cpu"],"timeout":99}`,
		TestInputRef:  "dev",
	}})
	if err != nil {
		t.Fatalf("requeue: %v", err)
	}
	if len(requeued) != 1 || requeued[0].ID != created[0].ID {
		t.Fatalf("requeue should reuse the row: %+v", requeued[0])
	}
	if requeued[0].Attempts != 1 {
		t.Errorf("attempts after requeue: want 1, got %d", requeued[0].Attempts)
	}
	if requeued[0].Config == created[0].Config {
		t.Error("requeue should refresh the config snapshot")
	}
	if requeued[0].TestInputRef != "dev" {
		t.Errorf("requeue should refresh TestInputRef: %q", requeued[0].TestInputRef)
	}
}

func TestClaimNextJob(t *testing.T) {
	s := newTestStore(t)
	_, env, commit := seedJobFixture(t, s)

	// Empty queue: nil, nil.
	job, err := s.ClaimNextJob(0)
	if err != nil || job != nil {
		t.Fatalf("empty queue: job=%v err=%v", job, err)
	}

	if _, err := s.CreateJobs([]*Job{{
		CommitID: commit.ID, EnvironmentID: env.ID, Tags: "cpu", Config: "{}",
	}}); err != nil {
		t.Fatal(err)
	}

	job, err = s.ClaimNextJob(0)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}
	if job.Status != JobRunning || job.StartedAt == nil {
		t.Fatalf("claimed job should be running with StartedAt: %+v", job)
	}

	// Queue drained again.
	job2, err := s.ClaimNextJob(0)
	if err != nil || job2 != nil {
		t.Fatalf("drained queue: job=%v err=%v", job2, err)
	}
}

func TestFinishJobAndResetStaleRunning(t *testing.T) {
	s := newTestStore(t)
	_, env, commit := seedJobFixture(t, s)
	jobs, err := s.CreateJobs([]*Job{{
		CommitID: commit.ID, EnvironmentID: env.ID, Tags: "cpu", Config: "{}",
	}})
	if err != nil {
		t.Fatal(err)
	}
	job := jobs[0]

	if err := s.FinishJob(job.ID, JobFailed, "ssh dial failed"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListJobs(10)
	if err != nil || len(got) < 1 {
		t.Fatalf("list: %v %d", err, len(got))
	}
	var row Job
	for _, j := range got {
		if j.ID == job.ID {
			row = j
		}
	}
	if row.Status != JobFailed || row.Error != "ssh dial failed" || row.FinishedAt == nil {
		t.Fatalf("finished job wrong: %+v", row)
	}

	// Simulate a crash: reset a job directly to running.
	if err := s.DB.Model(&Job{}).Where("id = ?", job.ID).
		Updates(map[string]any{"status": JobRunning, "error": "", "finished_at": nil}).Error; err != nil {
		t.Fatal(err)
	}
	n, err := s.ResetStaleRunning()
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Fatalf("reset count: want >=1, got %d", n)
	}
}

func TestFindJobsByCommits(t *testing.T) {
	s := newTestStore(t)
	_, env, commit := seedJobFixture(t, s)
	other := &Commit{Repo: "group/code", SHA: "fff000" + t.Name(), PushedAt: time.Now()}
	if _, err := s.GetOrCreateCommit(other); err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateJobs([]*Job{
		{CommitID: commit.ID, EnvironmentID: env.ID, Tags: "cpu", Config: "{}"},
		{CommitID: other.ID, EnvironmentID: env.ID, Tags: "cpu", Config: "{}"},
	}); err != nil {
		t.Fatal(err)
	}

	// Mark the second job done — it drops out of the overlay.
	if err := s.DB.Model(&Job{}).Where("commit_id = ?", other.ID).
		Update("status", JobDone).Error; err != nil {
		t.Fatal(err)
	}

	jobs, err := s.FindJobsByCommits([]int64{env.ID}, []int64{commit.ID, other.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := jobs[EnvCommit{Env: env.ID, Commit: commit.ID}]; !ok {
		t.Fatalf("expected live job for (env, commit): %v", jobs)
	}
	if _, ok := jobs[EnvCommit{Env: env.ID, Commit: other.ID}]; ok {
		t.Fatal("done job should not be in the overlay")
	}
}

func TestNormalizeTags(t *testing.T) {
	cases := []struct{ in, want string }{
		{"CPU, Cuda", "cpu,cuda"},
		{"cpu cpu", "cpu"},
		{"  GPU ,  cuda ", "gpu,cuda"},
		{"", ""},
		{"cpu,mpi gpu", "cpu,mpi,gpu"},
	}
	for _, c := range cases {
		if got := NormalizeTags(c.in); got != c.want {
			t.Errorf("NormalizeTags(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
