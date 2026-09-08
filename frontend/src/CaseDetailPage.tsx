import type { CaseResult } from './api'

interface Props {
  runId: number
  caseResult: CaseResult
  onBack: () => void
}

// CaseDetailPage is the per-case detail view (error breakdown, result
// visualization). The detailed comparison and plots are not implemented
// yet — this page shows the summary fields that are already known.
export default function CaseDetailPage({ runId, caseResult, onBack }: Props) {
  const failed = caseResult.status === 'failed'
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

      <div className="event">
        <dl className="dash-summary">
          <div>
            <dt>Status</dt>
            <dd className={failed ? 'text-danger' : 'text-success'}>
              {failed ? '✗ failed' : '✓ passed'}
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
          Detailed case results (error breakdown, reference comparison and
          plots) are not implemented yet. This page is a placeholder.
        </p>
      </div>
    </div>
  )
}
