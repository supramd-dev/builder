package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestArtifactKey(t *testing.T) {
	cases := []struct {
		name     string
		prefix   string
		runID    int64
		kind     string
		artifact string
		dup      int
		want     string
	}{
		{
			name: "plain", runID: 12, kind: "results", artifact: "out.xml",
			want: "runs/12/results/out.xml",
		},
		{
			name: "prefix", prefix: "artifacts", runID: 12, kind: "results", artifact: "out.xml",
			want: "artifacts/runs/12/results/out.xml",
		},
		{
			name: "prefix with slashes", prefix: "/artifacts/", runID: 1, kind: "file", artifact: "a",
			want: "artifacts/runs/1/file/a",
		},
		{
			// A source path stays readable but flat: one segment per
			// artifact, so the key cannot escape the run's namespace.
			name: "path separators are flattened", runID: 3, kind: "file", artifact: "build/sub/libfoo.a",
			want: "runs/3/file/build_sub_libfoo.a",
		},
		{
			name: "duplicates are suffixed", runID: 3, kind: "file", artifact: "a.log", dup: 1,
			want: "runs/3/file/a.log-2",
		},
		{
			name: "third duplicate", runID: 3, kind: "file", artifact: "a.log", dup: 2,
			want: "runs/3/file/a.log-3",
		},
		{
			name: "empty name", runID: 4, kind: "log", artifact: "",
			want: "runs/4/log/_",
		},
		{
			name: "dot name cannot escape", runID: 4, kind: "log", artifact: "..",
			want: "runs/4/log/_",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ArtifactKey(tc.prefix, tc.runID, tc.kind, tc.artifact, tc.dup); got != tc.want {
				t.Fatalf("ArtifactKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestArtifactKeyLongNameStaysDistinct(t *testing.T) {
	long := strings.Repeat("a", 300)
	one := ArtifactKey("", 1, "file", long, 0)
	two := ArtifactKey("", 1, "file", long+"b", 0)
	if one == two {
		t.Fatal("truncated names must not collide")
	}
	if len(one) > maxKeySegment+len("runs/1/file/") {
		t.Fatalf("key too long: %d", len(one))
	}
}

func TestRunsPrefix(t *testing.T) {
	if got := RunsPrefix(""); got != "runs/" {
		t.Fatalf("RunsPrefix(\"\") = %q", got)
	}
	if got := RunsPrefix("artifacts/"); got != "artifacts/runs/" {
		t.Fatalf("RunsPrefix = %q", got)
	}
	// Every artifact key lives under the prefix the sweep lists.
	key := ArtifactKey("artifacts", 9, "results", "out.xml", 0)
	if !strings.HasPrefix(key, RunsPrefix("artifacts")) {
		t.Fatalf("%q is outside %q", key, RunsPrefix("artifacts"))
	}
}

func TestConfigValidate(t *testing.T) {
	full := Config{Endpoint: "127.0.0.1:9000", AccessKey: "ak", SecretKey: "sk", Bucket: "b", ConfigPath: "test.yaml"}
	if err := full.Validate(); err != nil {
		t.Fatalf("complete config: %v", err)
	}
	for _, missing := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"endpoint", Config{AccessKey: "ak", SecretKey: "sk", Bucket: "b", ConfigPath: "test.yaml"}, "objectStorage.endpoint"},
		{"access key", Config{Endpoint: "e", SecretKey: "sk", Bucket: "b", ConfigPath: "test.yaml"}, "objectStorage.accessKey"},
		{"secret key", Config{Endpoint: "e", AccessKey: "ak", Bucket: "b", ConfigPath: "test.yaml"}, "objectStorage.secretKey"},
		{"bucket", Config{Endpoint: "e", AccessKey: "ak", SecretKey: "sk", ConfigPath: "test.yaml"}, "objectStorage.bucket"},
	} {
		t.Run(missing.name, func(t *testing.T) {
			err := missing.cfg.Validate()
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), missing.want) || !strings.Contains(err.Error(), "test.yaml") {
				t.Fatalf("err = %v, want %q and the config path", err, missing.want)
			}
			// The message points at the file without echoing the value.
			if strings.Contains(err.Error(), "sk") && missing.name != "secret key" {
				t.Fatalf("err leaks a value: %v", err)
			}
		})
	}
}

// A config printed by accident (a log line, a failing test) must not expose
// the credentials.
func TestConfigStringMasksSecrets(t *testing.T) {
	cfg := Config{Endpoint: "127.0.0.1:9000", AccessKey: "AKIAEXAMPLE", SecretKey: "hunter2", Bucket: "artifacts"}
	got := cfg.String()
	if strings.Contains(got, "hunter2") || strings.Contains(got, "AKIAEXAMPLE") {
		t.Fatalf("String() leaks credentials: %q", got)
	}
	if !strings.Contains(got, "artifacts") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("String() = %q", got)
	}
}

func TestConfigGCInterval(t *testing.T) {
	if got := (Config{}).GCInterval(); got != DefaultGCIntervalHours*time.Hour {
		t.Fatalf("default interval = %v", got)
	}
	if got := (Config{GCIntervalHours: 2}).GCInterval(); got != 2*time.Hour {
		t.Fatalf("interval = %v", got)
	}
	if got := (Config{GCIntervalHours: -1}).GCInterval(); got != DefaultGCIntervalHours*time.Hour {
		t.Fatalf("negative interval = %v", got)
	}
}

func TestMemoryStoreRoundTrip(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()

	meta, err := m.Put(ctx, "runs/1/results/out.xml", []byte("<x/>"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if meta.Size != 4 || meta.Key != "runs/1/results/out.xml" {
		t.Fatalf("meta = %+v", meta)
	}
	data, err := m.Get(ctx, meta.Key)
	if err != nil || string(data) != "<x/>" {
		t.Fatalf("get = %q, %v", data, err)
	}
	// The store owns its bytes: mutating the caller's slice must not change
	// what was stored.
	data[0] = 'X'
	if again, _ := m.Get(ctx, meta.Key); string(again) != "<x/>" {
		t.Fatalf("stored bytes were aliased: %q", again)
	}

	rc, size, err := m.Open(ctx, meta.Key)
	if err != nil || size != 4 {
		t.Fatalf("open: %d, %v", size, err)
	}
	_ = rc.Close()

	if _, err := m.Put(ctx, "runs/2/results/other.xml", []byte("y")); err != nil {
		t.Fatalf("put: %v", err)
	}
	list, err := m.List(ctx, "runs/1/")
	if err != nil || len(list) != 1 || list[0].Key != meta.Key {
		t.Fatalf("list = %+v, %v", list, err)
	}

	if _, err := m.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	if _, _, err := m.Open(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open missing: %v", err)
	}
	if err := m.Delete(ctx, meta.Key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := m.Delete(ctx, meta.Key); err != nil {
		t.Fatalf("delete missing should be a no-op: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("Len = %d, want 1", m.Len())
	}
	if err := m.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	if got := m.Describe(); got != "memory" {
		t.Fatalf("describe = %q", got)
	}
}

func TestMemoryPrefix(t *testing.T) {
	m := NewMemory()
	m.Prefix = "/artifacts/"
	if got := m.KeyPrefix(); got != "artifacts" {
		t.Fatalf("KeyPrefix = %q", got)
	}
}
