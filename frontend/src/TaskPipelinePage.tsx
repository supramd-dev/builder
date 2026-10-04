import { useEffect, useRef, useState } from 'react'
import { Link, useParams } from 'react-router'
import { LoaderCircle } from 'lucide-react'
import { getTask, isTerminalStatus, taskArtifactsZipUrl, type TaskDetail, type TaskNode } from './api'
import { StatusText, commitUrl } from './StatusViews'
import { statusGlyph, statusView } from './status'
import { Breadcrumbs } from './Breadcrumbs'
import TaskLogView from './TaskLogView'
import {
  GAP_X,
  GAP_Y,
  NODE_H,
  NODE_W,
  graphEdges,
  layerNodes,
  nodePositions,
  regressionStage,
} from './graphLayout'

// How often the graph page re-reads its task: a graph that may still move is
// followed at the first cadence, one that reads finished slows to the second.
// The idle cadence is not a stop — a finished graph can be re-dispatched, and
// nothing else would tell a page that has stopped asking (see graphLive).
const POLL_LIVE_MS = 3000
const POLL_IDLE_MS = 15000

// TaskPipelinePage shows one pipeline as two linked views: the dependency
// graph on top, the pipeline step list — with the selected step's log inlined
// under it — below. The graph is drawn from the graph's own task nodes: the
// real ones (clone, build, unit, and one per regression case) each have a run
// of their own, whose log the panel streams while the stage executes; a
// virtual one (the regression container) runs nothing itself — it summarizes
// its cases, so selecting it lists them. getTask is polled as above. The
// requested id may be any node (a dashboard cell links its container, for
// instance): a non-root id resolves to its root and preselects that node.
//
// Nodes a later dispatch dropped are not part of the graph any more, but they
// are not gone either: their runs, logs and artifacts stay readable, so they
// are listed below the current steps as history.
export default function TaskPipelinePage() {
  const params = useParams()
  const urlId = Number(params.taskId)
  const [root, setRoot] = useState<TaskDetail | null>(null)
  const [error, setError] = useState('')
  // Which node's panel is open; null = none.
  const [selected, setSelected] = useState<number | null>(null)
  // Mirror of `selected` for the poll loop. The automatic first selection has
  // to read the current selection, and it cannot do that from inside a state
  // updater: an updater must be pure (StrictMode runs it twice), and a flag
  // written on the first pass would then suppress the selection on the second.
  const selectedRef = useRef<number | null>(null)
  // A node id seen in the URL (e.g. /tasks/64) that wins the first selection
  // once its graph arrives.
  const wantedRef = useRef<number | null>(null)
  // Whether the automatic first selection has happened — later polls must not
  // re-select a node the user moved away from.
  const autoSelectedRef = useRef(false)
  // The current poll, for the wake-on-return effect below: the loop itself is
  // owned by the effect that fetches the graph.
  const pollRef = useRef<(() => void) | null>(null)

  // select is the only way the panel changes node, so the mirror cannot drift.
  const select = (id: number | null) => {
    selectedRef.current = id
    setSelected(id)
  }

  useEffect(() => {
    let live = true
    let timer: ReturnType<typeof setInterval> | undefined
    // The period the running timer was armed with, so a poll that wants the
    // cadence it already has leaves the timer alone.
    let period = 0
    let loaded = false
    let retries = 0
    function arm(ms: number) {
      if (period === ms) return
      if (timer) clearInterval(timer)
      period = ms
      timer = setInterval(poll, ms)
    }
    function disarm() {
      if (timer) clearInterval(timer)
      timer = undefined
      period = 0
    }
    // A new id is a different graph, and the route element is reused for it:
    // drop the old graph instead of rendering it under the new URL, and let
    // the new one make its own first selection.
    setRoot(null)
    select(null)
    wantedRef.current = null
    autoSelectedRef.current = false
    setError('')
    async function poll() {
      try {
        let t = await getTask(urlId)
        if (!live) return
        // A node id was addressed: forward to its root's graph and remember
        // the node to select.
        if (t.kind !== 'root' && t.rootId > 0 && t.rootId !== t.id) {
          wantedRef.current = t.id
          t = await getTask(t.rootId)
          if (!live) return
        }
        loaded = true
        setRoot(t)
        setError('') // an earlier blip must not keep the page on the error view
        // Automatic first selection: the URL-requested node when present —
        // including a retired one, which a link built before a re-dispatch can
        // still name — else the first running step (or the last one), where
        // the pipeline currently is.
        if (!autoSelectedRef.current && selectedRef.current === null) {
          const subs = t.subTasks ?? []
          const known = [...subs, ...(t.retiredTasks ?? [])]
          let pick: number | null = null
          if (wantedRef.current !== null && known.some((s) => s.id === wantedRef.current)) {
            pick = wantedRef.current
          } else if (subs.length > 0) {
            const active = subs.find((s) => s.status === 'running')
            pick = (active ?? subs[subs.length - 1]).id
          }
          if (pick !== null) {
            autoSelectedRef.current = true
            select(pick)
          }
        }
        // Keep following while anything in the graph may still change, and
        // slow down — not stop — once it looks finished: see graphLive.
        arm(graphLive(t) ? POLL_LIVE_MS : POLL_IDLE_MS)
      } catch (err) {
        if (!live) return
        // One failed request is not a dead page. Nothing has been shown yet,
        // so this is the whole page: retry a few times before giving up, which
        // rides out a blip without polling an id that does not exist for ever.
        // A failure after the graph is up keeps the last good graph and the
        // poll loop, which is already running.
        if (loaded) return
        setError(err instanceof Error ? err.message : 'Failed to load task')
        if (retries < 3) {
          retries++
          arm(POLL_LIVE_MS)
        } else {
          disarm()
        }
      }
    }
    pollRef.current = poll
    poll()
    return () => {
      live = false
      pollRef.current = null
      disarm()
    }
  }, [urlId])

  // Coming back to a page that was left open reads the graph at once: a
  // background tab's timers may have been suspended for as long as the user was
  // away, and the view should be current the moment it is looked at again
  // rather than at the next tick of the poll.
  useEffect(() => {
    const wake = () => {
      if (document.visibilityState === 'visible') pollRef.current?.()
    }
    window.addEventListener('focus', wake)
    document.addEventListener('visibilitychange', wake)
    return () => {
      window.removeEventListener('focus', wake)
      document.removeEventListener('visibilitychange', wake)
    }
  }, [])

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
  const retired = root.retiredTasks ?? []
  const live = graphLive(root)

  // Selecting a node opens the panel below the step list: the attempt's log
  // for a real node (the panel's own link goes on to the run's page, with the
  // artifacts and the other attempts), the child cases for a virtual one.
  const openNode = (node: TaskNode) => {
    select(node.id)
    requestAnimationFrame(() => {
      document
        .getElementById(`pipeline-step-${node.id}`)
        ?.scrollIntoView({ behavior: 'smooth', block: 'center' })
    })
  }

  return (
    <div>
      <TaskHeader task={root} />
      {subs.length > 0 ? (
        <>
          <GraphCanvas
            task={root}
            subs={subs}
            selected={selected}
            onNodeClick={openNode}
          />
          <p className="text-muted graph-hint">
            Click a stage to follow its output below; the reg test node has no
            run of its own — selecting it lists the cases, each with its own
            run.
          </p>
          <PipelineSection
            subs={subs}
            retired={retired}
            live={live}
            selected={selected}
            onSelect={openNode}
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
        <StatusText status={task.status} />
        {(task.trigger ?? 0) > 0 && (
          <span className="dash-trigger" title="manually triggered from the UI">
            manual
          </span>
        )}
      </div>
      {task.summary && <p className="text-muted">{task.summary}</p>}
      {task.error && <p className="task-error">{task.error}</p>}
      {/* Everything this graph stored, as one zip: every stage's files, each
          node under a directory of its own — a stage's own attempt is zipped
          from its run page instead. Only a graph that ran stores anything, so
          the link appears once it has an outcome. */}
      {(task.status === 'passed' || task.status === 'failed' || task.status === 'timeout') && (
        <p style={{ marginBottom: '0.5rem' }}>
          <a href={taskArtifactsZipUrl(task.id)}>Download the graph's artifacts (zip)</a>
        </p>
      )}
    </div>
  )
}

// GraphCanvas draws the dependency graph from the graph's task nodes. Clicking
// a node selects it in the pipeline list below (onNodeClick), where its run's
// log opens; the root node stands for the whole graph and is not clickable.
function GraphCanvas({
  task,
  subs,
  selected,
  onNodeClick,
}: {
  task: TaskDetail
  subs: TaskNode[]
  selected: number | null
  onNodeClick: (node: TaskNode) => void
}) {
  const stage = regressionStage(subs)
  const layers = layerNodes(task, subs, stage)
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
          {graphEdges(subs, stage, pos).map((e, i) => (
            <path key={i} d={e.d} className={'graph-edge' + (e.done ? ' graph-edge-done' : '')} />
          ))}
        </svg>
        {layers.flat().map((node) => {
          const p = pos.get(node.key)
          if (!p) return null
          const isRoot = node.node === null
          const status = node.status
          const cls = [
            'graph-node',
            isRoot ? 'graph-node-root' : '',
            node.virtual ? 'graph-node-derived' : '',
            selected === node.key ? 'graph-node-selected' : '',
            'graph-node-' + status,
          ]
            .filter(Boolean)
            .join(' ')
          return (
            <button
              type="button"
              key={node.key}
              className={cls}
              style={{ left: p.x, top: p.y, width: NODE_W, height: NODE_H }}
              onClick={isRoot ? undefined : () => onNodeClick(node.node as TaskNode)}
              title={
                isRoot
                  ? 'Pipeline root'
                  : node.virtual
                    ? `${status} · ${node.name} — a container whose status is its nodes' rollup; select it to list them`
                    : node.node?.runId
                      ? "Select the stage: its log is shown below"
                      : 'Select the stage: it has no run yet'
              }
            >
              <span className="graph-node-head">
                <span className="graph-node-status">
                  {status === 'running' ? (
                    <LoaderCircle size={13} className="spin" />
                  ) : (
                    statusGlyph(status)
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

// PipelineSection is the step list under the graph. Clicking a step opens its
// panel below (one at a time): the attempt's log for a real node, the child
// nodes for a virtual one. The retired nodes follow in their own list —
// history the current graph no longer defines.
function PipelineSection({
  subs,
  retired,
  live,
  selected,
  onSelect,
}: {
  subs: TaskNode[]
  retired: TaskNode[]
  live: boolean
  selected: number | null
  onSelect: (node: TaskNode) => void
}) {
  const all = [...subs, ...retired]
  const node = all.find((n) => n.id === selected)
  return (
    <section>
      <h3 className="task-section-title">Pipeline</h3>
      <StepList nodes={subs} selected={selected} onSelect={onSelect} />
      {node && <StepPanel node={node} nodes={all} live={live} />}
      {retired.length > 0 && (
        <>
          <h4 className="task-steps-title">
            History — {retired.length} node{retired.length === 1 ? '' : 's'} the current
            yaml no longer defines
          </h4>
          <StepList nodes={retired} selected={selected} onSelect={onSelect} retired />
        </>
      )}
    </section>
  )
}

// StepList renders the step rows: glyph, name, kind and status. A retired row
// is dimmed — it keeps its history but is never scheduled again.
function StepList({
  nodes,
  selected,
  onSelect,
  retired,
}: {
  nodes: TaskNode[]
  selected: number | null
  onSelect: (node: TaskNode) => void
  retired?: boolean
}) {
  return (
    <ol className="task-steps">
      {nodes.map((node) => (
        <li key={node.id} id={`pipeline-step-${node.id}`}>
          <button
            type="button"
            className={
              'task-step' +
              (selected === node.id ? ' task-step-selected' : '') +
              (retired || node.retired ? ' task-step-retired' : '')
            }
            onClick={() => onSelect(node)}
            title={node.error || node.summary || node.name}
          >
            <span className={'task-step-status ' + statusView(node.status).textCls}>
              {node.status === 'running' ? (
                <LoaderCircle size={13} className="spin" />
              ) : (
                statusGlyph(node.status)
              )}
            </span>
            <span className="task-step-name">{node.name}</span>
            <span className="text-muted task-step-kind">{node.kind}</span>
            {/* The counts are the stage's tests (a unit stage runs several);
                a node standing for one test says nothing new. */}
            {node.total > 1 && (
              <span className="text-muted task-step-kind">
                {node.passed}/{node.total}
              </span>
            )}
            <StatusText status={node.status} />
          </button>
        </li>
      ))}
    </ol>
  )
}

// StepPanel is the panel under the step list for the selected node: a real
// node shows its attempt's log, a virtual one its children — it runs nothing
// itself, so it has no log to show.
function StepPanel({
  node,
  nodes,
  live,
}: {
  node: TaskNode
  nodes: TaskNode[]
  live: boolean
}) {
  return (
    <div className="pipeline-log">
      <div className="pipeline-log-head">
        <span className={'pipeline-log-status ' + statusView(node.status).textCls}>
          {statusGlyph(node.status)}
        </span>
        <span className="pipeline-log-name">{node.name}</span>
        <span className="text-muted pipeline-log-kind">{node.kind}</span>
        {node.total > 1 && (
          <span className="text-muted pipeline-log-kind">
            {node.passed}/{node.total} passed
            {node.failed > 0 && `, ${node.failed} failed`}
            {node.skipped > 0 && `, ${node.skipped} skipped`}
          </span>
        )}
        {node.attempts > 1 && (
          <span className="text-muted pipeline-log-kind">
            attempt {node.attempts}
          </span>
        )}
        <StatusText status={node.status} />
        {live && node.status === 'running' && (
          <span className="text-muted pipeline-log-live">following output…</span>
        )}
        {node.runId ? (
          <Link to={`/runs/${node.runId}`} className="pipeline-run-link">
            Run details →
          </Link>
        ) : (
          node.virtual && (
            <Link to={`/tasks/${node.id}`} className="pipeline-run-link">
              Task details →
            </Link>
          )
        )}
      </div>
      {node.summary && <p className="text-muted">{node.summary}</p>}
      {node.virtual ? (
        <>
          {node.error && <p className="task-error">{node.error}</p>}
          <ul className="pipeline-cases">
            {nodes
              .filter((n) => n.parentId === node.id)
              .map((child) => (
                <li key={child.id}>
                  <span className="pipeline-cases-name">{child.name}</span>
                  <StatusText status={child.status} />
                  {child.runId ? (
                    <Link to={`/runs/${child.runId}`}>Run details →</Link>
                  ) : (
                    <span className="text-muted">no run yet</span>
                  )}
                </li>
              ))}
          </ul>
        </>
      ) : (
        // The panel names the attempt it shows: a re-dispatch during the
        // follow opens a new attempt, and asking for "the current one" every
        // tick would read the new attempt's log from the old one's sequence
        // (the per-attempt sequence restarts), hiding its early output. A
        // node with no attempt yet passes none: the API reads attempt=0 as an
        // error, and "the current attempt" is exactly right while there is one.
        <TaskLogView
          taskId={node.id}
          attempt={node.attempts > 0 ? node.attempts : undefined}
          status={node.status}
        />
      )}
    </div>
  )
}

// --- status glyphs ----------------------------------------------------------

// graphLive reports whether anything in the graph may still change — the
// question the page's poll loop asks. The root's own status is a rollup of its
// children, so it is normally enough; it is not always, and the difference
// freezes a page: a node that is still in flight while the root reads finished
// (a re-dispatched stage, or one the current yaml no longer defines, which the
// rollup leaves out) is advanced by the server with nobody asking for it. So
// every node counts, retired ones included, and any status this build does not
// recognise counts as still moving.
function graphLive(task: TaskDetail): boolean {
  if (!isTerminalStatus(task.status)) return true
  return [...(task.subTasks ?? []), ...(task.retiredTasks ?? [])].some(
    (node) => !isTerminalStatus(node.status),
  )
}

