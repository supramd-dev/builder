// Paging arithmetic for a paged list (the dashboard matrix is one), kept out
// of the component so `node --test` can cover the edge cases: an empty list, a
// page past the end, and a page size that arrives as zero.

// PageInfo is everything a pager renders: the page actually showing (clamped
// into range), how many pages the list has, and the 1-based inclusive range of
// items on it — "201–400 of 1234". from/to are 0 when there is nothing at all.
export interface PageInfo {
  page: number
  pages: number
  from: number
  to: number
  total: number
}

// pageInfo clamps a requested page into [1, pages]. The server clamps too
// (and a stale page number is a normal thing to hold onto: rows disappear as
// the history is pruned), so this mirrors it rather than trusting the caller.
export function pageInfo(total: number, pageSize: number, page: number): PageInfo {
  const t = Number.isFinite(total) ? Math.max(0, Math.floor(total)) : 0
  const size = Number.isFinite(pageSize) && pageSize >= 1 ? Math.floor(pageSize) : 1
  const pages = Math.max(1, Math.ceil(t / size))
  const requested = Number.isFinite(page) ? Math.floor(page) : 1
  const p = Math.min(Math.max(1, requested), pages)
  if (t === 0) {
    return { page: p, pages, from: 0, to: 0, total: 0 }
  }
  return { page: p, pages, from: (p - 1) * size + 1, to: Math.min(p * size, t), total: t }
}

// rangeLabel says which slice of a list is on screen, or that there is none.
// Numbers get thousands separators: a page range is read at a glance, and
// "1201–1400 of 12345" is harder to parse than "1,201–1,400 of 12,345".
export function rangeLabel(info: PageInfo): string {
  if (info.total === 0) return 'no rows'
  return `${group(info.from)}–${group(info.to)} of ${group(info.total)}`
}

function group(n: number): string {
  return n.toLocaleString('en-US')
}
