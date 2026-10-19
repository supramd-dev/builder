// One benchmark case's history, drawn with Plotly: a scatter of the commits
// that measured the case's metric, one trace (colour) per test environment.
//
// The points arrive already aggregated — the server folds a commit's repeated
// measurements into one median (see server/performance) — so every marker is
// one commit, and a hover says how many measurements it stands for.
//
// plotly.js is ~3.5 MB minified, so <Plot/> is loaded lazily exactly as
// PlotSection does: the split chunk only downloads when a chart is first
// drawn, and a performance page with no data downloads nothing.

import { lazy, Suspense } from 'react'
import type * as Plotly from 'plotly.js'
import { axisTitle, formatValue, shortSHA, type Better, type Trace } from '../performance'
import { formatTime } from '../timezone'

const Plot = lazy(async () => {
  const mod = await import('react-plotly.js')
  return { default: mod.default }
})

// chartHeight is the drawing height of one card's chart: enough for a dense
// scatter to stay readable, small enough that two cards fit on a screen.
const chartHeight = 260

interface Props {
  metric: string
  unit: string
  better: Better
  traces: Trace[]
}

// PerformanceChart renders one case's scatter. A case with no trace in the
// selected window renders a note instead — and, deliberately, does not load
// the (large) plotter to say so.
export default function PerformanceChart({ metric, unit, better, traces }: Props) {
  if (traces.length === 0) {
    return <p className="text-muted perf-chart-note">No measurements in this window.</p>
  }

  const data: Plotly.Data[] = traces.map((trace) => ({
    type: 'scatter',
    mode: 'markers',
    name: trace.environment,
    x: trace.points.map((p) => new Date(p.x).toISOString()),
    y: trace.points.map((p) => p.value),
    text: trace.points.map((p) => hoverText(p.value, unit, p.samples, p.sha, p.x)),
    // The template only says where each piece goes; the text itself is built
    // above, so a unit containing "%" cannot be read as a template variable.
    hovertemplate: '%{text}<extra>%{fullData.name}</extra>',
    marker: { size: 9 },
  }))

  const layout: Partial<Plotly.Layout> = {
    height: chartHeight,
    autosize: true,
    margin: { t: 24, r: 16, b: 40, l: 72 },
    xaxis: { type: 'date' },
    yaxis: { title: { text: axisTitle(metric, unit, better) } },
    // One line in the legend is noise; the card's title already names the
    // environment when there is only one.
    showlegend: traces.length > 1,
    hovermode: 'closest',
  }

  return (
    <div className="perf-chart">
      <Suspense fallback={<p className="text-muted perf-chart-note">Loading plot renderer…</p>}>
        <Plot
          data={data}
          layout={layout}
          style={{ width: '100%', minWidth: 320 }}
          useResizeHandler
          config={{ displaylogo: false, responsive: true }}
        />
      </Suspense>
    </div>
  )
}

// hoverText is one marker's tooltip: the measurement with its unit, the
// commit it was taken on, when it was taken, and — only when repeated runs
// were folded into the point — how many.
function hoverText(value: number, unit: string, samples: number, sha: string, at: number): string {
  const lines = [
    formatValue(value, unit),
    `${shortSHA(sha)} · ${formatTime(new Date(at).toISOString())}`,
  ]
  if (samples > 1) {
    lines.push(`${samples} measurements (median)`)
  }
  return lines.join('<br>')
}
