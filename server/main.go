// md-builder is the backend server for the md-builder scientific computing
// test platform. It hosts the frontend assets, serves the JSON API, and
// provides CLI subcommands (adduser, seed) for user management and demo
// data. Serving is the default action; -config names the server config file
// the API and the seed subcommand read (see the config package).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	// Embed the IANA time zone database so the site-config timezone setting
	// (and time.LoadLocation) works on hosts without system zoneinfo (e.g.
	// scratch containers).
	_ "time/tzdata"

	"md-builder/server/api"
	"md-builder/server/config"
	"md-builder/server/runner"
	"md-builder/server/storage"
	"md-builder/server/store"
)

const listenAddr = ":8080"

// version is the build's source revision, embedded at link time (see the
// Makefile's build-server target): -ldflags "-X main.version=<git describe>".
// "dev" means a plain `go build` without the stamp.
var version = "dev"

// defaultDSN determines the database DSN: MD_BUILDER_DSN env var first, then
// a sensible SQLite file location.
func defaultDSN() string {
	if d := os.Getenv("MD_BUILDER_DSN"); d != "" {
		return d
	}
	return "md-builder.db" // SQLite file in the working directory
}

// openObjectStorage loads the server config file and connects to the artifact
// store, verifying that the bucket exists. It is the startup gate for every
// subcommand that records artifacts.
//
// configPath is the -config flag ("" to search the usual places).
func openObjectStorage(configPath string) (*storage.MinIO, storage.Config, error) {
	cfg, source, err := config.Load(configPath)
	if err != nil {
		return nil, storage.Config{}, err
	}
	objs, err := storage.NewMinIO(cfg)
	if err != nil {
		return nil, storage.Config{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), objectStorageTimeout)
	defer cancel()
	if err := objs.EnsureBucket(ctx); err != nil {
		return nil, storage.Config{}, fmt.Errorf("config %s: %w", source, err)
	}
	log.Printf("object storage ready: %s (config: %s)", objs.Describe(), source)
	return objs, cfg, nil
}

// objectStorageTimeout bounds the startup connectivity check.
const objectStorageTimeout = 15 * time.Second

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
	// Subcommand dispatch: md-builder adduser|seed ...
	if len(os.Args) > 1 && os.Args[1] == "adduser" {
		os.Exit(adduserSubcommand())
	}
	if len(os.Args) > 1 && os.Args[1] == "seed" {
		os.Exit(seedSubcommand())
	}

	// Serving the API is what the binary does when no subcommand is given;
	// -config names the server config file to read.
	flags := flag.NewFlagSet("md-builder", flag.ExitOnError)
	configPath := flags.String(config.FlagName, "", config.FlagUsage)
	_ = flags.Parse(os.Args[1:])
	if flags.NArg() > 0 {
		// A mistyped subcommand would otherwise start the server silently.
		log.Fatalf("unexpected argument %q (subcommands: adduser, seed — their flags follow the name, e.g. md-builder seed -config FILE; see -h)", flags.Arg(0))
	}

	// Object storage is mandatory: artifacts live there, so a deployment
	// without a reachable backend cannot record test output. Fail before
	// serving anything rather than on the first artifact write.
	objs, objCfg, err := openObjectStorage(*configPath)
	if err != nil {
		log.Fatalf("object storage: %v", err)
	}

	s, err := store.Open(defaultDSN(), store.WithObjects(objs))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer s.Close()

	// Artifacts recorded before object storage existed are moved out of the
	// database once, at startup.
	migrateCtx, cancelMigrate := context.WithTimeout(context.Background(), 10*time.Minute)
	if n, err := s.MigrateInlineArtifacts(migrateCtx); err != nil {
		log.Printf("artifact migration: %d artifact(s) moved, some failed: %v", n, err)
	}
	cancelMigrate()

	// Reclaim objects that no artifact row references any more (runs deleted
	// or replaced after they were written).
	if objCfg.GC {
		gcCtx, cancelGC := context.WithCancel(context.Background())
		defer cancelGC()
		s.StartArtifactGC(gcCtx, objCfg.GCInterval())
	}

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
	apiServer.Version = version
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

	log.Printf("md-builder %s listening on http://localhost%s (dist: %s, db: %s)",
		version, listenAddr, distDir, defaultDSN())
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
