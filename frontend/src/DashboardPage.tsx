import { Fragment, useCallback, useEffect, useState } from 'react'
import { useNavigate } from 'react-router'
import { Network } from 'lucide-react'
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
import { formatTimeShort } from './timezone'

interface Props {
  onError: (message: string) => void
}

// DashboardPage renders the test result matrix, one row per commit: the
// commit column (linked short sha, author, push date, message) followed by
// one column per environment. The full view gives each environment four
// sub-columns (build, unit, reg, graph); the single kinds show one stage.
export default function DashboardPage({ onError }: Props) {
  // Stage cells navigate with plain router navigation (run detail when a
  // run is recorded, the task graph while the pipeline is live).
  const navigate = useNavigate()
  const onOpenRun = (runId: number) => navigate(`/runs/${runId}`)
  const onOpenTask = (taskId: number) => navigate(`/tasks/${taskId}`)
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

  // Both dashboard payloads carry repoFilter: the commit repo the matrix is
  // scoped to (empty = no site codeRepo configured → all repos). Shown in the
  // header so a switched code repo explains "missing" seed rows; repoUrl (the
  // repo's web URL, when derivable) turns it into a link to the repo page.
  const repoFilter = (full ?? dash)?.repoFilter ?? ''
  const repoUrl = (full ?? dash)?.repoUrl ?? ''

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
        <h2 style={{ marginBottom: 0 }}>
          Test dashboard
          {repoFilter &&
            (repoUrl ? (
              <a
                href={repoUrl}
                target="_blank"
                rel="noreferrer"
                className="text-muted"
                style={{ marginLeft: '0.5rem', fontSize: '0.875rem', fontWeight: 400 }}
                title="Open the repository page"
              >
                {repoFilter}
              </a>
            ) : (
              <span className="text-muted" style={{ marginLeft: '0.5rem', fontSize: '0.875rem', fontWeight: 400 }}>
                {repoFilter}
              </span>
            ))}
        </h2>
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
        commit for the repository, or a graph link for the pipeline (graph
        and live logs on one page).
      </p>

      {error && <div className="alert alert-danger">{error}</div>}
      {effectiveKind !== kind && <p className="text-muted">Loading…</p>}

      {effectiveKind === kind && kind === 'full' && full && (
        <MatrixTable
          environments={full.environments}
          kind={kind}
          rows={full.rows.map((r) => ({
            commit: r.commit,
            cells: full.environments.map((env) => {
              const stages = r.stages[String(env.id)] ?? []
              const taskId = r.taskIds[String(env.id)]
              const trigger = r.triggers?.[String(env.id)]
              return { kind: 'full' as const, stages, taskId, trigger }
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
              (cell) => ({ kind: 'single' as const, cell, taskId: cell?.taskId }),
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
// entry per (commit, environment) holding either the environment's stage
// list (full view) or the single-kind's run cell. trigger is the root
// graph's origin (1 = manually dispatched from the UI).
type MatrixCell =
  | { kind: 'single'; cell: RunCell | null; taskId?: number }
  | { kind: 'full'; stages: FullStage[]; taskId?: number; trigger?: number }

interface MatrixRow {
  commit: DashboardCommit
  cells: MatrixCell[]
}

// MatrixTable renders the shared compact matrix: one row per commit — the
// commit column (linked sha, author, date, gray message) then one column
// per environment. The full view splits each environment into four
// sub-columns (build / unit / reg / graph) under a two-row header; the
// single kinds show just that stage's status.
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
          No test environments yet. Create one in the Runner Envs page first —
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

  const isFull = kind === 'full'

  return (
    <div className="dash-scroll">
      <table className="table dash-matrix">
        <thead>
          {/* Row 1: the environment name + tags, spanning its sub-columns. */}
          <tr>
            <th className="dash-corner" rowSpan={isFull ? 2 : 1}>
              commit
            </th>
            {environments.map((env) => (
              <th
                key={env.id}
                colSpan={isFull ? 4 : 1}
                className={'dash-env-head' + (env.enabled ? '' : ' dash-row-disabled')}
                title={env.description}
              >
                {env.name}
                {!env.enabled && <span className="text-muted"> (off)</span>}
                {env.tags && <span className="dash-env-tags">{env.tags}</span>}
              </th>
            ))}
          </tr>
          {/* Row 2 (full view only): the build / unit / reg / graph labels. */}
          {isFull && (
            <tr className="dash-subhead">
              {environments.map((env) => (
                <Fragment key={env.id}>
                  <th className="dash-stage-head dash-env-start">build</th>
                  <th className="dash-stage-head">unit test</th>
                  <th className="dash-stage-head">reg test</th>
                  <th className="dash-stage-head">graph</th>
                </Fragment>
              ))}
            </tr>
          )}
        </thead>
        <tbody>
          {rows.map((row, ri) => (
            <tr
              key={row.commit.id}
              className={
                row.commit.superseded
                  ? 'dash-row-superseded'
                  : ri % 2 === 1
                    ? 'dash-row-alt'
                    : undefined
              }
            >
              <td className="dash-commit-cell">
                <CommitCell commit={row.commit} />
              </td>
              {row.cells.map((cell, i) => (
                <Fragment key={environments[i].id}>
                  {cell.kind === 'full' ? (
                    <>
                      <td className="dash-cell dash-env-start">
                        <SingleCell
                          cell={stageToRunCell(cell.stages.find((s) => s.kind === 'build'))}
                          onOpenRun={onOpenRun}
                          onOpenTask={onOpenTask}
                          fallbackTaskId={cell.taskId}
                        />
                      </td>
                      <td className="dash-cell">
                        <SingleCell
                          cell={stageToRunCell(cell.stages.find((s) => s.kind === 'unit'))}
                          onOpenRun={onOpenRun}
                          onOpenTask={onOpenTask}
                          fallbackTaskId={cell.taskId}
                        />
                      </td>
                      <td className="dash-cell">
                        <SingleCell
                          cell={stageToRunCell(cell.stages.find((s) => s.kind === 'regression'))}
                          onOpenRun={onOpenRun}
                          onOpenTask={onOpenTask}
                          fallbackTaskId={cell.taskId}
                        />
                      </td>
                      <td className="dash-cell">
                        {cell.taskId ? (
                          <>
                            <a
                              href="#"
                              className="dash-full-graph"
                              title="Open the task dependency graph"
                              onClick={(e) => {
                                e.preventDefault()
                                onOpenTask(cell.taskId as number)
                              }}
                            >
                              <Network size={14} aria-label="graph" />
                            </a>
                            {cell.trigger !== undefined && cell.trigger > 0 && (
                              <span className="dash-trigger" title="manually triggered">
                                M
                              </span>
                            )}
                          </>
                        ) : (
                          <span className="text-muted">—</span>
                        )}
                      </td>
                    </>
                  ) : (
                    <td className="dash-cell dash-env-start">
                      <SingleCell
                        cell={cell.cell}
                        onOpenRun={onOpenRun}
                        onOpenTask={onOpenTask}
                        showCounts={kind === 'unit' || kind === 'regression'}
                      />
                    </td>
                  )}
                </Fragment>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// stageToRunCell adapts a full-matrix stage to the RunCell shape the
// SingleCell renderer expects. The full view does not show counts, so the
// totals stay zero; the stage's taskId is carried for the live link.
function stageToRunCell(st: FullStage | undefined): RunCell | null {
  if (!st) return null
  return {
    runId: st.runId,
    taskId: st.taskId,
    status: st.status as RunCell['status'],
    total: 0,
    passed: 0,
    failed: 0,
    startedAt: st.startedAt,
    finishedAt: st.finishedAt,
    error: st.error,
  }
}

// commitEventLabel maps a commit row's event kind to a short badge. Rows
// recorded before the event column existed (or plain pushes) show nothing.
const commitEventLabels: Record<string, { label: string; title: string }> = {
  tag_push: { label: 'tag', title: 'triggered by a tag push event' },
  merge_request: { label: 'MR', title: 'triggered by a merge request event' },
  manual: { label: 'M', title: 'manually dispatched test' },
  manual_yaml: { label: 'M', title: 'manually dispatched yaml matrix' },
}

// CommitCell is the matrix row header: one line with the short sha (bold,
// linked to the commit on the repository host), author, push date — in the
// normal text color — and the gray commit message, with gaps between the
// parts; the line truncates with an ellipsis when too wide.
function CommitCell({ commit }: { commit: DashboardCommit }) {
  const url = commitUrl(commit.repoUrl, commit.sha)
  const ev = commit.event ? commitEventLabels[commit.event] : undefined
  return (
    <div className="dash-commit-cell" title={`${commit.repo} ${commit.sha} — ${commit.message}`}>
      {url ? (
        <a href={url} target="_blank" rel="noreferrer" className="dash-sha">
          {commit.shortSha}
        </a>
      ) : (
        <span className="dash-sha">{commit.shortSha}</span>
      )}
      <span className="dash-commit-author" title={commit.author}>
        {truncate(commit.author, 16)}
      </span>
      <span className="dash-commit-date">{formatTimeShort(commit.pushedAt)}</span>
      {commit.message && <span className="dash-commit-msg">{commit.message}</span>}
      {ev && (
        <span className="dash-event" title={ev.title}>
          {ev.label}
        </span>
      )}
      {commit.superseded && (
        <span className="dash-superseded" title="a newer attempt of this commit exists">
          superseded
        </span>
      )}
    </div>
  )
}

// SingleCell renders one matrix cell: a plain-text stage status linked to
// the run or task details, or an em dash when neither exists. With
// showCounts (the unit and regression dashboards) a finished run shows its
// passed/total counts whether it passed or failed; the full view keeps the
// plain ok/fail words.
function SingleCell({
  cell,
  onOpenRun,
  onOpenTask,
  fallbackTaskId,
  showCounts,
}: {
  cell: RunCell | null
  onOpenRun: (runId: number) => void
  onOpenTask: (taskId: number) => void
  // When the cell has no stage task of its own (live stages carry one in
  // the full view), fall back to the graph's root task.
  fallbackTaskId?: number
  showCounts?: boolean
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
      : cell.taskId || fallbackTaskId
        ? () => onOpenTask((cell.taskId ?? fallbackTaskId) as number)
        : undefined
  const status = cell.status
  const label =
    showCounts && (status === 'passed' || status === 'failed')
      ? `${cell.passed}/${cell.total}`
      : undefined
  const title = cell.runId
    ? (cell.error ||
        (status === 'pending'
          ? 'queued — the stage has not reported yet'
          : status === 'running'
            ? 'running — following the stage live'
            : `${cell.passed}/${cell.total} passed`)) + ' — click for details'
    : cell.error ||
      (status === 'pending'
        ? 'Task queued — click for details'
        : 'Task running — click to follow the log')
  return (
    <>
      <StageStatus status={status} label={label} onClick={onClick} title={title} />
      {cell.trigger !== undefined && cell.trigger > 0 && (
        <span className="dash-trigger" title="manually triggered">
          M
        </span>
      )}
    </>
  )
}
