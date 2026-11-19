package performance

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"md-builder/server/api"
	"md-builder/server/storage"
	"md-builder/server/store"
)

// newTestServer opens an in-memory store with an in-memory artifact backend
// and mounts the module the way main does — behind the API's own
// authentication, which is the part of the wiring worth exercising.
func newTestServer(t *testing.T) (*store.Store, *http.ServeMux) {
	t.Helper()
	st, err := store.Open("file::memory:?cache=shared", store.WithObjects(storage.NewMemory()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	mux := http.NewServeMux()
	New(st).Register(mux, api.New(st).AuthMiddleware)
	return st, mux
}

// seedSession creates an account with a live session and returns its token.
func seedSession(t *testing.T, st *store.Store) string {
	t.Helper()
	u := &store.User{Username: "perf", Email: "perf@example.com", PasswordHash: "hash"}
	if err := st.CreateUser(u); err != nil {
		t.Fatalf("create user: %v", err)
	}
	const token = "perf-session"
	err := st.CreateSession(&store.Session{
		Token: token, UserID: u.ID,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return token
}

// seedEnv creates a test environment owned by the seeded account.
func seedEnv(t *testing.T, st *store.Store, name string) *store.TestEnvironment {
	t.Helper()
	env := &store.TestEnvironment{OwnerID: ownerOf(t, st).ID, Name: name, Host: "host", Username: "user", PrivateKey: "key"}
	if err := st.CreateEnvironment(env); err != nil {
		t.Fatalf("create environment: %v", err)
	}
	return env
}

// seedRun dispatches a performance stage for one (environment, commit) pair and
// finishes it with the given series artifacts, the way the runner does after a
// benchmark. PushedAt is when the commit arrived, which is what the window
// filters on.
func seedRun(t *testing.T, st *store.Store, env *store.TestEnvironment, sha string, pushedAt time.Time, files map[string]string) {
	t.Helper()
	commit := &store.Commit{Repo: "group/code", SHA: sha, Ref: "main", PushedAt: pushedAt}
	if _, err := st.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("create commit: %v", err)
	}

	root := &store.Task{Kind: store.TaskKindRoot, Name: "root", CommitID: commit.ID, EnvironmentID: env.ID, Config: "{}"}
	nodes := []store.TaskNode{{Task: &store.Task{
		Kind: store.TaskKindPerformance, NodeKey: store.TaskKindPerformance, Name: "performance",
		CommitID: commit.ID, EnvironmentID: env.ID, Config: "{}",
	}}}
	stored, err := st.UpsertTaskGraph(root, nodes)
	if err != nil {
		t.Fatalf("dispatch graph: %v", err)
	}
	task := stored[1]

	artifacts := make([]store.ArtifactInput, 0, len(files))
	for name, content := range files {
		artifacts = append(artifacts, store.ArtifactInput{Kind: store.ArtifactKindSeries, Name: name, Content: content})
	}
	if _, err := st.FinishAttempt(task.ID, store.AttemptResult{Status: store.StatusPassed, Artifacts: artifacts}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
}

// get calls the history route, with the session cookie when token is set.
func get(mux *http.ServeMux, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.AddCookie(&http.Cookie{Name: "md_session", Value: token})
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// historyResponse mirrors the endpoint's JSON, so decoding it also checks the
// field names the page reads.
type historyResponse struct {
	Since        time.Time `json:"since"`
	GeneratedAt  time.Time `json:"generatedAt"`
	Truncated    bool      `json:"truncated"`
	Environments []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"environments"`
	Cases []struct {
		Name    string `json:"name"`
		Metrics []struct {
			Name   string `json:"name"`
			Unit   string `json:"unit"`
			Better string `json:"better"`
		} `json:"metrics"`
	} `json:"cases"`
	Points []struct {
		Case          string    `json:"case"`
		Metric        string    `json:"metric"`
		Unit          string    `json:"unit"`
		Better        string    `json:"better"`
		EnvironmentID int64     `json:"environmentId"`
		Environment   string    `json:"environment"`
		At            time.Time `json:"at"`
		CommitID      int64     `json:"commitId"`
		SHA           string    `json:"sha"`
		Samples       int       `json:"samples"`
		Value         float64   `json:"value"`
	} `json:"points"`
	Ignored []struct {
		ArtifactID int64  `json:"artifactId"`
		RunID      int64  `json:"runId"`
		Name       string `json:"name"`
		Reason     string `json:"reason"`
	} `json:"ignored"`
}

func decodeHistory(t *testing.T, rec *httptest.ResponseRecorder) historyResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body: %s", rec.Code, rec.Body.String())
	}
	var out historyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	return out
}

func TestHistoryRequiresASession(t *testing.T) {
	st, mux := newTestServer(t)
	token := seedSession(t, st)

	if rec := get(mux, "/api/performance", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: status %d, want 401", rec.Code)
	}
	// The route is read-only: the session lets the method check answer.
	req := httptest.NewRequest(http.MethodPost, "/api/performance", nil)
	req.AddCookie(&http.Cookie{Name: "md_session", Value: token})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST: status %d, want 405", rec.Code)
	}
}

func TestHistoryServesAggregatedPoints(t *testing.T) {
	st, mux := newTestServer(t)
	token := seedSession(t, st)

	now := time.Now().UTC()
	env := seedEnv(t, st, "cpu-node-1")
	seedRun(t, st, env, "aaaa111", now.AddDate(0, 0, -2), map[string]string{
		"perf.json": `{"schema":1,"date":"` + now.AddDate(0, 0, -2).Format(time.RFC3339) + `","cases":[
			{"name":"nvt-1000","metrics":{"throughput":{"value":100,"unit":"step/s"}}}]}`,
		"bench.log": "all good\n",
	})
	seedRun(t, st, env, "bbbb222", now, map[string]string{
		"perf.json": `{"schema":1,"date":"` + now.Format(time.RFC3339) + `","cases":[
			{"name":"nvt-1000","metrics":{"throughput":{"value":150,"unit":"step/s"}}}]}`,
	})

	out := decodeHistory(t, get(mux, "/api/performance", token))

	// The log is not a report and is dropped without a word.
	if len(out.Ignored) != 0 {
		t.Fatalf("ignored = %+v, want none", out.Ignored)
	}
	if len(out.Points) != 2 {
		t.Fatalf("points = %+v, want 2", out.Points)
	}
	first, second := out.Points[0], out.Points[1]
	if first.Value != 100 || second.Value != 150 {
		t.Fatalf("points are not in chronological order: %+v", out.Points)
	}
	if first.Case != "nvt-1000" || first.Metric != "throughput" || first.Unit != "step/s" || first.Better != BetterHigher {
		t.Fatalf("point labels = %+v", first)
	}
	if first.Environment != "cpu-node-1" || first.EnvironmentID != env.ID {
		t.Fatalf("point environment = %+v, want %s (%d)", first, env.Name, env.ID)
	}
	if first.SHA != "aaaa111" || first.Samples != 1 {
		t.Fatalf("point provenance = %+v", first)
	}
	if len(out.Environments) != 1 || out.Environments[0].Name != "cpu-node-1" {
		t.Fatalf("environments = %+v", out.Environments)
	}
	if len(out.Cases) != 1 || len(out.Cases[0].Metrics) != 1 || out.Cases[0].Metrics[0].Unit != "step/s" {
		t.Fatalf("cases = %+v", out.Cases)
	}
	if out.Truncated {
		t.Fatal("truncated on a two-commit history")
	}
	if out.Since.IsZero() || out.GeneratedAt.IsZero() {
		t.Fatalf("missing window stamps: %+v", out)
	}
	if want := now.AddDate(0, 0, -defaultDays); out.Since.After(want.Add(time.Minute)) {
		t.Fatalf("since = %v, want about %v", out.Since, want)
	}
}

func TestHistoryWindowAndEnvironmentFilter(t *testing.T) {
	st, mux := newTestServer(t)
	token := seedSession(t, st)

	now := time.Now().UTC()
	old := seedEnv(t, st, "old-node")
	seedRun(t, st, old, "cccc333", now.AddDate(0, 0, -40), map[string]string{
		"perf.json": report(now.AddDate(0, 0, -40), "nvt", "throughput", 7),
	})
	recent := seedEnv(t, st, "new-node")
	seedRun(t, st, recent, "dddd444", now.AddDate(0, 0, -1), map[string]string{
		"perf.json": report(now.AddDate(0, 0, -1), "nvt", "throughput", 9),
	})

	// The window is measured from the commit's arrival, so a commit older than
	// the requested days is out entirely.
	out := decodeHistory(t, get(mux, "/api/performance?days=7", token))
	if len(out.Points) != 1 || out.Points[0].Value != 9 {
		t.Fatalf("days=7: points = %+v, want only the recent commit", out.Points)
	}

	// The environment filter is the id, the way the page's selector sends it.
	out = decodeHistory(t, get(mux, "/api/performance?environment="+strconv.FormatInt(old.ID, 10), token))
	if len(out.Points) != 1 || out.Points[0].EnvironmentID != old.ID {
		t.Fatalf("environment=%d: points = %+v, want only that environment", old.ID, out.Points)
	}
	// Without a filter both are there, in environment-name order.
	out = decodeHistory(t, get(mux, "/api/performance", token))
	if len(out.Points) != 2 || out.Points[0].EnvironmentID != recent.ID {
		t.Fatalf("both environments: points = %+v, want both, name-ordered", out.Points)
	}
}

func TestHistoryRejectsBadParameters(t *testing.T) {
	st, mux := newTestServer(t)
	token := seedSession(t, st)

	for _, path := range []string{
		"/api/performance?days=0",
		"/api/performance?days=-5",
		"/api/performance?days=abc",
		"/api/performance?days=99999",
		"/api/performance?environment=0",
		"/api/performance?environment=abc",
	} {
		rec := get(mux, path, token)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400 (body %s)", path, rec.Code, rec.Body.String())
		}
		if body := rec.Body.String(); !contains(body, `"error"`) {
			t.Fatalf("%s: body %s has no error message", path, body)
		}
	}
}

func TestHistoryListsBrokenReports(t *testing.T) {
	st, mux := newTestServer(t)
	token := seedSession(t, st)
	now := time.Now().UTC()
	seedRun(t, st, seedEnv(t, st, "cpu-node-1"), "eeee555", now, map[string]string{
		"perf.json": report(now, "nvt", "throughput", 42),
		"typo.json": `{"schema":1,"cases":[]}`,
		"notes.txt": "not json at all",
	})

	out := decodeHistory(t, get(mux, "/api/performance", token))
	if len(out.Points) != 1 {
		t.Fatalf("points = %+v, want the one readable report", out.Points)
	}
	if len(out.Ignored) != 1 {
		t.Fatalf("ignored = %+v, want the malformed report", out.Ignored)
	}
	if out.Ignored[0].Name != "typo.json" || out.Ignored[0].RunID == 0 {
		t.Fatalf("ignored entry = %+v, want the artifact's name and run", out.Ignored[0])
	}
	if !contains(out.Ignored[0].Reason, "date is required") {
		t.Fatalf("ignored reason = %q", out.Ignored[0].Reason)
	}
}

// ownerOf returns the account seedSession created, for the environments that
// need an owner.
func ownerOf(t *testing.T, st *store.Store) *store.User {
	t.Helper()
	u, err := st.GetUserByUsername("perf")
	if err != nil {
		t.Fatalf("load the seeded user: %v", err)
	}
	return u
}
