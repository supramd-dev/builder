// Package storage is the object-storage backend that holds md-builder's test
// artifacts.
//
// Test output files (gtest results XML, build artifacts, per-case series data)
// live in an S3-compatible store — MinIO in the supported deployment — and the
// database keeps only a reference: the object key plus its size. Nothing in
// this package knows about runs or artifacts; it stores opaque byte blobs
// under caller-chosen keys, so the store layer owns the key layout
// (see ArtifactKey) and the API layer owns the read paths.
//
// The backend is mandatory: a deployment without object storage cannot record
// artifacts, and the server refuses to start without it. Memory is a test
// double for the unit suite, not a fallback.
package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned by Get, Open and Stat when the object does not
// exist. Callers map it to a 404 rather than a backend failure.
var ErrNotFound = errors.New("object not found")

// ErrNotConfigured is returned by operations on a Store that has no backend
// wired up (an empty Store interface value).
var ErrNotConfigured = errors.New("object storage is not configured")

// ObjectMeta describes one stored object.
type ObjectMeta struct {
	// Key is the full object key, including any configured prefix.
	Key string
	// Size is the object's length in bytes.
	Size int64
	// LastModified is when the object was last written (zero when the
	// backend does not report it).
	LastModified time.Time
}

// Store stores artifact blobs. Implementations must be safe for concurrent
// use: the runner's worker pool and the API server share one instance.
type Store interface {
	// Put stores data under key, overwriting any previous object.
	Put(ctx context.Context, key string, data []byte) (ObjectMeta, error)
	// Get returns the whole object. It returns ErrNotFound when the key
	// does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Open streams the object instead of buffering it; size is the
	// object's length, or -1 when unknown. The caller closes the reader.
	Open(ctx context.Context, key string) (rc io.ReadCloser, size int64, err error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// List returns the objects under prefix, recursively.
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	// Ping reports whether the backend is reachable and the bucket
	// usable. It backs the site health board's object-storage row.
	Ping(ctx context.Context) error
	// Describe names the backend for display, e.g.
	// "minio 127.0.0.1:9000/md-builder". It must never include
	// credentials.
	Describe() string
	// KeyPrefix is the namespace this backend's artifact keys start with
	// ("" when the whole bucket is ours). Callers pass it to ArtifactKey.
	KeyPrefix() string
}

// Config is the object-storage section of the server config file
// (md-builder-server.yaml).
type Config struct {
	// Endpoint is the host:port of the S3 API, without a scheme
	// ("127.0.0.1:9000"). Required.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// AccessKey / SecretKey are the credentials. Required. SecretKey is
	// registered with the redaction helpers at load time so it can never
	// appear in an error, a log line, or the health board.
	AccessKey string `yaml:"accessKey" json:"accessKey"`
	SecretKey string `yaml:"secretKey" json:"secretKey"`
	// Bucket holds every artifact. Required. Created on startup when
	// AutoCreateBucket is set.
	Bucket string `yaml:"bucket" json:"bucket"`
	// UseSSL selects https for the API connection.
	UseSSL bool `yaml:"useSSL" json:"useSSL"`
	// Region is optional; empty lets the SDK resolve it.
	Region string `yaml:"region,omitempty" json:"region,omitempty"`
	// Prefix namespaces every key this server writes, so one bucket can
	// be shared with other tools. Optional ("artifacts").
	Prefix string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	// AutoCreateBucket creates Bucket when it is missing at startup.
	AutoCreateBucket bool `yaml:"autoCreateBucket" json:"autoCreateBucket"`
	// GC enables the background sweep that reclaims objects no run
	// references any more.
	GC bool `yaml:"gc" json:"gc"`
	// GCIntervalHours is how often that sweep runs. Defaults to 6.
	GCIntervalHours int `yaml:"gcIntervalHours,omitempty" json:"gcIntervalHours,omitempty"`

	// ConfigPath records where this configuration was read from, for
	// error messages. Set by the config loader; not a file key.
	ConfigPath string `yaml:"-" json:"-"`
}

// String renders the config with the secret masked, so an accidental %v (a
// log line, a test failure) can never leak the credential.
func (c Config) String() string {
	return fmt.Sprintf("objectStorage{endpoint:%s bucket:%s prefix:%s useSSL:%t accessKey:%s secretKey:%s}",
		c.Endpoint, c.Bucket, c.Prefix, c.UseSSL, mask(c.AccessKey), mask(c.SecretKey))
}

// mask shows just enough of a credential to tell whether it is set.
func mask(s string) string {
	if s == "" {
		return "(unset)"
	}
	return "REDACTED"
}

// Validate reports the first missing required setting, naming the config file
// so the operator knows where to fix it. It never echoes a credential.
func (c Config) Validate() error {
	where := c.ConfigPath
	if where == "" {
		where = "the server config file"
	}
	for _, req := range []struct {
		name  string
		value string
	}{
		{"objectStorage.endpoint", c.Endpoint},
		{"objectStorage.accessKey", c.AccessKey},
		{"objectStorage.secretKey", c.SecretKey},
		{"objectStorage.bucket", c.Bucket},
	} {
		if strings.TrimSpace(req.value) == "" {
			return fmt.Errorf("%s: %s is required", where, req.name)
		}
	}
	return nil
}

// DefaultGCIntervalHours is the sweep period when GCIntervalHours is unset.
const DefaultGCIntervalHours = 6

// GCInterval returns the configured sweep period.
func (c Config) GCInterval() time.Duration {
	h := c.GCIntervalHours
	if h <= 0 {
		h = DefaultGCIntervalHours
	}
	return time.Duration(h) * time.Hour
}

// Describe names the backend for display without credentials.
func (c Config) Describe() string {
	scheme := "http"
	if c.UseSSL {
		scheme = "https"
	}
	return fmt.Sprintf("minio %s://%s/%s", scheme, c.Endpoint, c.Bucket)
}

// KeyPrefix returns the namespace every artifact key of this server starts
// with, with any trailing slash removed ("artifacts", or "" when unset).
func (c Config) KeyPrefix() string {
	return strings.Trim(c.Prefix, "/")
}

// RunsPrefix is the key namespace holding every artifact of every run. The
// orphan sweep lists this prefix; ArtifactKey must keep building keys under it.
func RunsPrefix(keyPrefix string) string {
	keyPrefix = strings.Trim(keyPrefix, "/")
	if keyPrefix == "" {
		return "runs/"
	}
	return keyPrefix + "/runs/"
}

// ArtifactKey builds the object key for one artifact.
//
// The key is deterministic — "<prefix>/runs/<runID>/<kind>/<name>" — so a
// re-report of the same run overwrites its objects in place instead of
// accumulating versions, and the run id is readable in the key, which is what
// lets the orphan sweep decide whether an object is still referenced. dup
// (0-based) disambiguates artifacts that share a name within one batch.
func ArtifactKey(keyPrefix string, runID int64, kind, name string, dup int) string {
	segments := []string{"runs", strconv.FormatInt(runID, 10), keySegment(kind), keySegment(name)}
	if dup > 0 {
		segments[len(segments)-1] += "-" + strconv.Itoa(dup+1)
	}
	if p := strings.Trim(keyPrefix, "/"); p != "" {
		segments = append([]string{p}, segments...)
	}
	return strings.Join(segments, "/")
}

// maxKeySegment bounds one path segment; object stores reject very long keys
// and the artifact name is an arbitrary source path.
const maxKeySegment = 128

// keySegment makes one path segment safe to use as an object key: path
// separators and anything that could escape the prefix are replaced, and long
// names are truncated with a hash suffix so distinct names stay distinct.
func keySegment(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '/' || r == '\\' || r == ':' || r == '*' || r == '?' ||
			r == '"' || r == '<' || r == '>' || r == '|':
			// A source path ("out/libfoo.a") stays readable but flat:
			// the separator becomes an underscore.
			b.WriteByte('_')
		case r < 0x20 || r == 0x7f:
			// Control characters would corrupt the key.
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "_"
	}
	if len(out) > maxKeySegment {
		// Keep the segment within the bound: the hash of the full name
		// is what makes two long names with the same prefix distinct.
		sum := sha256.Sum256([]byte(out))
		suffix := "-" + hex.EncodeToString(sum[:4])
		out = out[:maxKeySegment-len(suffix)] + suffix
	}
	return out
}
