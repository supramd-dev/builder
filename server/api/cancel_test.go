package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"md-builder/server/store"
)

// cancelResult is what POST /api/tasks/{id}/cancel answers with.
type cancelResult struct {
	TaskID    int64  `json:"taskId"`
	Cancelled int    `json:"cancelled"`
	Aborted   int    `json:"aborted"`
	Summary   string `json:"summary"`
}

// TestCancelTaskStopsTheWorkItNames walks the manual cancellation end to end:
// one stage on its own, then the whole regression container, plus the answers
// for a task that has nothing left to cancel, an unknown task, the wrong
// method and no session at all.
//
// The caller is deliberately not the environment's owner: cancelling is not
// gated on owning anything — any signed-in user may stop a run.
func TestCancelTaskStopsTheWorkItNames(t *testing.T) {
	apiServer, s := newDispatchTestServer(t, dispatchYAML)
	mux := http.NewServeMux()
	apiServer.Register(mux)
	if err := s.SaveSiteConfig(&store.SiteConfig{ID: 1,
		CodeRepo: "https://gitlab.com/group/code"}); err != nil {
		t.Fatal(err)
	}
	seedDispatchEnv(t, s, "cancel-api", "cpu", true)

	rec := postWebhook(t, mux, s, pushBody("group/code", "cancel1234"), "X-Gitlab-Event", "Push Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook: %d %s", rec.Code, rec.Body.String())
	}
	roots, err := s.ListRootTasks(10)
	if err != nil || len(roots) != 1 {
		t.Fatalf("root tasks: %d (%v), want the one graph", len(roots), err)
	}
	root := roots[0]
	nodes, err := s.ListActiveNodes(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	byKind := map[string]*store.Task{}
	for i := range nodes {
		byKind[nodes[i].Kind] = &nodes[i]
	}
	clone, unit := byKind[store.TaskKindClone], byKind[store.TaskKindUnit]
	stage := byKind[store.TaskKindRegressionStage]
	if clone == nil || unit == nil || stage == nil {
		t.Fatalf("the graph is missing clone/unit/regression: %v", byKind)
	}
	cases, err := s.ListChildren(stage.ID)
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases of the regression container: %d (%v), want the cpu entry's one", len(cases), err)
	}
	kase := &cases[0]

	// The environment belongs to somebody who cannot even log in: the caller
	// below is a plain user with no relationship to it.
	seedUser(t, s, "carol", "carol@example.com", "s3cret")
	cookie := &http.Cookie{Name: sessionCookie, Value: loginAndGetCookie(t, mux, "carol", "s3cret")}
	cancel := func(id int64) (*httptest.ResponseRecorder, cancelResult) {
		t.Helper()
		rec := doJSON(t, mux, http.MethodPost, "/api/tasks/"+strconv.FormatInt(id, 10)+"/cancel", "", cookie)
		var res cancelResult
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
				t.Fatalf("decode cancel result: %v", err)
			}
		}
		return rec, res
	}

	// Let clone finish: a task that has reached an outcome has nothing to
	// cancel, and the caller is told so rather than given a silent success.
	if _, err := s.ClaimReadyTask(); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, err := s.FinishAttempt(clone.ID, store.AttemptResult{
		Status: store.StatusPassed, Attempt: clone.Attempts, Total: 1, Passed: 1,
	}); err != nil {
		t.Fatalf("finish clone: %v", err)
	}
	rec, _ = cancel(clone.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancelling a finished task: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if !containsAll(rec.Body.String(), "nothing to cancel", "unfinished") {
		t.Fatalf("the refusal does not say why: %s", rec.Body.String())
	}

	// A leaf: the unit stage alone goes, and the regression group beside it does
	// not.
	rec, res := cancel(unit.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel the unit stage: %d %s", rec.Code, rec.Body.String())
	}
	if res.TaskID != unit.ID || res.Cancelled != 1 || res.Aborted != 0 {
		t.Fatalf("cancelling the unit stage answered %+v", res)
	}
	if res.Summary != "cancelled by carol" {
		t.Fatalf("the cancellation is summarised as %q, want the caller named", res.Summary)
	}
	gone, err := s.GetTask(unit.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.Status != store.StatusCancelled || gone.Summary != "cancelled by carol" {
		t.Fatalf("the cancelled stage is %q/%q", gone.Status, gone.Summary)
	}
	for _, n := range []*store.Task{stage, kase} {
		other, err := s.GetTask(n.ID)
		if err != nil {
			t.Fatal(err)
		}
		if other.Status != store.StatusPending {
			t.Fatalf("node %q beside the cancelled stage became %q", other.NodeKey, other.Status)
		}
	}

	// The container: every case under it goes, the one that never started
	// included, and the graph above is left queued rather than painted
	// cancelled — clone has not run what is still queued behind it.
	rec, res = cancel(stage.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel the container: %d %s", rec.Code, rec.Body.String())
	}
	if res.TaskID != stage.ID || res.Cancelled != 2 {
		t.Fatalf("cancelling the container answered %+v, want the container and its case", res)
	}
	dropped, err := s.GetTask(kase.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Status != store.StatusCancelled || dropped.Summary != "cancelled by carol" {
		t.Fatalf("the case under the container is %q/%q", dropped.Status, dropped.Summary)
	}
	container, err := s.GetTask(stage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if container.Status != store.StatusCancelled {
		t.Fatalf("the cancelled container reads %q", container.Status)
	}
	// The run of the cancelled case's attempt is closed with it: the run detail
	// page has a cancelled run to show, not one that still says it is queued.
	caseRun, err := s.FindTaskRun(kase.ID, dropped.Attempts)
	if err != nil {
		t.Fatal(err)
	}
	if caseRun.Status != store.StatusCancelled || caseRun.Summary != "cancelled by carol" {
		t.Fatalf("the cancelled case's run is %q/%q", caseRun.Status, caseRun.Summary)
	}
	queued, err := s.GetTask(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Status != store.StatusPending {
		t.Fatalf("the graph with work still queued reads %q", queued.Status)
	}

	// The same container again: nothing is left to stop.
	rec, _ = cancel(stage.ID)
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancelling the container twice: %d %s, want 409", rec.Code, rec.Body.String())
	}
	// And the case is not revived by the second press.
	again, err := s.GetTask(kase.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != store.StatusCancelled || again.Summary != "cancelled by carol" {
		t.Fatalf("the second cancel rewrote the case: %q/%q", again.Status, again.Summary)
	}

	// Routing: cancelling is a POST, an unknown task does not exist, and the
	// session is required.
	if rec := doJSON(t, mux, http.MethodGet, "/api/tasks/"+strconv.FormatInt(unit.ID, 10)+"/cancel", "", cookie); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET on /cancel: %d, want 405", rec.Code)
	}
	if rec := doJSON(t, mux, http.MethodPost, "/api/tasks/999999/cancel", "", cookie); rec.Code != http.StatusNotFound {
		t.Fatalf("cancelling an unknown task: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, mux, http.MethodPost, "/api/tasks/"+strconv.FormatInt(root.ID, 10)+"/cancel", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("cancelling without a session: %d, want 401", rec.Code)
	}
}

// containsAll reports whether s contains every fragment — the refusal message
// is prose, so the test checks for the words that carry its meaning.
func containsAll(s string, fragments ...string) bool {
	for _, f := range fragments {
		if !strings.Contains(s, f) {
			return false
		}
	}
	return true
}
