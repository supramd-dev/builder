import { useEffect, useState } from 'react'
import { commitUrl } from './StatusViews'
import { getTestRun, type CaseResult } from './api'
import { formatDuration } from './gtest'

interface Props {
  runId: number
  caseResult: CaseResult
  onBack: () => void
}

// CaseDetailPage is the per-case detail view (error breakdown, result
// visualization). The detailed comparison and plots are not implemented
// yet — this page shows the summary fields that are already known.
export default function CaseDetailPage({ runId, caseResult, onBack }: Props) {
  // The run's repository/commit context — the page routes here with only
  // the case row, so the run detail is fetched for the repo link.
  const [repo, setRepo] = useState<{
    repoUrl: string | null
    repo: string | null
    sha: string | null
  } | null>(null)

  useEffect(() => {
    let cancelled = false
    getTestRun(runId)
      .then((r) => {
        if (cancelled) return
        setRepo({
          repoUrl: r.commitRepoUrl,
          repo: r.commitRepo,
          sha: r.commitSha,
        })
      })
      .catch(() => {
        // The repo link is optional decoration; a failed fetch just
        // omits it.
      })
    return () => {
      cancelled = true
    }
  }, [runId])

  const failed = caseResult.status === 'failed'
  const skipped = caseResult.status === 'skipped'
  const url = repo ? commitUrl(repo.repoUrl ?? undefined, repo.sha ?? '') : ''
  return (
    <div>
      <p style={{ marginBottom: '0.5rem' }}>
        <a
          href="#"
          onClick={(e) => {
            e.preventDefault()
            onBack()
          }}
        >
          ← Back to test run
        </a>
      </p>

      <h2>
        <code>{caseResult.name}</code>
      </h2>

      {/* Link to the commit under test on the hosting site (GitLab,
          GitHub, ...) when the repository is derivable. */}
      {repo && (url || repo.repo) && (
        <p style={{ marginTop: '-0.25rem', marginBottom: '1rem' }}>
          {url ? (
            <a href={url} target="_blank" rel="noreferrer">
              {repo.repo} · {repo.sha?.slice(0, 7)}
            </a>
          ) : (
            <span className="text-muted">
              {repo.repo}
              {repo.sha ? ` · ${repo.sha.slice(0, 7)}` : ''}
            </span>
          )}
        </p>
      )}

      <div className="event">
        <dl className="dash-summary">
          <div>
            <dt>Status</dt>
            <dd
              className={
                failed ? 'text-danger' : skipped ? 'text-warn' : 'text-success'
              }
            >
              {failed ? '✗ failed' : skipped ? '⤼ skipped' : '✓ passed'}
            </dd>
          </div>
          <div>
            <dt>Error</dt>
            <dd>
              {caseResult.errorValue !== 0 ? (
                <code>{caseResult.errorValue.toExponential(3)}</code>
              ) : (
                <span className="text-muted">—</span>
              )}
            </dd>
          </div>
          <div>
            <dt>Duration</dt>
            <dd>{formatDuration(caseResult.durationMillis)}</dd>
          </div>
          <div>
            <dt>Note</dt>
            <dd className="text-muted">{caseResult.message || '—'}</dd>
          </div>
          <div>
            <dt>Run</dt>
            <dd>#{runId}</dd>
          </div>
        </dl>
      </div>

      <h3>Details</h3>
      <div className="event">
        <p className="text-muted" style={{ margin: 0 }}>
          Detailed case analysis (per-case log, series comparison and plots)
          is not implemented yet — the case's log and series data will be
          fetched from the stored artifacts and rendered here.
        </p>
      </div>
    </div>
  )
}
