import { LoaderCircle } from 'lucide-react'

// Shared small view helpers for statuses, commit links and truncation.

// truncate shortens s to at most n characters, marking the cut with an
// ellipsis (used for author names in the matrix).
export function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + '…' : s
}

// commitUrl builds the repository web URL of a commit ("" when the repo's
// web URL is unknown and the sha should stay plain text).
export function commitUrl(repoUrl: string | undefined, sha: string): string {
  return repoUrl ? `${repoUrl}/commit/${sha}` : ''
}

// StageStatus renders a dashboard stage as plain colored text — "✓ pass" /
// "✗ fail" / "⤼ skip" / spinner "run" / gray "pending" — linking to the run
// or task details when clickable. label overrides the pass/fail word (e.g.
// the passed/total counts on the unit and regression dashboards).
export function StageStatus({ status, label, onClick, title }: {
  status: string
  label?: string
  onClick?: () => void
  title?: string
}) {
  const cls =
    status === 'passed' ? 'dash-st-ok'
      : status === 'failed' ? 'dash-st-fail'
        : status === 'skipped' ? 'dash-st-skip'
          : status === 'running' ? 'dash-st-run'
            : 'dash-st-pending'
  const inner =
    status === 'running' ? (
      <>
        <LoaderCircle size={11} className="spin" /> run
      </>
    )
      : status === 'pending' ? 'pending'
        : status === 'skipped' ? '⤼ skip'
          : `${status === 'passed' ? '✓' : '✗'} ${label ?? (status === 'passed' ? 'pass' : 'fail')}`
  if (onClick) {
    return (
      <a
        href="#"
        className={'dash-st ' + cls}
        title={title}
        onClick={(e) => {
          e.preventDefault()
          onClick()
        }}
      >
        {inner}
      </a>
    )
  }
  return (
    <span className={'dash-st ' + cls} title={title}>
      {inner}
    </span>
  )
}

// TaskStatusText renders a task status as colored text for page titles and
// step rows: ✓ done / ✗ failed / ⤼ skipped / spinner running / · pending.
export function TaskStatusText({ status }: { status: string }) {
  const cls =
    status === 'done' ? 'text-success'
      : status === 'failed' ? 'text-danger'
        : status === 'skipped' ? 'text-warn'
          : status === 'running' ? 'text-run'
            : 'text-muted'
  return (
    <span className={'task-status-text ' + cls}>
      {status === 'running' ? (
        <>
          <LoaderCircle size={13} className="spin" /> running
        </>
      ) : (
        <>
          {statusGlyph(status)} {status}
        </>
      )}
    </span>
  )
}

function statusGlyph(status: string): string {
  switch (status) {
    case 'done':
      return '✓'
    case 'failed':
      return '✗'
    case 'skipped':
      return '⤼'
    default:
      return '·'
  }
}
