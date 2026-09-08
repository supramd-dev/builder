package main

import (
	"log"
	"net/http"
	"os"
	"strings"
)

const listenAddr = ":8080"

// distDir is resolved from the working directory so the binary can run from
// either the project root or the server/ directory.
var distDir = resolveDistDir()

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
	mux := http.NewServeMux()

	// --- API placeholder ---
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// --- Static frontend assets (with SPA fallback) ---
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

	log.Printf("md-builder listening on http://localhost%s (dist: %s)", listenAddr, distDir)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
