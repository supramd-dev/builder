package runner

import (
	"fmt"
	"os"
	"os/exec"
)

// YAMLFetcher returns the md-builder.yaml contents at a specific commit of the
// code repository. The default implementation uses git to read a single blob.
type YAMLFetcher func(codeRepoURL, sha string) ([]byte, error)

// GitYAMLFetcher fetches md-builder.yaml at the given commit of the code
// repository by cloning into a temp directory and reading the blob. The
// temporary clone is removed afterwards. Requires git on PATH and network
// reachability to the repository from the server.
func GitYAMLFetcher(codeRepoURL, sha string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "md-builder-yaml-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	clone := exec.Command("git", "clone", "--quiet", "--no-checkout", codeRepoURL, dir)
	if out, err := clone.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone --no-checkout: %w (%s)", err, string(out))
	}
	cat := exec.Command("git", "show", sha+":md-builder.yaml")
	cat.Dir = dir
	out, err := cat.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("git show %s:md-builder.yaml: %w (%s)", sha, err, string(ee.Stderr))
		}
		return nil, fmt.Errorf("git show %s:md-builder.yaml: %w", sha, err)
	}
	return out, nil
}
