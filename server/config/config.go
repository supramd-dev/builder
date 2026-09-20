// Package config loads the server's own configuration file
// (md-builder-server.yaml) — deployment settings that belong to the host, not
// to a test matrix.
//
// It is deliberately distinct from the pipeline configuration
// (md-builder.yaml), which is fetched from the code repository per dispatch
// and parsed by the runner package. This file is read once at startup.
//
// It holds where the API listens, which database to open, where the built
// frontend lives, how many workers to run, and the object storage section
// (MinIO / S3). The last one is mandatory: artifacts live in the object
// store, so a server without it cannot run.
//
// Every setting can also be supplied through the environment (MD_BUILDER_*),
// which wins over the file — that is how containers and CI deployments
// usually inject the credentials. A value neither source provides falls back
// to the built-in default, so a file may set only what it needs.
//
// The file is found in this order: the -config flag, $MD_BUILDER_CONFIG, then
// md-builder-server.yaml in the working directory or under ./server.
package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"md-builder/server/storage"
)

// DefaultFileName is the config file looked up in the working directory (and
// then in ./server, so the binary also runs from the project root).
const DefaultFileName = "md-builder-server.yaml"

// EnvConfigPath overrides the config file location.
const EnvConfigPath = "MD_BUILDER_CONFIG"

// FlagName is the command-line flag that pins the config file; FlagUsage is
// its help text, shared by the server and the subcommands that read the
// config so the lookup order is documented in one place.
const FlagName = "config"

const FlagUsage = "server config file (default: $" + EnvConfigPath +
	", ./" + DefaultFileName + ", ./server/" + DefaultFileName + ")"

// Environment variables. Each one is an override of a config file key, and
// each wins over the file when it is set and non-empty.
const (
	// EnvAddr / EnvPort override server.addr / server.port. EnvAddr on its
	// own replaces the file's address and the port inside it; set EnvPort
	// too to override that port as well.
	EnvAddr = "MD_BUILDER_ADDR"
	EnvPort = "MD_BUILDER_PORT"
	// EnvDSN overrides database.dsn.
	EnvDSN = "MD_BUILDER_DSN"
	// EnvDist overrides dist.
	EnvDist = "MD_BUILDER_DIST"
	// EnvWorkers overrides worker.count.
	EnvWorkers = "MD_BUILDER_WORKERS"
	// EnvDisableWorker overrides worker.enabled (it is named for what it
	// turns off, so it inverts).
	EnvDisableWorker = "MD_BUILDER_DISABLE_WORKER"
	// EnvPublicURL overrides server.publicURL.
	EnvPublicURL = "MD_BUILDER_PUBLIC_URL"
)

// DefaultDSN is the database opened when neither the file nor the environment
// names one: a SQLite file in the working directory.
const DefaultDSN = "md-builder.db"

// Server is the `server` section: where the API listens.
type Server struct {
	// Addr is the listen address, either a host ("127.0.0.1", "::1") or a
	// host and port ("127.0.0.1:9000"). Empty listens on every interface.
	Addr string `yaml:"addr"`
	// Port overrides the port inside Addr. 0 takes the port from Addr, and
	// 0 with an Addr that has none means 8080.
	Port int `yaml:"port"`
	// PublicURL is the address users reach this site at, e.g.
	// "https://md.example.com" (no trailing slash). It exists because the
	// GitLab sign-in integration has to hand GitLab an absolute callback
	// address, and GitLab matches it against the application's registered
	// redirect URI character for character.
	//
	// It is configured rather than derived from the request's Host header:
	// that header is attacker-controlled, and behind a reverse proxy it is
	// often not the address the browser used. Empty disables GitLab sign-in
	// — the integration reports itself as not configured.
	PublicURL string `yaml:"publicURL"`
}

// PublicBaseURL returns the configured public URL with any trailing slashes
// removed, or "" when it is not configured.
func (c *Config) PublicBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(c.Server.PublicURL), "/")
}

// Database is the `database` section.
type Database struct {
	// DSN is a SQLite file path or a PostgreSQL URL.
	DSN string `yaml:"dsn"`
}

// Worker is the `worker` section: the scheduling pool that executes
// dispatched tasks.
type Worker struct {
	// Enabled is false to record dispatches without executing them, which
	// is how a demo deployment shows the task graphs without test nodes.
	Enabled bool `yaml:"enabled"`
	// Count is the pool size. 0 uses the runner's built-in default.
	Count int `yaml:"count"`
}

// Config is the whole configuration file, and also the resolved configuration
// Load returns: the file's values with the environment overlaid on top.
type Config struct {
	Server   Server   `yaml:"server"`
	Database Database `yaml:"database"`
	// Dist is the directory holding the built frontend. Empty searches the
	// usual places (frontend/dist, ../frontend/dist, ./dist), which is what
	// running from the project root or from server/ needs.
	Dist string `yaml:"dist"`
	// Worker is the scheduling pool.
	Worker Worker `yaml:"worker"`
	// ObjectStorage configures the artifact store (MinIO / S3).
	ObjectStorage storage.Config `yaml:"objectStorage"`
}

// defaults is the configuration used when neither the file nor the
// environment supplies a value. The file is decoded onto it, so a key left
// out keeps the value here rather than becoming a zero.
func defaults() Config {
	return Config{
		// Addr "" and Port 0: every interface, port 8080 (see
		// resolveListenAddr).
		Server:   Server{},
		Database: Database{DSN: DefaultDSN},
		// Count 0 leaves the pool size to the runner's default.
		Worker: Worker{Enabled: true},
	}
}

// Load reads the server configuration: the file, the environment on top, and
// the built-in defaults under both.
//
// pinned is the path given on the command line (the -config flag, "" when it
// was not used); it wins over $MD_BUILDER_CONFIG and over the default
// locations. The returned source is where the configuration came from — the
// file's path, or "(environment)" when no file was found — and is meant for
// startup logging and error messages.
//
// The object storage section is not validated here: parsing the file and
// enforcing the store's requirements are separate concerns, and a caller that
// never opens the store should not need a complete section. The check happens
// where the store is opened (openObjectStorage), which is also where a
// deployment with no config file at all is told what it is missing.
func Load(pinned string) (Config, string, error) {
	return load(pinned, true)
}

// LoadOptional is Load for a caller that can run without object storage, such
// as the adduser subcommand: a missing config file yields the defaults and
// the environment instead of an error, since the command must work on a host
// that has never been configured. A file that exists but does not parse is
// still an error, so a broken configuration is never silently ignored.
func LoadOptional(pinned string) (Config, string, error) {
	return load(pinned, false)
}

func load(pinned string, fileRequired bool) (Config, string, error) {
	path, source, err := locate(pinned, fileRequired)
	if err != nil {
		return Config{}, "", err
	}

	cfg := defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, "", fmt.Errorf("read %s: %w", path, err)
		}
		// Strict decoding: a misspelled key (server.adr) must fail loudly
		// instead of silently leaving the setting at its default.
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, "", fmt.Errorf("parse %s: %w", path, err)
		}
		cfg.ObjectStorage.ConfigPath = path
	}

	if err := applyEnv(&cfg); err != nil {
		return Config{}, "", err
	}
	// The env may be the only source, so record where the values came from
	// for the validation message.
	if cfg.ObjectStorage.ConfigPath == "" {
		cfg.ObjectStorage.ConfigPath = source
	}
	return cfg, source, nil
}

// locate resolves which config file to read: -config, then
// $MD_BUILDER_CONFIG, then md-builder-server.yaml in the working directory or
// under ./server. An empty path without an error means "no file at all".
//
// When fileRequired is set, that is only acceptable if the environment
// carries the object storage settings, since the server cannot run without a
// store; otherwise the caller's defaults and environment are enough.
func locate(pinned string, fileRequired bool) (path, source string, err error) {
	if p := strings.TrimSpace(pinned); p != "" {
		if _, statErr := os.Stat(p); statErr != nil {
			return "", "", fmt.Errorf("config file %s (-config): %w", p, statErr)
		}
		return p, p, nil
	}
	if p := strings.TrimSpace(os.Getenv(EnvConfigPath)); p != "" {
		if _, statErr := os.Stat(p); statErr != nil {
			return "", "", fmt.Errorf("%s points at %s, which does not exist", EnvConfigPath, p)
		}
		return p, p, nil
	}
	for _, candidate := range []string{DefaultFileName, filepath.Join("server", DefaultFileName)} {
		if st, statErr := os.Stat(candidate); statErr == nil && !st.IsDir() {
			return candidate, candidate, nil
		}
	}
	if fileRequired && !envComplete() {
		return "", "", fmt.Errorf("no %s found (looked in the working directory and ./server) and no %s/%s environment settings; pass -config to name one",
			DefaultFileName, "MD_BUILDER_S3_ENDPOINT", "MD_BUILDER_S3_BUCKET")
	}
	return "", "(environment)", nil
}

// envComplete reports whether the environment alone configures the store, so
// a deployment can run without a config file at all.
func envComplete() bool {
	for _, key := range []string{"MD_BUILDER_S3_ENDPOINT", "MD_BUILDER_S3_ACCESS_KEY", "MD_BUILDER_S3_SECRET_KEY", "MD_BUILDER_S3_BUCKET"} {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			return false
		}
	}
	return true
}

// applyEnv overlays the environment onto the file's values. Only set
// variables win, so the file stays the documented source of truth for a
// deployment that has one, while a container can be pointed elsewhere from
// its environment alone.
//
// A variable that is set but unparseable is an error rather than a silent
// no-op: MD_BUILDER_S3_GC=ture would otherwise leave orphan reclamation
// switched off without a word, and MD_BUILDER_PORT=eighty would quietly serve
// on 8080.
func applyEnv(cfg *Config) error {
	setString := func(key string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*dst = v
		}
	}
	setBool := func(key string, dst *bool) error {
		v := strings.TrimSpace(os.Getenv(key))
		if v == "" {
			return nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %q is not a boolean", key, v)
		}
		*dst = b
		return nil
	}

	setString(EnvAddr, &cfg.Server.Addr)
	if v := strings.TrimSpace(os.Getenv(EnvPort)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > 65535 {
			return fmt.Errorf("%s: %q is not a port number", EnvPort, v)
		}
		cfg.Server.Port = n
	} else if strings.TrimSpace(os.Getenv(EnvAddr)) != "" {
		// An address from the environment carries its own port, and the
		// file's port setting must not quietly override it:
		// MD_BUILDER_ADDR=127.0.0.1:9000 with server.port: 8080 in the file
		// has to listen on 9000. Set MD_BUILDER_PORT as well to replace the
		// port inside the address.
		cfg.Server.Port = 0
	}
	setString(EnvDSN, &cfg.Database.DSN)
	setString(EnvDist, &cfg.Dist)
	setString(EnvPublicURL, &cfg.Server.PublicURL)
	if v := strings.TrimSpace(os.Getenv(EnvWorkers)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("%s: %q is not a positive number of workers", EnvWorkers, v)
		}
		cfg.Worker.Count = n
	}
	if v := strings.TrimSpace(os.Getenv(EnvDisableWorker)); v != "" {
		disabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s: %q is not a boolean", EnvDisableWorker, v)
		}
		cfg.Worker.Enabled = !disabled
	}

	setString("MD_BUILDER_S3_ENDPOINT", &cfg.ObjectStorage.Endpoint)
	setString("MD_BUILDER_S3_ACCESS_KEY", &cfg.ObjectStorage.AccessKey)
	setString("MD_BUILDER_S3_SECRET_KEY", &cfg.ObjectStorage.SecretKey)
	setString("MD_BUILDER_S3_BUCKET", &cfg.ObjectStorage.Bucket)
	setString("MD_BUILDER_S3_REGION", &cfg.ObjectStorage.Region)
	setString("MD_BUILDER_S3_PREFIX", &cfg.ObjectStorage.Prefix)
	for _, b := range []struct {
		key string
		dst *bool
	}{
		{"MD_BUILDER_S3_USE_SSL", &cfg.ObjectStorage.UseSSL},
		{"MD_BUILDER_S3_AUTO_CREATE_BUCKET", &cfg.ObjectStorage.AutoCreateBucket},
		{"MD_BUILDER_S3_GC", &cfg.ObjectStorage.GC},
	} {
		if err := setBool(b.key, b.dst); err != nil {
			return err
		}
	}
	if v := strings.TrimSpace(os.Getenv("MD_BUILDER_S3_GC_INTERVAL_HOURS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return fmt.Errorf("MD_BUILDER_S3_GC_INTERVAL_HOURS: %q is not a positive number of hours", v)
		}
		cfg.ObjectStorage.GCIntervalHours = n
	}
	return nil
}
