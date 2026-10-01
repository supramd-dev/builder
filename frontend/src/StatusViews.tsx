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

// StatusText renders a task's or a run's status as colored inline text: ✓
// passed / ✗ failed / ⤼ skipped / spinner running / gray · pending. Tasks and
// runs share one status vocabulary — a task's status is its latest attempt's
// — so one component serves both page titles and node rows.
export function StatusText({ status }: { status: string }) {
  if (status === 'running') {
    return (
      <span className="text-run">
        <LoaderCircle size={13} className="spin" /> running
      </span>
    )
  }
  const [cls, text] =
    status === 'passed'
      ? ['text-success', '✓ passed']
      : status === 'skipped'
        ? ['text-warn', '⤼ skipped']
        : status === 'pending'
          ? ['text-muted', '· pending']
          : ['text-danger', '✗ failed']
  return <span className={cls}>{text}</span>
}
