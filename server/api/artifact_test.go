package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"md-builder/server/storage"
	"md-builder/server/store"
)

// artifactFixture creates a build run with one artifact and returns the test
// environment, the run and the artifact row.
func artifactFixture(t *testing.T, objs storage.Store) (dashboardTestEnv, *store.TestRun, *store.TestArtifact) {
	t.Helper()
	apiServer, env := newDashboardEnvWithObjects(t, objs)

	var env1 store.TestEnvironment
	if err := apiServer.Store.DB.Where("name = ?", "cpu-node-1").First(&env1).Error; err != nil {
		t.Fatalf("load environment: %v", err)
	}
	commit := &store.Commit{Repo: "group/md-code", SHA: "3333333"}
	if _, err := apiServer.Store.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("get commit: %v", err)
	}
	run, err := apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: env1.ID, CommitID: commit.ID, Kind: store.RunKindBuild,
		Artifacts: []store.ArtifactInput{
			{Kind: store.ArtifactKindResults, Name: "build/test_detail.xml", Content: "<testsuites/>"},
		},
	})
	if err != nil {
		t.Fatalf("upsert run: %v", err)
	}
	artifacts, err := apiServer.Store.ListRunArtifacts(run.ID)
	if err != nil || len(artifacts) != 1 {
		t.Fatalf("artifacts: %+v, %v", artifacts, err)
	}
	return env, run, &artifacts[0]
}

// The content endpoint and the download read the object the row references —
// the bytes are not cached in the database.
func TestArtifactContentServedFromObjectStorage(t *testing.T) {
	objs := storage.NewMemory()
	env, run, a := artifactFixture(t, objs)

	if a.ObjectKey == "" || a.Content != "" {
		t.Fatalf("artifact row should reference the object: %+v", a)
	}
	if objs.Len() != 1 {
		t.Fatalf("expected 1 object, got %d", objs.Len())
	}

	// The run detail reports the object's size without reading it.
	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d", run.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		Artifacts []struct {
			ID   int64 `json:"id"`
			Size int   `json:"size"`
		} `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if len(detail.Artifacts) != 1 || detail.Artifacts[0].Size != len("<testsuites/>") {
		t.Fatalf("artifact refs wrong: %+v", detail.Artifacts)
	}

	// The JSON content endpoint returns what the backend holds.
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d", a.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("content: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		ID      int64  `json:"id"`
		RunID   int64  `json:"runId"`
		Kind    string `json:"kind"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode content: %v", err)
	}
	if body.Content != "<testsuites/>" || body.RunID != run.ID || body.Kind != "results" {
		t.Fatalf("content body wrong: %+v", body)
	}

	// Replace the object behind the row: the next read must see the new
	// bytes, which only holds if the API reads the backend.
	if _, err := objs.Put(context.Background(), a.ObjectKey, []byte("<replaced/>")); err != nil {
		t.Fatalf("replace object: %v", err)
	}
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d", a.ID), "")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode replaced content: %v", err)
	}
	if body.Content != "<replaced/>" {
		t.Fatalf("content should come from the object store: %q", body.Content)
	}
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d/download", a.ID), "")
	if rec.Body.String() != "<replaced/>" {
		t.Fatalf("download should come from the object store: %q", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprint(len("<replaced/>")) {
		t.Errorf("Content-Length = %q", got)
	}

	// The row still exists, so a missing object is a 404 (the artifact was
	// lost), not a 502 (the backend failed).
	if err := objs.Delete(context.Background(), a.ObjectKey); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	for _, path := range []string{
		fmt.Sprintf("/api/test-artifacts/%d", a.ID),
		fmt.Sprintf("/api/test-artifacts/%d/download", a.ID),
	} {
		rec = env.authed(http.MethodGet, path, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: expected 404 for a missing object, got %d", path, rec.Code)
		}
	}

	// A failing backend is a 502: the request was fine, the dependency was
	// not.
	objs.GetErr = errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d", a.ID), "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("content with a broken backend: expected 502, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("the backend error should be logged, not returned: %s", rec.Body.String())
	}
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d/download", a.ID), "")
	if rec.Code != http.StatusBadGateway {
		t.Errorf("download with a broken backend: expected 502, got %d", rec.Code)
	}

}

// A zip download whose backend is down is a 502, not an archive of empty
// files: the request is fine, the dependency is not.
func TestArtifactZipFailsWhenBackendDown(t *testing.T) {
	objs := storage.NewMemory()
	env, run, _ := artifactFixture(t, objs)

	objs.GetErr = errors.New("dial tcp 127.0.0.1:9000: connect: connection refused")
	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d/artifacts/zip", run.ID), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("the backend error should be logged, not returned: %s", rec.Body.String())
	}

	// Same when the object behind the row is gone: the artifact was lost,
	// the backend is fine.
	if err := objs.Delete(context.Background(), mustObjectKey(t, objs)); err != nil {
		t.Fatalf("delete object: %v", err)
	}
	objs.GetErr = nil
	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d/artifacts/zip", run.ID), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for a missing object, got %d: %s", rec.Code, rec.Body.String())
	}
}

// A failure part-way through the stream cannot change the status code any
// more; the archive must be left truncated rather than carrying an entry with
// no bytes behind it, or a valid zip quietly missing a file.
func TestArtifactZipTruncatesOnMidStreamFailure(t *testing.T) {
	objs := storage.NewMemory()
	env, run, a := artifactFixture(t, objs)
	// A second artifact, so there is something to stream after the failure.
	if err := env.server.Store.AppendRunArtifactsByRun(run.ID, []store.ArtifactInput{
		{Kind: store.ArtifactKindFile, Name: "build/notes.txt", Content: "later"},
	}); err != nil {
		t.Fatalf("append artifact: %v", err)
	}

	// The first artifact fails: nothing has been written yet, so the status
	// is still honest.
	objs.GetErrKeys = map[string]error{a.ObjectKey: errors.New("read timeout")}
	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d/artifacts/zip", run.ID), "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d: %s", rec.Code, rec.Body.String())
	}

	// Now fail the *second* artifact: the first is already on the wire.
	artifacts, err := env.server.Store.ListRunArtifacts(run.ID)
	if err != nil || len(artifacts) != 2 {
		t.Fatalf("artifacts: %+v, %v", artifacts, err)
	}
	last := artifacts[len(artifacts)-1]
	if last.ObjectKey == a.ObjectKey {
		t.Fatalf("expected the appended artifact to be last: %+v", artifacts)
	}
	objs.GetErrKeys = map[string]error{last.ObjectKey: errors.New("read timeout")}

	rec = env.authed(http.MethodGet, fmt.Sprintf("/api/test-runs/%d/artifacts/zip", run.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 once the stream started, got %d", rec.Code)
	}
	// The status can no longer change, so the archive is left without its
	// central directory: a truncated download the caller can see, rather
	// than a valid zip that silently lacks a file.
	if _, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len())); err == nil {
		t.Error("a truncated archive should not parse as a complete zip")
	}
}

// mustObjectKey returns the key of the fixture's only artifact.
func mustObjectKey(t *testing.T, objs *storage.Memory) string {
	t.Helper()
	keys := objs.Keys()
	if len(keys) != 1 {
		t.Fatalf("expected 1 object, got %v", keys)
	}
	return keys[0]
}

// Rows written before the object store existed are still served from the
// inline column.
func TestArtifactContentFallsBackToInlineRow(t *testing.T) {
	apiServer, env := newDashboardEnv(t)
	var env1 store.TestEnvironment
	if err := apiServer.Store.DB.Where("name = ?", "cpu-node-1").First(&env1).Error; err != nil {
		t.Fatalf("load environment: %v", err)
	}
	commit := &store.Commit{Repo: "group/md-code", SHA: "4444444"}
	if _, err := apiServer.Store.GetOrCreateCommit(commit); err != nil {
		t.Fatalf("get commit: %v", err)
	}
	run, err := apiServer.Store.UpsertTestRun(&store.RunInput{
		EnvironmentID: env1.ID, CommitID: commit.ID, Kind: store.RunKindUnit,
	})
	if err != nil {
		t.Fatalf("upsert run: %v", err)
	}
	legacy := store.TestArtifact{RunID: run.ID, Kind: store.ArtifactKindResults, Name: "old.xml", Content: "<old/>"}
	if err := apiServer.Store.DB.Create(&legacy).Error; err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	rec := env.authed(http.MethodGet, fmt.Sprintf("/api/test-artifacts/%d", legacy.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("content: %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode legacy content: %v", err)
	}
	if body.Content != "<old/>" {
		t.Fatalf("legacy content missing: %q", body.Content)
	}
}
