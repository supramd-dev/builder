// Stage scheduling windows: the md-builder.yaml `window: "22:00-06:00"` a
// build/unit/performance/regression stage may carry.
//
// A window restricts when a stage may *start* (the server holds the node in
// pending until the window opens; a running stage is never interrupted), and
// it is read in the site timezone. The server is the one that enforces it —
// everything here is display: which window a stage has, and whether it is
// open right now, so a pending node does not read as stuck when it is simply
// waiting for tonight.
//
// Plain TypeScript (no JSX, no React) so it is testable under `node --test`,
// which cannot strip types out of a .tsx file.

// StageWindow is a daily interval as minutes since midnight. start > end
// means the window wraps over midnight ("22:00-06:00").
export interface StageWindow {
  start: number
  end: number
}

// parseClock reads the server's "HH:MM" (two digits each; "24:00" is the end
// of the day) into minutes since midnight; null when it is not that shape.
function parseClock(text: string): number | null {
  const m = /^(\d{2}):(\d{2})$/.exec(text)
  if (!m) return null
  const h = Number(m[1])
  const min = Number(m[2])
  if (min > 59) return null
  if (h === 24 ? min !== 0 : h > 23) return null
  return h * 60 + min
}

// formatClock renders minutes since midnight back as "HH:MM" (24:00 for the
// very end of the day).
export function formatClock(minutes: number): string {
  const h = Math.floor(minutes / 60)
  const m = minutes % 60
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`
}

// parseWindow parses the stored window spelling. It returns null for "any
// time" — an unset or empty window, and also one this build cannot make sense
// of (the server schedules an unparsable window any time too, so showing "any
// time" matches what actually happens).
export function parseWindow(raw?: string): StageWindow | null {
  const text = (raw ?? '').trim()
  if (!text) return null
  const dash = text.indexOf('-')
  if (dash < 0) return null
  const start = parseClock(text.slice(0, dash).trim())
  const end = parseClock(text.slice(dash + 1).trim())
  if (start === null || end === null) return null
  // start == end would be "never open" server-side; it is not a window.
  if (start === end) return null
  return { start, end }
}

// windowOpenAt reports whether the window is open at the given minute of the
// day: the start is included, the end excluded, and a wrapping window is open
// on either side of midnight. It mirrors the server's store.Window.OpenAt.
export function windowOpenAt(w: StageWindow, minutes: number): boolean {
  if (w.start < w.end) return minutes >= w.start && minutes < w.end
  return minutes >= w.start || minutes < w.end
}

// minutesOfDay is the wall-clock minute in the given IANA timezone ("" = the
// viewer's own zone), which is the clock the server reads the window in.
export function minutesOfDay(now: Date, timezone: string): number {
  const opts: Intl.DateTimeFormatOptions = {
    hour: '2-digit',
    minute: '2-digit',
    hourCycle: 'h23',
  }
  if (timezone) opts.timeZone = timezone
  try {
    const parts = new Intl.DateTimeFormat('en-US', opts).formatToParts(now)
    const get = (type: string) => Number(parts.find((p) => p.type === type)?.value ?? '0')
    return (get('hour') % 24) * 60 + get('minute')
  } catch {
    // An unknown zone name must not blank the page: fall back to local time.
    return now.getHours() * 60 + now.getMinutes()
  }
}

// isWindowOpen reports whether a stage may start right now. No window (or one
// that cannot be parsed) means any time, so the answer is yes.
export function isWindowOpen(raw: string | undefined, now: Date, timezone: string): boolean {
  const w = parseWindow(raw)
  if (!w) return true
  return windowOpenAt(w, minutesOfDay(now, timezone))
}

// windowLabel is the compact spelling for a chip: "22:00–06:00", or "" when
// the stage may start any time.
export function windowLabel(raw?: string): string {
  const w = parseWindow(raw)
  return w ? `${formatClock(w.start)}–${formatClock(w.end)}` : ''
}

// windowNote is the one-line explanation shown next to a stage that has a
// window: the interval and whether it is open right now, in the zone the
// server reads it in.
export function windowNote(raw: string | undefined, now: Date, timezone: string): string {
  const w = parseWindow(raw)
  if (!w) return ''
  const zone = timezone || 'local time'
  const state = windowOpenAt(w, minutesOfDay(now, timezone)) ? 'open now' : 'closed now'
  return `runs only between ${windowLabel(raw)} (${zone}) — ${state}`
}
