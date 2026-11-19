// The performance page's chart model: selector options, trace grouping, and
// how a measurement reads. The statistics are the server's (and its tests
// cover them); what is checked here is what the page does with the response.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  axisTitle,
  casesWithMetric,
  formatValue,
  metricOf,
  metricOptions,
  shortSHA,
  tracesFor,
} from './performance.ts'
import type { PerformanceHistory, PerformancePoint } from './api.ts'

// point builds one aggregated measurement.
function point(overrides: Partial<PerformancePoint> = {}): PerformancePoint {
  return {
    case: 'nvt-1000',
    metric: 'throughput',
    unit: 'step/s',
    better: 'higher',
    environmentId: 1,
    environment: 'cpu-node-1',
    at: '2026-09-01T08:00:00Z',
    commitId: 1,
    sha: 'abcdef0123456789',
    samples: 1,
    value: 100,
    ...overrides,
  }
}

// history builds a response around the given cases and points.
function history(
  cases: PerformanceHistory['cases'],
  points: PerformancePoint[] = [],
  environments = [{ id: 1, name: 'cpu-node-1' }],
): PerformanceHistory {
  return {
    since: '2026-04-19T00:00:00Z',
    generatedAt: '2026-09-01T09:00:00Z',
    truncated: false,
    environments,
    cases,
    points,
    ignored: [],
  }
}

const nvt = {
  name: 'nvt-1000',
  metrics: [
    { name: 'throughput', unit: 'step/s', better: 'higher' as const },
    { name: 'wall_time', unit: 's', better: 'lower' as const },
  ],
}
const poisson = {
  name: 'poisson',
  metrics: [{ name: 'throughput', unit: 'step/s', better: 'higher' as const }],
}

test('metricOptions offers each metric once, with the cases that measured it', () => {
  const options = metricOptions(history([nvt, poisson]))
  assert.deepEqual(
    options.map((o) => o.name),
    ['throughput', 'wall_time'],
  )
  assert.deepEqual(options[0].cases, ['nvt-1000', 'poisson'])
  assert.equal(options[0].unit, 'step/s')
  assert.equal(options[0].better, 'higher')
  assert.deepEqual(options[1].cases, ['nvt-1000'])
  assert.equal(options[1].better, 'lower')
})

test('metricOptions of an empty history is empty', () => {
  assert.deepEqual(metricOptions(history([])), [])
})

test('casesWithMetric keeps the cases that measured the metric', () => {
  assert.deepEqual(
    casesWithMetric(history([nvt, poisson]), 'wall_time').map((c) => c.name),
    ['nvt-1000'],
  )
  assert.deepEqual(casesWithMetric(history([nvt, poisson]), 'nope'), [])
})

test('metricOf finds what a case said about its metric', () => {
  const h = history([nvt, poisson])
  assert.equal(metricOf(h, 'nvt-1000', 'wall_time')?.better, 'lower')
  assert.equal(metricOf(h, 'poisson', 'wall_time'), undefined)
  assert.equal(metricOf(h, 'missing', 'throughput'), undefined)
})

test('tracesFor groups one case and metric into one trace per environment', () => {
  const points = [
    point({ at: '2026-09-03T08:00:00Z', value: 130 }),
    point({ at: '2026-09-01T08:00:00Z', value: 100 }),
    point({ environmentId: 2, environment: 'gpu-a100', at: '2026-09-02T08:00:00Z', value: 900 }),
    // Not part of this chart: another case, and another metric.
    point({ case: 'poisson', value: 7 }),
    point({ metric: 'wall_time', value: 3 }),
  ]

  const traces = tracesFor(points, 'nvt-1000', 'throughput')
  assert.deepEqual(
    traces.map((t) => t.environment),
    ['cpu-node-1', 'gpu-a100'],
  )
  // Points come back in time order whatever order the response had.
  assert.deepEqual(
    traces[0].points.map((p) => p.value),
    [100, 130],
  )
  assert.equal(traces[0].points[0].x, Date.parse('2026-09-01T08:00:00Z'))
  assert.deepEqual(
    traces[1].points.map((p) => p.value),
    [900],
  )
})

test('tracesFor keeps only the selected environment', () => {
  const points = [
    point({ value: 100 }),
    point({ environmentId: 2, environment: 'gpu-a100', value: 900 }),
  ]
  const traces = tracesFor(points, 'nvt-1000', 'throughput', 2)
  assert.equal(traces.length, 1)
  assert.equal(traces[0].environmentId, 2)
  assert.deepEqual(tracesFor(points, 'nvt-1000', 'throughput', 99), [])
})

test('tracesFor carries the hover fields and skips an unreadable date', () => {
  const points = [
    point({ samples: 3, sha: 'abc1234', value: 42 }),
    point({ at: 'not a date', value: 1 }),
  ]
  const [trace] = tracesFor(points, 'nvt-1000', 'throughput')
  assert.equal(trace.points.length, 1)
  assert.equal(trace.points[0].samples, 3)
  assert.equal(trace.points[0].sha, 'abc1234')
  assert.equal(trace.points[0].environment, 'cpu-node-1')
})

test('axisTitle names the metric, its unit and the good direction', () => {
  assert.equal(axisTitle('throughput', 'step/s', 'higher'), 'throughput (step/s) · higher is better')
  assert.equal(axisTitle('wall_time', 's', 'lower'), 'wall_time (s) · lower is better')
  // "1" is SI for a plain count and is not written out.
  assert.equal(axisTitle('iterations', '1', 'higher'), 'iterations · higher is better')
})

test('formatValue groups thousands and keeps the unit', () => {
  assert.equal(formatValue(1523.4, 'step/s'), '1,523 step/s')
  assert.equal(formatValue(12.44, 's'), '12.4 s')
  assert.equal(formatValue(8.03, 's'), '8.03 s')
  assert.equal(formatValue(0.877, '%'), '0.88%')
  assert.equal(formatValue(88, '1'), '88')
  assert.equal(formatValue(88, ''), '88')
  // Negative values format like positive ones (a delta, say).
  assert.equal(formatValue(-12.5, 's'), '-12.5 s')
})

test('shortSHA keeps the seven-character spelling', () => {
  assert.equal(shortSHA('abcdef0123456789'), 'abcdef0')
  assert.equal(shortSHA('abc'), 'abc')
  assert.equal(shortSHA(''), '')
})
