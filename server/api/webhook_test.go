package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"md-builder/server/runner"
	"md-builder/server/store"
)

// postWebhook sends a GitLab event the way GitLab does: with the site's
// current webhook token in X-Gitlab-Token. Extra headers are passed as
// key/value pairs. The token is read from the store on every call, so a test
// that has just rotated it (or saved a config without one, which the store
// heals on load) still sends the value the server expects.
func postWebhook(t *testing.T, mux *http.ServeMux, s *store.Store, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatalf("load site config: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", strings.NewReader(body))
	req.Header.Set("X-Gitlab-Token", cfg.WebhookToken)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestWebhookEventKinds walks the webhook chain for each GitLab event type:
// push, tag push and merge request events are recorded as commits with the
// matching event kind, and dispatched to task graphs when the project is the
// configured code repository. Other events stay ignored.
func TestWebhookEventKinds(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cpu-events", "cpu", true)

	post := func(t *testing.T, body string) map[string]any {
		t.Helper()
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return res
	}

	// Push: event recorded and dispatched.
	res := post(t, pushBody("group/code", "1111111111111111111111111111111111111111"))
	if res["event"] != "push" {
		t.Fatalf("push event kind: %v", res["event"])
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("push jobsCreated: want 1, got %v", res["jobsCreated"])
	}
	c, err := s.GetCommitByID(int64(res["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventPush {
		t.Fatalf("commit event: want push, got %q", c.Event)
	}
	if c.Ref != "main" {
		t.Fatalf("push ref: want main, got %q", c.Ref)
	}

	// Tag push: refs/tags/v1.0 stripped, tag SHA dispatched, event kind
	// tag_push on the commit row.
	tagBody := fmt.Sprintf(`{
		"object_kind": "tag_push",
		"project": {"name": "code", "path_with_namespace": "group/code", "web_url": "https://gitlab.com/group/code"},
		"ref": "refs/tags/v1.0",
		"before": "0000000000000000000000000000000000000000",
		"after": "2222222222222222222222222222222222222222",
		"user_name": "bob",
		"commits": []
	}`)
	res = post(t, tagBody)
	if res["event"] != "tag_push" {
		t.Fatalf("tag event kind: %v", res["event"])
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("tag jobsCreated: want 1, got %v", res["jobsCreated"])
	}
	if res["ref"] != "v1.0" {
		t.Fatalf("tag ref: want v1.0, got %v", res["ref"])
	}
	c, err = s.GetCommitByID(int64(res["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventTagPush {
		t.Fatalf("commit event: want tag_push, got %q", c.Event)
	}
	if c.Ref != "v1.0" {
		t.Fatalf("commit ref: want v1.0, got %q", c.Ref)
	}

	// Merge request (action=merge): last_commit SHA recorded and dispatched
	// under the source branch.
	mrBody := `{
		"object_kind": "merge_request",
		"project": {"name": "code", "path_with_namespace": "group/code", "web_url": "https://gitlab.com/group/code"},
		"user_name": "carol",
		"object_attributes": {
			"action": "merge",
			"title": "Fix the energy drift",
			"source_branch": "fix/drift",
			"target_branch": "main",
			"state": "merged",
			"last_commit": {"id": "3333333333333333333333333333333333333333", "message": "fix: energy drift\n\nlong body"}
		}
	}`
	res = post(t, mrBody)
	if res["event"] != "merge_request" {
		t.Fatalf("mr event kind: %v", res["event"])
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("mr jobsCreated: want 1, got %v", res["jobsCreated"])
	}
	if res["ref"] != "fix/drift" {
		t.Fatalf("mr ref: want fix/drift, got %v", res["ref"])
	}
	c, err = s.GetCommitByID(int64(res["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventMergeRequest {
		t.Fatalf("commit event: want merge_request, got %q", c.Event)
	}
	if c.Ref != "fix/drift" {
		t.Fatalf("commit ref: want fix/drift, got %q", c.Ref)
	}
	if c.Message != "fix: energy drift" {
		t.Fatalf("commit message: want first line, got %q", c.Message)
	}

	// Merge request with a non-dispatching action (update): recorded but
	// NOT dispatched.
	mrUpdate := strings.Replace(mrBody, `"action": "merge"`, `"action": "update"`, 1)
	mrUpdate = strings.Replace(mrUpdate,
		"3333333333333333333333333333333333333333", "4444444444444444444444444444444444444444", 1)
	res = post(t, mrUpdate)
	if _, ok := res["jobsCreated"]; ok {
		t.Fatalf("mr update should not dispatch: %v", res)
	}
	if res["action"] != "update" {
		t.Fatalf("mr action: %v", res["action"])
	}

	// Unknown event kind: still ignored.
	res = post(t, `{"object_kind": "pipeline", "project": {"path_with_namespace": "group/code"}}`)
	if res["status"] != "ignored" {
		t.Fatalf("pipeline event should be ignored: %v", res)
	}

	// The graphs: one root per dispatched event (push, tag, merge) on the
	// single cpu environment.
	roots, err := s.ListRootTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 3 {
		t.Fatalf("want 3 dispatched roots, got %d", len(roots))
	}
	for _, root := range roots {
		if root.Trigger != store.TaskTriggerWebhook {
			t.Fatalf("root %d trigger: want webhook(0), got %d", root.ID, root.Trigger)
		}
		subs, err := s.ListActiveNodes(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) == 0 {
			t.Fatalf("root %d has no nodes", root.ID)
		}
	}
}

// TestWebhookPushThenMergeRequestOfTheSameCommit covers the sequence GitLab
// sends when a branch is pushed and then opened as a merge request: two events
// carry the same SHA, both dispatch, and the second one requeues the graph the
// first one created. What the dashboard shows is one commit row — restamped
// from push to merge_request, not duplicated — and one graph whose nodes move
// on to a second attempt, the attempt the push started being closed as
// superseded. So a push-triggered task can be relabelled MR and restart from
// the beginning, which is the behaviour this test pins down.
func TestWebhookPushThenMergeRequestOfTheSameCommit(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	env := seedDispatchEnv(t, s, "cpu-push-mr", "cpu", true)

	post := func(t *testing.T, body string) map[string]any {
		t.Helper()
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return res
	}

	const sha = "5555555555555555555555555555555555555555"
	// The push of a feature branch: recorded as a push event, dispatched once.
	push := fmt.Sprintf(`{
		"object_kind": "push",
		"project": {"name": "code", "path_with_namespace": "group/code", "web_url": "https://gitlab.com/group/code"},
		"ref": "refs/heads/fix/drift",
		"before": "0000000",
		"after": %q,
		"user_name": "alice",
		"commits": [{"id": %q, "message": "wip: energy drift"}]
	}`, sha, sha)
	res := post(t, push)
	if res["event"] != "push" {
		t.Fatalf("push event kind: %v", res["event"])
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("push jobsCreated: want 1, got %v", res["jobsCreated"])
	}
	commitID := int64(res["commitId"].(float64))
	c, err := s.GetCommitByID(commitID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventPush || c.Message != "wip: energy drift" {
		t.Fatalf("commit after the push: %+v", c)
	}

	// The MR opening that same commit: the same row, an event restamp, and a
	// second dispatch — `open` means a new source state to test.
	mr := fmt.Sprintf(`{
		"object_kind": "merge_request",
		"project": {"name": "code", "path_with_namespace": "group/code", "web_url": "https://gitlab.com/group/code"},
		"user_name": "carol",
		"object_attributes": {
			"action": "open",
			"title": "Fix the energy drift",
			"source_branch": "fix/drift",
			"target_branch": "main",
			"state": "opened",
			"last_commit": {"id": %q, "message": "fix: energy drift"}
		}
	}`, sha)
	res = post(t, mr)
	if res["event"] != "merge_request" {
		t.Fatalf("mr event kind: %v", res["event"])
	}
	if int64(res["commitId"].(float64)) != commitID {
		t.Fatalf("the MR recorded commit %v, want the pushed row %d",
			res["commitId"], commitID)
	}
	if res["jobsCreated"].(float64) != 1 {
		t.Fatalf("mr jobsCreated: want 1, got %v", res["jobsCreated"])
	}

	// One row, restamped in place: no second commit, no push event left.
	c, err = s.GetCommitByID(commitID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventMergeRequest {
		t.Fatalf("commit event after the MR: want merge_request, got %q", c.Event)
	}
	if c.Ref != "fix/drift" {
		t.Fatalf("commit ref after the MR: want the source branch, got %q", c.Ref)
	}
	if c.Message != "fix: energy drift" {
		t.Fatalf("commit message after the MR: want the MR title, got %q", c.Message)
	}
	all, err := s.ListCommits("group/code", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("want one commit row for the SHA, got %d", len(all))
	}

	// One graph, requeued: the same root (same id, same creation) on its
	// second attempt, with the attempt the push opened superseded and a fresh
	// pending one waiting for the scheduler.
	root, err := s.FindRootTaskByCommitEnv(commitID, env.ID)
	if err != nil {
		t.Fatalf("find the root: %v", err)
	}
	if root.Attempts != 2 {
		t.Fatalf("root attempts after the MR: want 2, got %d", root.Attempts)
	}
	if root.Status != store.StatusPending {
		t.Fatalf("root status after the MR: want pending, got %q", root.Status)
	}
	roots, err := s.ListRootTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 {
		t.Fatalf("want one graph for the (commit, environment), got %d", len(roots))
	}
	nodes, err := s.ListActiveNodes(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) == 0 {
		t.Fatal("the requeued graph has no nodes")
	}
	real := 0
	for _, n := range nodes {
		if n.Attempts != 2 {
			t.Fatalf("node %q attempts: want 2, got %d", n.NodeKey, n.Attempts)
		}
		if n.Virtual {
			// A container's attempt is the dispatch itself: it has no run to
			// supersede, only its counter to move.
			if runs, err := s.ListTaskRuns(n.ID); err != nil {
				t.Fatal(err)
			} else if len(runs) != 0 {
				t.Fatalf("virtual node %q has %d runs", n.NodeKey, len(runs))
			}
			continue
		}
		real++
		runs, err := s.ListTaskRuns(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(runs) != 2 {
			t.Fatalf("node %q runs: want the superseded attempt and the new one, got %d", n.NodeKey, len(runs))
		}
		byAttempt := map[int]store.TestRun{}
		for _, r := range runs {
			byAttempt[r.Attempt] = r
		}
		first, second := byAttempt[1], byAttempt[2]
		if first.TaskID == 0 || second.TaskID == 0 {
			t.Fatalf("node %q attempts: want 1 and 2, got %v", n.NodeKey, byAttempt)
		}
		if first.Status != store.StatusSkipped || first.Summary != store.SupersededSummary {
			t.Fatalf("node %q attempt 1: want skipped/%q, got %q/%q",
				n.NodeKey, store.SupersededSummary, first.Status, first.Summary)
		}
		if second.Status != store.StatusPending {
			t.Fatalf("node %q attempt 2: want pending, got %q", n.NodeKey, second.Status)
		}
	}
	if real == 0 {
		t.Fatal("the requeued graph has no stage nodes to check")
	}
}

// setOverlapPolicy stores the site's repeated-commit policy the way the
// settings page does: the one column, without touching the rest of the row
// (the webhook token postWebhook reads is part of that rest).
func setOverlapPolicy(t *testing.T, s *store.Store, policy string) {
	t.Helper()
	if err := s.UpdateSiteConfig(&store.SiteConfig{DuplicateCommitPolicy: policy}, "DuplicateCommitPolicy"); err != nil {
		t.Fatalf("set the repeated-commit policy to %q: %v", policy, err)
	}
}

// mrBody is a GitLab merge_request event opening the given SHA.
func mrBody(repo, sha, title string) string {
	return fmt.Sprintf(`{
		"object_kind": "merge_request",
		"project": {"name": "code", "path_with_namespace": %q, "web_url": "https://gitlab.com/%s"},
		"user_name": "carol",
		"object_attributes": {
			"action": "open",
			"title": %q,
			"source_branch": "fix/drift",
			"target_branch": "main",
			"state": "opened",
			"last_commit": {"id": %q, "message": %q}
		}
	}`, repo, repo, title, sha, title)
}

// TestWebhookForkPolicyGivesEachEventItsOwnGraph covers the `fork` policy: the
// second event for a SHA records its own commit row and its own graph, and the
// first event's graph is left exactly as it was — still pending, still on its
// first attempt. Two events, two matrix rows, two independent runs.
func TestWebhookForkPolicyGivesEachEventItsOwnGraph(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	env := seedDispatchEnv(t, s, "cpu-fork", "cpu", true)
	setOverlapPolicy(t, s, store.CommitOverlapFork)

	post := func(t *testing.T, body string) map[string]any {
		t.Helper()
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return res
	}

	const sha = "6666666666666666666666666666666666666666"
	push := post(t, pushBody("group/code", sha))
	if push["jobsCreated"].(float64) != 1 {
		t.Fatalf("push jobsCreated: want 1, got %v", push["jobsCreated"])
	}
	pushCommit := int64(push["commitId"].(float64))

	// The MR opening the same SHA: its own row, its own graph.
	mr := post(t, mrBody("group/code", sha, "fix: energy drift"))
	if mr["jobsCreated"].(float64) != 1 {
		t.Fatalf("mr jobsCreated: want 1, got %v", mr["jobsCreated"])
	}
	mrCommit := int64(mr["commitId"].(float64))
	if mrCommit == pushCommit {
		t.Fatalf("the MR reused the pushed row %d; the fork policy gives it one of its own", pushCommit)
	}
	if got := mr["graphsCancelled"].(float64); got != 0 {
		t.Fatalf("fork graphsCancelled: want 0, got %v", got)
	}

	// Two rows for one SHA, each with the event that recorded it.
	all, err := s.ListCommits("group/code", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("want two commit rows for the SHA, got %d", len(all))
	}
	byID := map[int64]store.Commit{}
	for _, c := range all {
		byID[c.ID] = c
	}
	if byID[pushCommit].Event != store.CommitEventPush {
		t.Fatalf("the pushed row's event: want push, got %q", byID[pushCommit].Event)
	}
	if byID[mrCommit].Event != store.CommitEventMergeRequest {
		t.Fatalf("the MR row's event: want merge_request, got %q", byID[mrCommit].Event)
	}

	// Two graphs, one per row, both on their first attempt — the fork left the
	// pushed one alone instead of requeueing it.
	pushRoot, err := s.FindRootTaskByCommitEnv(pushCommit, env.ID)
	if err != nil {
		t.Fatalf("the pushed graph: %v", err)
	}
	mrRoot, err := s.FindRootTaskByCommitEnv(mrCommit, env.ID)
	if err != nil {
		t.Fatalf("the MR graph: %v", err)
	}
	if pushRoot.ID == mrRoot.ID {
		t.Fatalf("both rows share the graph %d", pushRoot.ID)
	}
	for _, root := range []*store.Task{pushRoot, mrRoot} {
		if root.Attempts != 1 {
			t.Fatalf("graph %d attempts: want 1, got %d", root.ID, root.Attempts)
		}
		if root.Status != store.StatusPending {
			t.Fatalf("graph %d status: want pending, got %q", root.ID, root.Status)
		}
	}
}

// TestWebhookForkCancelPolicyCancelsTheEarlierGraph covers the `fork_cancel`
// policy: the MR gets its own row and graph, and the pushed revision's graph is
// cancelled — its unfinished nodes, and the run of the attempt that was still
// going, with the reason the policy dropped it. Work that had already finished
// keeps its outcome.
func TestWebhookForkCancelPolicyCancelsTheEarlierGraph(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	env := seedDispatchEnv(t, s, "cpu-fork-cancel", "cpu", true)
	setOverlapPolicy(t, s, store.CommitOverlapForkCancel)

	const sha = "7777777777777777777777777777777777777777"
	rec := postWebhook(t, mux, s, pushBody("group/code", sha), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("push webhook: %d %s", rec.Code, rec.Body.String())
	}
	var push map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &push); err != nil {
		t.Fatal(err)
	}
	pushCommit := int64(push["commitId"].(float64))
	pushRoot, err := s.FindRootTaskByCommitEnv(pushCommit, env.ID)
	if err != nil {
		t.Fatalf("the pushed graph: %v", err)
	}

	// Finish one node and leave another running: the cancellation must drop
	// the second and leave the first's verdict alone.
	nodes, err := s.ListActiveNodes(pushRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	var finished, running store.Task
	for i := range nodes {
		n := nodes[i]
		if n.Virtual {
			continue
		}
		if finished.ID == 0 {
			finished = n
			continue
		}
		running = n
		break
	}
	if finished.ID == 0 || running.ID == 0 {
		t.Fatalf("the pushed graph has too few stage nodes: %d", len(nodes))
	}
	if _, err := s.FinishAttempt(finished.ID, store.AttemptResult{
		Status: store.StatusPassed, Summary: "1/1 case passed", Attempt: finished.Attempts,
		StartedAt: time.Now(), FinishedAt: time.Now(),
	}); err != nil {
		t.Fatalf("finish %q: %v", finished.NodeKey, err)
	}
	claimed, err := s.ClaimReadyTask()
	if err != nil || claimed == nil {
		t.Fatalf("claim a node to leave running: task=%v err=%v", claimed, err)
	}
	if claimed.ID != running.ID {
		// Another node was ready first; that is fine as long as one is running.
		running = *claimed
	}
	// The claim's snapshot is the candidate it read, so reload for the status
	// the claim wrote.
	reloaded, err := s.GetTask(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	running = *reloaded
	if running.Status != store.StatusRunning {
		t.Fatalf("the claimed node is %q, want running", running.Status)
	}

	// The MR: its own row and graph, and the pushed graph cancelled.
	rec = postWebhook(t, mux, s, mrBody("group/code", sha, "fix: energy drift"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("mr webhook: %d %s", rec.Code, rec.Body.String())
	}
	var mr map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &mr); err != nil {
		t.Fatal(err)
	}
	if got := mr["graphsCancelled"].(float64); got != 1 {
		t.Fatalf("graphsCancelled: want 1, got %v", got)
	}
	mrCommit := int64(mr["commitId"].(float64))
	if mrCommit == pushCommit {
		t.Fatalf("the MR reused the pushed row %d", pushCommit)
	}

	// The earlier graph: cancelled as a whole, the reason on the unfinished
	// nodes, and the finished node untouched.
	cancelled, err := s.FindRootTaskByCommitEnv(pushCommit, env.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != store.StatusCancelled {
		t.Fatalf("the pushed graph: want cancelled, got %q", cancelled.Status)
	}
	// The root is a container: its summary is the rollup, which names the
	// cancelled stages rather than carrying the policy's own wording.
	if !strings.Contains(cancelled.Summary, "cancelled: ") {
		t.Fatalf("the pushed graph summary: want a rollup naming the cancelled stages, got %q",
			cancelled.Summary)
	}
	for _, n := range nodes {
		node, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		switch node.ID {
		case finished.ID:
			if node.Status != store.StatusPassed {
				t.Fatalf("the finished node: want passed, got %q", node.Status)
			}
		default:
			if node.Status != store.StatusCancelled {
				t.Fatalf("node %q: want cancelled, got %q", node.NodeKey, node.Status)
			}
		}
	}
	// The attempt that was running is closed as cancelled, with the policy's
	// reason rather than a stage verdict.
	runs, err := s.ListTaskRuns(running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 {
		t.Fatalf("the running node %q has no run", running.NodeKey)
	}
	if runs[0].Status != store.StatusCancelled || runs[0].Summary != store.CancelledSummary {
		t.Fatalf("the running node's attempt: want cancelled/%q, got %q/%q",
			store.CancelledSummary, runs[0].Status, runs[0].Summary)
	}
	if runs[0].FinishedAt.IsZero() {
		t.Fatal("the cancelled attempt has no finish time")
	}
	// The new graph is untouched by the cancellation.
	mrRoot, err := s.FindRootTaskByCommitEnv(mrCommit, env.ID)
	if err != nil {
		t.Fatalf("the MR graph: %v", err)
	}
	if mrRoot.Status != store.StatusPending {
		t.Fatalf("the MR graph: want pending, got %q", mrRoot.Status)
	}

	// Whatever the queue still holds belongs to the MR's graph: no node of the
	// cancelled one is handed out (a cancelled node is terminal, so nothing
	// claims it again).
	for i := 0; i <= len(nodes); i++ {
		waiting, err := s.ClaimReadyTask()
		if err != nil {
			t.Fatal(err)
		}
		if waiting == nil {
			break // queue drained
		}
		if waiting.RootID == pushRoot.ID {
			t.Fatalf("the queue hands out task %d (%q) from the cancelled graph",
				waiting.ID, waiting.NodeKey)
		}
	}
}

// TestWebhookForkCancelKeepsRunningWorkWhenTheNewDispatchFails: a dispatch that
// creates nothing — the yaml cannot be read, no entry matches — is not a reason
// to drop a running test. The cancellation waits for a successful dispatch.
func TestWebhookForkCancelKeepsRunningWorkWhenTheNewDispatchFails(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	env := seedDispatchEnv(t, s, "cpu-fork-cancel-fail", "cpu", true)
	setOverlapPolicy(t, s, store.CommitOverlapForkCancel)

	const sha = "8888888888888888888888888888888888888888"
	rec := postWebhook(t, mux, s, pushBody("group/code", sha), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("push webhook: %d %s", rec.Code, rec.Body.String())
	}
	var push map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &push); err != nil {
		t.Fatal(err)
	}
	pushCommit := int64(push["commitId"].(float64))
	pushRoot, err := s.FindRootTaskByCommitEnv(pushCommit, env.ID)
	if err != nil {
		t.Fatalf("the pushed graph: %v", err)
	}

	// The next dispatch reads no yaml at all.
	apiServer.Runner.FetchYAML = func(ctx context.Context, repoURL, sha string, creds *runner.GitCredentials) ([]byte, error) {
		return nil, fmt.Errorf("repo unreachable")
	}

	rec = postWebhook(t, mux, s, mrBody("group/code", sha, "fix: energy drift"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("mr webhook: %d %s", rec.Code, rec.Body.String())
	}
	var mr map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &mr); err != nil {
		t.Fatal(err)
	}
	if _, ok := mr["dispatchError"]; !ok {
		t.Fatalf("the failed dispatch should be surfaced: %v", mr)
	}
	if got := mr["graphsCancelled"].(float64); got != 0 {
		t.Fatalf("graphsCancelled after a failed dispatch: want 0, got %v", got)
	}
	still, err := s.GetTask(pushRoot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != store.StatusPending {
		t.Fatalf("the pushed graph after the failed dispatch: want pending, got %q", still.Status)
	}
}

// TestWebhookManualCommitEvents checks the manual dispatch paths stamp their
// own event kind on the commit rows (manual / manual_yaml).
func TestWebhookManualCommitEvents(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	user := seedUser(t, s, "eventuser", "event@example.com", "pw")
	env := seedOwnedDispatchEnv(t, s, user, "cpu-mev", "cpu", true)

	apiServer.Runner.ResolveRef = func(ctx context.Context, repoURL, ref string, creds *runner.GitCredentials) (string, error) {
		// Distinct SHAs per ref so the two dispatches record separate rows
		// (same-SHA dispatches dedup onto the first row and would keep its
		// event kind).
		if ref == "v1.0" {
			return "bbbb5678bbbb5678bbbb5678bbbb5678bbbb5678", nil
		}
		return "aaaa1234aaaa1234aaaa1234aaaa1234aaaa1234", nil
	}

	cookie := loginAndGetCookie(t, mux, "eventuser", "pw")
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/jobs/manual", strings.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Manual test: commit row event = manual.
	rec := post(fmt.Sprintf(`{"buildCommand":"make","environmentIds":[%d]}`, env.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("manual trigger: %d %s", rec.Code, rec.Body.String())
	}
	commits, err := s.ListCommits("", 10)
	if err != nil || len(commits) == 0 {
		t.Fatalf("manual commit rows: %v %d", err, len(commits))
	}
	if commits[0].Event != store.CommitEventManual {
		t.Fatalf("manual commit event: want manual, got %q", commits[0].Event)
	}

	// Manual yaml dispatch: commit row event = manual_yaml.
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/jobs/manual-yaml", strings.NewReader(`{"ref":"v1.0"}`))
	req2.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("manual-yaml trigger: %d %s", rec2.Code, rec2.Body.String())
	}
	var res struct {
		CommitID int64 `json:"commitId"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	c, err := s.GetCommitByID(res.CommitID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventManualYAML {
		t.Fatalf("manual-yaml commit event: want manual_yaml, got %q", c.Event)
	}
}

// TestWebhookReadsYAMLWithoutCloning is the end-to-end form of the webhook
// timeout fix: the handler runs against the real yaml fetcher, pointed at a
// fake code host, and must dispatch from a single small request. A fallback
// clone here would be talking to a host that serves no git at all, so the
// test fails loudly if the fast path is not the one that ran.
func TestWebhookReadsYAMLWithoutCloning(t *testing.T) {
	const sha = "21c8dc33c771d5002df19de1cc71bb5a0c87568e"
	var requests int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if want := "/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw"; r.URL.EscapedPath() != want {
			t.Errorf("requested %q, want %q", r.URL.EscapedPath(), want)
		}
		if want := "ref=" + sha; r.URL.RawQuery != want {
			t.Errorf("query = %q, want %q", r.URL.RawQuery, want)
		}
		_, _ = w.Write([]byte(dispatchYAML))
	}))
	defer host.Close()

	apiServer, s := newTestServer(t)
	apiServer.SetRunner(&runner.Service{Store: s, FetchYAML: runner.NewYAMLFetcher()})
	mux := http.NewServeMux()
	apiServer.Register(mux)

	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: host.URL + "/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cpu-rawfetch", "cpu", true)

	rec := postWebhook(t, mux, s, pushBody("group/code", sha), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res["dispatchError"] != nil {
		t.Fatalf("dispatchError: %v", res["dispatchError"])
	}
	if got := res["jobsCreated"].(float64); got != 1 {
		t.Fatalf("jobsCreated: want 1, got %v", got)
	}
	if requests != 1 {
		t.Fatalf("the code host was asked %d time(s), want exactly 1", requests)
	}
}

// TestWebhookIgnoresDeletions covers GitLab's deletion events: a branch or tag
// deletion carries the null object id as "after", and a merge request whose
// source branch is gone carries it as last_commit.id. Recording one would put
// a dashboard column on the matrix that no run can ever fill, and dispatching
// it would fail at the clone with an error that names no cause — so the event
// is acknowledged and dropped.
func TestWebhookIgnoresDeletions(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cpu-deletion", "cpu", true)

	post := func(t *testing.T, body, header string) map[string]any {
		t.Helper()
		rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", header)
		if rec.Code != http.StatusOK {
			t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if res["status"] != "ignored" {
			t.Fatalf("deletion should be ignored, got %v", res)
		}
		if _, ok := res["commitId"]; ok {
			t.Fatalf("deletion should record no commit: %v", res)
		}
		return res
	}

	// Branch deletion: the pushed ref is gone and "after" is the null SHA.
	deleted := strings.Replace(pushBody("group/code", nullSHA), "refs/heads/main", "refs/heads/gone", 1)
	if res := post(t, deleted, "Push Hook"); res["ref"] != "gone" {
		t.Fatalf("deletion ref: %v", res["ref"])
	}
	// Tag deletion.
	post(t, fmt.Sprintf(`{
		"object_kind": "tag_push",
		"project": {"name": "code", "path_with_namespace": "group/code", "web_url": "https://gitlab.com/group/code"},
		"ref": "refs/tags/v1.0",
		"before": %q,
		"after": %q,
		"user_name": "bob",
		"commits": []
	}`, nullSHA, nullSHA), "Tag Push Hook")
	// Merge request whose source branch is gone.
	post(t, fmt.Sprintf(`{
		"object_kind": "merge_request",
		"project": {"name": "code", "path_with_namespace": "group/code"},
		"user_name": "carol",
		"object_attributes": {
			"action": "open",
			"title": "gone",
			"source_branch": "fix/gone",
			"last_commit": {"id": %q, "message": "wip"}
		}
	}`, nullSHA), "Merge Request Hook")

	// None of the three left a trace: no commit column, no task graph.
	commits, err := s.ListCommits("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 0 {
		t.Fatalf("deletions recorded %d commit row(s): %+v", len(commits), commits)
	}
	roots, err := s.ListRootTasks(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Fatalf("deletions dispatched %d task graph(s)", len(roots))
	}
}

// TestWebhookFallsBackToTheEventHeader posts a payload without object_kind and
// relies on X-Gitlab-Event alone. The header names the event GitLab's own way
// ("Push Hook"), so it has to be translated into the payload vocabulary: taken
// literally it matches no case and the event is silently ignored.
func TestWebhookFallsBackToTheEventHeader(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cpu-header", "cpu", true)

	const sha = "5555555555555555555555555555555555555555"
	body := strings.Replace(pushBody("group/code", sha), `"object_kind": "push",`, "", 1)
	rec := postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res["event"] != "push" {
		t.Fatalf("header-only event kind: %v", res["event"])
	}
	if got, _ := res["jobsCreated"].(float64); got != 1 {
		t.Fatalf("header-only jobsCreated: want 1, got %v", res["jobsCreated"])
	}
	c, err := s.GetCommitByID(int64(res["commitId"].(float64)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Event != store.CommitEventPush || c.SHA != sha {
		t.Fatalf("header-only commit: %+v", c)
	}

	// An unrecognised header is still ignored (rather than dispatched as
	// whatever the header happened to say).
	rec = postWebhook(t, mux, s, `{"project": {"path_with_namespace": "group/code"}}`,
		"X-Gitlab-Event", "Pipeline Hook")
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode ignored: %v", err)
	}
	if res["status"] != "ignored" {
		t.Fatalf("unknown header should stay ignored: %v", res)
	}
}

// TestWebhookWithoutRunnerRecordsWhy checks both event paths explain an empty
// matrix row when the server has no runner: the commit is recorded, nothing is
// dispatched, and the reason lands on the commit row (the push path did this
// from the start, the merge request path did not).
func TestWebhookWithoutRunnerRecordsWhy(t *testing.T) {
	apiServer, s := newTestServer(t) // no SetRunner: dispatch is not configured
	mux := http.NewServeMux()
	apiServer.Register(mux)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, body, header string
	}{
		{"push", pushBody("group/code", "6666666666666666666666666666666666666666"), "Push Hook"},
		{"merge request", `{
			"object_kind": "merge_request",
			"project": {"name": "code", "path_with_namespace": "group/code"},
			"user_name": "carol",
			"object_attributes": {
				"action": "open",
				"title": "no runner",
				"source_branch": "fix/x",
				"last_commit": {"id": "7777777777777777777777777777777777777777", "message": "wip"}
			}
		}`, "Merge Request Hook"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postWebhook(t, mux, s, tc.body, "X-Gitlab-Event", tc.header)
			if rec.Code != http.StatusOK {
				t.Fatalf("webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
			}
			var res map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if res["dispatchSkipped"] != msgDispatchNotConfigured {
				t.Fatalf("dispatchSkipped = %v, want %q", res["dispatchSkipped"], msgDispatchNotConfigured)
			}
			c, err := s.GetCommitByID(int64(res["commitId"].(float64)))
			if err != nil {
				t.Fatal(err)
			}
			if c.DispatchError != msgDispatchNotConfigured {
				t.Fatalf("commit dispatch error = %q, want %q", c.DispatchError, msgDispatchNotConfigured)
			}
		})
	}
}
