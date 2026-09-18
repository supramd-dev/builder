// Package config loads the server's own configuration file
// (md-builder-server.yaml) — deployment settings that belong to the host, not
// to a test matrix.
//
// It is deliberately distinct from the pipeline configuration
// (md-builder.yaml), which is fetched from the code repository per dispatch
// and parsed by the runner package. This file is read once at startup.
//
// Object storage is the only section so far. It is mandatory: artifacts live
// in the object store, so a server without it cannot run. Every setting can
// also be supplied through the environment (MD_BUILDER_S3_*), which is how
// containers and CI deployments usually inject the credentials.
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

// File is the parsed config file.
type File struct {
	// ObjectStorage configures the artifact store (MinIO / S3).
	ObjectStorage storage.Config `yaml:"objectStorage"`
}

// Load reads the server config file and applies the environment overrides.
//
// pinned is the path given on the command line (the -config flag, "" when it
// was not used); it wins over $MD_BUILDER_CONFIG and over the default
// locations. The returned source is where the configuration came from — the
// file's path, or "(environment)" when only MD_BUILDER_S3_* variables were
// set — and is meant for startup logging and error messages.
func Load(pinned string) (storage.Config, string, error) {
	path, source, err := locate(pinned)
	if err != nil {
		return storage.Config{}, "", err
	}

	cfg := storage.Config{}
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return storage.Config{}, "", fmt.Errorf("read %s: %w", path, err)
		}
		// Strict decoding: a misspelled key (objectStorage.endpont) must
		// fail loudly instead of silently leaving the endpoint empty.
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		var f File
		if err := dec.Decode(&f); err != nil {
			return storage.Config{}, "", fmt.Errorf("parse %s: %w", path, err)
		}
		cfg = f.ObjectStorage
		cfg.ConfigPath = path
	}

	applyEnv(&cfg)
	// The env may be the only source, so record where the values came from
	// for the validation message.
	if cfg.ConfigPath == "" {
		cfg.ConfigPath = source
	}
	if err := cfg.Validate(); err != nil {
		return storage.Config{}, "", err
	}
	return cfg, source, nil
}

// locate resolves which config file to read: -config, then
// $MD_BUILDER_CONFIG, then md-builder-server.yaml in the working directory or
// under ./server. An empty path without an error means "no file at all" —
// the environment alone must configure the store, which the caller's
// Validate rejects when it does not.
func locate(pinned string) (path, source string, err error) {
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
	if !envComplete() {
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
// variables win, so the file stays the documented source of truth.
func applyEnv(cfg *storage.Config) {
	setString := func(key string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			*dst = v
		}
	}
	setString("MD_BUILDER_S3_ENDPOINT", &cfg.Endpoint)
	setString("MD_BUILDER_S3_ACCESS_KEY", &cfg.AccessKey)
	setString("MD_BUILDER_S3_SECRET_KEY", &cfg.SecretKey)
	setString("MD_BUILDER_S3_BUCKET", &cfg.Bucket)
	setString("MD_BUILDER_S3_REGION", &cfg.Region)
	setString("MD_BUILDER_S3_PREFIX", &cfg.Prefix)
	if v := strings.TrimSpace(os.Getenv("MD_BUILDER_S3_USE_SSL")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.UseSSL = b
		}
	}
}
