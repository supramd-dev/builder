package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"md-builder/server/runner"
	"md-builder/server/sshcheck"
	"md-builder/server/store"
)

// Per-phase (clone, build) and overall bounds for ad-hoc build tests. They
// mirror the generous bounds of scheduled jobs; a build test is still an
// interactive feature, so the overall session is capped.
const (
	buildTestStageTimeout = 30 * 60 // per phase: clone / build
	buildTestTotalTimeout = 65 * 60 // whole SSH session
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

// handleBuildTest runs an ad-hoc build test on the environment: it reuses
// the job runner's script generation (clone the code repository — with the
// site-configured deploy key/token when needed — at the requested ref, run
// a build command) but executes it once over SSH without touching the job
// queue or the test-run database. Intended for trying out builds before
// wiring them into the md-builder.yaml matrix.
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

	// Ad-hoc entry: clone (with credentials) + the requested build command.
	// No unit/regression stages; the build command runs under timeout.
	command := strings.TrimSpace(req.BuildCommand)
	if command == "" {
		command = buildTestDefaultCmd
	}
	ref := strings.TrimSpace(req.Ref)
	if ref == "" {
		ref = buildTestCommitSHA
	}

	entry := &runner.MergedEntry{
		Tags:    []string{"build-test"},
		Timeout: buildTestStageTimeout,
		Build: runner.BuildConfig{
			Generator: runner.GeneratorScript,
			Command:   command,
		},
	}

	script, err := runner.BuildScript(&runner.ScriptInput{
		CommitSHA:    ref,
		CodeRepoURL:  cfg.CodeRepo,
		EnvName:      env.Name,
		EnvTags:      env.Tags,
		Entry:        entry,
		StreamOutput: true,
		Creds: &runner.GitCredentials{
			DeployKey:       cfg.DeployKey,
			DeployToken:     cfg.DeployToken,
			DeployTokenUser: cfg.DeployTokenUser,
		},
	})
	if err != nil {
		log.Printf("build-test: build script: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	start := time.Now()
	res := sshcheck.ScriptWithTimeout(env.Host, env.Username, env.PrivateKey, "bash -s", script,
		time.Duration(buildTestTotalTimeout)*time.Second)
	elapsed := time.Since(start).Milliseconds()

	writeJSON(w, http.StatusOK, buildTestResult{
		Success:              res.ExitCode == 0,
		Stdout:               res.Stdout,
		Stderr:               res.Stderr,
		ExitCode:             res.ExitCode,
		DurationMilliSeconds: elapsed,
	})
}
