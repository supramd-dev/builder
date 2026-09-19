package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// YAMLFetcher returns the md-builder.yaml contents at a specific commit of the
// code repository. Production uses NewYAMLFetcher (the code host's file
// route, with this file's clone as the fallback); credentials may be nil
// (public repositories only). The context bounds the network work.
type YAMLFetcher func(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error)

// YAMLPath is the test-matrix file read from the code repository.
const YAMLPath = "md-builder.yaml"

// fileNotFoundErr is what both fetch paths report when md-builder.yaml is not
// in the repository at that commit. It is the most common dispatch failure —
// every push before the yaml is committed runs into it — so it is spelled out
// for the dashboard, and shared so the two paths cannot drift apart.
func fileNotFoundErr(sha, codeRepoURL string) error {
	return fmt.Errorf("%s:%s: file not found — commit the md-builder.yaml to the repository root (site config code repo %s)",
		sha, YAMLPath, codeRepoURL)
}

// GitYAMLFetcher fetches md-builder.yaml at the given commit of the code
// repository: a full but checkout-less clone into a temp directory (the
// pushed commit is not necessarily the remote HEAD, so the history must be
// complete), then the blob is read straight from the commit's tree. The
// temporary clone is removed afterwards. Requires network reachability to
// the repository from the server; the token (when configured) travels as
// HTTPS Basic auth ("oauth2":<token>).
//
// It downloads the whole repository to read one file, so it is the fallback
// rather than the default: see codehost.go.
func GitYAMLFetcher(ctx context.Context, codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
	dir, err := os.MkdirTemp("", "md-builder-yaml-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:        HTTPURL(codeRepoURL),
		Auth:       gitAuth(creds),
		NoCheckout: true,
	})
	if err != nil {
		return nil, redactErr(fmt.Errorf("git clone (yaml fetch): %w", err), creds.Token())
	}

	h := plumbing.NewHash(strings.ToLower(strings.TrimSpace(sha)))
	if h.IsZero() {
		// No SHA given: fall back to the remote HEAD.
		head, err := repo.Head()
		if err != nil {
			return nil, fmt.Errorf("resolve HEAD: %w", err)
		}
		h = head.Hash()
	}
	commit, err := repo.CommitObject(h)
	if err != nil {
		return nil, redactErr(fmt.Errorf("read commit %s: %w", sha, err), creds.Token())
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}
	entry, err := tree.File(YAMLPath)
	if err != nil {
		if errors.Is(err, object.ErrFileNotFound) {
			return nil, fileNotFoundErr(sha, codeRepoURL)
		}
		return nil, fmt.Errorf("%s:%s: %w", sha, YAMLPath, err)
	}
	content, err := entry.Contents()
	if err != nil {
		return nil, err
	}
	return []byte(content), nil
}
