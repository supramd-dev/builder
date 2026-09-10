import { useEffect, useState } from 'react'
import {
  getTestRun,
  type CaseResult,
  type TestRunDetail,
} from './api'

interface Props {
  runId: number
  onBack: () => void
  onOpenCase: (runId: number, caseResult: CaseResult) => void
  onError: (message: string) => void
}

// TestRunDetailPage shows one test run in the sr.ht build style: the title
// (kind, status in color), a summary block (environment, commit, results,
// time), the run's one-paragraph conclusion and the per-case results.
export default function TestRunDetailPage({ runId, onBack, onOpenCase, onError }: Props) {
  const [run, setRun] = useState<TestRunDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    getTestRun(runId)
      .then((r) => {
        if (!cancelled) setRun(r)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load'
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [runId, onError])

  if (loading) {
    return (
      <div>
        <BackLink onBack={onBack} />
        <p className="text-muted">Loading…</p>
      </div>
    )
  }
  if (error || !run) {
    return (
      <div>
        <BackLink onBack={onBack} />
        {error && <div className="alert alert-danger">{error}</div>}
      </div>
    )
  }

  const failed = run.status === 'failed'
  const skipped = run.status === 'skipped'
  const statusCls = failed ? 'text-danger' : skipped ? 'text-warn' : 'text-success'
  const statusText = failed ? '✗ failed' : skipped ? '⤼ skipped' : '✓ passed'
  const kindLabel =
    run.kind === 'regression' ? 'Regression tests' : run.kind === 'build' ? 'Build' : 'Unit tests'

  return (
    <div>
      <BackLink onBack={onBack} />

      {/* Title: kind + status in color. */}
      <h2 className="task-title">
        {kindLabel} · <span className={statusCls}>{statusText}</span>
        {run.commitShortSha && (
          <>
            {' · '}
            <code>{run.commitShortSha}</code>
          </>
        )}
        {run.environmentName && <span className="text-muted"> on {run.environmentName}</span>}
      </h2>

      {/* Summary: commit, author, results, time. */}
      <div className="task-summary text-muted">
        {run.commitMessage && <>{run.commitMessage} · </>}
        {run.commitAuthor && <>{run.commitAuthor} · </>}
        {skipped ? (
          <span className="text-warn">not executed (upstream failure)</span>
        ) : (
          <>
            <span className={failed ? 'text-danger' : 'text-success'}>
              {run.passed}/{run.total} passed
            </span>
            {run.failed > 0 && <span className="text-danger"> ({run.failed} failed)</span>}
          </>
        )}
        {run.startedAt && (
          <>
            {' · '}
            {formatTime(run.startedAt)}
            {run.finishedAt ? ` → ${formatTime(run.finishedAt)}` : ''}
          </>
        )}
      </div>

      {skipped && (
        <p className="dash-skip-note">
          This stage was skipped: an upstream task failed before it could run,
          so no tests were executed. See the summary below for the upstream
          failure.
        </p>
      )}

      {run.summary && <pre className="dash-run-summary">{run.summary}</pre>}

      <h3 className="task-section-title">Test cases</h3>
      {run.cases.length === 0 ? (
        <p className="text-muted">
          No per-case results were reported for this run — see the summary
          above.
        </p>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Case</th>
              <th>Status</th>
              <th>Error</th>
              <th>Note</th>
            </tr>
          </thead>
          <tbody>
            {run.cases.map((c) => (
              <tr
                key={c.id}
                className="dash-case-row"
                onClick={() => onOpenCase(runId, c)}
                title="Click for case details"
              >
                <td>{c.name}</td>
                <td>
                  {c.status === 'passed' ? (
                    <span className="text-success">✓ passed</span>
                  ) : (
                    <span className="text-danger">✗ failed</span>
                  )}
                </td>
                <td>
                  {c.errorValue !== 0 ? (
                    <code>{formatError(c.errorValue)}</code>
                  ) : (
                    <span className="text-muted">—</span>
                  )}
                </td>
                <td className="text-muted">{c.message || '—'}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  )
}

// BackLink is the "← back to dashboard" navigation.
function BackLink({ onBack }: { onBack: () => void }) {
  return (
    <p style={{ marginBottom: '0.5rem' }}>
      <a
        href="#"
        onClick={(e) => {
          e.preventDefault()
          onBack()
        }}
      >
        ← Back to dashboard
      </a>
    </p>
  )
}

// formatError renders a numeric error value with meaningful precision.
function formatError(v: number): string {
  if (v === 0) return '0'
  const abs = Math.abs(v)
  if (abs >= 1e4 || abs < 1e-3) {
    return v.toExponential(3)
  }
  return String(Number(v.toPrecision(4)))
}

// formatTime renders an RFC3339 timestamp for display.
function formatTime(ts: string): string {
  return ts.replace('T', ' ').replace('Z', ' UTC')
}
