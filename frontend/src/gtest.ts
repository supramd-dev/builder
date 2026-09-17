// Browser-side googletest results parsing. The backend stores the results
// file verbatim as a run artifact (plus the aggregate counts read from its
// root attributes); the per-case list — name, status, duration, failure
// message — is parsed here, where it is displayed.

// GTestCaseStatus covers the stored case statuses. The browser-parsed
// results files only ever yield passed/failed/skipped; pending/running come
// from the store's dispatch-time placeholder child runs (regression detail).
export type GTestCaseStatus = 'passed' | 'failed' | 'skipped' | 'pending' | 'running'

export interface GTestCase {
  name: string
  status: GTestCaseStatus
  durationMs: number
  message: string
}

// parseGTestResults parses a googletest results file (XML or JSON, sniffed
// by the first non-space character). Throws on unrecognizable or malformed
// content — callers show the raw file as a fallback.
export function parseGTestResults(content: string): GTestCase[] {
  const trimmed = content.trimStart()
  if (trimmed.startsWith('<')) {
    return parseGTestXML(trimmed)
  }
  if (trimmed.startsWith('{')) {
    return parseGTestJSON(trimmed)
  }
  throw new Error('not a googletest results file (expected XML or JSON)')
}

// --- XML (--gtest_output=xml:...) ---

function parseGTestXML(content: string): GTestCase[] {
  const doc = new DOMParser().parseFromString(content, 'application/xml')
  if (doc.querySelector('parsererror')) {
    throw new Error('malformed XML results file')
  }
  const cases: GTestCase[] = []
  // testcase elements may sit at the root (single suite) or under testsuite
  // nodes (testsuites wrapper); querySelectorAll covers both.
  for (const el of doc.querySelectorAll('testcase')) {
    const name = el.getAttribute('name') ?? ''
    const className = el.getAttribute('classname') ?? ''
    const failures = el.getElementsByTagName('failure')
    const skipped = el.getAttribute('status') === 'notrun' || el.getElementsByTagName('skipped').length > 0
    let status: GTestCaseStatus
    let message = ''
    if (skipped) {
      status = 'skipped'
      message = 'skipped (disabled)'
    } else if (failures.length > 0) {
      status = 'failed'
      // Concatenate failure messages (usually one) in full — the table cell
      // shows a preview, the dialog the complete text.
      const parts: string[] = []
      for (const f of Array.from(failures)) {
        parts.push(f.getAttribute('message') ?? f.textContent ?? '')
      }
      message = parts.filter((p) => p !== '').join('\n')
    } else {
      status = 'passed'
    }
    cases.push({
      name: className && className !== name ? `${className}.${name}` : name,
      status,
      durationMs: parseSeconds(el.getAttribute('time')),
      message,
    })
  }
  if (cases.length === 0) {
    throw new Error('XML results file contains no test cases')
  }
  return cases
}

// --- JSON (--gtest_output=json:...) ---

// googletest JSON nests as {"testsuites": [suite, ...]} with each suite
// carrying its cases under "testsuite" (singular).
interface GTestJSONCase {
  name: string
  classname?: string
  time?: number | string
  status?: string
  result?: string
  failures?: { failure?: string; message?: string }[]
}

interface GTestJSONSuite {
  name?: string
  testsuite?: GTestJSONCase[]
}

interface GTestJSONDoc {
  testsuites?: GTestJSONSuite[]
}

function parseGTestJSON(content: string): GTestCase[] {
  let doc: GTestJSONDoc
  try {
    doc = JSON.parse(content) as GTestJSONDoc
  } catch {
    throw new Error('malformed JSON results file')
  }
  const cases: GTestCase[] = []
  for (const suite of doc.testsuites ?? []) {
    for (const c of suite.testsuite ?? []) {
      const name = c.name ?? ''
      const className = c.classname ?? ''
      const failed = (c.failures ?? []).length > 0
      const skipped = c.result === 'SKIPPED' || c.status === 'NOTRUN'
      let status: GTestCaseStatus
      let message = ''
      if (skipped) {
        status = 'skipped'
        message = 'skipped (disabled)'
      } else if (failed) {
        status = 'failed'
        message = (c.failures ?? [])
          .map((f) => f.failure ?? f.message ?? '')
          .filter((m) => m !== '')
          .join('\n')
      } else {
        status = 'passed'
      }
      cases.push({
        name: className && className !== name ? `${className}.${name}` : name,
        status,
        durationMs: parseSeconds(c.time),
        message,
      })
    }
  }
  if (cases.length === 0) {
    throw new Error('JSON results file contains no test cases')
  }
  return cases
}

// parseSeconds parses a googletest duration ("0.012s" or 0.012) into
// milliseconds.
function parseSeconds(v: number | string | null | undefined): number {
  if (v === null || v === undefined) {
    return 0
  }
  const n = typeof v === 'number' ? v : parseFloat(v)
  if (!isFinite(n)) {
    return 0
  }
  return n * 1000
}

// formatDuration renders a millisecond duration for display. A zero
// duration is a real value (tests can be that fast) — only undefined/NaN
// renders as a placeholder.
export function formatDuration(ms: number): string {
  if (!isFinite(ms)) {
    return '—'
  }
  if (ms < 1000) {
    return `${Math.round(ms)} ms`
  }
  return `${(ms / 1000).toFixed(2)} s`
}
