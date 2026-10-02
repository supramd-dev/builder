// Plotly plotting for run artifacts.
//
// A run's artifact list may carry one or more `*.plot.json` /
// `*.plotly.json` files — verbatim Plotly figure documents: a `data` array
// of traces plus an optional `layout` object (exactly what `<Plot/>`
// expects, minus the DOM-props). This component fetches each matching
// artifact, validates the JSON shape, and renders one chart per file with
// Plotly's responsive sizing. Malformed files render their parse error
// inline instead of breaking the page.

import { lazy, Suspense, useEffect, useState } from 'react'
import type * as Plotly from 'plotly.js'
import { getTestArtifact, type TestArtifactRef } from '../api'
import { isPlotArtifact } from '../artifacts'

// plotly.js is ~3.5 MB minified, so the <Plot/> component (and everything
// it drags in) is loaded lazily: the split chunk only downloads when a run
// actually has plot artifacts to chart.
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

// Figure height in pixels. A document's own height is what gets drawn — the
// author picked it — inside these bounds, which only keep a hairline or a
// runaway value from being taken literally. A document that names no height
// gets defaultFigureHeight: taller than Plotly's own 450, because on a run
// page a chart spans a thousand pixels and 360 read as squashed.
const minFigureHeight = 120
const maxFigureHeight = 2000
const defaultFigureHeight = 480

// figureHeight reads the height a figure document declares. `layout.height`
// is the Plotly spelling; a bare root-level `height` is what a hand-rolled
// exporter tends to write, so it is a fallback. Both are accepted as a number
// or as a numeric string, since JSON written by hand quotes them often
// enough. 0 means the document does not say.
function figureHeight(raw: { layout?: unknown; height?: unknown }): number {
  const layout = (raw.layout ?? {}) as { height?: unknown }
  return positiveNumber(layout.height) || positiveNumber(raw.height)
}

// positiveNumber reads a JSON scalar as a positive number, or 0.
function positiveNumber(v: unknown): number {
  if (typeof v === 'number' && Number.isFinite(v) && v > 0) return v
  if (typeof v === 'string') {
    const n = Number(v)
    if (Number.isFinite(n) && n > 0) return n
  }
  return 0
}

// drawnHeight resolves the pixel height to render a figure at, given the
// height its document declares (0 when it names none).
export function drawnHeight(declared: number): number {
  if (declared <= 0) return defaultFigureHeight
  return Math.min(Math.max(Math.round(declared), minFigureHeight), maxFigureHeight)
}

// heightNote says where a drawn height came from, for the label above the
// chart: the file's own number, the default, or the file's number after the
// bounds were applied.
function heightNote(declared: number, drawn: number): string {
  if (declared <= 0) return '(default)'
  if (drawn !== Math.round(declared)) return `(capped from ${Math.round(declared)})`
  return '(from the file)'
}

// parsePlotFigure validates a fetched artifact's content as a Plotly
// figure: an object with a non-empty `data` array. Throws a readable error
// otherwise — the message is shown under the file name.
//
// The height is normalized into `layout.height` here (see figureHeight) so
// the render path has one place to read it from, and a document that spells
// it outside the layout still gets the size it asked for.
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
  const fig = raw as { data?: unknown; layout?: unknown; height?: unknown }
  if (!Array.isArray(fig.data) || fig.data.length === 0) {
    throw new Error(`not a plot document (${name || 'file'} has no "data" traces)`)
  }
  const layout = { ...((fig.layout ?? {}) as Partial<Plotly.Layout>) }
  const height = figureHeight(fig)
  if (height > 0) layout.height = height
  return { data: fig.data as Plotly.Data[], layout }
}

// PlotSection renders every plot artifact of a run: one fetch per file,
// one <Plot/> per file. Runs without plot artifacts render nothing.
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
          <code>*.plot.json</code> / <code>*.plotly.json</code> artifacts)
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

  // The file's own layout wins. The width is the exception: a document that
  // carries one would overflow the page until the next resize, so it is
  // dropped and `autosize` lets Plotly take the container's — the height,
  // which is the author's choice, is the one dimension passed through.
  const layout: Partial<Plotly.Layout> = {
    margin: { t: 40, r: 20, b: 40, l: 50 },
    ...figure.layout,
    autosize: true,
  }
  const declared = typeof figure.layout?.height === 'number' ? figure.layout.height : 0
  const height = drawnHeight(declared)
  delete layout.width
  const style: React.CSSProperties = { width: '100%', minWidth: 320 }

  return (
    <div className="plot-figure">
      <p className="text-muted" style={{ marginBottom: '0.25rem' }}>
        <code>{ref2.name}</code>{' '}
        {/* Where the height came from, so "the json says nothing" is
            visible on the page rather than guessed at. */}
        <span>— {height} px {heightNote(declared, height)}</span>
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
