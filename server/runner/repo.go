package runner

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// This file implements the server-side clone: the code repository is
// cloned on the server (with the site-configured deploy key/token) and
// uploaded to the remote environment as a gzipped tar stream. The remote
// environment needs no git and no repository access. Test inputs are
// expected to live inside the code repository itself (or to be fetched
// by it), so there is no separate test-input clone.

// CloneRepo clones repoURL at ref into destDir. ref may be a branch, tag or
// commit SHA. creds may be nil (public repositories). git's progress output
// goes to logw when non-nil.
func CloneRepo(ctx context.Context, repoURL, ref, destDir string, creds *GitCredentials, logw io.Writer) error {
	if strings.TrimSpace(repoURL) == "" {
		return fmt.Errorf("repository URL is empty")
	}
	if err := os.MkdirAll(filepath.Dir(destDir), 0o755); err != nil {
		return fmt.Errorf("create parent dir: %w", err)
	}
	_ = os.RemoveAll(destDir)

	cloneURL := repoURL
	var keyFile string
	secret := ""
	if creds != nil {
		switch {
		case creds.HasToken():
			cloneURL = creds.AuthenticatedURL(repoURL)
			secret = creds.DeployToken
		case creds.HasKey():
			sshURL, ok := creds.SSHURL(repoURL)
			if !ok {
				return fmt.Errorf("cannot use deploy key with repository location %q", repoURL)
			}
			cloneURL = sshURL
			dir, err := os.MkdirTemp("", "md-builder-key-*")
			if err != nil {
				return fmt.Errorf("create temp dir: %w", err)
			}
			keyFile, err = writeTempKey(creds.DeployKey, dir)
			if err != nil {
				os.RemoveAll(dir)
				return err
			}
			defer os.RemoveAll(dir) // removes the key file with it
		}
	}

	args := []string{"clone", "--quiet"}
	if keyFile != "" {
		args = append(args,
			"-c", "credential.helper=",
			"-c", fmt.Sprintf("core.sshCommand=ssh -i %s -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes", shq(keyFile)),
		)
	}
	args = append(args, cloneURL, destDir)

	if err := runGit(ctx, args, logw, secret); err != nil {
		return fmt.Errorf("git clone %s: %w", repoURL, err)
	}

	// Checkout the requested ref when it is not the default HEAD.
	ref = strings.TrimSpace(ref)
	if ref == "" || ref == "HEAD" {
		return nil
	}
	checkout := exec.CommandContext(ctx, "git", "-C", destDir, "checkout", "--quiet", ref)
	if out, err := checkout.CombinedOutput(); err != nil {
		return fmt.Errorf("git checkout %s: %w (%s)", ref, err, Redact(string(out), secret))
	}
	return nil
}

// runGit executes git with args, logging combined output to logw (with the
// secret redacted) and failing with a redacted message.
func runGit(ctx context.Context, args []string, logw io.Writer, secret string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	var buf strings.Builder
	cmd.Stdout = io.MultiWriter(&buf, logwOrDiscard(logw))
	cmd.Stderr = io.MultiWriter(&buf, logwOrDiscard(logw))
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("%w (%s)", err, Redact(strings.TrimSpace(buf.String()), secret))
	}
	return nil
}

func logwOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
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
// via git ls-remote on the server. creds may be nil (public repositories).
func ResolveRef(ctx context.Context, repoURL, ref string, creds *GitCredentials) (string, error) {
	if strings.TrimSpace(repoURL) == "" {
		return "", fmt.Errorf("repository URL is empty")
	}
	ref = strings.TrimSpace(ref)

	cloneURL := repoURL
	var keyFile string
	secret := ""
	if creds != nil {
		switch {
		case creds.HasToken():
			cloneURL = creds.AuthenticatedURL(repoURL)
			secret = creds.DeployToken
		case creds.HasKey():
			sshURL, ok := creds.SSHURL(repoURL)
			if !ok {
				return "", fmt.Errorf("cannot use deploy key with repository location %q", repoURL)
			}
			cloneURL = sshURL
			dir, err := os.MkdirTemp("", "md-builder-key-*")
			if err != nil {
				return "", fmt.Errorf("create temp dir: %w", err)
			}
			keyFile, err = writeTempKey(creds.DeployKey, dir)
			if err != nil {
				os.RemoveAll(dir)
				return "", err
			}
			defer os.RemoveAll(dir)
		}
	}

	// A full 40-hex SHA needs no network round trip.
	if isFullSHA(ref) {
		return strings.ToLower(ref), nil
	}

	args := []string{"ls-remote", cloneURL}
	if ref == "" {
		args = append(args, "HEAD")
	} else {
		args = append(args, ref)
	}
	if keyFile != "" {
		gitArgs := append([]string{
			"-c", "credential.helper=",
			"-c", fmt.Sprintf("core.sshCommand=ssh -i %s -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes", shq(keyFile)),
		}, args...)
		args = gitArgs
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git ls-remote %s: %w (%s)", repoURL, err, Redact(strings.TrimSpace(buf.String()), secret))
	}
	return firstLSRemoteSHA(buf.String(), ref)
}

// firstLSRemoteSHA parses ls-remote output (lines of "<sha>\t<ref>") and
// returns the first SHA, preferring an exact ref match over the loosely
// matched remainder. Empty output is an error (unknown ref).
func firstLSRemoteSHA(out, ref string) (string, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if ref != "" {
		want := ref
		for _, l := range lines {
			if i := strings.IndexByte(l, '\t'); i > 0 && l[i+1:] == want {
				return l[:i], nil
			}
		}
	}
	for _, l := range lines {
		if i := strings.IndexByte(l, '\t'); i >= 40 {
			return l[:40], nil
		}
	}
	if ref == "" {
		return "", fmt.Errorf("git ls-remote: repository advertises no HEAD")
	}
	return "", fmt.Errorf("git ls-remote: ref %q not found", ref)
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
