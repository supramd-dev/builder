import { useEffect, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router'
import { LoaderCircle } from 'lucide-react'
import { getTask, type SubTask, type TaskDetail } from './api'
import { TaskStatusText, commitUrl } from './StatusViews'
import { Breadcrumbs } from './Breadcrumbs'
import TaskLogView from './TaskLogView'
import {
  GAP_X,
  GAP_Y,
  NODE_H,
  NODE_W,
  ROOT_KEY,
  graphEdges,
  layerNodes,
  nodePositions,
  regressionGroup,
  type LayoutNode,
} from './graphLayout'

// TaskPipelinePage shows one pipeline as two linked views: the dependency
// graph on top, the pipeline step list (with inline expandable stage logs)
// below it. Graph nodes come in two flavours (see graphLayout): real
// sub-tasks, which open the run they recorded, and derived containers (the
// root, and the reg test node summarizing the regression cases), which open
// the stage-wide run. A node with no run at all scrolls to the step list and
// expands that stage's log. Clicking a step toggles its log in place;
// statuses follow the live pipeline — getTask is polled (3s) only while the
// graph is pending/running. The requested id may be a root or a sub-task
// (legacy deep links); sub-task ids resolve to their root and preselect that
// sub-task's log.
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
            onNodeClick={(node) => {
              // A derived node opens the stage-wide run it summarizes.
              if (node.derived) {
                if (root.regressionRunId) navigate(`/runs/${root.regressionRunId}`)
                return
              }
              // A recorded run owns the click: straight to the run's
              // detail page. Otherwise select the step: expand its log
              // in the pipeline list and scroll it into view.
              if (node.sub?.runId) {
                navigate(`/runs/${node.sub.runId}`)
                return
              }
              if (node.sub) {
                const id = node.sub.id
                setExpanded(id)
                requestAnimationFrame(() => {
                  document
                    .getElementById(`pipeline-step-${id}`)
                    ?.scrollIntoView({ behavior: 'smooth', block: 'center' })
                })
              }
            }}
          />
          <p className="text-muted graph-hint">
            Click a stage with a recorded run to open its test details; the
            reg test node opens the whole regression run; other nodes expand
            the stage's log in the pipeline below.
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

// GraphCanvas draws the dependency graph. Real nodes with a recorded run
// open the run detail page, derived nodes the stage-wide run (onNodeClick);
// a node with neither expands its step's log in the pipeline list below. The
// root node is not clickable.
function GraphCanvas({
  task,
  subs,
  expanded,
  onNodeClick,
}: {
  task: TaskDetail
  subs: SubTask[]
  expanded: number | null
  onNodeClick: (node: LayoutNode) => void
}) {
  const root = task.kind === 'root' ? task : null
  const group = regressionGroup(subs)
  const layers = layerNodes(subs, root, group)
  const pos = nodePositions(layers)

  const width = Math.max(1, layers.length)
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
          {graphEdges(subs, root, group, pos).map((e, i) => (
            <path key={i} d={e.d} className={'graph-edge' + (e.done ? ' graph-edge-done' : '')} />
          ))}
        </svg>
        {layers.flat().map((node) => {
          const p = pos.get(node.key)
          if (!p) return null
          const isRoot = node.key === ROOT_KEY
          const status = node.status
          const cls = [
            'graph-node',
            isRoot ? 'graph-node-root' : '',
            node.derived ? 'graph-node-derived' : '',
            node.sub && expanded === node.key ? 'graph-node-selected' : '',
            'graph-node-' + status,
          ]
            .filter(Boolean)
            .join(' ')
          return (
            <button
              type="button"
              key={String(node.key)}
              className={cls}
              style={{ left: p.x, top: p.y, width: NODE_W, height: NODE_H }}
              onClick={isRoot ? undefined : () => onNodeClick(node)}
              title={
                isRoot
                  ? 'Pipeline root'
                  : node.derived
                    ? `Derived from ${group?.cases.length ?? 0} case nodes — open the regression run`
                    : node.sub?.runId
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
                <span className="graph-node-name">{node.name}</span>
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

// --- status glyphs ----------------------------------------------------------

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
