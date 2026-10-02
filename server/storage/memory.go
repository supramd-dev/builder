package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process Store used by tests (and by the fake-S3 harness that
// exercises the MinIO code path). It is deliberately not a runtime fallback:
// the server requires a real backend at startup, so nothing in production
// constructs one.
type Memory struct {
	// Prefix is the key namespace reported to callers; a test can set it
	// to exercise the prefixed key layout.
	Prefix string

	// GetErr and PingErr, when set, are returned by reads and by Ping. They
	// let tests drive the failure paths of the artifact API (a backend that
	// is down) and of the health board without a real backend.
	GetErr  error
	PingErr error

	// GetErrKeys fails only the reads of the named keys, so a test can break
	// one artifact of a run while the others stay readable.
	GetErrKeys map[string]error

	mu      sync.Mutex
	objects map[string]ObjectMeta
	data    map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		objects: make(map[string]ObjectMeta),
		data:    make(map[string][]byte),
	}
}

// Put stores a copy of data.
func (m *Memory) Put(_ context.Context, key string, data []byte) (ObjectMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	meta := ObjectMeta{Key: key, Size: int64(len(data)), LastModified: time.Now()}
	m.objects[key] = meta
	m.data[key] = append([]byte(nil), data...)
	return meta, nil
}

// Get returns the stored bytes.
func (m *Memory) Get(_ context.Context, key string) ([]byte, error) {
	if m.GetErr != nil {
		return nil, m.GetErr
	}
	if err := m.GetErrKeys[key]; err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return append([]byte(nil), data...), nil
}

// Open streams the stored bytes.
func (m *Memory) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	data, err := m.Get(context.Background(), key)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

// Delete removes an object; a missing key is not an error.
func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, key)
	delete(m.objects, key)
	return nil
}

// List returns the objects under prefix, ordered by key.
func (m *Memory) List(_ context.Context, prefix string) ([]ObjectMeta, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ObjectMeta
	for key, meta := range m.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Ping reports the injected failure, or success: an in-memory store cannot
// otherwise be unreachable.
func (m *Memory) Ping(context.Context) error { return m.PingErr }

// Describe names the backend for display.
func (m *Memory) Describe() string { return "memory" }

// KeyPrefix returns the configured key namespace.
func (m *Memory) KeyPrefix() string { return strings.Trim(m.Prefix, "/") }

// Keys returns every stored key, ordered — a test helper for assertions.
func (m *Memory) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.objects))
	for key := range m.objects {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// Len returns the number of stored objects.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}

// SetLastModified backdates an object so tests can exercise the sweep's grace
// period. It is a no-op when the key does not exist.
func (m *Memory) SetLastModified(key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if meta, ok := m.objects[key]; ok {
		meta.LastModified = at
		m.objects[key] = meta
	}
}
