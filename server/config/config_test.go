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
		EnvConfigPath, EnvAddr, EnvPort, EnvDSN, EnvDist, EnvWorkers,
		EnvDisableWorker,
		"MD_BUILDER_S3_ENDPOINT", "MD_BUILDER_S3_ACCESS_KEY",
		"MD_BUILDER_S3_SECRET_KEY", "MD_BUILDER_S3_BUCKET", "MD_BUILDER_S3_REGION",
		"MD_BUILDER_S3_PREFIX", "MD_BUILDER_S3_USE_SSL",
		"MD_BUILDER_S3_AUTO_CREATE_BUCKET", "MD_BUILDER_S3_GC",
		"MD_BUILDER_S3_GC_INTERVAL_HOURS",
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
	st := cfg.ObjectStorage
	if st.Endpoint != "minio.example.com:9000" || st.Bucket != "artifacts" || !st.UseSSL {
		t.Fatalf("cfg = %+v", st)
	}
	if st.Prefix != "ci" || !st.AutoCreateBucket || !st.GC || st.GCIntervalHours != 3 {
		t.Fatalf("cfg = %+v", st)
	}
	if st.KeyPrefix() != "ci" {
		t.Fatalf("KeyPrefix = %q", st.KeyPrefix())
	}
	if !strings.HasSuffix(source, DefaultFileName) {
		t.Fatalf("source = %q", source)
	}
}

// Every section the file can hold, read from one file.
func TestLoadAllSections(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
server:
  addr: 127.0.0.1
  port: 9000
database:
  dsn: /data/md-builder.db
dist: /srv/dist
worker:
  enabled: false
  count: 7
objectStorage:
  endpoint: minio:9000
  accessKey: ak
  secretKey: sk
  bucket: artifacts
`)
	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1" || cfg.Server.Port != 9000 {
		t.Fatalf("server = %+v", cfg.Server)
	}
	if cfg.Database.DSN != "/data/md-builder.db" {
		t.Fatalf("database = %+v", cfg.Database)
	}
	if cfg.Dist != "/srv/dist" {
		t.Fatalf("dist = %q", cfg.Dist)
	}
	if cfg.Worker.Enabled || cfg.Worker.Count != 7 {
		t.Fatalf("worker = %+v", cfg.Worker)
	}
}

// A file that sets only object storage leaves every other section at its
// default, so a deployment can grow into the file one setting at a time.
func TestLoadPartialFileKeepsDefaults(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "objectStorage:\n  endpoint: e\n  accessKey: ak\n  secretKey: sk\n  bucket: b\n")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Addr != "" || cfg.Server.Port != 0 {
		t.Fatalf("server = %+v, want the empty address and port 0", cfg.Server)
	}
	if cfg.Database.DSN != DefaultDSN {
		t.Fatalf("dsn = %q, want %q", cfg.Database.DSN, DefaultDSN)
	}
	if !cfg.Worker.Enabled || cfg.Worker.Count != 0 {
		t.Fatalf("worker = %+v, want it enabled with the runner's default count", cfg.Worker)
	}
	if cfg.Dist != "" {
		t.Fatalf("dist = %q", cfg.Dist)
	}
}

// With no file at all, every setting is its default. adduser runs this way on
// a host that has never been configured.
func TestLoadOptionalWithoutFile(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())

	cfg, source, err := LoadOptional("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Database.DSN != DefaultDSN || cfg.Server.Port != 0 || !cfg.Worker.Enabled {
		t.Fatalf("cfg = %+v", cfg)
	}
	if source != "(environment)" {
		t.Fatalf("source = %q", source)
	}

	// The server itself still refuses to start without a store.
	if _, _, err := Load(""); err == nil {
		t.Fatal("Load should require a config file when the environment has no store")
	}
}

// A file that exists but is broken is an error even for the lenient caller:
// falling back to the defaults would point adduser at a different database.
func TestLoadOptionalRejectsBrokenFile(t *testing.T) {
	clearEnv(t)
	writeConfig(t, "server:\n  port: not-a-number\n")
	if _, _, err := LoadOptional(""); err == nil {
		t.Fatal("a malformed file should be reported")
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

// The same for the new sections.
func TestLoadRejectsUnknownKeyInServerSection(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
server:
  adr: 127.0.0.1
objectStorage:
  endpoint: e
  accessKey: ak
  secretKey: sk
  bucket: b
`)
	if _, _, err := Load(""); err == nil || !strings.Contains(err.Error(), "adr") {
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

// Load parses; it is the caller that opens the store and enforces its
// requirements. The message still names the file to fix.
func TestLoadLeavesValidationToTheStore(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpoint: minio.example.com:9000
`)
	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = cfg.ObjectStorage.Validate()
	if err == nil || !strings.Contains(err.Error(), "objectStorage.accessKey") {
		t.Fatalf("err = %v", err)
	}
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
	st := cfg.ObjectStorage
	if st.Endpoint != "env.example.com:9000" || st.SecretKey != "from-env" {
		t.Fatalf("env did not win: %+v", st)
	}
	if st.AccessKey != "from-file" || st.Bucket != "from-file" {
		t.Fatalf("unset variables should not clear the file: %+v", st)
	}
	if !st.UseSSL {
		t.Fatal("UseSSL should come from the environment")
	}
}

// Every setting, not just the store's, is overridable from the environment.
func TestLoadEnvOverridesAllSections(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
server:
  addr: 127.0.0.1
  port: 9000
database:
  dsn: from-file.db
dist: /from/file
worker:
  enabled: true
  count: 3
objectStorage:
  endpoint: e
  accessKey: ak
  secretKey: sk
  bucket: b
`)
	t.Setenv(EnvAddr, "0.0.0.0")
	t.Setenv(EnvPort, "9100")
	t.Setenv(EnvDSN, "from-env.db")
	t.Setenv(EnvDist, "/from/env")
	t.Setenv(EnvWorkers, "9")
	t.Setenv(EnvDisableWorker, "1")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Addr != "0.0.0.0" || cfg.Server.Port != 9100 {
		t.Fatalf("server = %+v", cfg.Server)
	}
	if cfg.Database.DSN != "from-env.db" {
		t.Fatalf("database = %+v", cfg.Database)
	}
	if cfg.Dist != "/from/env" {
		t.Fatalf("dist = %q", cfg.Dist)
	}
	if cfg.Worker.Enabled || cfg.Worker.Count != 9 {
		t.Fatalf("worker = %+v", cfg.Worker)
	}
}

// An address from the environment replaces the file's address together with
// the port inside it, so a container can move the server off the port the
// file names. Adding MD_BUILDER_PORT overrides the port in the address too.
func TestLoadEnvAddrCarriesItsPort(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
server:
  addr: 127.0.0.1
  port: 8080
objectStorage:
  endpoint: e
  accessKey: ak
  secretKey: sk
  bucket: b
`)
	t.Setenv(EnvAddr, "0.0.0.0:9000")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Port 0 is what makes the port inside the address stick; see
	// resolveListenAddr in package main for the other half.
	if cfg.Server.Addr != "0.0.0.0:9000" || cfg.Server.Port != 0 {
		t.Fatalf("server = %+v, want the environment's address and no port override", cfg.Server)
	}

	t.Setenv(EnvPort, "9100")
	cfg, _, err = Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Server.Port != 9100 {
		t.Fatalf("server = %+v, want the environment's port", cfg.Server)
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
	if cfg.ObjectStorage.Endpoint != "minio:9000" || cfg.ObjectStorage.Bucket != "artifacts" {
		t.Fatalf("cfg = %+v", cfg.ObjectStorage)
	}
	if cfg.Server.Port != 0 || cfg.Database.DSN != DefaultDSN {
		t.Fatalf("cfg = %+v, want the defaults for everything the environment did not set", cfg)
	}
	if source != "(environment)" {
		t.Fatalf("source = %q", source)
	}
}

// The knobs that only a file could set before are reachable from the
// environment too, which is what a container deployment has.
func TestLoadEnvToggles(t *testing.T) {
	clearEnv(t)
	t.Chdir(t.TempDir())
	t.Setenv("MD_BUILDER_S3_ENDPOINT", "minio:9000")
	t.Setenv("MD_BUILDER_S3_ACCESS_KEY", "ak")
	t.Setenv("MD_BUILDER_S3_SECRET_KEY", "sk")
	t.Setenv("MD_BUILDER_S3_BUCKET", "artifacts")
	t.Setenv("MD_BUILDER_S3_AUTO_CREATE_BUCKET", "true")
	t.Setenv("MD_BUILDER_S3_GC", "1")
	t.Setenv("MD_BUILDER_S3_GC_INTERVAL_HOURS", "12")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.ObjectStorage.AutoCreateBucket || !cfg.ObjectStorage.GC {
		t.Fatalf("toggles did not apply: %+v", cfg.ObjectStorage)
	}
	if got := cfg.ObjectStorage.GCInterval(); got.Hours() != 12 {
		t.Fatalf("GCInterval = %v, want 12h", got)
	}
}

// An environment toggle wins over the file, so a container can flip one
// setting without rewriting the file.
func TestLoadEnvTogglesBeatFile(t *testing.T) {
	clearEnv(t)
	writeConfig(t, `
objectStorage:
  endpoint: minio:9000
  accessKey: ak
  secretKey: sk
  bucket: artifacts
  autoCreateBucket: false
  gc: false
  gcIntervalHours: 3
`)
	t.Setenv("MD_BUILDER_S3_AUTO_CREATE_BUCKET", "true")
	t.Setenv("MD_BUILDER_S3_GC", "true")
	t.Setenv("MD_BUILDER_S3_GC_INTERVAL_HOURS", "24")

	cfg, _, err := Load("")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	st := cfg.ObjectStorage
	if !st.AutoCreateBucket || !st.GC || st.GCIntervalHours != 24 {
		t.Fatalf("cfg = %+v", st)
	}
}

// A malformed toggle must fail the load: MD_BUILDER_S3_GC=ture would
// otherwise leave reclamation quietly switched off.
func TestLoadRejectsMalformedEnvToggle(t *testing.T) {
	for _, tc := range []struct {
		key, value string
	}{
		{"MD_BUILDER_S3_USE_SSL", "yes please"},
		{"MD_BUILDER_S3_AUTO_CREATE_BUCKET", "ture"},
		{"MD_BUILDER_S3_GC", "on"},
		{"MD_BUILDER_S3_GC_INTERVAL_HOURS", "often"},
		{"MD_BUILDER_S3_GC_INTERVAL_HOURS", "0"},
		{"MD_BUILDER_S3_GC_INTERVAL_HOURS", "-6"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Chdir(t.TempDir())
			t.Setenv("MD_BUILDER_S3_ENDPOINT", "minio:9000")
			t.Setenv("MD_BUILDER_S3_ACCESS_KEY", "ak")
			t.Setenv("MD_BUILDER_S3_SECRET_KEY", "sk")
			t.Setenv("MD_BUILDER_S3_BUCKET", "artifacts")
			t.Setenv(tc.key, tc.value)

			_, _, err := Load("")
			if err == nil {
				t.Fatalf("%s=%s should be rejected", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err should name the variable: %v", err)
			}
		})
	}
}

// The listen address and the worker pool are numbers too, and a typo must not
// quietly fall back to the default.
func TestLoadRejectsMalformedEnvNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, key, value string
	}{
		{"port not a number", EnvPort, "eighty"},
		{"port out of range", EnvPort, "70000"},
		{"port negative", EnvPort, "-1"},
		{"workers not a number", EnvWorkers, "two"},
		{"workers zero", EnvWorkers, "0"},
		{"workers negative", EnvWorkers, "-2"},
		{"disable worker not a boolean", EnvDisableWorker, "maybe"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Chdir(t.TempDir())
			t.Setenv(tc.key, tc.value)

			_, _, err := LoadOptional("")
			if err == nil {
				t.Fatalf("%s=%s should be rejected", tc.key, tc.value)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("err should name the variable: %v", err)
			}
		})
	}
}

// MD_BUILDER_DISABLE_WORKER is named for what it turns off, so it inverts.
func TestLoadDisableWorker(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"1", false},
		{"true", false},
		{"0", true},
		{"false", true},
		{"", true},
	} {
		t.Run("disable="+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Chdir(t.TempDir())
			t.Setenv(EnvDisableWorker, tc.value)

			cfg, _, err := LoadOptional("")
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.Worker.Enabled != tc.want {
				t.Fatalf("worker.enabled = %t, want %t", cfg.Worker.Enabled, tc.want)
			}
		})
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
	if cfg.ObjectStorage.Bucket != "b" {
		t.Fatalf("cfg = %+v", cfg.ObjectStorage)
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
	if cfg.ObjectStorage.Endpoint != "pinned" || cfg.ObjectStorage.Bucket != "pinned" {
		t.Fatalf("the pinned file should win: %+v", cfg.ObjectStorage)
	}
	if source != pinned || cfg.ObjectStorage.ConfigPath != pinned {
		t.Fatalf("source = %q, config path = %q", source, cfg.ObjectStorage.ConfigPath)
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
	if cfg.ObjectStorage.Endpoint != "pinned" {
		t.Fatalf("cfg = %+v", cfg.ObjectStorage)
	}

	cfg, _, err = Load("")
	if err != nil {
		t.Fatalf("load env: %v", err)
	}
	if cfg.ObjectStorage.Endpoint != "env" {
		t.Fatalf("cfg = %+v", cfg.ObjectStorage)
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
	if cfg.ObjectStorage.Endpoint != "minio:9000" || source != "(environment)" {
		t.Fatalf("cfg = %+v, source = %q", cfg.ObjectStorage, source)
	}
}
