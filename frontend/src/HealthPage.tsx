import { useEffect, useState } from 'react'
import { RefreshCw, LoaderCircle } from 'lucide-react'
import { getDeepHealth, getHealth, type DeepHealth, type HealthCheck } from './api'
import { formatTime } from './timezone'

interface Props {
  onError: (message: string) => void
}

// HealthPage is the site health board, linked from the footer. It answers
// "is everything md-builder depends on up?": the backend itself (liveness
// probe), the site-configured git repository (probed the same way a
// dispatch resolves refs — ls-remote with the site token), and the future
// object storage backend (a reserved row until it is configurable).
//
// The probes run server-side on demand when the page loads or the
// re-check button is pressed — there is no background polling to keep
// fresh, only the timestamp of the last check.
export default function HealthPage({ onError }: Props) {
  const [health, setHealth] = useState<DeepHealth | null>(null)
  const [liveness, setLiveness] = useState<'ok' | 'fail'>('ok')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  // load runs both probes: the cheap liveness endpoint (the page itself
  // being served already proves the HTTP server is up — this makes it an
  // explicit row) and the deep probe.
  const load = () => {
    let cancelled = false
    setLoading(true)
    setError('')
    getHealth()
      .then(() => {
        if (!cancelled) setLiveness('ok')
      })
      .catch(() => {
        if (!cancelled) setLiveness('fail')
      })
    getDeepHealth()
      .then((h) => {
        if (!cancelled) setHealth(h)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to run health checks'
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }

  useEffect(load, []) // eslint-disable-line react-hooks/exhaustive-deps

  const checks: HealthCheck[] = [
    // The backend row is client-side truth: this page loading over HTTP is
    // the probe. The server's deep endpoint cannot probe itself usefully.
    { name: 'md-builder backend', status: liveness, durationMillis: 0 },
    ...(health?.checks ?? []),
  ]

  return (
    <div>
      <h2 className="task-title">
        Site health
        {loading && (
          <LoaderCircle size={16} className="spin" style={{ verticalAlign: '-2px', marginLeft: '0.5rem' }} />
        )}
        {!loading && !error && (
          <button
            type="button"
            className="note-expand"
            onClick={load}
            title="Re-run the checks"
            style={{ marginLeft: '0.5rem', verticalAlign: '-2px' }}
          >
            <RefreshCw size={14} />
          </button>
        )}
      </h2>
      <p className="text-muted" style={{ marginTop: 0 }}>
        External dependencies of this md-builder instance, probed on demand.
        {health?.checkedAt && <> Last checked {formatTime(health.checkedAt)}.</>}
      </p>

      {error && <div className="alert alert-danger">{error}</div>}

      <table className="table">
        <thead>
          <tr>
            <th>Check</th>
            <th>Status</th>
            <th>Target</th>
            <th>Detail</th>
            <th>Took</th>
          </tr>
        </thead>
        <tbody>
          {checks.map((c) => (
            <tr key={c.name}>
              <td>{c.name}</td>
              <td>
                <StatusBadge status={c.status} />
              </td>
              <td className="text-muted">
                {c.target ? <code>{c.target}</code> : '—'}
              </td>
              <td className="text-muted">{c.detail ?? '—'}</td>
              <td className="text-muted">
                {c.durationMillis > 0 ? `${(c.durationMillis / 1000).toFixed(1)}s` : '—'}
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {health?.version && (
        <p className="text-muted">
          Server version <code>{health.version}</code>
        </p>
      )}
    </div>
  )
}

// StatusBadge renders a check outcome the way run statuses are colored:
// ok = green ✓, fail = red ✗, skipped = muted (not applicable).
function StatusBadge({ status }: { status: HealthCheck['status'] }) {
  if (status === 'ok') return <span className="text-success">✓ ok</span>
  if (status === 'fail') return <span className="text-danger">✗ fail</span>
  return <span className="text-muted">— skipped</span>
}
