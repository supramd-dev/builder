// The status presentation table (status.ts).
//
// Every page that shows a task's or a run's status reads this one table —
// the dashboard's stage cells, the task graph, the run page — so these cases
// are what keeps the same status from reading three ways. The `cancelled`
// status is the repeated-commit policy dropping a stage, and it must not read
// as a failure of the code.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { statusGlyph, statusView } from './status.ts'

// The statuses the server can report (store.TaskStatus plus the two
// in-flight ones). A status missing here would fall to the unknown case and
// render as a failure, which is the bug this list exists to catch.
const STATUSES = [
  'passed',
  'failed',
  'timeout',
  'skipped',
  'cancelled',
  'running',
  'pending',
]

test('every status has its own word, mark and class', () => {
  const seen = new Set<string>()
  for (const status of STATUSES) {
    const view = statusView(status)
    assert.ok(view.word, `${status}: no word`)
    assert.match(view.cls, /^dash-st-/, status)
    assert.match(view.textCls, /^text-/, status)
    assert.match(view.text, /\S/, status)
    assert.ok(!seen.has(view.cls), `${status} shares the class ${view.cls}`)
    seen.add(view.cls)
  }
  assert.equal(seen.size, STATUSES.length)
})

test('a status is spelled the same in a cell and in a page header', () => {
  // The dashboard cell shows "⊘ cancel" where the run page shows "⊘ cancelled":
  // the same mark, one short word and one long. A reader maps one to the other
  // by the mark and the color, so the mark has to match.
  for (const status of STATUSES) {
    const view = statusView(status)
    if (view.mark) {
      assert.ok(
        view.text.startsWith(view.mark + ' '),
        `${status}: the header form ${JSON.stringify(view.text)} does not lead with its mark`,
      )
      continue
    }
    // No mark: the status has no verdict to lead with, so the long form names
    // the state the short word does ("running" / "run").
    assert.ok(view.text.includes(view.word), `${status}: ${view.text} vs ${view.word}`)
  }
})

test('cancelled is neither a pass nor a failure', () => {
  const cancelled = statusView('cancelled')
  assert.equal(cancelled.cls, 'dash-st-cancel')
  assert.equal(cancelled.text, '⊘ cancelled')
  assert.equal(cancelled.live, false)
  assert.notEqual(cancelled.textCls, statusView('passed').textCls)
  assert.notEqual(cancelled.textCls, statusView('failed').textCls)
  // The failure mark is what a cancelled stage must not take: it says nothing
  // about the code.
  assert.notEqual(cancelled.mark, statusView('failed').mark)
})

test('only running is live', () => {
  const live = STATUSES.filter((s) => statusView(s).live)
  assert.deepEqual(live, ['running'])
})

test('an unknown status reads as a failure, and shows a neutral glyph', () => {
  // A row written by a newer server, or a status this build does not know: it
  // is certainly not a pass, so the colored text says failure...
  const view = statusView('dequeued')
  assert.equal(view.textCls, 'text-danger')
  assert.equal(view.text, '✗ failed')
  // ...but a list of nodes must not paint an unknown state as a failure.
  assert.equal(statusGlyph('dequeued'), '·')
})

test('statusGlyph falls back to the dot for a status with no mark', () => {
  assert.equal(statusGlyph('passed'), '✓')
  assert.equal(statusGlyph('cancelled'), '⊘')
  assert.equal(statusGlyph('pending'), '·')
  assert.equal(statusGlyph('running'), '·')
})
