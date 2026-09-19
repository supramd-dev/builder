package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSiteConfigAPIFlow(t *testing.T) {
	apiServer, _ := newTestServer(t)
	seedUser(t, apiServer.Store, "alice", "alice@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "alice", "s3cret")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}

	// Unauthenticated access is rejected.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/site-config", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth get: expected 401, got %d", rec.Code)
	}

	// Initial read returns an empty default config.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if cfg["codeRepo"] != "" {
		t.Fatalf("expected empty defaults, got %v", cfg)
	}

	// Validation: empty fields rejected.
	rec = authed(http.MethodPut, "/api/site-config", `{"codeRepo":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty update: expected 400, got %d", rec.Code)
	}

	// Repository hosts are NOT validated: self-hosted GitLab instances live
	// on arbitrary hosts, so any URL is accepted (including github.com).
	for _, repo := range []string{
		"https://gitlab.com/group/code",
		"https://gitlab.example.com/group/code", // self-hosted
		"git@gitlab.com:group/code.git",         // SSH
		"https://github.com/g/code",             // other platform, accepted
		"http://10.0.0.5/group/code",            // bare host, accepted
	} {
		rec = authed(http.MethodPut, "/api/site-config",
			fmt.Sprintf(`{"codeRepo":%q}`, repo))
		if rec.Code != http.StatusOK {
			t.Fatalf("repo %q: expected 200 (no host validation), got %d, body %s", repo, rec.Code, rec.Body.String())
		}
	}

	// Valid update round-trips.
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"git@gitlab.com:group/code.git"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if cfg["codeRepo"] != "git@gitlab.com:group/code.git" {
		t.Fatalf("update not echoed: %v", cfg)
	}

	// Re-read: persisted.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("re-get: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode re-get: %v", err)
	}
	if cfg["codeRepo"] != "git@gitlab.com:group/code.git" {
		t.Fatalf("expected persisted repo, got %v", cfg["codeRepo"])
	}

	// Timezone: invalid IANA names are rejected, valid ones round-trip, and
	// empty (browser-local) is the default.
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"git@gitlab.com:group/code.git","timezone":"Not/AZone"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad timezone: expected 400, got %d, body %s", rec.Code, rec.Body.String())
	}
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"git@gitlab.com:group/code.git","timezone":"Asia/Shanghai"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("timezone update: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatalf("decode timezone update: %v", err)
	}
	if cfg["timezone"] != "Asia/Shanghai" {
		t.Fatalf("timezone not echoed: %v", cfg["timezone"])
	}
	rec = authed(http.MethodGet, "/api/site-config", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["timezone"] != "Asia/Shanghai" {
		t.Fatalf("timezone not persisted: %v", cfg["timezone"])
	}
	// Clearing back to browser-local.
	rec = authed(http.MethodPut, "/api/site-config",
		`{"codeRepo":"git@gitlab.com:group/code.git","timezone":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("timezone clear: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["timezone"] != "" {
		t.Fatalf("timezone should clear to empty: %v", cfg["timezone"])
	}

	// Unsupported method is rejected.
	rec = authed(http.MethodDelete, "/api/site-config", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("delete: expected 405, got %d", rec.Code)
	}
}

func TestSiteConfigAccessToken(t *testing.T) {
	apiServer, _ := newTestServer(t)
	seedUser(t, apiServer.Store, "carol", "carol@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "carol", "s3cret")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}
	base := `{"codeRepo":"https://gitlab.com/g/code"`

	// Initial state: nothing set.
	rec := authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["accessTokenSet"] != false {
		t.Fatalf("expected no token, got %v", cfg)
	}

	// Set the token.
	rec = authed(http.MethodPut, "/api/site-config", base+`,"accessToken":"glpat-secret"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set token: %d body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["accessTokenSet"] != true {
		t.Fatalf("expected token set, got %v", cfg)
	}
	// The secret itself is never echoed.
	if strings.Contains(rec.Body.String(), "glpat-secret") {
		t.Fatalf("secret leaked in response: %s", rec.Body.String())
	}

	// Re-read: persisted, still masked.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["accessTokenSet"] != true {
		t.Fatalf("token not persisted: %v", cfg)
	}
	if strings.Contains(rec.Body.String(), "glpat-secret") {
		t.Fatalf("token leaked on read: %s", rec.Body.String())
	}

	// An update without the token keeps it (empty = keep).
	rec = authed(http.MethodPut, "/api/site-config", base+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update without token: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["accessTokenSet"] != true {
		t.Fatalf("token should be kept: %v", cfg)
	}

	// The explicit clear flag removes it.
	rec = authed(http.MethodPut, "/api/site-config", base+`,"clearAccessToken":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear token: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["accessTokenSet"] != false {
		t.Fatalf("token should be cleared: %v", cfg)
	}
}

func TestGitLabWebhook(t *testing.T) {
	apiServer, s := newTestServer(t)

	mux := http.NewServeMux()
	apiServer.Register(mux)

	post := func(headers map[string]string, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		mux.ServeHTTP(rec, req)
		return rec
	}
	authedPost := func(body string) *httptest.ResponseRecorder {
		return postWebhook(t, mux, s, body, "X-Gitlab-Event", "Push Hook")
	}

	// Non-POST is rejected.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/webhooks/gitlab", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET webhook: expected 405, got %d", rec.Code)
	}

	// The token is generated with the site config, so it is there from the
	// first event onward.
	cfg, err := s.GetSiteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebhookToken == "" {
		t.Fatal("webhook token should be generated with the site config")
	}

	// No token at all, and a wrong one: both rejected before the body is
	// even parsed (the payload here is valid, so a 400 would mean the order
	// is wrong).
	for _, tc := range []struct {
		name   string
		header map[string]string
	}{
		{"missing", nil},
		{"empty", map[string]string{"X-Gitlab-Token": ""}},
		{"wrong", map[string]string{"X-Gitlab-Token": cfg.WebhookToken + "x"}},
		{"prefix", map[string]string{"X-Gitlab-Token": cfg.WebhookToken[:8]}},
		{"secret-token-is-not-the-webhook-token", map[string]string{"X-Gitlab-Token": "MD_SECRET_TOKEN"}},
	} {
		rec = post(tc.header, `{"object_kind": "push", "project": {"path_with_namespace": "group/md-code"}}`)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s token: expected 401, got %d, body %s", tc.name, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), cfg.WebhookToken) {
			t.Fatalf("%s token: the token leaked in the response: %s", tc.name, rec.Body.String())
		}
	}

	// Malformed JSON is rejected (with a valid token, so the parser is what
	// rejects it).
	rec = authedPost(`{not-json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad payload: expected 400, got %d", rec.Code)
	}

	// A push event is accepted and recorded as a commit.
	push := `{
		"object_kind": "push",
		"project": {"name": "md-code", "path_with_namespace": "group/md-code", "web_url": "https://gitlab.com/group/md-code"},
		"ref": "refs/heads/main",
		"before": "0000000",
		"after": "9c8b7a6d5e4f",
		"user_name": "alice",
		"commits": [{"id": "9c8b7a6d5e4f", "message": "Fix integrator drift\n\nLonger body."}]
	}`
	rec = authedPost(push)
	if rec.Code != http.StatusOK {
		t.Fatalf("push webhook: expected 200, got %d, body %s", rec.Code, rec.Body.String())
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode webhook result: %v", err)
	}
	if res["status"] != "received" || res["event"] != "push" {
		t.Fatalf("unexpected webhook result: %v", res)
	}
	if res["project"] != "group/md-code" || res["ref"] != "main" {
		t.Fatalf("push details not extracted: %v", res)
	}
	commitID, ok := res["commitId"].(float64)
	if !ok || commitID <= 0 {
		t.Fatalf("expected positive commitId, got %v", res["commitId"])
	}
	if res["created"] != true {
		t.Fatalf("expected created=true for a new push, got %v", res["created"])
	}

	// The commit is persisted with the head commit's title.
	c, err := apiServer.Store.GetCommitByID(int64(commitID))
	if err != nil {
		t.Fatalf("load commit: %v", err)
	}
	if c.SHA != "9c8b7a6d5e4f" || c.Repo != "group/md-code" || c.Ref != "main" || c.Author != "alice" {
		t.Fatalf("unexpected commit: %+v", c)
	}
	if c.Message != "Fix integrator drift" {
		t.Fatalf("expected commit title as message, got %q", c.Message)
	}

	// The same push again is idempotent: same commit, created=false.
	rec = authedPost(push)
	if rec.Code != http.StatusOK {
		t.Fatalf("repeat push: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode repeat result: %v", err)
	}
	if res["created"] != false {
		t.Fatalf("expected created=false for repeat push, got %v", res["created"])
	}
	if res["commitId"].(float64) != commitID {
		t.Fatalf("expected same commitId, got %v", res["commitId"])
	}

	// Other event kinds are accepted but marked ignored.
	rec = postWebhook(t, mux, s, `{"object_kind":"pipeline"}`, "X-Gitlab-Event", "Pipeline Hook")
	if rec.Code != http.StatusOK {
		t.Fatalf("pipeline webhook: expected 200, got %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode ignored result: %v", err)
	}
	if res["status"] != "ignored" {
		t.Fatalf("expected ignored status, got %v", res)
	}
}

func TestSiteConfigSecretToken(t *testing.T) {
	apiServer, _ := newTestServer(t)
	seedUser(t, apiServer.Store, "carol", "carol@example.com", "s3cret")

	mux := http.NewServeMux()
	apiServer.Register(mux)
	cookie := loginAndGetCookie(t, mux, "carol", "s3cret")

	authed := func(method, target, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, target, nil)
		} else {
			req = httptest.NewRequest(method, target, strings.NewReader(body))
		}
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		mux.ServeHTTP(rec, req)
		return rec
	}
	base := `{"codeRepo":"https://gitlab.com/g/code"`

	// Initial state: nothing set.
	rec := authed(http.MethodGet, "/api/site-config", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get: %d", rec.Code)
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secretTokenSet"] != false {
		t.Fatalf("expected no secret token, got %v", cfg)
	}

	// Set the secret token (with shell metacharacters — it must survive
	// the export quoting).
	rec = authed(http.MethodPut, "/api/site-config", base+`,"secretToken":"s3cr't-$(pw)"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set secret: %d body %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secretTokenSet"] != true {
		t.Fatalf("expected secret set, got %v", cfg)
	}
	if strings.Contains(rec.Body.String(), "s3cr't-$(pw)") {
		t.Fatalf("secret leaked in response: %s", rec.Body.String())
	}

	// Re-read: persisted, still masked.
	rec = authed(http.MethodGet, "/api/site-config", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secretTokenSet"] != true {
		t.Fatalf("secret not persisted: %v", cfg)
	}
	if strings.Contains(rec.Body.String(), "s3cr't-$(pw)") {
		t.Fatalf("secret leaked on read: %s", rec.Body.String())
	}

	// An update without the field keeps it (empty = keep).
	rec = authed(http.MethodPut, "/api/site-config", base+`}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update without secret: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secretTokenSet"] != true {
		t.Fatalf("secret should be kept: %v", cfg)
	}

	// The explicit clear flag removes it.
	rec = authed(http.MethodPut, "/api/site-config", base+`,"clearSecretToken":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear secret: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["secretTokenSet"] != false {
		t.Fatalf("secret should be cleared: %v", cfg)
	}

	// The stored row itself carries the value while set (the runner's
	// scriptInput reads it there): set again and check the model.
	authed(http.MethodPut, "/api/site-config", base+`,"secretToken":"tok"}`)
	stored, err := apiServer.Store.GetSiteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if stored.SecretToken != "tok" {
		t.Fatalf("stored secret wrong: %q", stored.SecretToken)
	}
}

// TestSiteConfigWebhookToken covers the webhook secret: generated together
// with the site config, readable and rotatable by administrators only, kept
// apart from the secret token, and enforced by the webhook endpoint.
func TestSiteConfigWebhookToken(t *testing.T) {
	f := newUserFixture(t)
	adminCookie := f.login(t, "root", "root-pass")
	userCookie := f.login(t, "alice", "alice-pass")

	read := func(cookie *http.Cookie) map[string]any {
		t.Helper()
		rec := doJSON(t, f.mux, http.MethodGet, "/api/site-config", "", cookie)
		if rec.Code != http.StatusOK {
			t.Fatalf("get site config: %d, body %s", rec.Code, rec.Body.String())
		}
		var cfg map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("decode site config: %v", err)
		}
		return cfg
	}

	// The administrator is shown the generated token; a regular user only
	// learns the rest of the configuration.
	adminCfg := read(adminCookie)
	token, _ := adminCfg["webhookToken"].(string)
	if len(token) != 64 {
		t.Fatalf("expected a 64-char generated webhook token, got %q", token)
	}
	userCfg := read(userCookie)
	if v, ok := userCfg["webhookToken"]; ok && v != "" {
		t.Fatalf("the webhook token must not reach a regular user: %v", v)
	}
	// The secret token stays write-only for everyone: the two are separate
	// secrets and only one of them is readable.
	if v, ok := userCfg["secretToken"]; ok && v != "" {
		t.Fatalf("the secret token must stay write-only: %v", v)
	}
	if adminCfg["secretToken"] != nil {
		t.Fatalf("the secret token must stay write-only for administrators too: %v", adminCfg["secretToken"])
	}

	// Stable across reads: it is generated once, not per request.
	if again, _ := read(adminCookie)["webhookToken"].(string); again != token {
		t.Fatalf("webhook token changed between reads: %q -> %q", token, again)
	}

	// A regular user cannot rotate it — and cannot reach the endpoint at
	// all (requireAdmin rejects before the handler runs).
	rec := doJSON(t, f.mux, http.MethodPost, "/api/site-config/webhook-token", "", userCookie)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("regular user rotate: expected 403, got %d, body %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, f.mux, http.MethodPost, "/api/site-config/webhook-token", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous rotate: expected 401, got %d, body %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, f.mux, http.MethodGet, "/api/site-config/webhook-token", "", adminCookie)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET rotate: expected 405, got %d", rec.Code)
	}
	if after, _ := read(adminCookie)["webhookToken"].(string); after != token {
		t.Fatalf("denied rotation still changed the token: %q", after)
	}

	// The endpoint enforces the stored token before anything else.
	post := func(tok string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/gitlab",
			strings.NewReader(`{"object_kind":"pipeline"}`))
		req.Header.Set("X-Gitlab-Token", tok)
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(token); code != http.StatusOK {
		t.Fatalf("webhook with the current token: expected 200, got %d", code)
	}
	if code := post("nope"); code != http.StatusUnauthorized {
		t.Fatalf("webhook with a wrong token: expected 401, got %d", code)
	}

	// An administrator rotates it: the response carries the new value, the
	// old one stops working, the new one works.
	rec = doJSON(t, f.mux, http.MethodPost, "/api/site-config/webhook-token", "", adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d, body %s", rec.Code, rec.Body.String())
	}
	var rotated struct {
		WebhookToken string `json:"webhookToken"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.WebhookToken == token || len(rotated.WebhookToken) != 64 {
		t.Fatalf("rotation should produce a fresh token, got %q", rotated.WebhookToken)
	}
	stored, err := f.store.GetSiteConfig()
	if err != nil {
		t.Fatal(err)
	}
	if stored.WebhookToken != rotated.WebhookToken {
		t.Fatalf("rotated token not persisted: %q != %q", stored.WebhookToken, rotated.WebhookToken)
	}
	if code := post(token); code != http.StatusUnauthorized {
		t.Fatalf("the old token should be rejected after rotation, got %d", code)
	}
	if code := post(rotated.WebhookToken); code != http.StatusOK {
		t.Fatalf("the new token should be accepted, got %d", code)
	}

	// A plain config update keeps the token.
	rec = doJSON(t, f.mux, http.MethodPut, "/api/site-config",
		`{"codeRepo":"https://gitlab.com/group/other"}`, adminCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("plain update: %d", rec.Code)
	}
	cfg := read(adminCookie)
	if cfg["webhookToken"] != rotated.WebhookToken {
		t.Fatalf("a plain update rotated the token: %v", cfg["webhookToken"])
	}
	if cfg["codeRepo"] != "https://gitlab.com/group/other" {
		t.Fatalf("code repo not updated: %v", cfg["codeRepo"])
	}
}
