// Deep health check: the site-status endpoint behind the footer's health
// link. Unlike /api/health (a liveness probe — "is the HTTP server up"),
// this endpoint actively probes the site's external dependencies on
// demand, with a short timeout each, and reports their reachability.

package api

import (
	"context"
	"net/http"
	"time"

	"md-builder/server/runner"
	"md-builder/server/store"
)

// healthTimeout bounds each individual probe (the git ls-remote and the
// future object-storage ping). The endpoint's total latency is the slowest
// probe, not their sum — the probes run concurrently.
const healthTimeout = 10 * time.Second

// healthCheckJSON is one probe's outcome.
type healthCheckJSON struct {
	// Name identifies the check in the UI ("git repository", "object
	// storage", ...).
	Name string `json:"name"`
	// Status: "ok", "fail" or "skipped" (a dependency not configured —
	// e.g. no code repo set, no object storage backend yet).
	Status string `json:"status"`
	// Target is what was probed (the repo URL, endpoint, ...); empty for
	// skipped checks.
	Target string `json:"target,omitempty"`
	// Detail carries the error on failure (already redacted of secrets).
	Detail string `json:"detail,omitempty"`
	// DurationMillis is how long the probe took.
	DurationMillis int64 `json:"durationMillis"`
}

// healthDeepJSON is the response of GET /api/health/deep.
type healthDeepJSON struct {
	// Version is the served build's source revision, as in /api/health.
	Version string           `json:"version,omitempty"`
	Checks  []healthCheckJSON `json:"checks"`
	// CheckedAt is when the probes ran (RFC 3339, UTC).
	CheckedAt string `json:"checkedAt"`
}

// handleHealthDeep routes GET /api/health/deep — the site health board:
// probes the site-configured git repository (go-git ls-remote with the
// site's access token, the same path a dispatch takes) and, in the future,
// the object storage backend. Requires an authenticated user (the repo URL
// and token are site configuration); the liveness probe /api/health stays
// unauthenticated.
func (s *Server) handleHealthDeep(w http.ResponseWriter, r *http.Request, user *store.User) {
	_ = user
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	type result struct {
		json healthCheckJSON
	}
	out := make(chan result, 2)

	cfg, err := s.Store.GetSiteConfig()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}

	// Probe 1: the git code repository — resolve the HEAD ref the way a
	// manual dispatch does. An empty repo URL or auth failure both surface
	// here (skipped vs fail).
	go func() {
		check := healthCheckJSON{Name: "git repository", Status: "ok"}
		start := time.Now()
		if cfg.CodeRepo == "" {
			check.Status = "skipped"
			check.Detail = "no code repository configured"
		} else {
			check.Target = cfg.CodeRepo
			creds := &runner.GitCredentials{AccessToken: cfg.AccessToken}
			if _, err := runner.ResolveRef(ctx, cfg.CodeRepo, "", creds); err != nil {
				check.Status = "fail"
				check.Detail = err.Error()
			}
		}
		check.DurationMillis = time.Since(start).Milliseconds()
		out <- result{json: check}
	}()

	// Probe 2: the object storage backend. Not configurable yet — reserved
	// for the Garage/S3 artifact store so the board gains a row without an
	// API or schema change when it lands.
	go func() {
		check := healthCheckJSON{
			Name:           "object storage",
			Status:         "skipped",
			Detail:         "no object storage backend configured (planned)",
			DurationMillis: 0,
		}
		out <- result{json: check}
	}()

	checks := make([]healthCheckJSON, 0, 2)
	for i := 0; i < 2; i++ {
		checks = append(checks, (<-out).json)
	}

	writeJSON(w, http.StatusOK, healthDeepJSON{
		Version:   s.Version,
		Checks:    checks,
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
	})
}
