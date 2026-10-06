import { LoaderCircle } from 'lucide-react'

import { statusView } from './status'

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
// "✗ fail" / "⏱ timeout" / "⤼ skip" / "⊘ cancel" / spinner "run" / gray
// "pending" — linking to the run or task details when clickable. label
// overrides the status word (e.g. the passed/total counts on the unit and
// regression dashboards). The mark, the word and the class come from
// statusView (see status.ts), which every page shares.
export function StageStatus({ status, label, onClick, title }: {
  status: string
  label?: string
  onClick?: () => void
  title?: string
}) {
  const view = statusView(status)
  const inner =
    view.live ? (
      <>
        <LoaderCircle size={11} className="spin" /> {view.word}
      </>
    )
      : `${view.mark ? view.mark + ' ' : ''}${label ?? view.word}`
  if (onClick) {
    return (
      <a
        href="#"
        className={'dash-st ' + view.cls}
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
    <span className={'dash-st ' + view.cls} title={title}>
      {inner}
    </span>
  )
}

// StatusText renders a task's or a run's status as colored inline text: ✓
// passed / ✗ failed / ⏱ timed out / ⤼ skipped / ⊘ cancelled / spinner
// running / gray · pending. Tasks and runs share one status vocabulary — a
// task's status is its latest attempt's — so one component serves both page
// titles and node rows, and the wording comes from statusView (status.ts).
export function StatusText({ status }: { status: string }) {
  const view = statusView(status)
  if (view.live) {
    return (
      <span className={view.textCls}>
        <LoaderCircle size={13} className="spin" /> {view.text}
      </span>
    )
  }
  return <span className={view.textCls}>{view.text}</span>
}
