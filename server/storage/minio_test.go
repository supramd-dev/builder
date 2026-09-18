package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestMinIO points the production MinIO implementation at the in-process
// fake S3 server.
func newTestMinIO(t *testing.T, cfg Config) (*MinIO, *fakeS3) {
	t.Helper()
	fake := newFakeS3()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)

	cfg.Endpoint = strings.TrimPrefix(srv.URL, "http://")
	if cfg.AccessKey == "" {
		cfg.AccessKey = "test-access-key"
	}
	if cfg.SecretKey == "" {
		cfg.SecretKey = "test-secret-key"
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "artifacts"
	}
	objs, err := NewMinIO(cfg)
	if err != nil {
		t.Fatalf("new minio: %v", err)
	}
	return objs, fake
}

func TestMinIOEnsureBucketCreatesIt(t *testing.T) {
	objs, fake := newTestMinIO(t, Config{AutoCreateBucket: true})
	ctx := context.Background()

	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	if !fake.hasBucket("artifacts") {
		t.Fatal("the bucket should have been created")
	}
	// Idempotent: a second start finds it.
	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if err := objs.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestMinIOEnsureBucketWithoutAutoCreate(t *testing.T) {
	objs, _ := newTestMinIO(t, Config{AutoCreateBucket: false})
	err := objs.EnsureBucket(context.Background())
	if err == nil {
		t.Fatal("expected an error for a missing bucket")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err = %v, want a missing-bucket message", err)
	}
}

// The startup check must fail when nothing is listening, so a misconfigured
// deployment does not start.
func TestMinIOUnreachableEndpoint(t *testing.T) {
	srv := httptest.NewServer(newFakeS3())
	endpoint := strings.TrimPrefix(srv.URL, "http://")
	srv.Close() // nothing listens any more

	objs, err := NewMinIO(Config{Endpoint: endpoint, AccessKey: "ak", SecretKey: "sk", Bucket: "artifacts"})
	if err != nil {
		t.Fatalf("new minio: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := objs.EnsureBucket(ctx); err == nil {
		t.Fatal("expected a connection error")
	}
	if err := objs.Ping(ctx); err == nil {
		t.Fatal("expected ping to fail")
	}
}

func TestMinIOPutGetListDelete(t *testing.T) {
	objs, fake := newTestMinIO(t, Config{AutoCreateBucket: true, Prefix: "artifacts"})
	ctx := context.Background()
	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	key := ArtifactKey(objs.KeyPrefix(), 7, "results", "build/test_detail.xml", 0)
	meta, err := objs.Put(ctx, key, []byte("<testsuites/>"))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if meta.Key != key || meta.Size != int64(len("<testsuites/>")) {
		t.Fatalf("meta = %+v", meta)
	}
	if data, ok := fake.object("artifacts", key); !ok || string(data) != "<testsuites/>" {
		t.Fatalf("fake holds %q (present %t)", data, ok)
	}
	// Over plain HTTP the SDK streams the body with aws-chunked framing;
	// the assertion keeps the fake's decoder from silently rotting.
	if fake.chunkedPuts == 0 {
		t.Fatal("expected the upload to use aws-chunked framing")
	}

	// Get returns the bytes; the aws-chunked request framing must not leak
	// into what is stored.
	data, err := objs.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(data) != "<testsuites/>" {
		t.Fatalf("get = %q", data)
	}

	// Open streams the same bytes and reports their length.
	rc, size, err := objs.Open(ctx, key)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rc.Close()
	if size != int64(len("<testsuites/>")) {
		t.Fatalf("open size = %d", size)
	}
	streamed, err := io.ReadAll(rc)
	if err != nil || string(streamed) != "<testsuites/>" {
		t.Fatalf("stream = %q, %v", streamed, err)
	}

	// A second object, so the prefix filter has something to exclude.
	other, err := objs.Put(ctx, "elsewhere/keep.txt", []byte("keep"))
	if err != nil {
		t.Fatalf("put other: %v", err)
	}

	list, err := objs.List(ctx, RunsPrefix(objs.KeyPrefix()))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Key != key || list[0].Size != int64(len("<testsuites/>")) {
		t.Fatalf("list = %+v", list)
	}
	if list[0].LastModified.IsZero() {
		t.Error("LastModified should come back from the listing")
	}

	if err := objs.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := fake.object("artifacts", key); ok {
		t.Fatal("the object should be gone")
	}
	if _, err := objs.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v, want ErrNotFound", err)
	}
	// Deleting a missing key is not an error (S3 semantics).
	if err := objs.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if _, ok := fake.object("artifacts", other.Key); !ok {
		t.Fatal("the unrelated object should still exist")
	}
}

func TestMinIOEmptyObject(t *testing.T) {
	objs, _ := newTestMinIO(t, Config{AutoCreateBucket: true})
	ctx := context.Background()
	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	// An empty results file is legitimate; it must round-trip as empty
	// rather than as an error.
	if _, err := objs.Put(ctx, "runs/1/results/empty.xml", nil); err != nil {
		t.Fatalf("put: %v", err)
	}
	data, err := objs.Get(ctx, "runs/1/results/empty.xml")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("get = %q, want empty", data)
	}
}

func TestMinIOOpenMissingObject(t *testing.T) {
	objs, _ := newTestMinIO(t, Config{AutoCreateBucket: true})
	ctx := context.Background()
	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	if _, _, err := objs.Open(ctx, "runs/1/results/missing.xml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("open: %v, want ErrNotFound", err)
	}
	if _, err := objs.Get(ctx, "runs/1/results/missing.xml"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v, want ErrNotFound", err)
	}
}

func TestMinIODescribeHidesCredentials(t *testing.T) {
	objs, _ := newTestMinIO(t, Config{UseSSL: true, AccessKey: "AKIASECRET", SecretKey: "hunter2"})
	got := objs.Describe()
	if strings.Contains(got, "AKIASECRET") || strings.Contains(got, "hunter2") {
		t.Fatalf("describe leaks credentials: %q", got)
	}
	if !strings.HasPrefix(got, "minio https://") {
		t.Fatalf("describe = %q", got)
	}
}

// TestMinIOLiveServer runs the same round-trip against a real MinIO, for
// deployments that have one. Set MD_BUILDER_TEST_S3_ENDPOINT (plus
// MD_BUILDER_TEST_S3_ACCESS_KEY / _SECRET_KEY / _BUCKET) to enable it; it is
// skipped otherwise.
func TestMinIOLiveServer(t *testing.T) {
	endpoint := os.Getenv("MD_BUILDER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MD_BUILDER_TEST_S3_ENDPOINT not set; skipping the live MinIO test")
	}
	objs, err := NewMinIO(Config{
		Endpoint:         endpoint,
		AccessKey:        os.Getenv("MD_BUILDER_TEST_S3_ACCESS_KEY"),
		SecretKey:        os.Getenv("MD_BUILDER_TEST_S3_SECRET_KEY"),
		Bucket:           os.Getenv("MD_BUILDER_TEST_S3_BUCKET"),
		Prefix:           "md-builder-test",
		AutoCreateBucket: true,
		ConfigPath:       "(environment)",
	})
	if err != nil {
		t.Fatalf("new minio: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := objs.EnsureBucket(ctx); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	key := ArtifactKey(objs.KeyPrefix(), 1, "results", "live.xml", 0)
	t.Cleanup(func() { _ = objs.Delete(context.Background(), key) })

	if _, err := objs.Put(ctx, key, []byte("<live/>")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if data, err := objs.Get(ctx, key); err != nil || string(data) != "<live/>" {
		t.Fatalf("get = %q, %v", data, err)
	}
}
