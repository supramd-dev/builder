import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { LoaderCircle } from 'lucide-react'
import { getTask, type SubTask, type TaskDetail } from './api'
import { TaskStatusText, commitUrl } from './StatusViews'
import { Breadcrumbs } from './Breadcrumbs'
import TaskLogView from './TaskLogView'

// NODE_W/NODE_H size the graph nodes; GAP_X/GAP_Y the layer spacing.
const NODE_W = 180
const NODE_H = 44
const GAP_X = 56
const GAP_Y = 22

// TaskPipelinePage shows one pipeline as two linked views: the dependency
// graph on top, the pipeline step list (with inline expandable stage logs)
// below it. A graph node with a recorded run opens the run's detail page; a
// node without one scrolls to the step list and expands that stage's log.
// Clicking a step toggles its log in place; statuses follow the live
// pipeline — getTask is polled (3s) only while the graph is pending/running.
// The requested id may be a root or a sub-task (legacy deep links); sub-task
// ids resolve to their root and preselect that sub-task's log.
export default function TaskPipelinePage() {
  const params = useParams()
  const navigate = useNavigate()
  const urlId = Number(params.taskId)
  const [root, setRoot] = useState<TaskDetail | null>(null)
  const [error, setError] = useState('')
  // Which step's log is expanded; null = none.
  const [expanded, setExpanded] = useState<number | null>(null)
  // A sub-task id seen in the URL (e.g. /tasks/64) that wins the first
  // expansion once its graph arrives.
  const wantedRef = useRef<number | null>(null)
  // Whether the automatic first expansion has happened — later polls must
  // not re-expand a step the user collapsed.
  const autoSelectedRef = useRef(false)

  useEffect(() => {
    let live = true
    let timer: ReturnType<typeof setInterval> | undefined
    setError('')
    async function poll() {
      try {
        let t = await getTask(urlId)
        if (!live) return
        // A sub-task id was addressed: forward to its root's graph and
        // remember the step to expand.
        if (t.kind !== 'root' && t.rootId > 0 && t.rootId !== t.id) {
          wantedRef.current = t.id
          t = await getTask(t.rootId)
          if (!live) return
        }
        setRoot(t)
        // Automatic first expansion: the URL-requested sub-task when
        // present, else the first running step (or the last one) — where
        // the pipeline currently is.
        setExpanded((cur) => {
          if (cur !== null || autoSelectedRef.current) return cur
          const subs = t.subTasks ?? []
          let pick: number | null = null
          if (wantedRef.current !== null && subs.some((s) => s.id === wantedRef.current)) {
            pick = wantedRef.current
          } else if (subs.length > 0) {
            const active = subs.find((s) => s.status === 'running')
            pick = (active ?? subs[subs.length - 1]).id
          }
          if (pick !== null) autoSelectedRef.current = true
          return pick
        })
        // Follow the pipeline only while it is live; a terminal graph stops
        // the loop (a finished page costs two requests in total).
        if (t.status === 'pending' || t.status === 'running') {
          if (!timer) timer = setInterval(poll, 3000)
        } else if (timer) {
          clearInterval(timer)
          timer = undefined
        }
      } catch (err) {
        if (live) {
          setError(err instanceof Error ? err.message : 'Failed to load task')
        }
      }
    }
    poll()
    return () => {
      live = false
      if (timer) clearInterval(timer)
    }
  }, [urlId])

  if (error) {
    return (
      <div>
        <Breadcrumbs trail={[{ label: 'Dashboard', to: '/' }, { label: `Task #${urlId}` }]} />
        <div className="alert alert-danger">{error}</div>
      </div>
    )
  }
  if (!root) {
    return (
      <div>
        <Breadcrumbs trail={[{ label: 'Dashboard', to: '/' }, { label: `Task #${urlId}` }]} />
        <p className="text-muted">Loading…</p>
      </div>
    )
  }

  const subs = root.subTasks ?? []
  const live = root.status === 'pending' || root.status === 'running'

  return (
    <div>
      <TaskHeader task={root} />
      {subs.length > 0 ? (
        <>
          <GraphCanvas
            task={root}
            subs={subs}
            expanded={expanded}
            onNodeClick={(sub) => {
              // A recorded run owns the click: straight to the run's
              // detail page. Otherwise select the step: expand its log
              // in the pipeline list and scroll it into view.
              if (sub.runId) {
                navigate(`/runs/${sub.runId}`)
                return
              }
              setExpanded(sub.id)
              requestAnimationFrame(() => {
                document
                  .getElementById(`pipeline-step-${sub.id}`)
                  ?.scrollIntoView({ behavior: 'smooth', block: 'center' })
              })
            }}
          />
          <p className="text-muted graph-hint">
            Click a stage with a recorded run to open its test details;
            other nodes expand the stage's log in the pipeline below.
          </p>
          <PipelineSection
            subs={subs}
            live={live}
            expanded={expanded}
            onToggle={(id) => setExpanded((cur) => (cur === id ? null : id))}
          />
        </>
      ) : (
        <p className="text-muted">
          This task has no pipeline stages (it was not dispatched as a
          build-and-test graph).
        </p>
      )}
    </div>
  )
}

// TaskHeader renders the title (task number, commit, environment) and the
// summary line (author, ref, tags, status).
function TaskHeader({ task }: { task: TaskDetail }) {
  return (
    <div>
      <Breadcrumbs
        trail={[{ label: 'Dashboard', to: '/' }, { label: `Task #${task.id}` }]}
      />
      <h2 className="task-title">
        Task #{task.id}
        {task.commit && (
          <>
            {' · '}
            {commitUrl(task.commit.repoUrl, task.commit.sha) ? (
              <a
                href={commitUrl(task.commit.repoUrl, task.commit.sha)}
                target="_blank"
                rel="noreferrer"
              >
                <code>{task.commit.shortSha}</code>
              </a>
            ) : (
              <code>{task.commit.shortSha}</code>
            )}
            {task.commit.message && <span className="text-muted"> — {task.commit.message}</span>}
          </>
        )}
        {task.environment && <span className="text-muted"> on {task.environment.name}</span>}
      </h2>
      <div className="task-summary text-muted">
        {task.commit && (
          <>
            {task.commit.author} pushed to <code>{task.commit.ref}</code> ·{' '}
          </>
        )}
        {task.environment && (
          <>
            {task.environment.name}
            {task.tags && <> · tags: {task.tags}</>}
          </>
        )}
        {' · '}
        <TaskStatusText status={task.status} />
        {(task.trigger ?? 0) > 0 && (
          <span className="dash-trigger" title="manually triggered from the UI">
            manual
          </span>
        )}
      </div>
      {task.error && <p className="task-error">{task.error}</p>}
    </div>
  )
}

// GraphCanvas draws the dependency graph. Nodes with a recorded run open
// the run detail page (onNodeClick); the rest expand their step's log in
// the pipeline list below. The root node is not clickable.
function GraphCanvas({
  task,
  subs,
  expanded,
  onNodeClick,
}: {
  task: TaskDetail
  subs: SubTask[]
  expanded: number | null
  onNodeClick: (sub: SubTask) => void
}) {
  const root = task.kind === 'root' ? task : null
  const layers = layerGraph(subs)
  const pos = positions(layers, root)

  const rows: { id: number | 'root'; node: TaskDetail | SubTask }[] = []
  if (root) rows.push({ id: 'root', node: root })
  for (const sub of subs) rows.push({ id: sub.id, node: sub })

  const width = Math.max(1, layers.length + (root ? 1 : 0))
  const maxRows = Math.max(1, ...layers.map((l) => l.length))
  const height = Math.max(1, maxRows)

  return (
    <div className="graph-scroll">
      <div
        className="graph-canvas"
        style={{
          width: width * (NODE_W + GAP_X),
          height: height * (NODE_H + GAP_Y) + GAP_Y,
        }}
      >
        <svg className="graph-edges" width="100%" height="100%">
          {edges(subs, root, pos).map((e, i) => (
            <path key={i} d={e.d} className={'graph-edge' + (e.done ? ' graph-edge-done' : '')} />
          ))}
        </svg>
        {rows.map(({ id, node }) => {
          const p = pos.get(id)
          if (!p) return null
          const isRoot = id === 'root'
          const sub = isRoot ? null : (node as SubTask)
          const status = node.status
          const cls = [
            'graph-node',
            isRoot ? 'graph-node-root' : '',
            !isRoot && expanded === id ? 'graph-node-selected' : '',
            'graph-node-' + status,
          ]
            .filter(Boolean)
            .join(' ')
          return (
            <button
              type="button"
              key={String(id)}
              className={cls}
              style={{ left: p.x, top: p.y, width: NODE_W, height: NODE_H }}
              onClick={sub ? () => onNodeClick(sub) : undefined}
              title={
                isRoot
                  ? 'Pipeline root'
                  : sub?.runId
                    ? 'Open the run details'
                    : "Expand the stage's log below"
              }
            >
              <span className="graph-node-head">
                <span className="graph-node-status">
                  {status === 'running' ? (
                    <LoaderCircle size={13} className="spin" />
                  ) : (
                    nodeGlyph(status)
                  )}
                </span>
                <span className="graph-node-name">{isRoot ? 'task' : node.name}</span>
                {(status === 'pending' || status === 'running') && (
                  <span className={'graph-node-state text-' + status}>{status}</span>
                )}
              </span>
            </button>
          )
        })}
      </div>
    </div>
  )
}

// PipelineSection is the step list at the bottom of the page. Clicking a
// step toggles its log inline (accordion — one log at a time); a step with
// a recorded run also links to the run's detail page in the log head.
function PipelineSection({
  subs,
  live,
  expanded,
  onToggle,
}: {
  subs: SubTask[]
  live: boolean
  expanded: number | null
  onToggle: (id: number) => void
}) {
  const selected = subs.find((s) => s.id === expanded)
  return (
    <section>
      <h3 className="task-section-title">Pipeline</h3>
      <ol className="task-steps">
        {subs.map((sub) => (
          <li key={sub.id} id={`pipeline-step-${sub.id}`}>
            <button
              type="button"
              className={
                'task-step' + (expanded === sub.id ? ' task-step-selected' : '')
              }
              onClick={() => onToggle(sub.id)}
              title={sub.error || sub.name}
            >
              <span className={'task-step-status ' + statusTextClass(sub.status)}>
                {sub.status === 'running' ? (
                  <LoaderCircle size={13} className="spin" />
                ) : (
                  nodeGlyph(sub.status)
                )}
              </span>
              <span className="task-step-name">{sub.name}</span>
              <span className="text-muted task-step-kind">{sub.kind}</span>
              <TaskStatusText status={sub.status} />
            </button>
          </li>
        ))}
      </ol>
      {/* The selected step's log sits below the whole step list (one shared
          viewer), not inline under each row. */}
      {selected && (
        <div className="pipeline-log">
          <div className="pipeline-log-head">
            <span className={'pipeline-log-status ' + statusTextClass(selected.status)}>
              {nodeGlyph(selected.status)}
            </span>
            <span className="pipeline-log-name">{selected.name}</span>
            <span className="text-muted pipeline-log-kind">{selected.kind}</span>
            <TaskStatusText status={selected.status} />
            {live && selected.status === 'running' && (
              <span className="text-muted pipeline-log-live">following output…</span>
            )}
            {selected.runId !== undefined && (
              <Link to={`/runs/${selected.runId}`} className="pipeline-run-link">
                Run details →
              </Link>
            )}
          </div>
          <TaskLogView taskId={selected.id} live={live} />
        </div>
      )}
    </section>
  )
}

// --- layout -----------------------------------------------------------------

// layerGraph assigns each sub-task a dependency layer (longest path from a
// source) and orders nodes within a layer by id, producing a left-to-right
// DAG layout: clone in layer 0, build in layer 1, tests in layer 2.
function layerGraph(subs: SubTask[]): SubTask[][] {
  const byId = new Map(subs.map((s) => [s.id, s]))
  const depth = new Map<number, number>()
  function d(s: SubTask): number {
    const cached = depth.get(s.id)
    if (cached !== undefined) return cached
    const deps = (s.dependsOn ?? []).filter((dep) => byId.has(dep))
    depth.set(s.id, 0) // cycle guard
    const v = deps.length === 0 ? 0 : 1 + Math.max(...deps.map((dep) => d(byId.get(dep)!)))
    depth.set(s.id, v)
    return v
  }
  for (const s of subs) d(s)

  const layers: SubTask[][] = []
  for (const s of subs) {
    const l = depth.get(s.id) ?? 0
    ;(layers[l] ??= []).push(s)
  }
  return layers.filter((l) => l && l.length > 0)
}

// positions computes each node's pixel position: x by layer, y centered
// within its layer.
function positions(
  layers: SubTask[][],
  root: TaskDetail | null,
): Map<number | 'root', { x: number; y: number }> {
  const pos = new Map<number | 'root', { x: number; y: number }>()
  // The root sits in a leftmost virtual column, on the same horizontal
  // line as the first node of the first layer (clone) — the pipeline's
  // entry point reads as one row.
  if (root) {
    pos.set('root', { x: 0, y: 0 })
  }
  const stride = NODE_H + GAP_Y
  // Sub-tasks start at x offset 1 column when the root column is present.
  layers.forEach((layer, li) => {
    layer.forEach((s, i) => {
      pos.set(s.id, { x: (li + (root ? 1 : 0)) * (NODE_W + GAP_X), y: i * stride })
    })
  })
  return pos
}

// edges builds the bezier path between every node and each of its
// dependencies (root node included as the leftmost virtual column). An edge
// is "done" once its source node finished, for the animated draw-in.
function edges(
  subs: SubTask[],
  root: TaskDetail | null,
  pos: Map<number | 'root', { x: number; y: number }>,
): { d: string; done: boolean }[] {
  const out: { d: string; done: boolean }[] = []
  const mid = NODE_W / 2
  // Root's position mirrors positions(): first row of the leftmost column.
  const rootPos = root ? { x: 0, y: 0 } : undefined
  for (const s of subs) {
    const to = pos.get(s.id)
    if (!to) continue
    for (const dep of s.dependsOn ?? []) {
      const isRootDep = dep === root?.id
      const from = isRootDep ? rootPos : pos.get(dep)
      if (!from) continue
      const x1 = from.x + mid
      const y1 = from.y + NODE_H / 2
      const x2 = to.x + mid
      const y2 = to.y + NODE_H / 2
      const dx = Math.max(30, (x2 - x1) / 2)
      const srcStatus = isRootDep ? root?.status : subs.find((x) => x.id === dep)?.status
      out.push({
        d: `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`,
        done: srcStatus === 'done',
      })
    }
  }
  return out
}

function nodeGlyph(status: string): string {
  switch (status) {
    case 'done':
      return '✓'
    case 'failed':
      return '✗'
    case 'skipped':
      return '⤼'
    default:
      return '·'
  }
}

function statusTextClass(status: string): string {
  switch (status) {
    case 'done':
      return 'text-success'
    case 'failed':
      return 'text-danger'
    case 'skipped':
      return 'text-warn'
    case 'running':
      return 'text-run'
    default:
      return 'text-muted'
  }
}
