import { useCallback, useEffect, useState } from 'react'
import {
  getDashboard,
  getFullDashboard,
  type Dashboard,
  type DashboardCommit,
  type DashboardKind,
  type FullDashboard,
  type FullStage,
  type RunCell,
} from './api'
import { StageStatus, commitUrl, truncate } from './StatusViews'

interface Props {
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
  onError: (message: string) => void
}

// DashboardPage renders the test result matrix, one row per commit: the
// commit column (linked short sha, author, push date, message) followed by
// one column per environment. The full view shows the build/unit/regression
// stages per environment; the single kinds show just that stage.
export default function DashboardPage({ onOpenRun, onOpenTask, onError }: Props) {
  // "full" is the first tab: the complete per-commit, per-environment view
  // of every pipeline stage. Single kinds follow: build, unit, regression.
  const [kind, setKind] = useState<DashboardKind>('full')
  const [dash, setDash] = useState<Dashboard | null>(null)
  const [full, setFull] = useState<FullDashboard | null>(null)
  const [error, setError] = useState('')
  const [loadedKind, setLoadedKind] = useState<DashboardKind | null>(null)

  // While data for the selected kind is in flight, "loading" is derived —
  // no synchronous setState inside the effect.
  const effectiveKind = loadedKind ?? kind

  const refresh = useCallback(
    async (k: DashboardKind) => {
      try {
        if (k === 'full') {
          const d = await getFullDashboard()
          setFull(d)
          setDash(null)
        } else {
          const d = await getDashboard(k)
          setDash(d)
          setFull(null)
        }
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

  const tab = (k: DashboardKind, label: string) => (
    <button
      type="button"
      className={'btn btn-sm dash-tab' + (kind === k ? ' dash-tab-active' : '')}
      role="tab"
      aria-selected={kind === k}
      onClick={() => setKind(k)}
    >
      {label}
    </button>
  )

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
          {tab('full', 'All')}
          {tab('build', 'Build')}
          {tab('unit', 'Unit tests')}
          {tab('regression', 'Regression tests')}
        </div>
      </div>

      <p className="text-muted">
        One row per recent git push; one column per environment with its
        build, unit and regression stages. Click a stage for details, the
        commit for the repository, or a graph link for the task pipeline.
      </p>

      {error && <div className="alert alert-danger">{error}</div>}
      {effectiveKind !== kind && <p className="text-muted">Loading…</p>}

      {effectiveKind === kind && kind === 'full' && full && (
        <MatrixTable
          environments={full.environments}
          rows={full.rows.map((r) => ({
            commit: r.commit,
            cells: full.environments.map((env) => {
              const stages = r.stages[String(env.id)] ?? []
              const taskId = r.taskIds[String(env.id)]
              return { kind: 'full', stages, taskId } as MatrixCell
            }),
          }))}
          onOpenRun={onOpenRun}
          onOpenTask={onOpenTask}
        />
      )}
      {effectiveKind === kind && kind !== 'full' && dash && (
        <MatrixTable
          environments={dash.environments}
          rows={dash.rows.map((r) => ({
            commit: r.commit,
            cells: r.cells.map(
              (cell) => ({ kind: 'single', cell, taskId: cell?.taskId }) as MatrixCell,
            ),
          }))}
          kind={kind}
          onOpenRun={onOpenRun}
          onOpenTask={onOpenTask}
        />
      )}
    </div>
  )
}

// Normalized row shape shared by the full and single-kind matrices: one
// entry per (commit, environment) holding either a recorded run cell (with
// the single-kind's counts) or the environment's live stage list.
type MatrixCell =
  | { kind: 'single'; cell: RunCell | null; taskId?: number }
  | { kind: 'full'; stages: FullStage[]; taskId?: number }

interface MatrixRow {
  commit: DashboardCommit
  cells: MatrixCell[]
}

// MatrixTable renders the shared compact matrix: one row per commit — the
// commit column (linked sha, author, date, message) then one column per
// environment with plain-text stage statuses.
function MatrixTable({
  environments,
  rows,
  kind,
  onOpenRun,
  onOpenTask,
}: {
  environments: { id: number; name: string; description: string; tags: string; enabled: boolean }[]
  rows: MatrixRow[]
  kind?: DashboardKind
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
}) {
  if (environments.length === 0) {
    return (
      <div className="event">
        <p className="text-muted" style={{ margin: 0 }}>
          No test environments yet. Create one in the user center first —
          each environment becomes a column of the matrix.
        </p>
      </div>
    )
  }
  if (rows.length === 0) {
    return (
      <div className="event">
        <p className="text-muted" style={{ margin: 0 }}>
          No git pushes recorded yet. Point a GitLab push webhook at{' '}
          <code>/api/webhooks/gitlab</code> (see Settings) — each push to the
          code repository becomes a row of the matrix.
        </p>
      </div>
    )
  }

  return (
    <div className="dash-scroll">
      <table className="table dash-matrix">
        <thead>
          <tr>
            <th className="dash-corner">commit</th>
            {environments.map((env) => (
              <th
                key={env.id}
                className={'dash-env-head' + (env.enabled ? '' : ' dash-row-disabled')}
                title={env.description}
              >
                {env.name}
                {!env.enabled && <span className="text-muted"> (off)</span>}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.commit.id}>
              <td className="dash-commit-cell">
                <CommitCell commit={row.commit} />
              </td>
              {row.cells.map((cell, i) => (
                <td key={environments[i].id} className="dash-cell">
                  {cell.kind === 'full' ? (
                    <FullCell
                      cell={cell}
                      onOpenRun={onOpenRun}
                      onOpenTask={onOpenTask}
                    />
                  ) : (
                    <SingleCell
                      cell={cell.cell}
                      taskId={cell.taskId}
                      kind={kind}
                      onOpenRun={onOpenRun}
                      onOpenTask={onOpenTask}
                    />
                  )}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// CommitCell is the matrix row header: the short sha linking to the commit
// on the repository host, the author (truncated), the push date in gray and
// the commit message (truncated, gray).
function CommitCell({ commit }: { commit: DashboardCommit }) {
  const url = commitUrl(commit.repoUrl, commit.sha)
  return (
    <div className="dash-commit-cell" title={`${commit.repo} ${commit.sha}`}>
      <span className="dash-commit-line1">
        {url ? (
          <a href={url} target="_blank" rel="noreferrer" className="dash-sha">
            {commit.shortSha}
          </a>
        ) : (
          <span className="dash-sha">{commit.shortSha}</span>
        )}
        <span className="text-muted" title={commit.author}>
          {truncate(commit.author, 16)}
        </span>
        <span className="text-muted dash-commit-date">{commit.pushedAt.slice(0, 10)}</span>
      </span>
      {commit.message && (
        <span className="dash-commit-msg text-muted" title={commit.message}>
          {commit.message}
        </span>
      )}
    </div>
  )
}

// SingleCell renders one (commit, environment) cell of the single-kind
// matrices: a plain-text stage status linked to the run or task details, or
// an em dash when neither exists.
function SingleCell({
  cell,
  taskId,
  kind,
  onOpenRun,
  onOpenTask,
}: {
  cell: RunCell | null
  taskId?: number
  kind?: DashboardKind
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
}) {
  if (!cell) {
    return (
      <span className="text-muted dash-no-run" title="No test run recorded">
        —
      </span>
    )
  }
  const onClick =
    cell.runId > 0
      ? () => onOpenRun(cell.runId)
      : taskId
        ? () => onOpenTask(taskId)
        : undefined
  let status = cell.status
  let title = ''
  if (cell.runId > 0) {
    const label = kind === 'build' ? 'build' : kind === 'unit' ? 'unit' : 'regression'
    title =
      status === 'failed'
        ? (cell.error || `${label} failed`) + ' — click for details'
        : `${cell.passed}/${cell.total} ${label} passed${
            cell.failed > 0 ? `, ${cell.failed} failed` : ''
          } — click for details`
  } else {
    title =
      (cell.error ||
        (status === 'pending' ? 'Task queued — click for details' : 'Task running — click to follow the log'))
  }
  return <StageStatus status={status} onClick={onClick} title={title} />
}

// FullCell renders one (commit, environment) cell of the full matrix: the
// build/unit/regression stage statuses side by side (plain text, no
// separators) plus the graph link to the task pipeline.
function FullCell({
  cell,
  onOpenRun,
  onOpenTask,
}: {
  cell: { stages: FullStage[]; taskId?: number }
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
}) {
  if (cell.stages.length === 0 && !cell.taskId) {
    return (
      <span className="text-muted dash-no-run" title="No test run recorded">
        —
      </span>
    )
  }
  return (
    <span className="dash-full-cell">
      {cell.stages.map((st) => (
        <FullStageView
          key={st.kind}
          stage={st}
          onOpenRun={onOpenRun}
          onOpenStage={onOpenTask}
        />
      ))}
      {cell.taskId ? (
        <a
          href="#"
          className="dash-full-graph"
          title="Open the task dependency graph"
          onClick={(e) => {
            e.preventDefault()
            onOpenTask(cell.taskId as number)
          }}
        >
          graph
        </a>
      ) : null}
    </span>
  )
}

// FullStageView renders one stage of the full matrix as plain colored text:
// "✓ ok" / "✗ fail" / "⤼ skip" / spinner "run" / gray "pending", clickable
// to the run or task details when there is something to open.
function FullStageView({
  stage,
  onOpenRun,
  onOpenStage,
}: {
  stage: FullStage
  onOpenRun: (runId: number) => void
  onOpenStage: (taskId: number) => void
}) {
  const onClick =
    stage.runId > 0
      ? () => onOpenRun(stage.runId)
      : stage.taskId
        ? () => onOpenStage(stage.taskId as number)
        : undefined
  const title =
    (stage.error || stage.summary || `${stage.kind}: ${stage.status}`).slice(0, 200) +
    (onClick ? ' — click for details' : '')
  return (
    <span className="dash-full-stage">
      <span className="dash-full-kind">{stage.kind === 'regression' ? 'reg' : stage.kind}</span>
      <StageStatus status={stage.status} onClick={onClick} title={title} />
    </span>
  )
}
