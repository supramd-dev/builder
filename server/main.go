// md-builder is the backend server for the md-builder scientific computing
// test platform. It hosts the frontend assets, serves the JSON API, and
// provides CLI subcommands (adduser) for user management.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"md-builder/server/api"
	"md-builder/server/runner"
	"md-builder/server/sshcheck"
	"md-builder/server/store"
	"md-builder/server/worker"
)

const listenAddr = ":8080"

// defaultDSN determines the database DSN: MD_BUILDER_DSN env var first, then
// a sensible SQLite file location.
func defaultDSN() string {
	if d := os.Getenv("MD_BUILDER_DSN"); d != "" {
		return d
	}
	return "md-builder.db" // SQLite file in the working directory
}

// distDir is resolved from the working directory so the binary can run from
// either the project root or the server/ directory.
func resolveDistDir() string {
	// Prefer environment variable (for deployments).
	if d := os.Getenv("MD_BUILDER_DIST"); d != "" {
		return d
	}
	// Try common working directories in order.
	candidates := []string{
		"frontend/dist",    // run from project root
		"../frontend/dist", // run from server/
		"./dist",           // deployed alongside the binary
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return "frontend/dist"
}

func main() {
	// Subcommand dispatch: md-builder adduser ...
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		os.Exit(adduserSubcommand())
	}

	s, err := store.Open(defaultDSN())
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	mux := http.NewServeMux()

	// --- JSON API (auth) ---
	apiServer := api.New(s)
	apiServer.Register(mux)

	// --- Job dispatch + worker pool ---
	dispatcher := &worker.Dispatcher{
		Store:     s,
		FetchYAML: runner.GitYAMLFetcher,
	}
	apiServer.SetDispatcher(dispatcher)

	pool := &worker.Pool{
		Store:  s,
		Runner: &runner.Runner{Store: s, Exec: sshExecFunc},
	}
	if os.Getenv("MD_BUILDER_DISABLE_WORKER") != "1" {
		rootCtx, cancel := context.WithCancel(context.Background())
		pool.Start(rootCtx)
		defer cancel()
	}

	// --- Static frontend assets (with SPA fallback) ---
	distDir := resolveDistDir()
	fs := http.FileServer(http.Dir(distDir))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Serve existing static files directly.
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			fs.ServeHTTP(w, r)
			return
		}
		if _, err := os.Stat(distDir + "/" + path); err == nil {
			fs.ServeHTTP(w, r)
			return
		}
		// Fall back to index.html for unknown paths (client-side routing).
		http.ServeFile(w, r, distDir+"/index.html")
	})

	log.Printf("md-builder listening on http://localhost%s (dist: %s, db: %s)",
		listenAddr, distDir, defaultDSN())
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}

// sshExecFunc adapts sshcheck to the runner.ExecFunc signature: it runs the
// generated script over SSH with an overall timeout and returns stdout.
func sshExecFunc(env *store.TestEnvironment, script string, timeoutSecs int) (string, int, error) {
	res := sshcheck.ScriptWithTimeout(env.Host, env.Username, env.PrivateKey, "bash -s", script,
		time.Duration(timeoutSecs)*time.Second)
	if res.ExitCode < 0 && !res.Success {
		// Connection failure, timeout or run error: treat as execution error.
		return res.Stdout, res.ExitCode, errors.New(strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, res.ExitCode, nil
}
