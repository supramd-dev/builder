// A figure document, and the size to draw it at.
//
// These are the parts of the Plots section that are not rendering: reading a
// `*.plot.json` / `*.plotly.json` artifact as a Plotly figure, and choosing
// its height. They live here rather than in PlotSection.tsx so the tests
// (figure.test.ts, run by `node --test`) can import them the way node can —
// node strips types out of .ts, but knows nothing about .tsx.
//
// The one import is a type, so this module pulls in nothing at runtime.

import type * as Plotly from 'plotly.js'

// PlotFigure is the subset of a Plotly figure the component forwards: the
// traces, and the layout it draws them in.
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
  const inLayout = isRecord(raw.layout) ? positiveNumber(raw.layout.height) : 0
  return inLayout || positiveNumber(raw.height)
}

// isRecord reports whether a JSON value is a plain object — the only shape
// whose properties can be read or spread.
function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v)
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
// height its document declares (0 when it names none). The test is `> 0`
// rather than `<= 0` so a NaN — a height that is not a number at all — draws
// at the default instead of propagating.
export function drawnHeight(declared: number): number {
  if (!(declared > 0)) return defaultFigureHeight
  return Math.min(Math.max(Math.round(declared), minFigureHeight), maxFigureHeight)
}

// heightNote says where a drawn height came from, for the label above the
// chart: the file's own number, the default, or the file's number after the
// bounds were applied. It reads `declared` the way drawnHeight does, so the
// two agree on which heights are "no height".
export function heightNote(declared: number, drawn: number): string {
  if (!(declared > 0)) return '(default)'
  if (drawn !== Math.round(declared)) return `(capped from ${Math.round(declared)})`
  return '(from the file)'
}

// parsePlotFigure validates a fetched artifact's content as a Plotly
// figure: an object with a non-empty `data` array. Throws a readable error
// otherwise — the message is shown under the file name, and its first words
// are what PlotFigureView tells a parse error from a fetch error by.
//
// The height is normalized into `layout.height` here (see figureHeight) so
// the render path has one place to read it from, and a document that spells
// it outside the layout still gets the size it asked for. A `layout` that is
// not an object is dropped rather than spread: spreading a string would hand
// Plotly its characters as layout keys.
export function parsePlotFigure(content: string, name: string): PlotFigure {
  let raw: unknown
  try {
    raw = JSON.parse(content)
  } catch (err: unknown) {
    throw new Error(`invalid JSON: ${err instanceof Error ? err.message : String(err)}`)
  }
  if (!isRecord(raw)) {
    throw new Error('not a plot document (expected a JSON object)')
  }
  if (!Array.isArray(raw.data) || raw.data.length === 0) {
    throw new Error(`not a plot document (${name || 'file'} has no "data" traces)`)
  }
  const layout = isRecord(raw.layout) ? ({ ...raw.layout } as Partial<Plotly.Layout>) : {}
  const height = figureHeight(raw)
  if (height > 0) layout.height = height
  return { data: raw.data as Plotly.Data[], layout }
}
