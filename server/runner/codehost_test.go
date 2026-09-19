package runner

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newFileFetchFixture starts a fake code host answering with handler, and
// returns the fetcher under test, the repository URL to point it at, and the
// two things a test needs to observe: how often the clone fallback ran, and
// what the fake host was actually asked for.
func newFileFetchFixture(t *testing.T, handler http.HandlerFunc) (fetch YAMLFetcher, repoURL string, fallbackCalls *int, requests *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	calls := 0
	fallback := func(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		calls++
		return []byte("from clone"), nil
	}
	return newYAMLFetcher(srv.Client(), fallback), srv.URL + "/group/code", &calls, &seen
}

// The point of the whole exercise: one small request, no clone.
func TestFileFetchReadsFileWithoutCloning(t *testing.T) {
	const yaml = "version: 2\nmatrix: []\n"
	const sha = "21c8dc33c771d5002df19de1cc71bb5a0c87568e"
	fetch, repo, fallbackCalls, requests := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(yaml))
	})

	content, err := fetch(context.Background(), repo, sha, &GitCredentials{AccessToken: "glpat-secret"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(content) != yaml {
		t.Fatalf("content = %q, want %q", content, yaml)
	}
	if *fallbackCalls != 0 {
		t.Fatalf("clone fallback ran %d time(s) on a successful fetch", *fallbackCalls)
	}
	if len(*requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(*requests))
	}
	req := (*requests)[0]
	// The project must arrive as ONE escaped path segment: GitLab's API route
	// takes "group%2Fcode", and a real "group/code" would be read as a
	// different (nonexistent) route.
	const wantEscaped = "/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw"
	if got := req.URL.EscapedPath(); got != wantEscaped {
		t.Errorf("escaped path = %q, want %q", got, wantEscaped)
	}
	if got := req.URL.RawQuery; got != "ref="+sha {
		t.Errorf("query = %q, want ref=%s", got, sha)
	}
	if got := req.Header.Get("PRIVATE-TOKEN"); got != "glpat-secret" {
		t.Errorf("PRIVATE-TOKEN = %q, want the token", got)
	}
	// The token belongs in the header only — never in the URL, where it would
	// end up in logs and error strings.
	if strings.Contains(req.URL.String(), "glpat-secret") {
		t.Errorf("token leaked into the URL: %s", req.URL)
	}
}

// A public repository sends no credential at all.
func TestFileFetchWithoutTokenSendsNoAuth(t *testing.T) {
	fetch, repo, _, requests := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("version: 2\n"))
	})

	if _, err := fetch(context.Background(), repo, "abc", nil); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	req := (*requests)[0]
	if got := req.Header.Get("PRIVATE-TOKEN"); got != "" {
		t.Errorf("PRIVATE-TOKEN = %q, want none", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

// Anything that is not the file falls back to the clone, which answers
// precisely: the clone can tell "file not found at this commit" from "project
// unreadable", where a failed route often cannot.
func TestFileFetchFallsBackOnFailure(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"not found", http.StatusNotFound, `{"message":"404 File Not Found"}`},
		{"unauthorized", http.StatusUnauthorized, `{"message":"401 Unauthorized"}`},
		{"forbidden", http.StatusForbidden, `{"error":"insufficient_scope"}`},
		{"server error", http.StatusInternalServerError, "boom"},
		{"bad gateway", http.StatusBadGateway, "boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch, repo, fallbackCalls, _ := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			content, err := fetch(context.Background(), repo, "abc", &GitCredentials{AccessToken: "glpat-secret"})
			if err != nil {
				t.Fatalf("fetch: %v (the fallback should have covered it)", err)
			}
			if string(content) != "from clone" {
				t.Fatalf("content = %q, want the clone's", content)
			}
			if *fallbackCalls != 1 {
				t.Fatalf("clone fallback ran %d time(s), want 1", *fallbackCalls)
			}
		})
	}
}

// The trap the file route sets: a host that does not accept the token on it
// answers with a redirect to its sign-in page, which the client follows — so
// the fetch sees a perfectly successful 200 carrying HTML. Accepting that
// would hand the yaml parser a login page and never reach the fallback, which
// is strictly worse than not having the fast path at all.
func TestFileFetchRejectsSignInPage(t *testing.T) {
	var host *httptest.Server
	host = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/users/sign_in" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html><body>You are being redirected.</body></html>"))
			return
		}
		http.Redirect(w, r, host.URL+"/users/sign_in", http.StatusFound)
	}))
	defer host.Close()

	calls := 0
	fallback := func(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		calls++
		return []byte("from clone"), nil
	}
	content, err := newYAMLFetcher(host.Client(), fallback)(context.Background(),
		host.URL+"/group/code", "abc", &GitCredentials{AccessToken: "glpat-secret"})
	if err != nil {
		t.Fatalf("fetch: %v (the fallback should have covered it)", err)
	}
	if string(content) != "from clone" {
		t.Fatalf("content = %q, want the clone's", content)
	}
	if calls != 1 {
		t.Fatalf("clone fallback ran %d time(s), want 1", calls)
	}
}

// A response that is not the file must not reach the yaml parser, however
// well-formed the request was.
func TestFileFetchRejectsOversizedResponse(t *testing.T) {
	fetch, repo, fallbackCalls, _ := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), maxFileBytes+1))
	})

	content, err := fetch(context.Background(), repo, "abc", &GitCredentials{AccessToken: "glpat-secret"})
	if err != nil {
		t.Fatalf("fetch: %v (the fallback should have covered it)", err)
	}
	if string(content) != "from clone" {
		t.Fatalf("content = %q, want the clone's", content)
	}
	if *fallbackCalls != 1 {
		t.Fatalf("clone fallback ran %d time(s), want 1", *fallbackCalls)
	}
}

// A repository location the template cannot be built from is not an error the
// dispatch should fail on: it is the clone's turn.
func TestFileFetchFallsBackOnUnusableRepoURL(t *testing.T) {
	cases := []struct {
		name string
		repo string
	}{
		{"bare path", "group/code"},
		{"empty", ""},
		{"no project", "https://gitlab.example.com"},
		{"unknown scheme", "ftp://gitlab.example.com/group/code"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fetch, _, fallbackCalls, requests := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("the fake host was asked for %s; no request was expected", r.URL)
			})

			if _, err := fetch(context.Background(), tc.repo, "abc", nil); err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if len(*requests) != 0 {
				t.Fatalf("requests = %d, want 0", len(*requests))
			}
			if *fallbackCalls != 1 {
				t.Fatalf("clone fallback ran %d time(s), want 1", *fallbackCalls)
			}
		})
	}
}

// The fallback is silent to the caller but not to the operator: the log has to
// say why a dispatch was slow — and the token must not be in it.
func TestFileFetchLogsFallbackWithoutToken(t *testing.T) {
	fetch, repo, _, _ := newFileFetchFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 File Not Found"}`))
	})

	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(previous)

	if _, err := fetch(context.Background(), repo, "abc", &GitCredentials{AccessToken: "glpat-secret"}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "falling back to a full clone") {
		t.Errorf("log = %q, want the fallback notice", out)
	}
	if !strings.Contains(out, "404") {
		t.Errorf("log = %q, want the status that caused it", out)
	}
	// GitLab's own wording is worth keeping: it separates a missing file from
	// an unreadable project, which the status alone does not.
	if !strings.Contains(out, "404 File Not Found") {
		t.Errorf("log = %q, want the host's message", out)
	}
	if strings.Contains(out, "glpat-secret") {
		t.Errorf("log leaks the token: %q", out)
	}
}

// A code host that keeps its files in object storage answers the file route
// with a redirect to a pre-signed URL on another host. The fetch must survive
// that — and must not hand the token to the storage provider on the way.
func TestFileFetchDropsTokenOnCrossHostRedirect(t *testing.T) {
	var tokenAtStorage string
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenAtStorage = r.Header.Get("PRIVATE-TOKEN")
		_, _ = w.Write([]byte("version: 2\n"))
	}))
	defer storage.Close()

	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "glpat-secret" {
			t.Errorf("the code host itself got PRIVATE-TOKEN %q, want the token", got)
		}
		http.Redirect(w, r, storage.URL+"/presigned/md-builder.yaml?sig=xyz", http.StatusFound)
	}))
	defer host.Close()

	content, err := newYAMLFetcher(fileClient(), GitYAMLFetcher)(context.Background(),
		host.URL+"/group/code", "abc", &GitCredentials{AccessToken: "glpat-secret"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(content) != "version: 2\n" {
		t.Fatalf("content = %q, want the file from the redirect target", content)
	}
	if tokenAtStorage != "" {
		t.Fatalf("the token was forwarded to the redirect target: %q", tokenAtStorage)
	}
}

// The template expansion, including the repository forms the site config
// accepts (the clone normalises them the same way).
func TestFileURL(t *testing.T) {
	host, ok := hostForRepo()
	if !ok {
		t.Fatal("no code host is configured; NewYAMLFetcher would fall back to cloning forever")
	}
	const sha = "21c8dc33c771d5002df19de1cc71bb5a0c87568e"
	cases := []struct {
		name   string
		repo   string
		ref    string
		want   string
		wantOK bool
	}{
		{"https", "https://gitlab.example.com/group/code", sha, "https://gitlab.example.com/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"dot git suffix", "https://gitlab.example.com/group/code.git", sha, "https://gitlab.example.com/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"trailing slash", "https://gitlab.example.com/group/code/", sha, "https://gitlab.example.com/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"nested group", "https://gitlab.example.com/grp/sub/code", sha, "https://gitlab.example.com/api/v4/projects/grp%2Fsub%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"port", "https://gitlab.example.com:8443/group/code", sha, "https://gitlab.example.com:8443/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"scp form", "git@gitlab.example.com:group/code.git", sha, "https://gitlab.example.com/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"ssh form", "ssh://git@gitlab.example.com:2222/group/code.git", sha, "https://gitlab.example.com/api/v4/projects/group%2Fcode/repository/files/md-builder.yaml/raw?ref=" + sha, true},
		{"bare path", "group/code", sha, "", false},
		{"no project", "https://gitlab.example.com", sha, "", false},
		{"unknown scheme", "ftp://gitlab.example.com/group/code", sha, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := fileURL(host, tc.repo, tc.ref, YAMLPath)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("url = %q, want %q", got, tc.want)
			}
		})
	}
}
