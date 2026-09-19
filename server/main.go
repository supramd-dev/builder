// md-builder is the backend server for the md-builder scientific computing
// test platform. It hosts the frontend assets, serves the JSON API, and
// provides CLI subcommands (adduser, seed) for user management and demo
// data. Serving is the default action; -config names the server config file
// the API and the seed subcommand read, and everything else — where to
// listen, which database to open, how many workers to run — comes from that
// file or its MD_BUILDER_* environment overrides (see the config package).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
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

// defaultPort is the API's listen port when neither server.port (or
// $MD_BUILDER_PORT) nor the address itself says otherwise; defaultListenAddr
// is the matching address (all interfaces, so a container or a LAN host is
// reachable without further configuration).
const defaultPort = "8080"

const defaultListenAddr = ":" + defaultPort

// resolveListenAddr combines the configured address and port into the address
// the HTTP server listens on. The port wins over the port inside the address,
// and an address given without one ("127.0.0.1", "::1", "localhost") takes it
// from the port setting or the default.
func resolveListenAddr(addr string, port int) (string, error) {
	if port < 0 || port > 65535 {
		return "", fmt.Errorf("port %d is not a port number (server.port / %s)", port, config.EnvPort)
	}
	if addr = strings.TrimSpace(addr); addr == "" {
		addr = defaultListenAddr
	}
	host, portPart, err := net.SplitHostPort(addr)
	if err != nil {
		// A bare host — an IPv6 literal either way ("::1" or the bracketed
		// form "[::1]" with the port left off). Anything else with a colon
		// in it (127.0.0.1:8080:9000) is a typo worth reporting here rather
		// than as a listen error later.
		host = strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
		if strings.Contains(host, ":") && net.ParseIP(host) == nil {
			return "", fmt.Errorf("%s is not a host:port address (server.addr / %s)", addr, config.EnvAddr)
		}
		portPart = ""
	}
	if port != 0 {
		portPart = strconv.Itoa(port)
	}
	if portPart == "" {
		portPart = defaultPort
	}
	if n, err := strconv.Atoi(portPart); err != nil || n < 0 || n > 65535 {
		return "", fmt.Errorf("%s: %q is not a port number (server.addr / %s)", addr, portPart, config.EnvAddr)
	}
	return net.JoinHostPort(host, portPart), nil
}

// listenURL renders the resolved listen address for the startup log. An
// unspecified host (":8080", "0.0.0.0:8080") is shown as localhost, which is
// where the operator will open it.
func listenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// version is the build's source revision, embedded at link time (see the
// Makefile's build-server target): -ldflags "-X main.version=<git describe>".
// "dev" means a plain `go build` without the stamp.
var version = "dev"

// openObjectStorage connects to the artifact store, verifying that the bucket
// exists. It is the startup gate for every subcommand that records artifacts,
// and the place the object storage section is validated: a deployment with no
// config file and no store variables is told what it is missing here rather
// than on the first artifact write.
//
// source names where the configuration came from, for the log line.
func openObjectStorage(cfg config.Config, source string) (*storage.MinIO, error) {
	objs, err := storage.NewMinIO(cfg.ObjectStorage)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), objectStorageTimeout)
	defer cancel()
	if err := objs.EnsureBucket(ctx); err != nil {
		return nil, fmt.Errorf("config %s: %w", source, err)
	}
	log.Printf("object storage ready: %s (config: %s)", objs.Describe(), source)
	return objs, nil
}

// objectStorageTimeout bounds the startup connectivity check.
const objectStorageTimeout = 15 * time.Second

// resolveDistDir returns where the built frontend lives: the configured
// `dist` (or $MD_BUILDER_DIST), or the first of the common locations that
// exists, so the binary can run from either the project root or server/.
func resolveDistDir(configured string) string {
	if d := strings.TrimSpace(configured); d != "" {
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
	// -config names the configuration file to read. Where to listen, which
	// database to open and how many workers to run all come from that file
	// (or its MD_BUILDER_* overrides).
	flags := flag.NewFlagSet("md-builder", flag.ExitOnError)
	configPath := flags.String(config.FlagName, "", config.FlagUsage)
	_ = flags.Parse(os.Args[1:])
	if flags.NArg() > 0 {
		// A mistyped subcommand would otherwise start the server silently.
		log.Fatalf("unexpected argument %q (subcommands: adduser, seed — their flags follow the name, e.g. md-builder seed -config FILE; see -h)", flags.Arg(0))
	}

	cfg, source, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	listenAddr, err := resolveListenAddr(cfg.Server.Addr, cfg.Server.Port)
	if err != nil {
		log.Fatalf("listen address: %v", err)
	}

	// Object storage is mandatory: artifacts live there, so a deployment
	// without a reachable backend cannot record test output. Fail before
	// serving anything rather than on the first artifact write.
	objs, err := openObjectStorage(cfg, source)
	if err != nil {
		log.Fatalf("object storage: %v", err)
	}

	s, err := store.Open(cfg.Database.DSN, store.WithObjects(objs))
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
	if cfg.ObjectStorage.GC {
		gcCtx, cancelGC := context.WithCancel(context.Background())
		defer cancelGC()
		s.StartArtifactGC(gcCtx, cfg.ObjectStorage.GCInterval())
	}

	mux := http.NewServeMux()

	// --- Runner component: task dispatch + scheduling pool ---
	runnerSvc := runner.NewService(s)
	runnerSvc.Workers = cfg.Worker.Count
	if cfg.Worker.Enabled {
		rootCtx, cancel := context.WithCancel(context.Background())
		runnerSvc.Start(rootCtx)
		defer cancel()
	} else {
		log.Printf("worker pool disabled (worker.enabled: false / %s); dispatch still records tasks", config.EnvDisableWorker)
	}

	// --- JSON API (auth) ---
	apiServer := api.New(s)
	apiServer.Version = version
	apiServer.Register(mux)
	apiServer.SetRunner(runnerSvc)

	// --- Static frontend assets (with SPA fallback) ---
	distDir := resolveDistDir(cfg.Dist)
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

	log.Printf("md-builder %s listening on %s (dist: %s, db: %s)",
		version, listenURL(listenAddr), distDir, cfg.Database.DSN)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
