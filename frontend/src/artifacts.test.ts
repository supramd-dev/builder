// The name predicates the run page classifies artifacts by.
//
// They are a contract with whoever writes a pipeline — docs/test-matrix.md
// promises that `xxxx.plot.json` / `xxxx.plotly.json` become charts and
// `xxxx.html` becomes a previewed page — so the cases below are the promised
// spellings plus the near misses a pattern like this tends to get wrong.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { TestArtifactRef } from './api.ts'
import { isHtmlArtifact, isPlotArtifact } from './artifacts.ts'

// ref builds the smallest artifact reference the predicates read: only the
// name is looked at.
function ref(name: string): TestArtifactRef {
  return { id: 1, kind: 'results', name, size: 0 }
}

test('isPlotArtifact accepts the two documented spellings, case-insensitively', () => {
  for (const name of [
    'results/nvt-compare.plot.json',
    'results/nvt-compare.plotly.json',
    'figure.PLOT.JSON',
    'figure.Plotly.Json',
    '.plot.json',
  ]) {
    assert.equal(isPlotArtifact(ref(name)), true, name)
  }
})

test('isPlotArtifact leaves an ordinary result file alone', () => {
  for (const name of [
    'results/nvt-compare.json', // the naming rule is what makes it a figure
    'results/figure.json',
    'results/plot.json', // no stem, so not the documented spelling
    'results/nvt-compare.plot.json.txt',
    'results/nvt-compare.plotly.json.gz',
    'results/nvt-compare.plot.xml',
    'results/nvt-compare.plotlyjs.json',
    'results/summary.jsonl',
    'results/nvt-compare.html', // a page, not a chart (see isHtmlArtifact)
    'results/nvt-compare',
  ]) {
    assert.equal(isPlotArtifact(ref(name)), false, name)
  }
})

test('isHtmlArtifact accepts .html and .htm, case-insensitively', () => {
  for (const name of [
    'report/report.html',
    'report/index.htm',
    'results/REPORT.HTML',
    'results/Report.Htm',
    '.html',
  ]) {
    assert.equal(isHtmlArtifact(ref(name)), true, name)
  }
})

test('isHtmlArtifact leaves other markup and near misses alone', () => {
  for (const name of [
    'report/report.xhtml', // the dot before the suffix is part of the rule
    'report/report.htmlx',
    'report/report.html.bak',
    'report/report.html5',
    'report/report.htmx',
    'report/summary.txt',
    'results/nvt-compare.plotly.json',
  ]) {
    assert.equal(isHtmlArtifact(ref(name)), false, name)
  }
})

test('no name is served by both predicates', () => {
  const names = [
    'a.plot.json',
    'a.plotly.json',
    'a.html',
    'a.htm',
    'a.json',
    'a.txt',
    'a',
  ]
  for (const name of names) {
    const both = isPlotArtifact(ref(name)) && isHtmlArtifact(ref(name))
    assert.equal(both, false, name)
  }
})
