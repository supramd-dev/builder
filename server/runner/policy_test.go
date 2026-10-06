package runner

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// blockingExecer runs a stage that only ends when its context is cancelled:
// what a real long-running stage looks like from the runner's side, where
// RunSSH closes the session when ctx ends and reports the cancellation.
type blockingExecer struct {
	started chan struct{} // closed when the first script starts
	aborted chan struct{} // closed when a script returns because ctx ended

	startOnce sync.Once
	stopOnce  sync.Once
	mu        sync.Mutex
	scripts   []string
}

func newBlockingExecer() *blockingExecer {
	return &blockingExecer{started: make(chan struct{}), aborted: make(chan struct{})}
}

func (b *blockingExecer) RunScript(ctx context.Context, h SSHHost, cmd, script string, timeout time.Duration, stdout, stderr io.Writer) ExecResult {
	b.mu.Lock()
	b.scripts = append(b.scripts, script)
	b.mu.Unlock()
	b.startOnce.Do(func() { close(b.started) })
	<-ctx.Done()
	b.stopOnce.Do(func() { close(b.aborted) })
	// What RunSSH returns for a session its context closed.
	return ExecResult{Success: false, Stderr: ctx.Err().Error(), ExitCode: -1}
}

func (b *blockingExecer) ExecHost(h SSHHost, cmd string) ExecResult { return ExecResult{Success: true} }

func (b *blockingExecer) CheckHost(h SSHHost) Result { return Result{Success: true} }

func (b *blockingExecer) ExtractTarTo(ctx context.Context, h SSHHost, r io.Reader, destDir string, timeout time.Duration) error {
	_, _ = io.Copy(io.Discard, r)
	return nil
}

// TestCancelTaskAbortsARunningStage covers the abort itself: a stage this
// process is running ends as soon as CancelTask names its node — the context
// the runner gave the stage is cancelled, which is what closes a real SSH
// session under it — and the registration goes away with the stage, so a later
// call for a node nobody is running reports false instead of pretending.
func TestCancelTaskAbortsARunningStage(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	ctx := testContext()

	// Run the clone stage to completion: it is the build's dependency, and
	// clone does not go through the Execer (the server clone/upload does).
	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatalf("clone: %v", err)
	}

	// The next claimable node is build, which runs a script.
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil {
		t.Fatalf("claim build: %v %v", build, err)
	}
	if build.Kind != store.TaskKindBuild {
		t.Fatalf("want the build node, got %s", build.Kind)
	}

	// A service whose execer blocks: the stage it starts is still running when
	// the cancellation arrives.
	blk := newBlockingExecer()
	blocking := &Service{Store: s, SSH: blk, Clone: cloner}
	done := make(chan struct{})
	go func() {
		defer close(done)
		blocking.runClaimed(ctx, build)
	}()

	select {
	case <-blk.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the build stage never started")
	}
	if !blocking.CancelTask(build.ID) {
		t.Fatal("CancelTask reported no stage running for the node it had just started")
	}
	select {
	case <-blk.aborted:
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled stage did not end")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the execution did not return after the cancellation")
	}

	// The store was not cancelled, only the stage: the node ends with the
	// outcome the aborted session produced (this is what a fork-cancel
	// dispatch avoids by cancelling the store first — then the report is
	// refused and the cancelled status stands).
	node, err := s.GetTask(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Status == store.StatusRunning || node.Status == store.StatusPending {
		t.Fatalf("the aborted node is still %q", node.Status)
	}
	if !strings.Contains(node.Summary, "canceled") {
		t.Fatalf("the aborted node's summary does not name the cancellation: %q", node.Summary)
	}

	// The registration is gone with the stage: nothing is running for it.
	if blocking.CancelTask(build.ID) {
		t.Fatal("CancelTask still reports a stage for a node that finished")
	}
	if blocking.CancelTask(build.ID + 1000) {
		t.Fatal("CancelTask reported a stage for an unknown node")
	}
}

// TestCancelledStageReportIsDiscarded is the other order of the same
// cancellation, and the one the fork-cancel policy produces: the store drops
// the graph first (CancelGraph) and the runner aborts the stage it is executing
// second (CancelTask). The aborted stage still reports an outcome — a killed
// session looks like a failed command — and that report must be discarded: the
// node keeps the cancellation, the run stays the cancelled one, no attempt is
// opened behind it, and the runner says so in one line instead of warning that
// it gave up closing the attempt.
func TestCancelledStageReportIsDiscarded(t *testing.T) {
	svc, s, _, cloner, cloneTask := newExecuteFixture(t, execYAML)
	ctx := testContext()

	if err := svc.ExecuteTask(ctx, cloneTask); err != nil {
		t.Fatalf("clone: %v", err)
	}
	build, err := s.ClaimReadyTask()
	if err != nil || build == nil {
		t.Fatalf("claim build: %v %v", build, err)
	}

	blk := newBlockingExecer()
	blocking := &Service{Store: s, SSH: blk, Clone: cloner}
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)

	done := make(chan struct{})
	go func() {
		defer close(done)
		blocking.runClaimed(ctx, build)
	}()
	select {
	case <-blk.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the build stage never started")
	}

	// What cancelPriorWork does: the store first, the abort second.
	if _, err := s.CancelGraph(build.RootID, store.CancelledSummary); err != nil {
		t.Fatalf("cancel graph: %v", err)
	}
	blocking.CancelTask(build.ID)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the execution did not return after the cancellation")
	}

	node, err := s.GetTask(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if node.Status != store.StatusCancelled || node.Summary != store.CancelledSummary {
		t.Fatalf("the node after the aborted stage reported: %q/%q, want cancelled/%q",
			node.Status, node.Summary, store.CancelledSummary)
	}
	runs, err := s.ListTaskRuns(build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("the discarded report opened %d run(s), want the one cancelled attempt", len(runs))
	}
	if runs[0].Status != store.StatusCancelled {
		t.Fatalf("the cancelled run was rewritten: %+v", runs[0])
	}
	got := logged.String()
	if !strings.Contains(got, "was cancelled while it ran; its result was discarded") {
		t.Fatalf("the runner did not report the discarded result in one plain line; log: %q", got)
	}
	// One line, and no retry: the store's refusal is the answer, not a flake to
	// try again (finishAttempt's retry loop skips it) — a retried report would
	// announce itself here, and four tries would hold the worker for 700 ms for
	// an outcome already discarded.
	if strings.Contains(got, "retrying in") || strings.Contains(got, "gave up") {
		t.Fatalf("the discarded report was retried; log: %q", got)
	}
}

// TestCancelTaskUnknownNodeIsNotRunningHere documents the other half of the
// contract: the runner can only abort what it runs itself, so a node another
// process (or no one) is executing is not an error — the store's cancelled
// status is what ends it, and the local abort is an optimization on top.
func TestCancelTaskUnknownNodeIsNotRunningHere(t *testing.T) {
	svc := &Service{}
	if svc.CancelTask(1) {
		t.Fatal("a service with no executions reported a running stage")
	}
}

// TestDispatchForRefForkCancel covers the policy on the manual yaml-matrix
// path: the second dispatch of the same ref records its own commit row and its
// own graph, and the first dispatch's graph is cancelled — and reports how many
// graphs it dropped.
// refPolicyFixture is the fixture the ref-dispatch policy tests share: a store
// with one environment and the site's repeated-commit policy set to policy, and
// a service whose ref resolution and yaml fetch are stubbed to the same SHA and
// the same one-stage matrix on every call.
func refPolicyFixture(t *testing.T, policy string) (*store.Store, *store.TestEnvironment, *Service) {
	t.Helper()
	s, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	u := &store.User{Username: "refcancel", Email: "rc@example.com", PasswordHash: "x"}
	if err := s.CreateUser(u); err != nil {
		t.Fatal(err)
	}
	env := &store.TestEnvironment{
		OwnerID: u.ID, Name: "cpu-refcancel", Host: "h", Username: "u", PrivateKey: "k",
		Tags: "cpu", Enabled: true,
	}
	if err := s.CreateEnvironment(env); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.example.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSiteConfig(&store.SiteConfig{DuplicateCommitPolicy: policy},
		"DuplicateCommitPolicy"); err != nil {
		t.Fatal(err)
	}

	const sha = "c0ffee1234567890abcdef1234567890abcd1234"
	svc := &Service{Store: s}
	svc.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
		return sha, nil
	}
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, atSHA string, creds *GitCredentials) ([]byte, error) {
		return []byte(refCancelYAML), nil
	}
	return s, env, svc
}

// TestDispatchForRefForkLeavesTheOlderGraphAlone: the fork policy forks and
// nothing else. The point of it is that both recordings get tested, so a
// dispatch must not touch the work the row before it is still running —
// cancelling is the third policy's job, and only its own.
func TestDispatchForRefForkLeavesTheOlderGraphAlone(t *testing.T) {
	s, env, svc := refPolicyFixture(t, store.CommitOverlapFork)

	first, res := svc.DispatchForRef(testContext(), "main")
	if res.Err != nil {
		t.Fatalf("first dispatch: %v", res.Err)
	}
	firstRoot, err := s.FindRootTaskByCommitEnv(first.ID, env.ID)
	if err != nil {
		t.Fatalf("the first graph: %v", err)
	}
	// The older graph has work in flight — the state a cancellation would eat.
	claimed, err := s.ClaimReadyTask()
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimed == nil || claimed.RootID != firstRoot.ID {
		t.Fatalf("claimed %+v, want a node of the first graph %d", claimed, firstRoot.ID)
	}

	second, res2 := svc.DispatchForRef(testContext(), "main")
	if res2.Err != nil {
		t.Fatalf("second dispatch: %v", res2.Err)
	}
	if res2.Cancelled != 0 {
		t.Fatalf("the fork policy cancelled %d graph(s)", res2.Cancelled)
	}
	if !res2.CommitCreated || second.ID == first.ID {
		t.Fatalf("the fork policy must record its own commit row (created=%v, id=%d vs %d)",
			res2.CommitCreated, second.ID, first.ID)
	}
	got, err := s.GetTask(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusRunning {
		t.Fatalf("the older graph's running node is %q after the second dispatch, want running",
			got.Status)
	}
	if root, err := s.GetTask(firstRoot.ID); err != nil {
		t.Fatal(err)
	} else if root.Status != store.StatusRunning {
		t.Fatalf("the older graph after the second dispatch: want running, got %q", root.Status)
	}
	secondRoot, err := s.FindRootTaskByCommitEnv(second.ID, env.ID)
	if err != nil {
		t.Fatalf("the second graph: %v", err)
	}
	if secondRoot.Status != store.StatusPending {
		t.Fatalf("the second graph: want pending, got %q", secondRoot.Status)
	}
}

// TestDispatchForRefForkCancel: the third policy adds cancellation to the
// fork: the new recording gets a row and a graph of its own, and the work the
// earlier recordings left behind is dropped.
func TestDispatchForRefForkCancel(t *testing.T) {
	s, env, svc := refPolicyFixture(t, store.CommitOverlapForkCancel)

	first, res := svc.DispatchForRef(testContext(), "main")
	if res.Err != nil {
		t.Fatalf("first dispatch: %v", res.Err)
	}
	if res.Cancelled != 0 {
		t.Fatalf("the first dispatch cancelled %d graph(s); nothing was there to cancel", res.Cancelled)
	}
	firstRoot, err := s.FindRootTaskByCommitEnv(first.ID, env.ID)
	if err != nil {
		t.Fatalf("the first graph: %v", err)
	}

	// The second dispatch of the same ref: its own row, its own graph, and the
	// first graph cancelled.
	second, res2 := svc.DispatchForRef(testContext(), "main")
	if res2.Err != nil {
		t.Fatalf("second dispatch: %v", res2.Err)
	}
	if res2.Cancelled != 1 {
		t.Fatalf("the second dispatch cancelled %d graph(s), want 1", res2.Cancelled)
	}
	if !res2.CommitCreated {
		t.Fatal("the fork policy must record its own commit row")
	}
	if second.ID == first.ID {
		t.Fatalf("both dispatches share the commit row %d", first.ID)
	}
	if got, err := s.GetTask(firstRoot.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.StatusCancelled {
		t.Fatalf("the first graph after the second dispatch: want cancelled, got %q", got.Status)
	}
	secondRoot, err := s.FindRootTaskByCommitEnv(second.ID, env.ID)
	if err != nil {
		t.Fatalf("the second graph: %v", err)
	}
	if secondRoot.Status != store.StatusPending {
		t.Fatalf("the second graph: want pending, got %q", secondRoot.Status)
	}

	// A third dispatch of the same ref drops the second graph and nothing
	// else: the first was dropped by the dispatch before it, and a graph that
	// is already cancelled is not dropped again — the count says what this
	// dispatch did, not how many earlier rows happen to exist.
	third, res3 := svc.DispatchForRef(testContext(), "main")
	if res3.Err != nil {
		t.Fatalf("third dispatch: %v", res3.Err)
	}
	if res3.Cancelled != 1 {
		t.Fatalf("the third dispatch cancelled %d graph(s), want 1 (the second graph)", res3.Cancelled)
	}
	if got, err := s.GetTask(secondRoot.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.StatusCancelled {
		t.Fatalf("the second graph after the third dispatch: want cancelled, got %q", got.Status)
	}
	thirdRoot, err := s.FindRootTaskByCommitEnv(third.ID, env.ID)
	if err != nil {
		t.Fatalf("the third graph: %v", err)
	}
	if thirdRoot.Status != store.StatusPending {
		t.Fatalf("the third graph: want pending, got %q", thirdRoot.Status)
	}

	// A dispatch that fails (no yaml) cancels nothing: the running test of the
	// revision is not the price of a broken read.
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, atSHA string, creds *GitCredentials) ([]byte, error) {
		return nil, io.ErrUnexpectedEOF
	}
	if _, res4 := svc.DispatchForRef(testContext(), "main"); res4.Cancelled != 0 {
		t.Fatalf("a failed dispatch cancelled %d graph(s)", res4.Cancelled)
	}
	if got, err := s.GetTask(thirdRoot.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.StatusPending {
		t.Fatalf("the current graph after a failed dispatch: want pending, got %q", got.Status)
	}

	// The same goes for a yaml that parses but dispatches nothing — no entry
	// matching an environment here, which is a dispatch that created no graph.
	// The policy fires on what the new recording *replaces*, so an empty
	// dispatch leaves the older graph exactly where it is.
	svc.FetchYAML = func(ctx context.Context, codeRepoURL, atSHA string, creds *GitCredentials) ([]byte, error) {
		return []byte(refNoMatchYAML), nil
	}
	_, res5 := svc.DispatchForRef(testContext(), "main")
	if res5.Err != nil {
		t.Fatalf("a yaml matching no environment is not a failure: %v", res5.Err)
	}
	if res5.TasksCreated != 0 || res5.EntriesSkipped != 1 || res5.Cancelled != 0 {
		t.Fatalf("an empty dispatch created %d graph(s), skipped %d entry(ies) and cancelled %d; want 0, 1 and 0",
			res5.TasksCreated, res5.EntriesSkipped, res5.Cancelled)
	}
	if got, err := s.GetTask(thirdRoot.ID); err != nil {
		t.Fatal(err)
	} else if got.Status != store.StatusPending {
		t.Fatalf("the current graph after an empty dispatch: want pending, got %q", got.Status)
	}
}

const refCancelYAML = `version: 3
defaults:
  build:
    command: "make -j8"
matrix:
  - tags: [cpu]
    unit:
      command: "ctest -L unit"
`

// refNoMatchYAML is refCancelYAML with tags the fixture's environment does not
// carry: it parses, and it dispatches nothing.
const refNoMatchYAML = `version: 3
defaults:
  build:
    command: "make -j8"
matrix:
  - tags: [gpu]
    unit:
      command: "ctest -L unit"
`
