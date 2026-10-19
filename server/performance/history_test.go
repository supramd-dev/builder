package performance

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"md-builder/server/store"
)

// fakeLoader is a Loader over fixed observations: the query and the object
// store are tested where they live (server/store), so the module's tests can
// feed it exactly the artifacts they mean to.
type fakeLoader struct {
	obs     []store.PerformanceObservation
	content map[int64]string
	readErr map[int64]error
	cfg     *store.SiteConfig

	reads int // how many artifacts were fetched, for the size-cap test
}

func (f *fakeLoader) ListPerformanceObservations(time.Time, int64, string, int) ([]store.PerformanceObservation, error) {
	return f.obs, nil
}

func (f *fakeLoader) ArtifactContent(_ context.Context, a *store.TestArtifact) ([]byte, error) {
	f.reads++
	if err := f.readErr[a.ID]; err != nil {
		return nil, err
	}
	return []byte(f.content[a.ID]), nil
}

func (f *fakeLoader) GetSiteConfig() (*store.SiteConfig, error) {
	if f.cfg == nil {
		return nil, store.ErrNotFound
	}
	return f.cfg, nil
}

// obs is one observation of a report artifact, identified by artifactID.
func obs(artifactID int64, envID int64, envName string, commitID int64, sha string, at time.Time) store.PerformanceObservation {
	return store.PerformanceObservation{
		Artifact:  store.TestArtifact{ID: artifactID, RunID: artifactID * 10, Name: "perf.json"},
		CommitID:  commitID,
		CommitSHA: sha,
		CommitAt:  at,
		EnvID:     envID,
		EnvName:   envName,
	}
}

// report builds a one-case, one-metric report dated at at.
func report(at time.Time, caseName, metric string, value float64) string {
	number := strconv.FormatFloat(value, 'f', -1, 64)
	return `{"schema":1,"date":"` + at.Format(time.RFC3339) + `","cases":[
		{"name":"` + caseName + `","metrics":{"` + metric + `":{"value":` +
		number + `,"unit":"s"}}}]}`
}

func TestHistoryAggregatesRepeatedMeasurements(t *testing.T) {
	day := func(n int) time.Time {
		return time.Date(2026, 9, n, 12, 0, 0, 0, time.UTC)
	}
	loader := &fakeLoader{
		obs: []store.PerformanceObservation{
			obs(1, 1, "cpu-node-1", 100, "aaa", day(1)),
			obs(2, 1, "cpu-node-1", 100, "aaa", day(1)),
			obs(3, 1, "cpu-node-1", 101, "bbb", day(2)),
			obs(4, 2, "gpu-a100", 101, "bbb", day(2)),
		},
		content: map[int64]string{
			// Two measurements of the same commit: 10 and 20 -> median 15.
			1: report(day(1).Add(5*time.Minute), "nvt", "throughput", 10),
			2: report(day(1).Add(10*time.Minute), "nvt", "throughput", 20),
			3: report(day(2), "nvt", "throughput", 30),
			4: report(day(2), "nvt", "throughput", 40),
		},
	}

	h, err := New(loader).History(context.Background(), Options{Since: day(1).AddDate(0, 0, -1)})
	if err != nil {
		t.Fatalf("history: %v", err)
	}

	// Three points: (cpu, commit 100) folded from two measurements, (cpu,
	// commit 101) and (gpu, commit 101). The fold is a median.
	if len(h.Points) != 3 {
		t.Fatalf("got %d points, want 3: %+v", len(h.Points), h.Points)
	}
	folded := h.Points[0]
	if folded.Environment != "cpu-node-1" || folded.CommitID != 100 {
		t.Fatalf("points[0] = %+v, want the folded cpu point first", folded)
	}
	if folded.Value != 15 || folded.Samples != 2 {
		t.Fatalf("folded point = value %v samples %d, want 15 and 2", folded.Value, folded.Samples)
	}
	// The point sits at the later (median) of its two measurement times.
	if want := time.Date(2026, 9, 1, 12, 10, 0, 0, time.UTC); !folded.At.Equal(want) {
		t.Fatalf("folded At = %v, want %v", folded.At, want)
	}
	if folded.Unit != "s" || folded.Better != BetterHigher || folded.Case != "nvt" || folded.Metric != "throughput" {
		t.Fatalf("folded point lost its labels: %+v", folded)
	}
	if folded.SHA != "aaa" {
		t.Fatalf("folded SHA = %q", folded.SHA)
	}

	// Same case and metric as the folded point, so it comes next; then the
	// other environment.
	if h.Points[1].Environment != "cpu-node-1" || h.Points[1].Value != 30 {
		t.Fatalf("points[1] = %+v", h.Points[1])
	}
	if h.Points[2].Environment != "gpu-a100" || h.Points[2].Value != 40 {
		t.Fatalf("points[2] = %+v", h.Points[2])
	}

	if len(h.Cases) != 1 || h.Cases[0].Name != "nvt" || len(h.Cases[0].Metrics) != 1 {
		t.Fatalf("cases = %+v", h.Cases)
	}
	if m := h.Cases[0].Metrics[0]; m.Name != "throughput" || m.Unit != "s" || m.Better != BetterHigher {
		t.Fatalf("metric info = %+v", m)
	}
	if len(h.Environments) != 2 || h.Environments[0].Name != "cpu-node-1" || h.Environments[1].Name != "gpu-a100" {
		t.Fatalf("environments = %+v", h.Environments)
	}
	if h.Truncated {
		t.Fatal("truncated without reaching the limit")
	}
}

func TestHistorySortsCasesAndMetrics(t *testing.T) {
	loader := &fakeLoader{
		obs: []store.PerformanceObservation{obs(1, 1, "cpu", 1, "a", time.Now())},
		content: map[int64]string{1: `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[
			{"name":"zebra","metrics":{"time":{"value":1,"unit":"s"},"rate":{"value":2,"unit":"1"}}},
			{"name":"alpha","metrics":{"time":{"value":3,"unit":"s"}}}]}`},
	}
	h, err := New(loader).History(context.Background(), Options{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(h.Cases) != 2 || h.Cases[0].Name != "alpha" || h.Cases[1].Name != "zebra" {
		t.Fatalf("cases = %+v", h.Cases)
	}
	metrics := h.Cases[1].Metrics
	if len(metrics) != 2 || metrics[0].Name != "rate" || metrics[1].Name != "time" {
		t.Fatalf("zebra metrics = %+v", metrics)
	}
}

func TestHistoryDistinguishesNotReportsFromBrokenReports(t *testing.T) {
	loader := &fakeLoader{
		obs: []store.PerformanceObservation{
			obs(1, 1, "cpu", 1, "a", time.Now()), // a log: not a report
			obs(2, 1, "cpu", 1, "a", time.Now()), // claims schema 1, has no date
			obs(3, 1, "cpu", 1, "a", time.Now()), // a real report
		},
		content: map[int64]string{
			1: "make: nothing to be done\n",
			2: `{"schema":1,"cases":[{"name":"c","metrics":{"m":{"value":1,"unit":"s"}}}]}`,
			3: `{"schema":1,"date":"2026-09-01T08:00:00Z","cases":[
				{"name":"c","metrics":{"m":{"value":7,"unit":"s"}}}]}`,
		},
	}
	h, err := New(loader).History(context.Background(), Options{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	// The log is dropped in silence; the broken report is listed.
	if len(h.Ignored) != 1 {
		t.Fatalf("ignored = %+v, want only the broken report", h.Ignored)
	}
	ig := h.Ignored[0]
	if ig.ArtifactID != 2 || ig.RunID != 20 || ig.Name != "perf.json" {
		t.Fatalf("ignored entry = %+v", ig)
	}
	if !contains(ig.Reason, "date is required") {
		t.Fatalf("ignored reason = %q", ig.Reason)
	}
	if len(h.Points) != 1 || h.Points[0].Value != 7 {
		t.Fatalf("points = %+v", h.Points)
	}
}

func TestHistorySkipsOversizedArtifactsBeforeReading(t *testing.T) {
	o := obs(1, 1, "cpu", 1, "a", time.Now())
	o.Artifact.Size = maxArtifactBytes + 1
	loader := &fakeLoader{
		obs:     []store.PerformanceObservation{o},
		content: map[int64]string{1: report(time.Now(), "c", "m", 1)},
	}
	h, err := New(loader).History(context.Background(), Options{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if loader.reads != 0 {
		t.Fatalf("the oversized artifact was fetched %d times", loader.reads)
	}
	if len(h.Ignored) != 1 || !contains(h.Ignored[0].Reason, "larger than") {
		t.Fatalf("ignored = %+v", h.Ignored)
	}
}

func TestHistoryReportsUnreadableArtifacts(t *testing.T) {
	loader := &fakeLoader{
		obs:     []store.PerformanceObservation{obs(1, 1, "cpu", 1, "a", time.Now())},
		readErr: map[int64]error{1: errors.New("bucket unreachable")},
	}
	h, err := New(loader).History(context.Background(), Options{})
	if err != nil {
		t.Fatalf("a failing object read must not fail the whole page: %v", err)
	}
	if len(h.Ignored) != 1 || !contains(h.Ignored[0].Reason, "could not be read") {
		t.Fatalf("ignored = %+v", h.Ignored)
	}
	if len(h.Points) != 0 {
		t.Fatalf("points = %+v, want none", h.Points)
	}
}

func TestHistoryReportsTruncationAtTheLimit(t *testing.T) {
	loader := &fakeLoader{content: map[int64]string{}}
	for i := 0; i < maxObservations; i++ {
		loader.obs = append(loader.obs, obs(int64(i+1), 1, "cpu", 1, "a", time.Now()))
	}
	h, err := New(loader).History(context.Background(), Options{})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if !h.Truncated {
		t.Fatal("expected truncated when the artifact limit is reached")
	}
	// Every fetched file was a log, so nothing is plotted — and an empty
	// history still carries empty lists, not nulls, for the JSON.
	if h.Ignored == nil || h.Cases == nil || h.Environments == nil || h.Points == nil {
		t.Fatalf("empty history has nil slices: %+v", h)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
