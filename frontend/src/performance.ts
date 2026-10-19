// The performance page's chart model.
//
// The server does the reading and the statistics (server/performance):
// GET /api/performance returns points that are already aggregated per (case,
// metric, environment, commit) — one point per commit, the median of that
// commit's repeated measurements. What is left is presentation: which metrics
// and cases the page offers, how a chart's traces are grouped, how a value and
// its unit read. That lives here rather than in the page so `node --test` can
// import it — node runs .ts, not .tsx — and so the page stays markup.

import type {
  PerformanceCase,
  PerformanceHistory,
  PerformanceMetric,
  PerformancePoint,
} from './api'

export type Better = 'higher' | 'lower'

// MetricOption is one entry of the metric selector: a metric any case
// measured, with the unit and direction its reports declared, and the cases
// that measured it. Several cases sharing a metric name share one entry — a
// selector that listed "throughput" once per case would be unusable.
export interface MetricOption {
  name: string
  unit: string
  better: Better
  cases: string[]
}

// ChartPoint is one measurement as a chart draws it.
export interface ChartPoint {
  // Epoch milliseconds. Plotly takes numbers on a date axis, and the number is
  // what its hover is formatted from.
  x: number
  value: number
  sha: string
  environment: string
  // How many measurements the point folds (retries of the same commit).
  samples: number
}

// Trace is one environment's measurements of one case's metric: one scattered
// series in the chart.
export interface Trace {
  environmentId: number
  environment: string
  points: ChartPoint[]
}

// metricOptions returns the selector's choices: every metric any case
// measured, sorted by name, each listing its cases in the history's order
// (which the server already sorts by name).
export function metricOptions(history: PerformanceHistory): MetricOption[] {
  const byName = new Map<string, MetricOption>()
  for (const c of history.cases) {
    for (const m of c.metrics) {
      const option = byName.get(m.name)
      if (option) {
        if (!option.cases.includes(c.name)) option.cases.push(c.name)
        continue
      }
      byName.set(m.name, { name: m.name, unit: m.unit, better: m.better, cases: [c.name] })
    }
  }
  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name))
}

// casesWithMetric returns the cases that measured metric, in history order.
// The page draws one chart per case — different cases are different
// experiments, and putting two curves on one axis would compare them by
// accident.
export function casesWithMetric(history: PerformanceHistory, metric: string): PerformanceCase[] {
  return history.cases.filter((c) => c.metrics.some((m) => m.name === metric))
}

// metricOf returns what a case's report said about one of its metrics, or
// undefined when the case never measured it.
export function metricOf(
  history: PerformanceHistory,
  caseName: string,
  metric: string,
): PerformanceMetric | undefined {
  const c = history.cases.find((item) => item.name === caseName)
  return c?.metrics.find((m) => m.name === metric)
}

// tracesFor groups one case's metric into traces, one per environment, each
// ordered by measurement time. environmentId 0 (or omitted) keeps every
// environment; any other value keeps only that one — the selector's "all
// machines" and "this machine" cases.
export function tracesFor(
  points: PerformancePoint[],
  caseName: string,
  metric: string,
  environmentId = 0,
): Trace[] {
  const byEnv = new Map<number, Trace>()
  for (const p of points) {
    if (p.case !== caseName || p.metric !== metric) continue
    if (environmentId !== 0 && p.environmentId !== environmentId) continue
    const at = Date.parse(p.at)
    if (!Number.isFinite(at)) continue

    let trace = byEnv.get(p.environmentId)
    if (!trace) {
      trace = { environmentId: p.environmentId, environment: p.environment, points: [] }
      byEnv.set(p.environmentId, trace)
    }
    trace.points.push({
      x: at,
      value: p.value,
      sha: p.sha,
      environment: p.environment,
      samples: p.samples,
    })
  }

  const traces = [...byEnv.values()]
  // A scatter's x order is what a reader follows, so it is set here rather
  // than trusted from the response; the server sorts points for the same
  // reason (case, metric, environment, time) but a chart must not depend on it.
  for (const trace of traces) {
    trace.points.sort((a, b) => a.x - b.x)
  }
  traces.sort((a, b) => a.environment.localeCompare(b.environment))
  return traces
}

// axisTitle labels a chart's y axis: the metric, the unit it is measured in,
// and which direction is the good one — "throughput (step/s) · higher is
// better". The unit is what the report declared; "1" is SI for a plain count
// and is not written out.
export function axisTitle(metric: string, unit: string, better: Better): string {
  const unitLabel = unit === '1' || unit === '' ? metric : `${metric} (${unit})`
  return `${unitLabel} · ${better === 'lower' ? 'lower is better' : 'higher is better'}`
}

// formatValue renders one measurement for a hover or a table cell: grouped
// thousands, and no more decimals than the magnitude is worth.
//   formatValue(1523.4, 'step/s') -> "1,523 step/s"
//   formatValue(12.44, 's')       -> "12.44 s"
//   formatValue(0.877, '%')       -> "0.88%"
export function formatValue(value: number, unit: string): string {
  const text = formatNumber(value)
  if (unit === '' || unit === '1') return text
  if (unit === '%') return `${text}%`
  return `${text} ${unit}`
}

// formatNumber rounds to a few significant decimals: whole numbers for
// anything in the hundreds and up, one for the tens, two below.
function formatNumber(value: number): string {
  const abs = Math.abs(value)
  const digits = abs >= 100 ? 0 : abs >= 10 ? 1 : 2
  return value.toLocaleString('en-US', {
    minimumFractionDigits: 0,
    maximumFractionDigits: digits,
  })
}

// shortSHA is the seven-character commit spelling a hover shows; the full sha
// is long enough to push everything else off the tooltip.
export function shortSHA(sha: string): string {
  return sha.length > 7 ? sha.slice(0, 7) : sha
}
