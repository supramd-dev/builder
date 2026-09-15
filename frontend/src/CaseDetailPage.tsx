import { useEffect, useState } from 'react'
import { useParams } from 'react-router'
import { commitUrl } from './StatusViews'
import { getTestRun, type CaseResult } from './api'
import { formatDuration } from './gtest'
import { Breadcrumbs } from './Breadcrumbs'

// CaseDetailPage is the per-case detail view (error breakdown, result
// visualization). The detailed comparison and plots are not implemented
// yet — this page shows the summary fields that are already known.
//
// Routed as /runs/:runId/case/:caseId; the case row itself is looked up in
// the run detail (which is fetched anyway for the repo link).
export default function CaseDetailPage() {
  const runId = Number(useParams().runId)
  const caseId = Number(useParams().caseId)
  // The run's context — case row, repository/commit link, root task id.
  // Only the case row is required; the repo link and task crumb are
  // optional decoration.
  const [state, setState] = useState<{
    caseResult: CaseResult | null
    rootTaskId: number
    repoUrl: string | null
    repo: string | null
    sha: string | null
    error: string
  } | null>(null)

  useEffect(() => {
    let cancelled = false
    getTestRun(runId)
      .then((r) => {
        if (cancelled) return
        setState({
          caseResult: r.cases.find((c) => c.id === caseId) ?? null,
          rootTaskId: r.rootTaskId,
          repoUrl: r.commitRepoUrl,
          repo: r.commitRepo,
          sha: r.commitSha,
          error: '',
        })
      })
      .catch((err: unknown) => {
        if (cancelled) return
        setState({
          caseResult: null,
          rootTaskId: 0,
          repoUrl: null,
          repo: null,
          sha: null,
          error: err instanceof Error ? err.message : 'Failed to load test run',
        })
      })
    return () => {
      cancelled = true
    }
  }, [runId, caseId])

  if (!state) {
    return (
      <div>
        <Breadcrumbs
          trail={[
            { label: 'Dashboard', to: '/' },
            { label: `Run #${runId}`, to: `/runs/${runId}` },
            { label: 'Case' },
          ]}
        />
        <p className="text-muted">Loading…</p>
      </div>
    )
  }

  const { caseResult, rootTaskId, repo, repoUrl, sha, error } = state
  const trail = [
    { label: 'Dashboard', to: '/' },
    ...(rootTaskId > 0
      ? [{ label: `Task #${rootTaskId}`, to: `/tasks/${rootTaskId}` }]
      : []),
    { label: `Run #${runId}`, to: `/runs/${runId}` },
    { label: caseResult ? caseResult.name : `Case #${caseId}` },
  ]

  if (error) {
    return (
      <div>
        <Breadcrumbs trail={trail} />
        <div className="alert alert-danger">{error}</div>
      </div>
    )
  }
  if (!caseResult) {
    return (
      <div>
        <Breadcrumbs trail={trail} />
        <div className="alert alert-danger">Case not found in run #{runId}.</div>
      </div>
    )
  }

  const failed = caseResult.status === 'failed'
  const skipped = caseResult.status === 'skipped'
  const url = commitUrl(repoUrl ?? undefined, sha ?? '')
  return (
    <div>
      <Breadcrumbs trail={trail} />

      <h2>
        <code>{caseResult.name}</code>
      </h2>

      {/* Link to the commit under test on the hosting site (GitLab,
          GitHub, ...) when the repository is derivable. */}
      {(url || repo) && (
        <p style={{ marginTop: '-0.25rem', marginBottom: '1rem' }}>
          {url ? (
            <a href={url} target="_blank" rel="noreferrer">
              {repo} · {sha?.slice(0, 7)}
            </a>
          ) : (
            <span className="text-muted">
              {repo}
              {sha ? ` · ${sha.slice(0, 7)}` : ''}
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
