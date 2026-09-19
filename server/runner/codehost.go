package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file reads the test matrix straight out of the code host instead of
// cloning the repository: every platform serves the raw bytes of a file at a
// given ref over one small HTTP request, which replaces a full clone. That
// matters because the caller is usually a webhook, and GitLab gives a webhook
// ten seconds to answer (see GitYAMLFetcher for the fallback).

// codeHost describes how one platform serves a single file at a given ref.
// Platforms differ in the shape of that route and in where the access token
// goes, so both are data: adding a platform is an entry in codeHosts, not a
// branch in the fetch code.
type codeHost struct {
	// Name identifies the platform in logs and errors.
	Name string

	// URLTemplate is the file route, with placeholders
	//
	//	{base}        https://host
	//	{project}     group/sub/project, URL-escaped (safe as one path segment)
	//	{project_raw} the same, verbatim
	//	{path}        file path, URL-escaped
	//	{path_raw}    the same, verbatim
	//	{ref}         branch, tag or commit SHA
	//
	// The escaped and verbatim forms both exist because the two route shapes
	// need opposite things: an API route that takes the project as a single
	// path segment wants "group%2Fcode", a raw-file route that takes it as a
	// real path wants "group/code". The template says which, so no per-platform
	// branch is needed. {ref} is not escaped: it is a commit SHA here.
	URLTemplate string

	// TokenHeader is the request header carrying the access token. Empty
	// means the platform serves the file anonymously.
	TokenHeader string

	// TokenScheme prefixes the token inside TokenHeader ("Bearer"); empty
	// means the header holds the bare token.
	TokenScheme string

	// MissingMarker is the text the platform puts in a 404 body when the file
	// is not in the repository, as opposed to a project the token cannot see.
	// A 404 carrying it is the one failure with nothing for a clone to find, so
	// the fallback is skipped. A platform that does not distinguish the two
	// leaves this empty and always falls back — which is also what happens if
	// the platform ever rewords it, so a wrong marker costs a clone, never a
	// wrong answer.
	MissingMarker string
}

// codeHosts is the platform table. Only the platforms actually verified to
// serve a file this way are listed: an entry that guesses the route would turn
// every dispatch into a failed request plus the clone it was meant to avoid.
var codeHosts = map[string]codeHost{
	"gitlab": {
		Name: "gitlab",
		// GitLab's repository-files API, not the web route /-/raw/: the web
		// route authenticates through the browser session alone, so a request
		// carrying a token is answered with a redirect to the sign-in page
		// (verified against git.hpcer.dev — the token is ignored there however
		// it is passed). The API route takes the same read_repository-scoped
		// token the clone already uses, and reading a file needs no read_api
		// scope on top of it.
		URLTemplate: "{base}/api/v4/projects/{project}/repository/files/{path}/raw?ref={ref}",
		TokenHeader: "PRIVATE-TOKEN",
		// "404 File Not Found" is GitLab's word for a missing file; a project
		// the token cannot read answers "404 Project Not Found" instead.
		MissingMarker: "404 File Not Found",
	},
}

// Other platforms, to be filled in and verified before use:
//
//	github:    {base}/{project_raw}/raw/{ref}/{path_raw}  Authorization  Bearer
//	gitea:     {base}/{project_raw}/raw/{ref}/{path_raw}  Authorization  Bearer
//	bitbucket: {base}/{project_raw}/raw/{ref}/{path_raw}  Authorization  Bearer

// hostForRepo returns the platform serving the site's code repository.
//
// GitLab is the default and the only one wired up. Selecting another platform
// will be a site-configuration setting (a platform field on store.SiteConfig,
// chosen under Settings → Repository); it is deliberately not implemented
// yet, so this returns GitLab unconditionally. Every caller goes through here,
// so that setting lands in this one function.
func hostForRepo() (codeHost, bool) {
	h, ok := codeHosts["gitlab"]
	return h, ok
}

// fileURL expands a platform's template for the file at ref in the repository
// at codeRepoURL. ok is false when the repository location yields no usable
// https base and project path (a bare "group/repo", for instance).
func fileURL(h codeHost, codeRepoURL, ref, path string) (string, bool) {
	base, project, ok := splitRepo(codeRepoURL)
	if !ok {
		return "", false
	}
	// The placeholder sets do not overlap: at a position holding "{project_raw}"
	// the pattern "{project}" cannot match, because the next byte is "_".
	expanded := strings.NewReplacer(
		"{base}", base,
		"{project}", url.PathEscape(project),
		"{project_raw}", project,
		"{path}", url.PathEscape(path),
		"{path_raw}", path,
		"{ref}", strings.TrimSpace(ref),
	).Replace(h.URLTemplate)
	return expanded, true
}

// splitRepo splits a repository location into the platform's https base
// ("https://gitlab.example.com") and the project path ("group/sub/project").
// It goes through HTTPURL so the scp and ssh:// forms resolve to the same
// place the clone would use.
func splitRepo(codeRepoURL string) (base, project string, ok bool) {
	u, err := url.Parse(HTTPURL(codeRepoURL))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", "", false
	}
	project = strings.Trim(strings.TrimSuffix(u.Path, "/"), "/")
	project = strings.TrimSuffix(project, ".git")
	if project == "" {
		return "", "", false
	}
	return u.Scheme + "://" + u.Host, project, true
}

// fileFetchTimeout bounds one file request. The dispatch deadline
// (Service.FetchTimeout) is the real limit; this only stops a single stalled
// request from eating all of it.
const fileFetchTimeout = 20 * time.Second

// maxFileBytes caps the file response. md-builder.yaml is a small text file;
// anything past this is not it — a streamed archive or a huge error page — and
// must not be handed to the yaml parser.
const maxFileBytes = 4 << 20

// NewYAMLFetcher returns the production md-builder.yaml reader: the code
// host's file route when it can serve the file, with GitYAMLFetcher (a full
// clone) as the fallback.
func NewYAMLFetcher() YAMLFetcher {
	return newYAMLFetcher(fileClient(), GitYAMLFetcher)
}

// fileClient is the HTTP client for file reads. Redirects are followed — a
// code host may answer with a pre-signed URL for a file it keeps in object
// storage — but the access token is dropped as soon as the redirect leaves
// the host it was issued for: the target is a different origin, often a
// storage provider, and has no business seeing the credential. (Go strips
// only Authorization and Cookie on a cross-host redirect, so a platform
// header like PRIVATE-TOKEN would otherwise travel along.)
func fileClient() *http.Client {
	return &http.Client{
		Timeout: fileFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			if req.URL.Host != via[0].URL.Host {
				for _, h := range codeHosts {
					if h.TokenHeader != "" {
						req.Header.Del(h.TokenHeader)
					}
				}
			}
			return nil
		},
	}
}

// errFileAbsent marks the one failure that needs no fallback: the code host
// says, in its own words, that the file is not in the repository at that ref.
// A clone would spend seconds reaching the same conclusion — and this is the
// most common dispatch failure there is, since it is what every push to a
// repository whose md-builder.yaml is not committed yet runs into.
var errFileAbsent = errors.New("file absent from the repository")

// newYAMLFetcher composes the two paths, with the client and the fallback
// injected so tests can see which one ran.
func newYAMLFetcher(client *http.Client, fallback YAMLFetcher) YAMLFetcher {
	return func(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
		content, err := fetchFile(ctx, client, codeRepoURL, sha, creds)
		if err == nil {
			return content, nil
		}
		if errors.Is(err, errFileAbsent) {
			return nil, fileNotFoundErr(sha, codeRepoURL)
		}
		// Everything else — an unreadable project, a host that answers with a
		// sign-in page, a location the template cannot be built from — is
		// handed to the clone, which sees the repository itself and so reports
		// the real reason.
		log.Printf("runner: yaml fetch: %v — falling back to a full clone", err)
		return fallback(ctx, codeRepoURL, sha, creds)
	}
}

// fetchFile reads md-builder.yaml at ref through the code host's file route.
// Every failure — no platform, an unusable repository location, a non-200
// answer, a page instead of the file, a timeout — comes back as an error for
// the caller to fall back on. The token travels in a header, never in the URL,
// and is redacted from the error.
func fetchFile(ctx context.Context, client *http.Client, codeRepoURL, ref string, creds *GitCredentials) ([]byte, error) {
	h, ok := hostForRepo()
	if !ok {
		return nil, fmt.Errorf("no code host is configured")
	}
	u, ok := fileURL(h, codeRepoURL, ref, YAMLPath)
	if !ok {
		return nil, fmt.Errorf("cannot derive a %s file URL from repository %q", h.Name, codeRepoURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, redactErr(fmt.Errorf("%s file: %w", h.Name, err), creds.Token())
	}
	if token := creds.Token(); token != "" && h.TokenHeader != "" {
		req.Header.Set(h.TokenHeader, tokenWithScheme(h, token))
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, redactErr(fmt.Errorf("%s file %s: %w", h.Name, u, err), creds.Token())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readCapped(resp.Body, maxErrorBytes)
		if resp.StatusCode == http.StatusNotFound && h.MissingMarker != "" &&
			strings.Contains(string(body), h.MissingMarker) {
			return nil, fmt.Errorf("%w (%s says %q)", errFileAbsent, h.Name, h.MissingMarker)
		}
		// Otherwise the body still earns its place in the log: GitLab names the
		// reason there ("404 Project Not Found"), which a bare status cannot.
		return nil, redactErr(fmt.Errorf("%s file %s: %s%s", h.Name, u, resp.Status, bodyHint(body)), creds.Token())
	}
	// A 200 that is not the file is the trap this route sets: when the token
	// is not accepted, the host redirects to its sign-in page and the client
	// follows it, arriving here with a successful status and an HTML body. The
	// size cap would not catch it, and the yaml parser would report nonsense
	// instead of letting the clone fallback run.
	if isHTML(resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("%s file %s: answered with %q, not the file — a sign-in or error page (the access token may not be accepted on this route)",
			h.Name, u, resp.Header.Get("Content-Type"))
	}
	content, err := readCapped(resp.Body, maxFileBytes)
	if err != nil {
		return nil, fmt.Errorf("%s file %s: %w", h.Name, u, err)
	}
	return content, nil
}

// isHTML reports whether a Content-Type is a web page rather than a file.
func isHTML(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct == "text/html" || ct == "application/xhtml+xml"
}

// maxErrorBytes caps how much of an error body is read: enough for a platform's
// message, not enough for an error page.
const maxErrorBytes = 512

// bodyHint returns the platform's short error message for the log, collapsed
// to one line and capped: it is remote text and must not be able to fill the
// log with a page of HTML.
func bodyHint(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return " — " + strings.Join(strings.Fields(string(b)), " ")
}

// tokenWithScheme applies the platform's token prefix inside the header.
func tokenWithScheme(h codeHost, token string) string {
	if h.TokenScheme == "" {
		return token
	}
	return h.TokenScheme + " " + token
}

// readCapped reads r in full, refusing more than max bytes rather than
// truncating: a half file would parse as broken yaml and report the wrong
// problem.
func readCapped(r io.Reader, max int64) ([]byte, error) {
	content, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(content)) > max {
		return nil, fmt.Errorf("response exceeds %d bytes", max)
	}
	return content, nil
}
