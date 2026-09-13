package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"
)

// This file implements the server-side clone: the code repository is
// cloned on the server (with the site-configured Project Access Token)
// and uploaded to the remote environment as a gzipped tar stream. The
// remote environment needs no git and no repository access. Test inputs
// are expected to live inside the code repository itself (or to be
// fetched by it), so there is no separate test-input clone. Cloning goes
// through go-git — no git binary is required on the server.

// gitAuth builds the HTTP Basic-auth transport for the token (nil for
// public repositories). GitLab Project Access Tokens authenticate as
// "oauth2" over HTTPS.
func gitAuth(creds *GitCredentials) *http.BasicAuth {
	if user, pass, ok := creds.httpBasicAuth(); ok {
		return &http.BasicAuth{Username: user, Password: pass}
	}
	return nil
}

// CloneRepo clones repoURL at ref into destDir. ref may be a branch, tag
// or commit SHA (empty = the remote HEAD). creds may be nil (public
// repositories). Progress goes to logw when non-nil; the token is
// redacted from any error output.
func CloneRepo(ctx context.Context, repoURL, ref, destDir string, creds *GitCredentials, logw io.Writer) error {
	if strings.TrimSpace(repoURL) == "" {
		return fmt.Errorf("repository URL is empty")
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(destDir)

	cloneURL := HTTPURL(repoURL)
	secret := creds.Token()

	ref = strings.TrimSpace(ref)
	opts := &git.CloneOptions{
		URL:      cloneURL,
		Auth:     gitAuth(creds),
		Progress: logwOrDiscard(logw),
	}
	// A concrete ref: fetch only that branch/tag (a SHA goes through the
	// HEAD clone + checkout path below — git servers do not advertise
	// arbitrary commits as refs).
	if ref != "" && !isFullSHA(ref) {
		opts.ReferenceName = plumbing.NewBranchReferenceName(ref)
		if !refExists(ctx, cloneURL, creds, opts.ReferenceName) {
			// Not a branch: try a tag with the same name.
			opts.ReferenceName = plumbing.NewTagReferenceName(ref)
		}
	}
	repo, err := git.PlainCloneContext(ctx, destDir, false, opts)
	if err != nil {
		return redactErr(fmt.Errorf("git clone %s: %w", repoURL, err), secret)
	}

	// Checkout the requested SHA when the ref is not a branch/tag (empty
	// ref already cloned HEAD).
	if isFullSHA(ref) {
		h := plumbing.NewHash(strings.ToLower(ref))
		w, werr := repo.Worktree()
		if werr != nil {
			return redactErr(fmt.Errorf("git checkout %s: %w", ref, werr), secret)
		}
		if err := w.Checkout(&git.CheckoutOptions{Hash: h}); err != nil {
			return redactErr(fmt.Errorf("git checkout %s: %w", ref, err), secret)
		}
	}
	return nil
}

// refExists reports whether the remote advertises the named ref (used to
// decide between branch and tag spellings before cloning).
func refExists(ctx context.Context, cloneURL string, creds *GitCredentials, name plumbing.ReferenceName) bool {
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin", URLs: []string{cloneURL},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: gitAuth(creds)})
	if err != nil {
		return false
	}
	for _, r := range refs {
		if r.Name() == name {
			return true
		}
	}
	return false
}

// logwOrDiscard passes w through, or discards when nil.
func logwOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// redactErr redacts the secret from an error's message.
func redactErr(err error, secret string) error {
	if secret == "" {
		return err
	}
	return errors.New(Redact(err.Error(), secret))
}

// TarDir gzips the contents of dir into w (a single top-level entry per
// child of dir; the caller extracts into the remote workspace). Used to
// stream the cloned source tree to the remote environment.
func TarDir(ctx context.Context, dir string, w io.Writer) error {
	gzw := gzip.NewWriter(w)
	tw := tar.NewWriter(gzw)

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil // skip symlinks and special files
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gzw.Close()
}

// CloneAndUpload is the whole clone sub-task data path: clone the code
// repository into a temp dir, tar it and stream it into destDir on the
// remote host. Progress goes to logw. Returns the number of bytes uploaded.
func CloneAndUpload(ctx context.Context, h SSHHost, codeRepoURL, codeRef string, creds *GitCredentials, remoteWorkDir string, timeout time.Duration, logw io.Writer) (int64, error) {
	tmp, err := os.MkdirTemp("", "md-builder-clone-*")
	if err != nil {
		return 0, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	codeDir := filepath.Join(tmp, "code")
	if err := CloneRepo(ctx, codeRepoURL, codeRef, codeDir, creds, logw); err != nil {
		return 0, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	} else {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	pr, pw := io.Pipe()
	uploadErr := make(chan error, 1)
	go func() {
		err := TarDir(ctx, tmp, pw)
		_ = pw.CloseWithError(err)
		uploadErr <- err
	}()

	// Count the bytes actually shipped to the remote tar (diagnostics for
	// the "uploaded X MiB" log line).
	var uploaded int64
	counting := &countingReader{n: &uploaded, r: pr}
	if err := ExtractTarTo(ctx, h, counting, remoteWorkDir, timeout); err != nil {
		return uploaded, err
	}
	if err := <-uploadErr; err != nil {
		return uploaded, fmt.Errorf("tar: %w", err)
	}
	return uploaded, nil
}

// countingReader counts bytes read (diagnostics for upload logging).
type countingReader struct {
	n *int64
	r io.Reader
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += int64(n)
	return n, err
}

// ResolveRef resolves a git ref (branch, tag, short or full SHA; empty
// means HEAD) to the full commit SHA the repository currently points at,
// via go-git's remote listing (the equivalent of git ls-remote). creds
// may be nil (public repositories).
func ResolveRef(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
	if strings.TrimSpace(repoURL) == "" {
		return "", fmt.Errorf("repository URL is empty")
	}
	ref = strings.TrimSpace(ref)

	// A full 40-hex SHA needs no network round trip.
	if isFullSHA(ref) {
		return strings.ToLower(ref), nil
	}

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{
		Name: "origin", URLs: []string{HTTPURL(repoURL)},
	})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: gitAuth(creds)})
	if err != nil {
		return "", redactErr(fmt.Errorf("git ls-remote %s: %w", repoURL, err), creds.Token())
	}

	// Prefer an exact branch, then an exact tag; a short SHA matches any
	// advertised commit id by prefix.
	if ref != "" {
		for _, name := range []plumbing.ReferenceName{
			plumbing.NewBranchReferenceName(ref),
			plumbing.NewTagReferenceName(ref),
		} {
			for _, r := range refs {
				if r.Name() == name && !r.Hash().IsZero() {
					return r.Hash().String(), nil
				}
			}
		}
		for _, r := range refs {
			if !r.Hash().IsZero() && strings.HasPrefix(r.Hash().String(), strings.ToLower(ref)) {
				return r.Hash().String(), nil
			}
		}
		return "", fmt.Errorf("git ls-remote: ref %q not found", ref)
	}

	// Empty ref: HEAD. Some servers advertise HEAD with its commit hash;
	// others (the common case) send a symbolic reference pointing at the
	// default branch — follow the target through the same listing.
	for _, r := range refs {
		if r.Name() != plumbing.HEAD {
			continue
		}
		if !r.Hash().IsZero() {
			return r.Hash().String(), nil
		}
		if tgt := r.Target(); tgt != "" {
			for _, r2 := range refs {
				if r2.Name() == tgt && !r2.Hash().IsZero() {
					return r2.Hash().String(), nil
				}
			}
		}
	}
	return "", fmt.Errorf("git ls-remote: repository advertises no HEAD")
}

// isFullSHA reports whether s is a 40-character hex commit id.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// RemoteTaskDir is the remote workspace path for a commit: ~/.md-builder/
// tasks/<sha12> (as a $HOME reference — the remote shell expands it; both
// the stage scripts and the clone upload use it).
func RemoteTaskDir(sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	return "$HOME/.md-builder/tasks/" + short
}
