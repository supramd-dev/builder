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
import { drawnHeight, heightNote, parsePlotFigure, type PlotFigure } from './figure'

// plotly.js is ~3.5 MB minified, so the <Plot/> component (and everything
// it drags in) is loaded lazily: the split chunk only downloads when a run
// actually has plot artifacts to chart.
const Plot = lazy(async () => {
  const mod = await import('react-plotly.js')
  return { default: mod.default }
})

// What a figure document is, and how tall to draw it, is in figure.ts: read
// there, tested there.
//
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

// PlotFigureView fetches one artifact's content and renders its figure, at
// the height the document asks for (see drawnHeight) and the width of the
// container it is in.
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
