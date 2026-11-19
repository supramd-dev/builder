package performance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"md-builder/server/store"
)

// Loader is everything this module needs from the host server. *store.Store
// implements it; keeping it an interface (and a narrow one — data in, bytes
// out, no table layout) is what lets the module be read, tested and removed on
// its own.
type Loader interface {
	// ListPerformanceObservations returns the series artifacts of performance
	// runs, oldest first, at most limit of them.
	ListPerformanceObservations(since time.Time, envID int64, repo string, limit int) ([]store.PerformanceObservation, error)
	// ArtifactContent returns one artifact's bytes (object storage, or the
	// legacy inline column).
	ArtifactContent(ctx context.Context, a *store.TestArtifact) ([]byte, error)
	// GetSiteConfig returns the site configuration, whose CodeRepo scopes the
	// history to the repository this site tests.
	GetSiteConfig() (*store.SiteConfig, error)
}

const (
	// maxArtifactBytes caps one series file. A report is numbers: a megabyte is
	// already enormous, and refusing a runaway file before reading it keeps one
	// bad benchmark from being paid for on every page load.
	maxArtifactBytes = 1 << 20
	// maxObservations caps how many artifacts one request reads, newest first.
	maxObservations = 2000
	// readConcurrency is how many artifacts are fetched at once. The reads are
	// object-storage round trips, so they are worth overlapping — but not
	// without bound, or a page load would open a connection per artifact.
	readConcurrency = 8
)

// Options selects the window of history to read.
type Options struct {
	Since         time.Time // commits pushed before this are left out
	EnvironmentID int64     // 0 = every environment
	Repo          string    // "" = every repository
}

// Ignored is one artifact that was read as a report and turned out not to be
// one — malformed, of an unknown schema version, or too large to fetch. The
// page lists these: a benchmark that ran and produced a broken file is a fact
// about the run, and hiding it would look like the run never happened.
type Ignored struct {
	ArtifactID int64
	RunID      int64
	Name       string
	Reason     string
}

// Point is one measurement, ready to be plotted: the aggregated result of one
// case's metric, in one environment, on one commit.
type Point struct {
	Case          string
	Metric        string
	Unit          string
	Better        string
	EnvironmentID int64
	Environment   string
	At            time.Time // median measurement time of the group (see aggregate)
	CommitID      int64
	SHA           string
	Samples       int // how many measurements were aggregated into this point
	Value         float64
}

// MetricInfo names one metric of a case, with what a chart needs to label it.
type MetricInfo struct {
	Name   string
	Unit   string
	Better string
}

// CaseInfo is one benchmark case and the metrics it has measurements for.
type CaseInfo struct {
	Name    string
	Metrics []MetricInfo
}

// EnvInfo is one environment the history has measurements from.
type EnvInfo struct {
	ID   int64
	Name string
}

// History is the aggregated performance history for one request.
type History struct {
	Since time.Time
	// Truncated reports that the artifact limit was reached, so older
	// measurements exist but were not read. The newest ones are the ones kept.
	Truncated    bool
	Cases        []CaseInfo
	Environments []EnvInfo
	Points       []Point
	Ignored      []Ignored
}

// History reads the benchmark reports in the window and aggregates them. A
// file that is not a report is skipped in silence (an artifact of a performance
// run may well be a log); one that claims to be a report and is malformed goes
// into Ignored.
func (s *Service) History(ctx context.Context, opts Options) (*History, error) {
	obs, err := s.Store.ListPerformanceObservations(opts.Since, opts.EnvironmentID, opts.Repo, maxObservations)
	if err != nil {
		return nil, err
	}

	measurements, ignored := s.measurements(ctx, obs)
	points := aggregate(measurements)

	hist := &History{
		Since:     opts.Since,
		Truncated: len(obs) == maxObservations,
		Points:    points,
		Ignored:   ignored,
	}
	if hist.Ignored == nil {
		hist.Ignored = []Ignored{}
	}
	hist.Cases, hist.Environments = summarize(points)
	return hist, nil
}

// measurement is one metric of one report, resolved: the number that goes on
// the chart plus everything the chart labels it with.
type measurement struct {
	caseName string
	metric   string
	unit     string
	better   string
	envID    int64
	envName  string
	commitID int64
	sha      string
	at       time.Time
	value    float64
}

// artifactResult is what reading one artifact produced: the measurements it
// carried, or the reason it could not be read as a report.
type artifactResult struct {
	ms     []measurement
	broken *Ignored
}

// measurements reads every artifact and parses the ones that are reports. The
// artifacts are fetched concurrently but keep their input order, so the result
// is deterministic; the failures come back separately, in the same order.
func (s *Service) measurements(ctx context.Context, obs []store.PerformanceObservation) ([]measurement, []Ignored) {
	slots := make([]artifactResult, len(obs))
	sem := make(chan struct{}, readConcurrency)
	var wg sync.WaitGroup
	for i := range obs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			slots[i] = s.readOne(ctx, obs[i])
		}(i)
	}
	wg.Wait()

	out := make([]measurement, 0, len(obs))
	var ignored []Ignored
	for _, sl := range slots {
		out = append(out, sl.ms...)
		if sl.broken != nil {
			ignored = append(ignored, *sl.broken)
		}
	}
	return out, ignored
}

// readOne fetches and parses a single artifact, turning it into the
// measurements it carries. Failures are of two kinds, and the difference
// matters: a file that does not claim to be a report is dropped silently (a
// log, a figure document), while one that claims to be a report and is not
// readable is returned as an Ignored entry for the page to show.
func (s *Service) readOne(ctx context.Context, o store.PerformanceObservation) artifactResult {
	broken := func(reason string) artifactResult {
		return artifactResult{broken: &Ignored{
			ArtifactID: o.Artifact.ID,
			RunID:      o.Artifact.RunID,
			Name:       o.Artifact.Name,
			Reason:     reason,
		}}
	}

	// Size comes from the row, so an oversized file is refused without ever
	// being fetched.
	if o.Artifact.Size > maxArtifactBytes {
		return broken(fmt.Sprintf("larger than %d MiB", maxArtifactBytes>>20))
	}
	content, err := s.Store.ArtifactContent(ctx, &o.Artifact)
	if err != nil {
		// The artifact row says it is there; a failing read is an operational
		// problem, worth a log line even though the page carries on.
		log.Printf("performance: read artifact %d (%s): %v", o.Artifact.ID, o.Artifact.Name, err)
		return broken("could not be read from object storage")
	}
	if len(content) > maxArtifactBytes {
		// A row with no size (a legacy inline artifact) only reveals its length
		// once read.
		return broken(fmt.Sprintf("larger than %d MiB", maxArtifactBytes>>20))
	}

	report, err := ParseReport(content)
	if err != nil {
		if errors.Is(err, ErrNotReport) {
			return artifactResult{}
		}
		return broken(err.Error())
	}

	var ms []measurement
	for _, c := range report.Cases {
		for _, m := range c.Metrics {
			ms = append(ms, measurement{
				caseName: c.Name,
				metric:   m.Name,
				unit:     m.Unit,
				better:   m.Better,
				envID:    o.EnvID,
				envName:  o.EnvName,
				commitID: o.CommitID,
				sha:      o.CommitSHA,
				at:       report.Date,
				value:    m.Value,
			})
		}
	}
	return artifactResult{ms: ms}
}

// aggregate folds repeated measurements of the same case, metric, environment
// and commit into one point, and orders the result for plotting.
//
// The fold is a median, not a mean: the same commit measured twice (a retry, a
// manual re-run) usually differs because the machine was busy, not because the
// code changed, and a median is what survives one such outlier. The point is
// stamped with the later of the group's measurement times (the median of them
// for an even count), so re-runs never move a point backwards in time.
func aggregate(ms []measurement) []Point {
	type groupKey struct {
		caseName string
		metric   string
		envID    int64
		commitID int64
	}

	order := make([]groupKey, 0, len(ms))
	groups := make(map[groupKey][]measurement, len(ms))
	for _, m := range ms {
		k := groupKey{m.caseName, m.metric, m.envID, m.commitID}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], m)
	}

	points := make([]Point, 0, len(order))
	for _, k := range order {
		g := groups[k]
		values := make([]float64, len(g))
		times := make([]time.Time, len(g))
		for i, m := range g {
			values[i] = m.value
			times[i] = m.at
		}
		sort.Float64s(values)
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })

		// The unit and the direction come from the report; a group whose files
		// disagree keeps the first one read (the oldest artifact), because they
		// are properties of the benchmark, not of one run.
		points = append(points, Point{
			Case:          g[0].caseName,
			Metric:        g[0].metric,
			Unit:          g[0].unit,
			Better:        g[0].better,
			EnvironmentID: g[0].envID,
			Environment:   g[0].envName,
			At:            times[len(times)/2],
			CommitID:      g[0].commitID,
			SHA:           g[0].sha,
			Samples:       len(g),
			Value:         medianSorted(values),
		})
	}

	sort.SliceStable(points, func(i, j int) bool {
		a, b := points[i], points[j]
		if a.Case != b.Case {
			return a.Case < b.Case
		}
		if a.Metric != b.Metric {
			return a.Metric < b.Metric
		}
		if a.Environment != b.Environment {
			return a.Environment < b.Environment
		}
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.CommitID < b.CommitID
	})
	return points
}

// medianSorted returns the median of an ascending slice: the middle value, or
// the mean of the two middle ones when the count is even. The slice must be
// non-empty and sorted.
func medianSorted(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// summarize lists what the points contain: the cases with their metrics, and
// the environments. Both are sorted, so a selector built from them does not
// reshuffle between two loads of the same data.
func summarize(points []Point) ([]CaseInfo, []EnvInfo) {
	type caseKey struct{ name, metric string }
	metrics := make(map[string]map[string]MetricInfo)
	caseOrder := make([]string, 0, 8)
	for _, p := range points {
		if _, seen := metrics[p.Case]; !seen {
			metrics[p.Case] = make(map[string]MetricInfo)
			caseOrder = append(caseOrder, p.Case)
		}
		metrics[p.Case][p.Metric] = MetricInfo{Name: p.Metric, Unit: p.Unit, Better: p.Better}
	}
	sort.Strings(caseOrder)

	cases := make([]CaseInfo, 0, len(caseOrder))
	for _, name := range caseOrder {
		names := make([]string, 0, len(metrics[name]))
		for metricName := range metrics[name] {
			names = append(names, metricName)
		}
		sort.Strings(names)
		info := CaseInfo{Name: name, Metrics: make([]MetricInfo, 0, len(names))}
		for _, metricName := range names {
			info.Metrics = append(info.Metrics, metrics[name][metricName])
		}
		cases = append(cases, info)
	}

	envs := make(map[int64]string)
	for _, p := range points {
		envs[p.EnvironmentID] = p.Environment
	}
	ids := make([]int64, 0, len(envs))
	for id := range envs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if envs[ids[i]] != envs[ids[j]] {
			return envs[ids[i]] < envs[ids[j]]
		}
		return ids[i] < ids[j]
	})
	envList := make([]EnvInfo, 0, len(ids))
	for _, id := range ids {
		envList = append(envList, EnvInfo{ID: id, Name: envs[id]})
	}
	return cases, envList
}
