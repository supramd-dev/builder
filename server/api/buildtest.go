package api

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/runner"
	"md-builder/server/store"
)

// Per-phase (clone/upload, build) and overall bounds for ad-hoc build
// tests. They mirror the generous bounds of scheduled tasks; a build test
// is still an interactive feature, so the overall session is capped.
const (
	buildTestStageTimeout = 30 * 60 // per phase: clone+upload / build (seconds)
	buildTestDefaultCmd   = "cmake . && cmake --build . -j8"
	buildTestCommitSHA    = "HEAD"
)

// buildTestRequest is the body of POST /api/environments/{id}/build-test.
type buildTestRequest struct {
	// BuildCommand runs inside the cloned code directory, e.g.
	// "cmake . && make -j8". Empty means the CMake default above.
	BuildCommand string `json:"buildCommand"`
	// Ref optionally names the git ref to build (branch, tag or commit);
	// empty means HEAD.
	Ref string `json:"ref"`
}

// buildTestResult has the same wire shape as the exec/script results, so
// the frontend can reuse its result rendering.
type buildTestResult struct {
	Success              bool   `json:"success"`
	Stdout               string `json:"stdout"`
	Stderr               string `json:"stderr"`
	ExitCode             int    `json:"exitCode"`
	DurationMilliSeconds int64  `json:"durationMilliSeconds"`
}

// handleBuildTest runs an ad-hoc build test on the environment: the server
// clones the code repository (with the site-configured deploy key/token
// when needed) at the requested ref, uploads it over SSH and runs the build
// command — the same primitives the scheduled clone/build tasks use, but
// synchronous and without touching the task store. Intended for trying out
// builds before wiring them into the md-builder.yaml matrix.
func (s *Server) handleBuildTest(w http.ResponseWriter, r *http.Request, user *store.User, id int64) {
	env, err := s.Store.GetEnvironment(user.ID, id)
	if err != nil {
		respondEnvironmentError(w, err)
		return
	}
	if !env.Enabled {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "environment is disabled; enable it before running builds",
		})
		return
	}

	var req buildTestRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}

	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		log.Printf("build-test: site config: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if strings.TrimSpace(cfg.CodeRepo) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "no code repository configured; set it in the site settings first",
		})
		return
	}

	command := strings.TrimSpace(req.BuildCommand)
	if command == "" {
		command = buildTestDefaultCmd
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = buildTestCommitSHA
	}

	h := runner.SSHHostFromEnv(env)
	remoteDir := "$HOME/.md-builder/build-test"
	creds := &runner.GitCredentials{
		DeployKey:       cfg.DeployKey,
		DeployToken:     cfg.DeployToken,
		DeployTokenUser: cfg.DeployTokenUser,
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(),
		time.Duration(buildTestStageTimeout*2)*time.Second)
	defer cancel()

	// Phase 1: server-side clone + upload (the scheduled clone task's path).
	if _, err := s.Runner.Clone.CloneAndUpload(ctx, h, cfg.CodeRepo, ref,
		creds, remoteDir,
		time.Duration(buildTestStageTimeout)*time.Second, nil); err != nil {
		writeJSON(w, http.StatusOK, buildTestResult{
			Success:              false,
			Stderr:               "clone/upload failed: " + err.Error(),
			ExitCode:             -1,
			DurationMilliSeconds: time.Since(start).Milliseconds(),
		})
		return
	}

	// Phase 2: the build command in the uploaded code directory. The cd
	// target is unquoted so $HOME expands on the remote host.
	script := "#!/usr/bin/env bash\nset -uo pipefail\ncd " + remoteDir + "/code || exit 1\n" +
		"timeout " + itoa(buildTestStageTimeout) + " bash -c " + runner.ShellQuote(command) + "\nexit $?\n"
	var stdout, stderr strings.Builder
	res := s.Runner.SSH.RunScript(ctx, h, "bash -s", script,
		time.Duration(buildTestStageTimeout)*time.Second, &stdout, &stderr)

	writeJSON(w, http.StatusOK, buildTestResult{
		Success:              res.ExitCode == 0,
		Stdout:               stdout.String(),
		Stderr:               stderr.String(),
		ExitCode:             res.ExitCode,
		DurationMilliSeconds: time.Since(start).Milliseconds(),
	})
}

// itoa is a tiny helper for embedding constants in scripts.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
