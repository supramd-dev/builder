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

// File is the parsed config file.
type File struct {
	// ObjectStorage configures the artifact store (MinIO / S3).
	ObjectStorage storage.Config `yaml:"objectStorage"`
}

// Load reads the server config file and applies the environment overrides.
//
// The returned path is where the configuration came from (the file, or
// "(environment)" when only MD_BUILDER_S3_* variables were set) and is meant
// for startup logging and error messages.
func Load() (storage.Config, string, error) {
	path, explicit := resolvePath()
	cfg := storage.Config{}
	source := "(environment)"

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
		source = path
	} else if explicit {
		return storage.Config{}, "", fmt.Errorf("%s points at %s, which does not exist", EnvConfigPath, os.Getenv(EnvConfigPath))
	} else if !envComplete() {
		return storage.Config{}, "", fmt.Errorf("no %s found (looked in the working directory and ./server) and no %s/%s environment settings",
			DefaultFileName, "MD_BUILDER_S3_ENDPOINT", "MD_BUILDER_S3_BUCKET")
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

// resolvePath finds the config file: $MD_BUILDER_CONFIG first, then
// md-builder-server.yaml next to the working directory or under ./server.
// explicit reports whether the location was pinned by the environment.
func resolvePath() (path string, explicit bool) {
	if p := strings.TrimSpace(os.Getenv(EnvConfigPath)); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", true
		}
		return p, true
	}
	for _, candidate := range []string{DefaultFileName, filepath.Join("server", DefaultFileName)} {
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, false
		}
	}
	return "", false
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
