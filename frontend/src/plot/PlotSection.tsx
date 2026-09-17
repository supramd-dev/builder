// Plotly plotting for regression case artifacts.
//
// A case's artifact list may carry one or more `*.plot.json` files —
// verbatim Plotly figure documents: a `data` array of traces plus an
// optional `layout` object (exactly what `<Plot/>` expects, minus the
// DOM-props). This component fetches each matching artifact, validates the
// JSON shape, and renders one chart per file with Plotly's responsive
// sizing. Malformed files render their parse error inline instead of
// breaking the page.

import { lazy, Suspense, useEffect, useState } from 'react'
import type * as Plotly from 'plotly.js'
import { getTestArtifact, type TestArtifactRef } from '../api'

// plotly.js is ~3.5 MB minified, so the <Plot/> component (and everything
// it drags in) is loaded lazily: the split chunk only downloads when a run
// actually has *.plot.json artifacts to chart.
const Plot = lazy(async () => {
  const mod = await import('react-plotly.js')
  return { default: mod.default }
})

// PlotFigure is the subset of a Plotly figure the component forwards: the
// trace array and the layout. Anything else in the file is ignored.
export interface PlotFigure {
  data: Plotly.Data[]
  layout?: Partial<Plotly.Layout>
}

// isPlotArtifact reports whether an artifact reference looks like a plot
// document by its stored name (`xxxx.plot.json`).
export function isPlotArtifact(a: TestArtifactRef): boolean {
  return /\.plot\.json$/i.test(a.name)
}

// parsePlotFigure validates a fetched artifact's content as a Plotly
// figure: an object with a non-empty `data` array. Throws a readable error
// otherwise — the message is shown under the file name.
export function parsePlotFigure(content: string, name: string): PlotFigure {
  let raw: unknown
  try {
    raw = JSON.parse(content)
  } catch (err: unknown) {
    throw new Error(`invalid JSON: ${err instanceof Error ? err.message : String(err)}`)
  }
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error('not a plot document (expected a JSON object)')
  }
  const fig = raw as { data?: unknown; layout?: unknown }
  if (!Array.isArray(fig.data) || fig.data.length === 0) {
    throw new Error(`not a plot document (${name || 'file'} has no "data" traces)`)
  }
  return { data: fig.data as Plotly.Data[], layout: (fig.layout ?? {}) as Partial<Plotly.Layout> }
}

// PlotSection renders every `*.plot.json` artifact of a run: one fetch per
// file, one <Plot/> per file. Runs without plot artifacts render nothing.
export default function PlotSection({
  artifacts,
  onError,
}: {
  artifacts: TestArtifactRef[]
  onError: (message: string) => void
}) {
  const plots = artifacts.filter(isPlotArtifact)
  if (plots.length === 0) return null

  return (
    <section>
      <h3 className="task-section-title">
        Plots{' '}
        <span className="text-muted" style={{ fontWeight: 400 }}>
          ({plots.length} figure{plots.length === 1 ? '' : 's'} from{' '}
          <code>*.plot.json</code> artifacts)
        </span>
      </h3>
      {plots.map((ref) => (
        <PlotFigureView key={ref.id} ref2={ref} onError={onError} />
      ))}
    </section>
  )
}

// PlotFigureView fetches one artifact's content and renders its figure.
// The chart height honors the file's layout.height (capped) and otherwise
// defaults to 360px; width is responsive (the layout.width, when the file
// sets one, wins).
function PlotFigureView({
  ref2,
  onError,
}: {
  ref2: TestArtifactRef
  onError: (message: string) => void
}) {
  const [figure, setFigure] = useState<PlotFigure | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(true)

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    getTestArtifact(ref2.id)
      .then((a) => {
        if (cancelled) return
        setFigure(parsePlotFigure(a.content, a.name))
        setError('')
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load plot file'
        setError(msg)
        // Fetch/transport errors surface once; parse errors stay inline
        // under the file (the artifact still downloads from the table
        // above).
        if (!msg.startsWith('invalid JSON') && !msg.startsWith('not a plot document')) {
          onError(msg)
        }
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [ref2.id, onError])

  if (loading) {
    return <p className="text-muted">Loading {ref2.name}…</p>
  }
  if (error) {
    return (
      <div className="plot-figure">
        <p className="text-muted" style={{ marginBottom: '0.25rem' }}>
          <code>{ref2.name}</code>
        </p>
        <div className="alert alert-danger">{error}</div>
      </div>
    )
  }
  if (!figure) return null

  // The file's own layout wins; responsive height/width defaults keep the
  // chart readable when the document omits them.
  const layout: Partial<Plotly.Layout> = {
    margin: { t: 40, r: 20, b: 40, l: 50 },
    ...figure.layout,
  }
  const fileHeight = typeof figure.layout?.height === 'number' ? figure.layout.height : 0
  const height = fileHeight > 0 && fileHeight <= 1200 ? fileHeight : 360
  const style: React.CSSProperties = { width: '100%', minWidth: 320 }

  return (
    <div className="plot-figure">
      <p className="text-muted" style={{ marginBottom: '0.25rem' }}>
        <code>{ref2.name}</code>
      </p>
      <Suspense fallback={<p className="text-muted">Loading plot renderer…</p>}>
        <Plot
          data={figure.data}
          layout={{ ...layout, height }}
          style={style}
          useResizeHandler
          config={{ displaylogo: false, responsive: true }}
        />
      </Suspense>
    </div>
  )
}
