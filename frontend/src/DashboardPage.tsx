import { useCallback, useEffect, useState } from 'react'
import {
  getDashboard,
  type Dashboard,
  type DashboardKind,
  type RunCell,
} from './api'

interface Props {
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
  onError: (message: string) => void
}

// DashboardPage renders the test result matrix: one row per test
// environment, one column per recent git push (commit). Regression mode
// shows pass/fail counts per run; unit mode shows passed/total.
export default function DashboardPage({ onOpenRun, onOpenTask, onError }: Props) {
  const [kind, setKind] = useState<DashboardKind>('regression')
  const [dash, setDash] = useState<Dashboard | null>(null)
  const [error, setError] = useState('')
  const [loadedKind, setLoadedKind] = useState<DashboardKind | null>(null)

  // While data for the selected kind is in flight, "loading" is derived —
  // no synchronous setState inside the effect.
  const effectiveKind = loadedKind ?? kind

  const refresh = useCallback(
    async (k: DashboardKind) => {
      try {
        const d = await getDashboard(k)
        setDash(d)
        setLoadedKind(k)
        setError('')
      } catch (err) {
        const msg = err instanceof Error ? err.message : 'Failed to load'
        setError(msg)
        setLoadedKind(k)
        onError(msg)
      }
    },
    [onError],
  )

  useEffect(() => {
    refresh(kind)
  }, [kind, refresh])

  return (
    <div>
      <div
        style={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'baseline',
          flexWrap: 'wrap',
          gap: '0.5rem',
        }}
      >
        <h2 style={{ marginBottom: 0 }}>Test dashboard</h2>
        <div className="dash-tabs" role="tablist">
          <button
            type="button"
            className={'btn btn-sm dash-tab' + (kind === 'regression' ? ' dash-tab-active' : '')}
            role="tab"
            aria-selected={kind === 'regression'}
            onClick={() => setKind('regression')}
          >
            Regression tests
          </button>
          <button
            type="button"
            className={'btn btn-sm dash-tab' + (kind === 'unit' ? ' dash-tab-active' : '')}
            role="tab"
            aria-selected={kind === 'unit'}
            onClick={() => setKind('unit')}
          >
            Unit tests
          </button>
        </div>
      </div>

      <p className="text-muted">
        {kind === 'regression'
          ? 'Regression test results from the test input repository: one row per recent git push (commit), one column per test environment. Click a result for details.'
          : 'Unit tests: one row per recent git push (commit), one column per test environment. Click a result for details.'}
      </p>

      {error && <div className="alert alert-danger">{error}</div>}
      {effectiveKind !== kind && <p className="text-muted">Loading…</p>}

      {effectiveKind === kind && dash && (
        <DashboardMatrix
          dash={dash}
          kind={kind}
          onOpenRun={onOpenRun}
          onOpenTask={onOpenTask}
        />
      )}
    </div>
  )
}

// DashboardMatrix renders the commits × environments table: each row is a
// commit, each column an environment.
function DashboardMatrix({
  dash,
  kind,
  onOpenRun,
  onOpenTask,
}: {
  dash: Dashboard
  kind: DashboardKind
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
}) {
  if (dash.environments.length === 0) {
    return (
      <div className="event">
        <p className="text-muted" style={{ margin: 0 }}>
          No test environments yet. Create one in the user center first —
          each environment becomes a column of the matrix.
        </p>
      </div>
    )
  }
  if (dash.rows.length === 0) {
    return (
      <div className="event">
        <p className="text-muted" style={{ margin: 0 }}>
          No git pushes recorded yet. Point a GitLab push webhook at{' '}
          <code>/api/webhooks/gitlab</code> (see Settings) — each push to the
          code repository becomes a row of the matrix.
          {dash.repoFilter && (
            <>
              {' '}
              Pushes are currently filtered to <code>{dash.repoFilter}</code>.
            </>
          )}
        </p>
      </div>
    )
  }

  return (
    <div className="dash-scroll">
      <table className="table dash-matrix">
        <thead>
          <tr>
            <th className="dash-corner">Commit \ Environment</th>
            {dash.environments.map((env) => (
              <th
                key={env.id}
                className={'dash-env-head' + (env.enabled ? '' : ' dash-row-disabled')}
                title={env.description}
              >
                {env.name}
                {!env.enabled && <span className="text-muted"> (off)</span>}
                {env.tags && (
                  <span className="dash-env-tags text-muted">{env.tags}</span>
                )}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {dash.rows.map((row) => (
            <tr key={row.commit.id}>
              <th className="dash-commit-cell" title={`${row.commit.repo} ${row.commit.sha}`}>
                <span className="dash-sha">{row.commit.shortSha}</span>
                <span className="dash-commit-meta text-muted">
                  {row.commit.author} · {formatDay(row.commit.pushedAt)}
                </span>
                {row.commit.message && (
                  <span className="dash-commit-msg text-muted" title={row.commit.message}>
                    {row.commit.message}
                  </span>
                )}
              </th>
              {row.cells.map((cell, i) => (
                <td key={i} className="dash-cell">
                  <RunCellView
                    cell={cell}
                    kind={kind}
                    onOpen={
                      cell
                        ? cell.runId > 0
                          ? () => onOpenRun(cell.runId)
                          : cell.taskId
                            ? () => onOpenTask(cell.taskId as number)
                            : undefined
                        : undefined
                    }
                  />
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// RunCellView renders one matrix cell: a clickable summary of a recorded
// run, a live task-graph state (running…/queued/failed before reporting —
// clickable through to the task detail), or an em dash when neither exists
// for that (commit, environment).
function RunCellView({
  cell,
  kind,
  onOpen,
}: {
  cell: RunCell | null
  kind: DashboardKind
  onOpen: (() => void) | undefined
}) {
  if (!cell) {
    return (
      <span className="text-muted dash-no-run" title="No test run recorded">
        —
      </span>
    )
  }
  // Live task overlay (runId 0): the run has not been reported yet.
  if (cell.runId === 0) {
    if (cell.status === 'running') {
      return (
        <button
          type="button"
          className="btn btn-sm dash-run dash-run-live"
          title="Task graph running — click to follow the log"
          onClick={onOpen}
        >
          running…
        </button>
      )
    }
    if (cell.status === 'pending') {
      return (
        <button
          type="button"
          className="btn btn-sm dash-run dash-run-live"
          title="Task queued — click for details"
          onClick={onOpen}
        >
          queued
        </button>
      )
    }
    return (
      <button
        type="button"
        className="btn btn-sm dash-run dash-run-failed"
        title={(cell.error || 'Task failed before reporting a run') + ' — click for details'}
        onClick={onOpen}
      >
        ✗
      </button>
    )
  }
  const failed = cell.status === 'failed'
  const label =
    kind === 'regression'
      ? `${failed ? '✗' : '✓'} ${cell.passed}/${cell.total}`
      : `${cell.passed}/${cell.total}`
  const title = `${cell.passed}/${cell.total} passed${
    cell.failed > 0 ? `, ${cell.failed} failed` : ''
  } — click for details`
  return (
    <button
      type="button"
      className={
        'btn btn-sm dash-run' + (failed ? ' dash-run-failed' : ' dash-run-passed')
      }
      title={title}
      onClick={onOpen}
    >
      {label}
    </button>
  )
}

// formatDay renders YYYY-MM-DD from an RFC3339 timestamp.
function formatDay(ts: string): string {
  return ts.slice(0, 10)
}
