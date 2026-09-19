package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		subs, err := s.ListSubTasks(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(subs) == 0 {
			t.Fatalf("root %d has no sub-tasks", root.ID)
		}
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
	seedUser(t, s, "eventuser", "event@example.com", "pw")
	env := seedDispatchEnv(t, s, "cpu-mev", "cpu", true)

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
