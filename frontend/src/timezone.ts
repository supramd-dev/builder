// Site timezone handling. The site config may set an IANA timezone name that
// every displayed timestamp is rendered in; empty means "the viewer's
// browser-local zone". The choice is cached in localStorage so pages render
// without waiting for the config round-trip, and refreshed whenever the
// config is (re)loaded or updated.

const cacheKey = 'md-builder.timezone'

// The active display timezone: an IANA name, or "" for browser-local.
let activeTimezone = ''

// Subscribers notified when the active timezone changes (pages re-render
// their timestamps).
const listeners = new Set<() => void>()

function readCache(): string {
  try {
    return localStorage.getItem(cacheKey) ?? ''
  } catch {
    return ''
  }
}

function writeCache(tz: string): void {
  try {
    if (tz) {
      localStorage.setItem(cacheKey, tz)
    } else {
      localStorage.removeItem(cacheKey)
    }
  } catch {
    // localStorage may be unavailable (private mode); the setting just
    // won't persist across reloads.
  }
}

// Init from the cache so the very first render already uses the right zone.
activeTimezone = readCache()

// applySiteTimezone records the site-configured display timezone. Called on
// app load and after settings updates; skips the notification when nothing
// changed.
export function applySiteTimezone(tz: string): void {
  const next = tz || ''
  if (next === activeTimezone) {
    return
  }
  activeTimezone = next
  writeCache(next)
  for (const fn of listeners) {
    fn()
  }
}

// subscribeTimezone registers a callback fired when the display timezone
// changes; returns the unsubscribe function.
export function subscribeTimezone(fn: () => void): () => void {
  listeners.add(fn)
  return () => {
    listeners.delete(fn)
  }
}

// siteTimezone returns the active display timezone ("" = browser local).
export function siteTimezone(): string {
  return activeTimezone
}

// formatTime renders an RFC3339/ISO timestamp in the site timezone, e.g.
// "2025-09-12 21:45:03 (UTC+8)". Invalid/empty input renders as "—".
export function formatTime(ts: string): string {
  if (!ts) {
    return '—'
  }
  const d = new Date(ts)
  if (isNaN(d.getTime())) {
    return '—'
  }
  // en-CA gives ISO-like year-month-day ordering; the timezone comes from
  // the site setting, falling back to the browser's own zone.
  const opts: Intl.DateTimeFormatOptions = {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }
  if (activeTimezone) {
    opts.timeZone = activeTimezone
  }
  let text: string
  try {
    text = new Intl.DateTimeFormat('en-CA', opts).format(d)
  } catch {
    // Unknown zone name in the cache (stale config): fall back to local.
    text = new Intl.DateTimeFormat('en-CA', { ...opts, timeZone: undefined }).format(d)
  }
  // en-CA renders "2025-09-12, 21:45:03"; drop the comma.
  return `${text.replace(', ', ' ')} (${utcOffsetLabel(d)})`
}

// formatTimeShort renders only the date part (dashboard commit rows), in
// the site timezone: "2025-09-12".
export function formatTimeShort(ts: string): string {
  if (!ts) {
    return '—'
  }
  const d = new Date(ts)
  if (isNaN(d.getTime())) {
    return '—'
  }
  const opts: Intl.DateTimeFormatOptions = {
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }
  if (activeTimezone) {
    opts.timeZone = activeTimezone
  }
  try {
    return new Intl.DateTimeFormat('en-CA', opts).format(d)
  } catch {
    return new Intl.DateTimeFormat('en-CA', { ...opts, timeZone: undefined }).format(d)
  }
}

// utcOffsetLabel renders the zone's current offset from UTC, e.g. "UTC+8"
// or "UTC-5" (DST-aware, computed for the timestamp's instant).
function utcOffsetLabel(d: Date): string {
  const opts: Intl.DateTimeFormatOptions = {
    timeZone: activeTimezone || undefined,
    timeZoneName: 'shortOffset',
  }
  try {
    const parts = new Intl.DateTimeFormat('en-US', opts).formatToParts(d)
    const name = parts.find((p) => p.type === 'timeZoneName')?.value ?? ''
    // shortOffset gives "GMT+8" / "GMT-5" / "GMT+5:30"; normalize to UTC±.
    if (name) {
      return name.replace('GMT', 'UTC').replace('UTC', 'UTC')
    }
  } catch {
    // fall through to the computed offset below
  }
  const offset = activeTimezone ? tzOffsetMinutes(d, activeTimezone) : -d.getTimezoneOffset()
  if (offset === 0) {
    return 'UTC'
  }
  const sign = offset > 0 ? '+' : '-'
  const abs = Math.abs(offset)
  const h = Math.floor(abs / 60)
  const m = abs % 60
  return `UTC${sign}${h}${m > 0 ? `:${String(m).padStart(2, '0')}` : ''}`
}

// tzOffsetMinutes computes the zone's offset from UTC in minutes at the
// given instant (fallback when timeZoneName rendering is unavailable).
function tzOffsetMinutes(d: Date, timeZone: string): number {
  const dtf = new Intl.DateTimeFormat('en-US', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
  const parts = dtf.formatToParts(d)
  const get = (type: string) =>
    Number(parts.find((p) => p.type === type)?.value ?? '0')
  const asUTC = Date.UTC(
    get('year'),
    get('month') - 1,
    get('day'),
    get('hour') % 24,
    get('minute'),
    get('second'),
  )
  return Math.round((asUTC - d.getTime()) / 60000)
}

// commonTimezones is the curated top of the picker list; the full IANA set
// follows below it in the settings select.
export const commonTimezones = [
  'UTC',
  'Asia/Shanghai',
  'Asia/Tokyo',
  'Asia/Singapore',
  'Asia/Kolkata',
  'Asia/Dubai',
  'Europe/Moscow',
  'Europe/Berlin',
  'Europe/Paris',
  'Europe/London',
  'America/New_York',
  'America/Chicago',
  'America/Denver',
  'America/Los_Angeles',
  'Australia/Sydney',
]

// browserTimezone returns the viewer's own IANA zone name (for the picker's
// "browser" hint); "" when the browser doesn't expose it.
export function browserTimezone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  } catch {
    return ''
  }
}

// allTimezones returns every zone name the runtime knows (the full picker
// list).
export function allTimezones(): string[] {
  try {
    // ES2022+; TS lib is ES2023 so the typing exists.
    const zones = Intl.supportedValuesOf('timeZone') as string[]
    if (zones.length > 0) {
      return zones
    }
  } catch {
    // fall through to the common list
  }
  return commonTimezones
}
