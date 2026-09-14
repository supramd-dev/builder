import { useEffect, useState } from 'react'
import { LoaderCircle } from 'lucide-react'
import { getTask, type SubTask, type TaskDetail } from './api'
import { TaskStatusText, commitUrl } from './StatusViews'

interface Props {
  taskId: number
  onBack: () => void
  // onOpenRun jumps to a test run's detail page (build/unit/regression
  // nodes with a recorded run).
  onOpenRun: (runId: number) => void
  // onOpenLog jumps to the task's pipeline log view (clone/root nodes and
  // stages without a recorded run).
  onOpenLog: (taskId: number) => void
}

// NODE_W/NODE_H size the graph nodes; GAP_X/GAP_Y the layer spacing.
const NODE_W = 180
const NODE_H = 44
const GAP_X = 56
const GAP_Y = 22

// TaskGraphPage renders a task graph in the sr.ht build style: a title, a
// short summary (commit, environment), the pipeline status, then the
// dependency graph itself — the root on the left, the sub-tasks layered by
// dependency depth, edges drawn between the columns. Clicking a node opens
// its detail (the stage's live log or the recorded run).
export default function TaskGraphPage({ taskId, onBack, onOpenRun, onOpenLog }: Props) {
  const [task, setTask] = useState<TaskDetail | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let live = true
    async function poll() {
      try {
        const t = await getTask(taskId)
        if (live) {
          setTask(t)
          setError('')
        }
      } catch (err) {
        if (live) {
          setError(err instanceof Error ? err.message : 'Failed to load task')
        }
      }
    }
    poll()
    // Follow a live graph: node states advance while the pipeline runs.
    const timer = setInterval(poll, 3000)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [taskId])

  return (
    <div>
      <p>
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

      {error ? (
        <div className="alert alert-danger">{error}</div>
      ) : !task ? (
        <p className="text-muted">Loading…</p>
      ) : (
        <TaskGraph task={task} onOpenRun={onOpenRun} onOpenLog={onOpenLog} />
      )}
    </div>
  )
}

function TaskGraph({
  task,
  onOpenRun,
  onOpenLog,
}: {
  task: TaskDetail
  onOpenRun: (runId: number) => void
  onOpenLog: (taskId: number) => void
}) {
  const root = task.kind === 'root' ? task : null
  const subs = task.subTasks ?? []
  const layers = layerGraph(subs)
  const pos = positions(layers, root)

  // The root column exists only for a root detail view.
  const rows: { id: number | 'root'; node: TaskDetail | SubTask }[] = []
  if (root) rows.push({ id: 'root', node: root })
  for (const sub of subs) rows.push({ id: sub.id, node: sub })

  const width = Math.max(1, layers.length + (root ? 1 : 0))
  const maxRows = Math.max(1, ...layers.map((l) => l.length))
  const height = Math.max(1, maxRows)

  return (
    <div>
      {/* Title: task number, commit, environment, status. */}
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

      {/* Summary: commit author/ref, environment tags, error when failed. */}
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

      {/* The dependency graph. */}
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
            const status = node.status
            const runId = !isRoot && 'runId' in node ? node.runId : undefined
            // Test stages with a recorded run jump to run details; everything
            // else (clone, root, stages still pending) opens the pipeline log.
            const onClick = runId
              ? () => onOpenRun(runId)
              : () => onOpenLog(task.id)
            const cls =
              'graph-node graph-node-link graph-node-' +
              (isRoot ? 'root ' : '') +
              status
            return (
              <button
                type="button"
                key={String(id)}
                className={cls}
                style={{ left: p.x, top: p.y, width: NODE_W, height: NODE_H }}
                onClick={onClick}
                title={runId ? 'Open the test details' : node.error || 'Open the task log'}
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

      <p className="text-muted graph-hint">
        Click a test node (build / unit / regression) with a recorded run to
        open its details; other nodes open the task's log view.
      </p>
    </div>
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
