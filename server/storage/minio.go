package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// MinIO is the S3-compatible Store used in deployments. Only this file
// imports the MinIO SDK, so the rest of the server stays testable against
// Memory.
type MinIO struct {
	client *minio.Client
	cfg    Config
}

// NewMinIO builds a client from cfg. It does not talk to the server; call
// EnsureBucket (or Ping) to check connectivity.
func NewMinIO(cfg Config) (*MinIO, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	}
	if cfg.Region != "" {
		opts.Region = cfg.Region
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("object storage %s: %w", cfg.Describe(), err)
	}
	return &MinIO{client: client, cfg: cfg}, nil
}

// EnsureBucket verifies that the configured bucket exists, creating it when
// AutoCreateBucket is set. It is the startup connectivity check: the server
// refuses to start when object storage is unreachable, so a misconfigured
// deployment fails immediately instead of on the first artifact write.
func (m *MinIO) EnsureBucket(ctx context.Context) error {
	exists, err := m.client.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("object storage %s: %w", m.cfg.Describe(), mapError(err))
	}
	if exists {
		return nil
	}
	if !m.cfg.AutoCreateBucket {
		return fmt.Errorf("object storage %s: bucket %q does not exist (set objectStorage.autoCreateBucket to create it)",
			m.cfg.Describe(), m.cfg.Bucket)
	}
	opts := minio.MakeBucketOptions{}
	if m.cfg.Region != "" {
		opts.Region = m.cfg.Region
	}
	if err := m.client.MakeBucket(ctx, m.cfg.Bucket, opts); err != nil {
		// A concurrent start (or a bucket created out of band between
		// the check and the call) is not a failure.
		if resp := minio.ToErrorResponse(err); resp.StatusCode == http.StatusConflict ||
			strings.Contains(resp.Code, "BucketAlreadyOwnedByYou") {
			return nil
		}
		return fmt.Errorf("object storage %s: create bucket %q: %w", m.cfg.Describe(), m.cfg.Bucket, mapError(err))
	}
	return nil
}

// Put stores data under key, overwriting any previous object.
func (m *MinIO) Put(ctx context.Context, key string, data []byte) (ObjectMeta, error) {
	info, err := m.client.PutObject(ctx, m.cfg.Bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	if err != nil {
		return ObjectMeta{}, fmt.Errorf("object storage %s: put %s: %w", m.cfg.Describe(), key, mapError(err))
	}
	size := info.Size
	if size == 0 {
		// UploadInfo.Size is only set by multipart uploads; a single
		// PUT (every artifact) knows the size from the request.
		size = int64(len(data))
	}
	return ObjectMeta{Key: key, Size: size, LastModified: time.Now()}, nil
}

// Get returns the whole object.
func (m *MinIO) Get(ctx context.Context, key string) ([]byte, error) {
	rc, _, err := m.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("object storage %s: read %s: %w", m.cfg.Describe(), key, mapError(err))
	}
	return data, nil
}

// Open streams the object. The size comes from a HEAD, so a missing key is
// reported here rather than on the first read.
func (m *MinIO) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := m.client.GetObject(ctx, m.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, fmt.Errorf("object storage %s: get %s: %w", m.cfg.Describe(), key, mapError(err))
	}
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, 0, fmt.Errorf("object storage %s: stat %s: %w", m.cfg.Describe(), key, mapError(err))
	}
	return obj, info.Size, nil
}

// Delete removes the object. S3 treats deleting a missing key as a success, so
// this is idempotent.
func (m *MinIO) Delete(ctx context.Context, key string) error {
	if err := m.client.RemoveObject(ctx, m.cfg.Bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("object storage %s: delete %s: %w", m.cfg.Describe(), key, mapError(err))
	}
	return nil
}

// List returns the objects under prefix, recursively.
func (m *MinIO) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	var out []ObjectMeta
	for info := range m.client.ListObjects(ctx, m.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if info.Err != nil {
			return nil, fmt.Errorf("object storage %s: list %s: %w", m.cfg.Describe(), prefix, mapError(info.Err))
		}
		out = append(out, ObjectMeta{Key: info.Key, Size: info.Size, LastModified: info.LastModified})
	}
	return out, nil
}

// Ping checks that the backend answers and the bucket is present.
func (m *MinIO) Ping(ctx context.Context) error {
	exists, err := m.client.BucketExists(ctx, m.cfg.Bucket)
	if err != nil {
		return fmt.Errorf("object storage %s: %w", m.cfg.Describe(), mapError(err))
	}
	if !exists {
		return fmt.Errorf("object storage %s: bucket %q does not exist", m.cfg.Describe(), m.cfg.Bucket)
	}
	return nil
}

// Describe names the backend for display (never credentials).
func (m *MinIO) Describe() string { return m.cfg.Describe() }

// KeyPrefix returns the configured key namespace.
func (m *MinIO) KeyPrefix() string { return m.cfg.KeyPrefix() }

// Bucket returns the configured bucket name.
func (m *MinIO) Bucket() string { return m.cfg.Bucket }

// mapError turns a missing-object response into ErrNotFound so callers can
// answer 404 instead of reporting a backend failure. Everything else is
// wrapped as-is: the message carries the endpoint but never a credential.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return err
	}
	resp := minio.ToErrorResponse(err)
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w (%s)", ErrNotFound, firstNonEmpty(resp.Code, "HTTP 404"))
	}
	return err
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
