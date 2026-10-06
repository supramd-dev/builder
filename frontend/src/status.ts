// How a task or run status is shown: the mark, the word, the CSS class. One
// status vocabulary (the server's, see api.TaskStatus) reads the same
// everywhere — the dashboard's stage cells, the task graph, the run page — so
// this lives here rather than in each page's own ternary chain.
//
// Plain TypeScript (no JSX, no React) so the mapping is testable under
// `node --test`, which cannot strip types out of a .tsx file.

// StatusView is one status' presentation.
export interface StatusView {
  // cls is the dashboard cell's class (dash-st-*): the colored text of a
  // stage in the matrix.
  cls: string
  // textCls is the page-header class (text-*): the same status as inline
  // text on the task and run pages.
  textCls: string
  // word is the short label shown next to the mark ("pass", "fail", ...).
  word: string
  // mark is the single character before the word. It is empty for pending,
  // which is a state rather than a verdict, and empty for running, whose mark
  // is the spinner the caller renders.
  mark: string
  // text is the long form the page headers print ("✓ passed", "⏱ timed out").
  text: string
  // live reports that the status is still moving: the caller renders a
  // spinner (and follows the task) instead of a mark.
  live: boolean
}

// STATUS_VIEWS maps every status the server can report to its presentation.
// An unrecognized status — an older row's, or one this build does not know —
// is treated as a failure: it is certainly not a pass, and saying so is safer
// than showing a green cell for something unknown.
const STATUS_VIEWS: Record<string, StatusView> = {
  passed: { cls: 'dash-st-ok', textCls: 'text-success', word: 'pass', mark: '✓', text: '✓ passed', live: false },
  failed: { cls: 'dash-st-fail', textCls: 'text-danger', word: 'fail', mark: '✗', text: '✗ failed', live: false },
  timeout: { cls: 'dash-st-timeout', textCls: 'text-timeout', word: 'timeout', mark: '⏱', text: '⏱ timed out', live: false },
  skipped: { cls: 'dash-st-skip', textCls: 'text-warn', word: 'skip', mark: '⤼', text: '⤼ skipped', live: false },
  // cancelled is the site's repeated-commit policy dropping unfinished work
  // (see api.CommitOverlapPolicy). It is not a verdict on the code, so it
  // gets neither the pass nor the fail mark: ⊘ reads as "dropped".
  cancelled: { cls: 'dash-st-cancel', textCls: 'text-cancel', word: 'cancel', mark: '⊘', text: '⊘ cancelled', live: false },
  running: { cls: 'dash-st-run', textCls: 'text-run', word: 'run', mark: '', text: 'running', live: true },
  pending: { cls: 'dash-st-pending', textCls: 'text-muted', word: 'pending', mark: '', text: '· pending', live: false },
}

const UNKNOWN: StatusView = STATUS_VIEWS.failed

// statusView returns the presentation of a status. Callers that must not
// color an unknown status as a failure (a summary line, say) should switch
// on the status themselves; this one answers "what does it look like".
export function statusView(status: string): StatusView {
  return STATUS_VIEWS[status] ?? UNKNOWN
}

// statusGlyph is the bare mark of a status, for the places that render it on
// its own (the graph's node rows prefix a node's label with it). A status
// with no mark of its own — pending and running — and one this build does not
// know both show "·", the neutral dot: unlike statusView, which colors an
// unknown status as a failure, a list of nodes must not call an unknown state
// a failure.
export function statusGlyph(status: string): string {
  if (!Object.prototype.hasOwnProperty.call(STATUS_VIEWS, status)) return '·'
  return STATUS_VIEWS[status].mark || '·'
}
