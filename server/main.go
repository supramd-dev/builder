// md-builder is the backend server for the md-builder scientific computing
// test platform. It hosts the frontend assets, serves the JSON API, and
// provides CLI subcommands (adduser) for user management.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strings"

	"md-builder/server/api"
	"md-builder/server/runner"
	"md-builder/server/store"
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

	// --- Runner component: task dispatch + scheduling pool ---
	runnerSvc := runner.NewService(s)
	if os.Getenv("MD_BUILDER_DISABLE_WORKER") != "1" {
		rootCtx, cancel := context.WithCancel(context.Background())
		runnerSvc.Start(rootCtx)
		defer cancel()
	} else {
		log.Print("worker pool disabled (MD_BUILDER_DISABLE_WORKER=1); dispatch still records tasks")
	}

	// --- JSON API (auth) ---
	apiServer := api.New(s)
	apiServer.Register(mux)
	apiServer.SetRunner(runnerSvc)

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
