// Reading a figure document, and sizing it.
//
// The height rules are the ones docs/test-matrix.md states: the document's
// own height is drawn, bounded to 120–2000 px, 480 px when it names none.
// The shapes here are the ones real exporters write — Plotly's own
// `write_html`/`write_json` sets `layout.height`, and hand-rolled exporters
// put a bare `height` at the root or quote the number as a string.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { drawnHeight, heightNote, parsePlotFigure } from './figure.ts'

// figure builds a minimal but valid figure document around one layout.
function figure(layout: unknown = {}): string {
  return JSON.stringify({ data: [{ type: 'scatter', y: [1, 2] }], layout })
}

// messageOf runs a parse that is expected to fail and returns the message;
// '(no error)' if it did not.
function messageOf(content: string, name: string): string {
  try {
    parsePlotFigure(content, name)
  } catch (err) {
    return (err as Error).message
  }
  return '(no error)'
}

test('drawnHeight draws the height the document asked for', () => {
  assert.equal(drawnHeight(960), 960)
  assert.equal(drawnHeight(120), 120)
  assert.equal(drawnHeight(2000), 2000)
  // A fractional height is rounded: it is a pixel count.
  assert.equal(drawnHeight(960.4), 960)
  assert.equal(drawnHeight(960.6), 961)
})

test('drawnHeight bounds a hairline and a runaway value', () => {
  assert.equal(drawnHeight(119), 120)
  assert.equal(drawnHeight(40), 120)
  assert.equal(drawnHeight(1), 120)
  assert.equal(drawnHeight(2001), 2000)
  assert.equal(drawnHeight(100000), 2000)
})

test('drawnHeight falls back when the document names no height', () => {
  assert.equal(drawnHeight(0), 480)
  assert.equal(drawnHeight(-5), 480)
  assert.equal(drawnHeight(Number.NaN), 480)
})

test('heightNote says where a drawn height came from', () => {
  assert.equal(heightNote(0, 480), '(default)')
  assert.equal(heightNote(Number.NaN, drawnHeight(Number.NaN)), '(default)')
  assert.equal(heightNote(960, 960), '(from the file)')
  assert.equal(heightNote(119, 120), '(capped from 119)')
  assert.equal(heightNote(4200, 2000), '(capped from 4200)')
  // Rounded but in range: the file's own number, not a capped one.
  assert.equal(heightNote(960.4, 960), '(from the file)')
})

test('parsePlotFigure keeps the traces and the layout', () => {
  const fig = parsePlotFigure(
    JSON.stringify({
      data: [{ type: 'scatter', y: [1, 2] }],
      layout: { title: { text: 'energy drift' }, height: 960 },
    }),
    'results/nvt-compare.plotly.json',
  )
  assert.equal(fig.data.length, 1)
  assert.deepEqual(fig.layout?.title, { text: 'energy drift' })
  assert.equal(fig.layout?.height, 960)
})

test('parsePlotFigure reads a height from the layout or from the root', () => {
  // layout.height is the Plotly spelling, as a number or quoted.
  assert.equal(parsePlotFigure(figure({ height: 960 }), 'f.plot.json').layout?.height, 960)
  assert.equal(parsePlotFigure(figure({ height: '640' }), 'f.plot.json').layout?.height, 640)
  // A bare root-level height is what a hand-rolled exporter writes.
  const root = (height: string) =>
    parsePlotFigure(`{"data":[{"y":[1]}],"height":${height}}`, 'f.plot.json').layout?.height
  assert.equal(root('800'), 800)
  assert.equal(root('"800"'), 800)
  // The layout's own height wins when both are there.
  assert.equal(
    parsePlotFigure(
      '{"data":[{"y":[1]}],"layout":{"height":300},"height":800}',
      'f.plot.json',
    ).layout?.height,
    300,
  )
})

// declaredOf is how PlotFigureView reads a parsed figure's height.
function declaredOf(doc: string): number {
  const fig = parsePlotFigure(doc, 'f.plot.json')
  return typeof fig.layout?.height === 'number' ? fig.layout.height : 0
}

test('a document that names no usable height draws at the default', () => {
  for (const doc of [
    figure({}),
    figure({ height: 0 }),
    figure({ height: -1 }),
    figure({ height: 'tall' }),
    figure({ height: null }),
    '{"data":[{"y":[1]}],"height":0}',
    '{"data":[{"y":[1]}],"height":"tall"}',
  ]) {
    assert.equal(drawnHeight(declaredOf(doc)), 480, doc)
  }
})

test('parsePlotFigure does not invent a height for a document that has none', () => {
  // A root-level height is promoted into the layout; a junk one is dropped
  // rather than written out as a non-number Plotly would be handed.
  for (const doc of ['{"data":[{"y":[1]}]}', '{"data":[{"y":[1]}],"height":0}', '{"data":[{"y":[1]}],"height":"tall"}']) {
    const fig = parsePlotFigure(doc, 'f.plot.json')
    assert.equal('height' in (fig.layout ?? {}), false, doc)
  }
})

test('parsePlotFigure drops a layout that is not an object', () => {
  // Spreading a string would hand Plotly its characters as layout keys.
  for (const layout of ['tall', ['height'], 42, true, null]) {
    const fig = parsePlotFigure(figure(layout), 'f.plot.json')
    assert.deepEqual(fig.layout, {}, JSON.stringify(layout))
  }
})

test('parsePlotFigure rejects a document that is not a figure', () => {
  const bad: Array<[string, string]> = [
    ['invalid JSON', ''],
    ['invalid JSON', '{"data":'],
    ['not a plot document', '[]'],
    ['not a plot document', '"scatter"'],
    ['not a plot document', '42'],
    ['not a plot document', 'null'],
    ['not a plot document', '{}'],
    ['not a plot document', '{"data":[]}'],
    ['not a plot document', '{"data":"scatter"}'],
  ]
  for (const [prefix, content] of bad) {
    // The view tells a parse error from a fetch error by these first words.
    assert.throws(
      () => parsePlotFigure(content, 'results/summary.json'),
      (err: Error) => err.message.startsWith(prefix),
      content,
    )
  }
})

test('parsePlotFigure names the file it could not read', () => {
  // The message is shown under the file's name, so it has to say which one —
  // and a nameless artifact has to read as something.
  assert.match(messageOf('{"data":[]}', 'results/summary.json'), /results\/summary\.json/)
  assert.match(messageOf('{}', ''), /file/)
})
