package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a config file into a temp dir and makes it the working
// directory, so Load's default lookup finds it.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, DefaultFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Chdir(dir)
	return path
}

// clearEnv removes the settings Load reads, so a developer's environment
// cannot change the outcome.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		EnvConfigPath, "MD_BUILDER_S3_ENDPOINT", "MD_BUILDER_S3_ACCESS_KEY",
		"MD_BUILDER_S3_SECRET_KEY", "MD_BUILDER_S3_BUCKET", "MD_BUILDER_S3_REGION",
		"MD_BUILDER_S3_PREFIX", "MD_BUILDER_S3_USE_SSL",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadFromFile(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpoint: minio.example.com:9000
  accessKey: md-builder
  secretKey: changeme
  bucket: artifacts
  useSSL: true
  prefix: ci
  autoCreateBucket: true
  gc: true
  gcIntervalHours: 3
`)
	cfg, source, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "minio.example.com:9000" || cfg.Bucket != "artifacts" || !cfg.UseSSL {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.Prefix != "ci" || !cfg.AutoCreateBucket || !cfg.GC || cfg.GCIntervalHours != 3 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if cfg.KeyPrefix() != "ci" {
		t.Fatalf("KeyPrefix = %q", cfg.KeyPrefix())
	}
	if !strings.HasSuffix(source, DefaultFileName) {
		t.Fatalf("source = %q", source)
	}
}

// A misspelled key must fail loudly instead of silently leaving a setting
// unset.
func TestLoadRejectsUnknownKey(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpont: minio.example.com:9000
  accessKey: md-builder
  secretKey: changeme
  bucket: artifacts
`)
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), "endpont") {
		t.Fatalf("err = %v, want an unknown-field error", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), DefaultFileName) {
		t.Fatalf("err = %v, want a message naming the config file", err)
	}
}

func TestLoadRejectsIncompleteFile(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpoint: minio.example.com:9000
`)
	_, _, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "objectStorage.accessKey") {
		t.Fatalf("err = %v", err)
	}
	// The message names the file to fix.
	if !strings.Contains(err.Error(), DefaultFileName) {
		t.Fatalf("err = %v, want it to name the config file", err)
	}
}

// The environment can supply the values, and wins over the file — how
// deployments inject the secret.
func TestLoadEnvOverrides(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpoint: file.example.com:9000
  accessKey: from-file
  secretKey: from-file
  bucket: from-file
`)
	t.Setenv("MD_BUILDER_S3_ENDPOINT", "env.example.com:9000")
	t.Setenv("MD_BUILDER_S3_SECRET_KEY", "from-env")
	t.Setenv("MD_BUILDER_S3_USE_SSL", "true")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "env.example.com:9000" || cfg.SecretKey != "from-env" {
		t.Fatalf("env did not win: %+v", cfg)
	}
	if cfg.AccessKey != "from-file" || cfg.Bucket != "from-file" {
		t.Fatalf("unset variables should not clear the file: %+v", cfg)
	}
	if !cfg.UseSSL {
		t.Fatal("UseSSL should come from the environment")
	}
}

// A container deployment may have no config file at all, as long as the
// environment carries every required value.
func TestLoadFromEnvOnly(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("MD_BUILDER_S3_ENDPOINT", "minio:9000")
	t.Setenv("MD_BUILDER_S3_ACCESS_KEY", "ak")
	t.Setenv("MD_BUILDER_S3_SECRET_KEY", "sk")
	t.Setenv("MD_BUILDER_S3_BUCKET", "artifacts")

	cfg, source, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "minio:9000" || cfg.Bucket != "artifacts" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if source != "(environment)" {
		t.Fatalf("source = %q", source)
	}
}

// A partial environment without a file is an error: the store would be
// half-configured.
func TestLoadPartialEnvWithoutFile(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("MD_BUILDER_S3_ENDPOINT", "minio:9000")
	if _, _, err := Load(""); err == nil {
		t.Fatal("expected an error for a half-configured store")
	}
}

func TestLoadExplicitPathMustExist(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv(EnvConfigPath, filepath.Join(t.TempDir(), "nope.yaml"))
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v", err)
	}
}

// The binary also runs from the project root, where the config file lives
// under server/.
func TestLoadFindsServerSubdirectory(t *testing.T) {
	clearEnv(t)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "server"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := "objectStorage:\n  endpoint: e\n  accessKey: ak\n  secretKey: sk\n  bucket: b\n"
	if err := os.WriteFile(filepath.Join(dir, "server", DefaultFileName), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Chdir(dir)

	cfg, source, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Bucket != "b" {
		t.Fatalf("cfg = %+v", cfg)
	}
	if !strings.Contains(source, "server") {
		t.Fatalf("source = %q", source)
	}
}

// A path given on the command line (-config) is read even when the working
// directory holds a different, valid config file.
func TestLoadFromPinnedPath(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "objectStorage:\n  endpoint: from-cwd\n  accessKey: ak\n  secretKey: sk\n  bucket: cwd\n")

	dir := t.TempDir()
	pinned := filepath.Join(dir, "elsewhere.yaml")
	if err := os.WriteFile(pinned, []byte("objectStorage:\n  endpoint: pinned\n  accessKey: ak\n  secretKey: sk\n  bucket: pinned\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, source, err := Load(pinned)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "pinned" || cfg.Bucket != "pinned" {
		t.Fatalf("the pinned file should win: %+v", cfg)
	}
	if source != pinned || cfg.ConfigPath != pinned {
		t.Fatalf("source = %q, config path = %q", source, cfg.ConfigPath)
	}
}

// -config beats $MD_BUILDER_CONFIG, which beats the default lookup.
func TestLoadPinnedPathBeatsEnv(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "objectStorage:\n  endpoint: from-cwd\n  accessKey: ak\n  secretKey: sk\n  bucket: cwd\n")

	dir := t.TempDir()
	fromEnv := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(fromEnv, []byte("objectStorage:\n  endpoint: env\n  accessKey: ak\n  secretKey: sk\n  bucket: env\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	pinned := filepath.Join(dir, "pinned.yaml")
	if err := os.WriteFile(pinned, []byte("objectStorage:\n  endpoint: pinned\n  accessKey: ak\n  secretKey: sk\n  bucket: pinned\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv(EnvConfigPath, fromEnv)

	cfg, _, err := Load(pinned)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "pinned" {
		t.Fatalf("cfg = %+v", cfg)
	}

	cfg, _, err = Load("")
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	if cfg.Endpoint != "env" {
		t.Fatalf("cfg = %+v", cfg)
	}
}

// A pinned path that does not exist names the flag rather than falling back
// to another file: a typo must not silently serve a different deployment.
func TestLoadPinnedPathMustExist(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "objectStorage:\n  endpoint: from-cwd\n  accessKey: ak\n  secretKey: sk\n  bucket: cwd\n")

	missing := filepath.Join(t.TempDir(), "nope.yaml")
	_, _, err := Load(missing)
	if err == nil || !strings.Contains(err.Error(), "-config") {
		t.Fatalf("err = %v", err)
	}
}

// The environment can still configure the store when no file is pinned.
func TestLoadPinnedEmptyKeepsEnvOnlyPath(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("MD_BUILDER_S3_ENDPOINT", "minio:9000")
	t.Setenv("MD_BUILDER_S3_ACCESS_KEY", "ak")
	t.Setenv("MD_BUILDER_S3_SECRET_KEY", "sk")
	t.Setenv("MD_BUILDER_S3_BUCKET", "b")

	cfg, source, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Endpoint != "minio:9000" || source != "(environment)" {
		t.Fatalf("cfg = %+v, source = %q", cfg, source)
	}
}
