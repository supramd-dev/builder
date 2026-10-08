// Stage scheduling windows (windows.ts).
//
// The server owns the decision to start a stage; these cases pin the display
// and the open/closed reading to the server's rule (start inclusive, end
// exclusive, wrapping windows open on either side of midnight, read in the
// site timezone) — a chip that says "open now" while the scheduler holds the
// node would send the reader looking for a bug that is not there.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  formatClock,
  isWindowOpen,
  minutesOfDay,
  parseWindow,
  windowLabel,
  windowNote,
  windowOpenAt,
} from './windows.ts'

test('parseWindow reads the stored spelling and reports any time as null', () => {
  assert.deepEqual(parseWindow('22:00-06:00'), { start: 22 * 60, end: 6 * 60 })
  assert.deepEqual(parseWindow(' 09:30 - 17:00 '), { start: 9 * 60 + 30, end: 17 * 60 })
  assert.deepEqual(parseWindow('00:00-24:00'), { start: 0, end: 24 * 60 })
  // Any time: unset, empty, or a spelling this build cannot read (the server
  // schedules an unparsable window any time, so the display must agree).
  for (const raw of [undefined, '', '   ', 'nighttime', '22:00', '22:00-22:00', '6:00-7:00', '22:00-25:00']) {
    assert.equal(parseWindow(raw), null, `parseWindow(${JSON.stringify(raw)})`)
  }
})

test('formatClock round-trips the minutes of a day', () => {
  assert.equal(formatClock(0), '00:00')
  assert.equal(formatClock(9 * 60 + 5), '09:05')
  assert.equal(formatClock(22 * 60), '22:00')
  assert.equal(formatClock(24 * 60), '24:00')
})

test('windowOpenAt includes the start, excludes the end', () => {
  const day = { start: 8 * 60, end: 18 * 60 }
  assert.equal(windowOpenAt(day, 7 * 60 + 59), false)
  assert.equal(windowOpenAt(day, 8 * 60), true)
  assert.equal(windowOpenAt(day, 17 * 60 + 59), true)
  assert.equal(windowOpenAt(day, 18 * 60), false)
  assert.equal(windowOpenAt(day, 23 * 60), false)
})

test('a wrapping window is open on either side of midnight', () => {
  const night = { start: 22 * 60, end: 6 * 60 }
  assert.equal(windowOpenAt(night, 21 * 60 + 59), false)
  assert.equal(windowOpenAt(night, 22 * 60), true)
  assert.equal(windowOpenAt(night, 23 * 60 + 30), true)
  assert.equal(windowOpenAt(night, 0), true)
  assert.equal(windowOpenAt(night, 5 * 60 + 59), true)
  assert.equal(windowOpenAt(night, 6 * 60), false)
  assert.equal(windowOpenAt(night, 12 * 60), false)
})

test('isWindowOpen reads the window on the site clock, not the viewer clock', () => {
  // 23:30 in Shanghai is 15:30 UTC: the same instant, two answers — the site
  // timezone is the one the server reads the window in.
  const at = new Date('2025-06-01T15:30:00Z')
  assert.equal(minutesOfDay(at, 'Asia/Shanghai'), 23 * 60 + 30)
  assert.equal(minutesOfDay(at, 'UTC'), 15 * 60 + 30)
  assert.equal(isWindowOpen('22:00-06:00', at, 'Asia/Shanghai'), true)
  assert.equal(isWindowOpen('22:00-06:00', at, 'UTC'), false)
  // An unknown zone falls back to the viewer's zone rather than throwing.
  assert.equal(minutesOfDay(at, 'Not/AZone'), at.getHours() * 60 + at.getMinutes())
})

test('no window means any time — and so does an unreadable one', () => {
  const at = new Date('2025-06-01T15:30:00Z')
  for (const raw of [undefined, '', 'whenever the machine is free']) {
    assert.equal(isWindowOpen(raw, at, 'UTC'), true, `isWindowOpen(${JSON.stringify(raw)})`)
  }
  assert.equal(windowLabel(undefined), '')
  assert.equal(windowNote(undefined, at, 'UTC'), '')
})

test('windowLabel and windowNote spell the interval and its state', () => {
  const night = new Date('2025-06-01T15:30:00Z') // 23:30 in Shanghai
  assert.equal(windowLabel('22:00-06:00'), '22:00–06:00')
  assert.equal(
    windowNote('22:00-06:00', night, 'Asia/Shanghai'),
    'runs only between 22:00–06:00 (Asia/Shanghai) — open now',
  )
  assert.equal(
    windowNote('22:00-06:00', night, 'UTC'),
    'runs only between 22:00–06:00 (UTC) — closed now',
  )
  // An unset site timezone reads as the server's local time.
  assert.equal(
    windowNote('08:00-10:00', night, ''),
    'runs only between 08:00–10:00 (local time) — closed now',
  )
})
