package storage

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fakeS3 is an in-process S3-compatible server covering the operations this
// package performs: bucket create/exists, object put/get/head/delete and
// ListObjectsV2. It exists so MinIO, the production implementation, is
// regression-tested against the real SDK without a MinIO deployment.
//
// It deliberately ignores authentication: the client still signs every
// request (and streams the body with aws-chunked framing over plain HTTP,
// which the fake decodes), but verifying the signature is MinIO's job, not
// this fake's.
type fakeS3 struct {
	mu      sync.Mutex
	buckets map[string]map[string]fakeObject

	// requests counts the calls the client made, keyed "METHOD /path".
	requests map[string]int
	// chunkedPuts counts uploads that arrived with aws-chunked framing, so
	// a test can prove the decoder below is actually exercised.
	chunkedPuts int
}

// fakeObject is one stored object.
type fakeObject struct {
	data     []byte
	modified time.Time
}

func newFakeS3() *fakeS3 {
	return &fakeS3{
		buckets:  map[string]map[string]fakeObject{},
		requests: map[string]int{},
	}
}

// createBucket seeds an existing bucket.
func (f *fakeS3) createBucket(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.buckets[name]; !ok {
		f.buckets[name] = map[string]fakeObject{}
	}
}

// object returns a stored object's bytes.
func (f *fakeS3) object(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.buckets[bucket][key]
	return obj.data, ok
}

// hasBucket reports whether the bucket exists.
func (f *fakeS3) hasBucket(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.buckets[name]
	return ok
}

// keys lists a bucket's keys, sorted.
func (f *fakeS3) keys(bucket string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.buckets[bucket]))
	for key := range f.buckets[bucket] {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// count returns how many requests matched "METHOD /path".
func (f *fakeS3) count(method, path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[method+" "+path]
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests[r.Method+" "+r.URL.Path]++
	f.mu.Unlock()

	// Paths are /<bucket> or /<bucket>/<key>; object keys never contain a
	// slash in this package's layout, but the split handles either.
	rest := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(rest, "/")
	if bucket == "" {
		f.writeError(w, http.StatusNotFound, "NoSuchBucket", "no bucket in path")
		return
	}

	switch {
	case key == "" && r.Method == http.MethodHead:
		if !f.hasBucket(bucket) {
			f.writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
			return
		}
		w.WriteHeader(http.StatusOK)

	case key == "" && r.Method == http.MethodPut:
		f.createBucket(bucket)
		w.WriteHeader(http.StatusOK)

	case key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2":
		f.listObjects(w, r, bucket)

	case key == "" && r.Method == http.MethodGet:
		// GetBucketLocation — the client asks when it needs to resolve a
		// region.
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)

	case key != "" && r.Method == http.MethodPut:
		f.putObject(w, r, bucket, key)

	case key != "" && r.Method == http.MethodGet:
		f.getObject(w, r, bucket, key, true)

	case key != "" && r.Method == http.MethodHead:
		f.getObject(w, r, bucket, key, false)

	case key != "" && r.Method == http.MethodDelete:
		f.mu.Lock()
		if _, ok := f.buckets[bucket]; !ok {
			f.mu.Unlock()
			f.writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
			return
		}
		delete(f.buckets[bucket], key)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	default:
		f.writeError(w, http.StatusNotImplemented, "NotImplemented", r.Method+" "+r.URL.Path)
	}
}

func (f *fakeS3) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if !f.hasBucket(bucket) {
		f.writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
		return
	}
	data, chunked, err := f.readBody(r)
	if err != nil {
		f.writeError(w, http.StatusBadRequest, "InvalidRequest", err.Error())
		return
	}
	f.mu.Lock()
	f.buckets[bucket][key] = fakeObject{data: data, modified: time.Now().UTC()}
	if chunked {
		f.chunkedPuts++
	}
	f.mu.Unlock()

	sum := md5.Sum(data)
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.WriteHeader(http.StatusOK)
}

func (f *fakeS3) getObject(w http.ResponseWriter, r *http.Request, bucket, key string, body bool) {
	f.mu.Lock()
	obj, ok := f.buckets[bucket][key]
	f.mu.Unlock()
	if !ok {
		f.writeError(w, http.StatusNotFound, "NoSuchKey", "the specified key does not exist")
		return
	}
	sum := md5.Sum(obj.data)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
	w.Header().Set("Last-Modified", obj.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(obj.data)))
	w.WriteHeader(http.StatusOK)
	if body {
		_, _ = w.Write(obj.data)
	}
}

// listObjects answers ListObjectsV2 (the client lists recursively, so no
// delimiter handling is needed).
func (f *fakeS3) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	if !f.hasBucket(bucket) {
		f.writeError(w, http.StatusNotFound, "NoSuchBucket", "bucket does not exist")
		return
	}
	prefix := r.URL.Query().Get("prefix")

	type content struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int64  `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	var result struct {
		XMLName     xml.Name  `xml:"ListBucketResult"`
		Xmlns       string    `xml:"xmlns,attr"`
		Name        string    `xml:"Name"`
		Prefix      string    `xml:"Prefix"`
		KeyCount    int       `xml:"KeyCount"`
		MaxKeys     int       `xml:"MaxKeys"`
		IsTruncated bool      `xml:"IsTruncated"`
		Contents    []content `xml:"Contents"`
	}
	result.Xmlns = "http://s3.amazonaws.com/doc/2006-03-01/"
	result.Name = bucket
	result.Prefix = prefix
	result.MaxKeys = 1000

	f.mu.Lock()
	for _, key := range f.keysLocked(bucket) {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		obj := f.buckets[bucket][key]
		sum := md5.Sum(obj.data)
		result.Contents = append(result.Contents, content{
			Key:          key,
			LastModified: obj.modified.Format(time.RFC3339Nano),
			ETag:         `"` + hex.EncodeToString(sum[:]) + `"`,
			Size:         int64(len(obj.data)),
			StorageClass: "STANDARD",
		})
	}
	f.mu.Unlock()

	result.KeyCount = len(result.Contents)
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	enc := xml.NewEncoder(w)
	if err := enc.Encode(result); err != nil {
		return
	}
}

// keysLocked lists keys with the lock already held.
func (f *fakeS3) keysLocked(bucket string) []string {
	out := make([]string, 0, len(f.buckets[bucket]))
	for key := range f.buckets[bucket] {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func (f *fakeS3) writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, xml.Header+`<Error><Code>%s</Code><Message>%s</Message></Error>`, code, message)
}

// readBody returns the object bytes from a PUT, and whether the request
// carried aws-chunked framing. Over plain HTTP the SDK signs the payload with
// the streaming signature, which wraps the body as
// <hex size>;chunk-signature=<sig>\r\n<data>\r\n.
func (f *fakeS3) readBody(r *http.Request) ([]byte, bool, error) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false, err
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Encoding")), "aws-chunked") {
		return raw, false, nil
	}
	var out []byte
	for {
		i := bytes.Index(raw, []byte("\r\n"))
		if i < 0 {
			return nil, true, fmt.Errorf("malformed chunk header")
		}
		header, rest := string(raw[:i]), raw[i+2:]
		sizeHex, _, _ := strings.Cut(header, ";")
		size, err := strconv.ParseInt(strings.TrimSpace(sizeHex), 16, 64)
		if err != nil {
			return nil, true, fmt.Errorf("chunk size %q: %w", sizeHex, err)
		}
		raw = rest
		if size == 0 {
			return out, true, nil
		}
		if int64(len(raw)) < size {
			return nil, true, fmt.Errorf("truncated chunk: want %d bytes, have %d", size, len(raw))
		}
		out = append(out, raw[:size]...)
		raw = raw[size:]
		if len(raw) >= 2 {
			raw = raw[2:] // the chunk's trailing CRLF
		}
	}
}
