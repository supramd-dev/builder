// The performance tab: where a benchmark's history is meant to be read.
//
// A `performance:` stage stores each of its artifact files as a series
// artifact (see the runner strategy docs) — a measurement, not a verdict.
// The plan for this page is to collect those series per test environment and
// plot, one chart per machine, how a metric evolves over the commits it was
// measured on. That collection endpoint does not exist yet, so the page
// renders what it can already know — the environments a benchmark could run
// on — with a sample chart standing in for the real one. It is deliberately
// empty of numbers: nothing here is measured.

import { useEffect, useState } from 'react'
import { listEnvironments, type TestEnvironment } from './api'

interface Props {
  onError: (message: string) => void
}

// SamplePlot is the stand-in for the history chart: axes, a metric that
// rises and falls with the commits, and a "sample data" badge so a glance at
// the page cannot mistake the line for a real measurement. Drawn as inline
// SVG rather than with Plotly: the real chart will arrive with the data, and
// until then there is nothing for a plotting library to do.
function SamplePlot({ seed }: { seed: number }) {
  // Fixed points, offset per environment so two cards do not look like one
  // chart copied twice. The shape (a step up, a regression, a recovery) is
  // what a real series of a benchmark looks like.
  const values = [0.52, 0.55, 0.54, 0.71, 0.88, 0.79, 0.57, 0.51]
  const n = values.length
  const jitter = ((seed % 5) - 2) * 0.02
  const points = values.map((v, i) => {
    const x = (i / (n - 1)) * 100
    const y = 90 - Math.min(0.95, Math.max(0.05, v + jitter)) * 80
    return `${x.toFixed(2)},${y.toFixed(2)}`
  })
  return (
    <svg
      className="perf-plot"
      viewBox="0 0 100 100"
      preserveAspectRatio="none"
      role="img"
      aria-label="sample performance history chart, no real data"
    >
      {/* Axes: commits on x, metric (higher is better) on y. */}
      <line x1="0" y1="90" x2="100" y2="90" className="perf-plot-axis" />
      <line x1="0" y1="10" x2="0" y2="90" className="perf-plot-axis" />
      <polyline points={points.join(' ')} className="perf-plot-line" />
      {points.map((p, i) => {
        const [x, y] = p.split(',')
        return <circle key={i} cx={x} cy={y} r="1.6" className="perf-plot-dot" />
      })}
    </svg>
  )
}

// PerformancePage is the tab placeholder. What it shows: one card per test
// environment (the machines a performance stage can run on) with a sample
// chart, and — when the site has no environment yet — what to do first.
export default function PerformancePage({ onError }: Props) {
  const [environments, setEnvironments] = useState<TestEnvironment[] | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    listEnvironments()
      .then((envs) => {
        if (!cancelled) setEnvironments(envs)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load environments'
        setError(msg)
        onError(msg)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  if (error) {
    return (
      <div>
        <h2 className="task-title">Performance</h2>
        <div className="alert alert-danger">{error}</div>
      </div>
    )
  }
  if (!environments) {
    return (
      <div>
        <h2 className="task-title">Performance</h2>
        <p className="text-muted">Loading…</p>
      </div>
    )
  }

  // Disabled environments are machines nothing is dispatched to, so they are
  // left out of the charts: a line that stops has no meaning here.
  const measured = environments.filter((e) => e.enabled)

  return (
    <div>
      <h2 className="task-title">Performance</h2>
      <p className="text-muted" style={{ maxWidth: '70ch' }}>
        One chart per test machine, plotting a benchmark metric over the
        commits it was measured on. The charts below are{' '}
        <strong>placeholders with sample data</strong> — the collection of
        history has not been built yet. A performance stage
        (&ldquo;<code>performance:</code>&rdquo; in md-builder.yaml) already
        stores its artifact files as series data, which is what these charts
        will read. Its <code>window:</code> controls when the benchmark may
        start, so a load-sensitive run can be held to the night.
      </p>

      {measured.length === 0 ? (
        <div className="event text-muted">
          No test environment is enabled yet, so there is no machine to
          measure. Add one under <strong>Runner Envs</strong> first.
        </div>
      ) : (
        <div className="perf-grid">
          {measured.map((env, i) => (
            <section className="perf-card" key={env.id}>
              <header className="perf-card-head">
                <h3 className="perf-card-title">{env.name}</h3>
                <span className="perf-badge">sample data</span>
              </header>
              <p className="text-muted perf-card-sub">
                {env.tags.length > 0 ? env.tags.join(', ') : 'no tags'}
                {' · '}
                {env.host}
              </p>
              <SamplePlot seed={env.id + i} />
              <p className="text-muted perf-card-foot">
                x: commit · y: metric · one point per measured commit
              </p>
            </section>
          ))}
        </div>
      )}
    </div>
  )
}
