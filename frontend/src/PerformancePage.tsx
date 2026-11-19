// The performance tab: how each benchmark case's result evolved over the
// commits it was measured on.
//
// A `performance:` stage stores each of its artifact files as a series
// artifact (see the runner strategy docs), and a report among them says which
// cases and metrics the benchmark measured. The server parses those reports
// and aggregates repeated measurements of a commit into one point
// (server/performance); this page chooses what to draw — the window, the
// machine, the metric — and hands each case's points to Plotly. One chart per
// case: different cases are different experiments, and overlaying them would
// compare values with different units.
//
// The page reads the whole window in one request; the metric and environment
// selectors then filter what is already loaded, so switching them does not
// refetch.

import { useEffect, useState } from 'react'
import { Link } from 'react-router'
import { getPerformanceHistory, type PerformanceHistory } from './api'
import { casesWithMetric, metricOptions, tracesFor } from './performance'
import PerformanceChart from './plot/PerformanceChart'

// windows are the range selector: how far back the history is read. The
// server takes days; 3650 is its maximum and means "everything this site has".
const windows = [
  { label: '30 days', days: 30 },
  { label: '90 days', days: 90 },
  { label: '180 days', days: 180 },
  { label: '1 year', days: 365 },
  { label: 'All', days: 3650 },
]
const defaultWindowDays = 180

interface Props {
  onError: (message: string) => void
}

export default function PerformancePage({ onError }: Props) {
  const [history, setHistory] = useState<PerformanceHistory | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  // The window and the environment are the server's filters (they decide what
  // is read); the metric is filtered here.
  const [days, setDays] = useState(defaultWindowDays)
  const [environmentId, setEnvironmentId] = useState(0)
  const [metricName, setMetricName] = useState('')

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    getPerformanceHistory(days, environmentId === 0 ? undefined : environmentId)
      .then((h) => {
        if (cancelled) return
        setHistory(h)
        setError('')
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load the performance history'
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [days, environmentId, onError])

  const options = history ? metricOptions(history) : []
  // The first option is the default; a chosen metric that the new window no
  // longer has falls back to it rather than rendering an empty page.
  const metric = options.find((o) => o.name === metricName) ?? options[0]
  const cases = history && metric ? casesWithMetric(history, metric.name) : []

  return (
    <div>
      <h2 className="task-title">Performance</h2>
      <p className="text-muted" style={{ maxWidth: '78ch' }}>
        One chart per benchmark case, plotting the metric over the commits it
        was measured on — one marker per commit, one colour per test machine.
        The points come from the JSON reports a{' '}
        <code>performance:</code> stage stores as artifacts; repeated runs of
        one commit are folded into their median.
      </p>

      {!history && loading ? (
        <p className="text-muted">Loading…</p>
      ) : error ? (
        <div className="alert alert-danger">{error}</div>
      ) : history ? (
        <>
          <div className="perf-controls">
            <label>
              Window{' '}
              <select value={days} onChange={(e) => setDays(Number(e.target.value))}>
                {windows.map((w) => (
                  <option key={w.days} value={w.days}>
                    {w.label}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Machine{' '}
              <select value={environmentId} onChange={(e) => setEnvironmentId(Number(e.target.value))}>
                <option value={0}>All machines</option>
                {history.environments.map((env) => (
                  <option key={env.id} value={env.id}>
                    {env.name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              Metric{' '}
              <select
                value={metric?.name ?? ''}
                onChange={(e) => setMetricName(e.target.value)}
                disabled={options.length === 0}
              >
                {options.map((o) => (
                  <option key={o.name} value={o.name}>
                    {o.name}
                    {o.unit !== '' && o.unit !== '1' ? ` (${o.unit})` : ''}
                    {o.better === 'lower' ? ' ↓' : ' ↑'}
                  </option>
                ))}
              </select>
            </label>
            {loading ? <span className="text-muted perf-loading">loading…</span> : null}
          </div>

          {history.truncated ? (
            <div className="alert alert-warning">
              The window holds more measurements than one request reads; the
              latest ones are shown. Narrow the window to see a denser
              history.
            </div>
          ) : null}

          {options.length === 0 ? (
            <div className="event text-muted">
              No benchmark measurements{environmentId === 0 ? '' : ' on this machine'} in this
              window. A <code>performance:</code> stage in md-builder.yaml runs a benchmark; its
              artifacts must include a report file — see{' '}
              <Link to="/docs/test-matrix">Test matrix → Performance artifacts</Link> for the
              format.
            </div>
          ) : metric ? (
            <div className="perf-grid">
              {cases.map((c) => {
                const traces = tracesFor(history.points, c.name, metric.name, environmentId)
                const measured = traces.reduce((n, t) => n + t.points.length, 0)
                return (
                  <section className="perf-card" key={c.name}>
                    <header className="perf-card-head">
                      <h3 className="perf-card-title">{c.name}</h3>
                      <span className="text-muted perf-card-count">
                        {measured} commit{measured === 1 ? '' : 's'}
                      </span>
                    </header>
                    <p className="text-muted perf-card-sub">
                      {traces.length === 1
                        ? traces[0].environment
                        : `${traces.length} machines`}
                    </p>
                    <PerformanceChart
                      metric={metric.name}
                      unit={metric.unit}
                      better={metric.better}
                      traces={traces}
                    />
                  </section>
                )
              })}
            </div>
          ) : null}

          {history.ignored.length > 0 ? (
            <details className="perf-ignored">
              <summary className="text-muted">
                {history.ignored.length} artifact
                {history.ignored.length === 1 ? '' : 's'} could not be read as a report
              </summary>
              <ul className="text-muted">
                {history.ignored.map((ig) => (
                  <li key={ig.artifactId}>
                    <code>{ig.name}</code> — {ig.reason}{' '}
                    <Link to={`/runs/${ig.runId}`}>run #{ig.runId}</Link>
                  </li>
                ))}
              </ul>
            </details>
          ) : null}

          <p className="text-muted perf-foot">
            x: when the benchmark measured (the report&apos;s <code>date</code>) · y: the
            metric · each marker is one commit; a commit measured more than
            once is its median.
          </p>
        </>
      ) : null}
    </div>
  )
}
