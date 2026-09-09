package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// YAMLFetcher returns the md-builder.yaml contents at a specific commit of the
// code repository. The default implementation uses git to read a single blob.
// Credentials may be nil (public repositories only).
type YAMLFetcher func(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error)

// GitYAMLFetcher fetches md-builder.yaml at the given commit of the code
// repository by cloning into a temp directory and reading the blob. The
// temporary clone is removed afterwards. Requires git on PATH and network
// reachability to the repository from the server.
//
// When creds carries a deploy token, the clone goes over HTTPS with the
// token embedded in the URL (system credential helpers are disabled so the
// token is the only authentication tried). When it carries a deploy key, the
// repository is converted to its SSH form and cloned with the key via
// GIT_SSH_COMMAND (a temp key file, removed afterwards).
func GitYAMLFetcher(codeRepoURL, sha string, creds *GitCredentials) ([]byte, error) {
	dir, err := os.MkdirTemp("", "md-builder-yaml-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	cloneURL := codeRepoURL
	var keyFile string
	var secret string // for redacting error output
	if creds != nil {
		switch {
		case creds.HasToken():
			cloneURL = creds.AuthenticatedURL(codeRepoURL)
			secret = creds.DeployToken
		case creds.HasKey():
			sshURL, ok := creds.SSHURL(codeRepoURL)
			if !ok {
				return nil, fmt.Errorf("cannot use deploy key with repository location %q", codeRepoURL)
			}
			cloneURL = sshURL
			keyFile, err = writeTempKey(creds.DeployKey, dir)
			if err != nil {
				return nil, err
			}
			defer os.Remove(keyFile)
		}
	}

	args := []string{"clone", "--quiet", "--no-checkout"}
	if keyFile != "" {
		args = append(args,
			"-c", "credential.helper=",
			"-c", fmt.Sprintf("core.sshCommand=ssh -i %s -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes", shq(keyFile)))
	}
	args = append(args, cloneURL, dir)
	clone := exec.Command("git", args...)
	if out, err := clone.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git clone --no-checkout: %w (%s)", err, Redact(string(out), secret))
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

// writeTempKey writes a PEM key inside dir with 0600 permissions; git/ssh
// refuse world-readable key files.
func writeTempKey(pem, dir string) (string, error) {
	if !strings.Contains(pem, "-----BEGIN") {
		return "", fmt.Errorf("deploy key is not PEM-encoded")
	}
	path := dir + "/deploy-key"
	if err := os.WriteFile(path, []byte(pem), 0o600); err != nil {
		return "", fmt.Errorf("write deploy key: %w", err)
	}
	return path, nil
}
