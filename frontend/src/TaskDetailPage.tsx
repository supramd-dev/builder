import { useEffect, useRef, useState } from 'react'
import { Clock, LoaderCircle } from 'lucide-react'
import {
  getTask,
  getTaskLogs,
  type SubTask,
  type TaskDetail,
  type TaskStatus,
} from './api'

interface Props {
  taskId: number
  onBack: () => void
}

// TaskDetailPage renders one task graph: the root's status and context, the
// sub-task pipeline (clone → build → tests) with each stage's state, and the
// selected sub-task's log, following a running task incrementally.
export default function TaskDetailPage({ taskId, onBack }: Props) {
  const [task, setTask] = useState<TaskDetail | null>(null)
  const [error, setError] = useState('')
  // Which sub-task's log is shown; null = the root overview (no log).
  const [selected, setSelected] = useState<number | null>(null)

  useEffect(() => {
    let live = true
    async function poll() {
      try {
        const t = await getTask(taskId)
        if (!live) return
        setTask(t)
        setError('')
        // Default the log view to the first non-done sub-task (or the last
        // one) once the sub-task list arrives.
        setSelected((cur) => {
          if (cur !== null || !t.subTasks || t.subTasks.length === 0) return cur
          const active = t.subTasks.find((s) => s.status === 'running')
          return (active ?? t.subTasks[t.subTasks.length - 1]).id
        })
      } catch (err) {
        if (live) {
          setError(err instanceof Error ? err.message : 'Failed to load task')
        }
      }
    }
    poll()
    const timer = setInterval(poll, 2000)
    return () => {
      live = false
      clearInterval(timer)
    }
  }, [taskId])

  if (error) {
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
        <div className="alert alert-danger">{error}</div>
      </div>
    )
  }
  if (!task) {
    return <p className="text-muted">Loading…</p>
  }

  const live = task.status === 'pending' || task.status === 'running'

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

      <h2>
        Task #{task.id} <TaskStatusBadge status={task.status} />
      </h2>

      <dl className="task-meta">
        <dt>Commit</dt>
        <dd>
          {task.commit ? (
            <>
              <code>{task.commit.shortSha}</code>
              {task.commit.message && <span className="text-muted"> — {task.commit.message}</span>}
              <span className="text-muted"> · {task.commit.author}</span>
            </>
          ) : (
            <span className="text-muted">#{task.commitId}</span>
          )}
        </dd>
        <dt>Environment</dt>
        <dd>
          {task.environment ? (
            <>
              {task.environment.name}
              {task.tags && <span className="text-muted"> · tags: {task.tags}</span>}
            </>
          ) : (
            <span className="text-muted">#{task.environmentId}</span>
          )}
        </dd>
        {task.error && (
          <>
            <dt>Error</dt>
            <dd className="task-error">{task.error}</dd>
          </>
        )}
      </dl>

      {task.subTasks && task.subTasks.length > 0 && (
        <SubTaskList
          subs={task.subTasks}
          selected={selected}
          onSelect={setSelected}
        />
      )}

      {selected !== null && <TaskLogView taskId={selected} live={live} />}
    </div>
  )
}

// SubTaskList renders the graph's stages as a vertical step list in
// dependency (creation) order.
function SubTaskList({
  subs,
  selected,
  onSelect,
}: {
  subs: SubTask[]
  selected: number | null
  onSelect: (id: number) => void
}) {
  return (
    <section>
      <h3 style={{ marginBottom: '0.5rem' }}>Pipeline</h3>
      <ol className="task-steps">
        {subs.map((sub) => (
          <li key={sub.id}>
            <button
              type="button"
              className={
                'btn btn-sm task-step' +
                (selected === sub.id ? ' task-step-selected' : '')
              }
              onClick={() => onSelect(sub.id)}
              title={sub.error || sub.name}
            >
              <span className="task-step-status">
                {sub.status === 'running' ? (
                  <LoaderCircle size={13} className="spin" />
                ) : sub.status === 'pending' ? (
                  <Clock size={13} />
                ) : (
                  statusGlyph(sub.status)
                )}
              </span>
              <span className="task-step-name">{sub.name}</span>
              <span className={'text-muted task-step-kind task-kind-' + sub.kind}>
                {sub.kind}
              </span>
              <TaskStatusBadge status={sub.status} />
            </button>
          </li>
        ))}
      </ol>
    </section>
  )
}

// TaskLogView shows one sub-task's log, polling incrementally with after=
// lastSeq while the parent task is live.
function TaskLogView({ taskId, live }: { taskId: number; live: boolean }) {
  const [text, setText] = useState('')
  const [truncated, setTruncated] = useState(false)
  const lastSeq = useRef(0)
  const preRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    // New selection: reset the stream.
    setText('')
    setTruncated(false)
    lastSeq.current = 0

    let stop = false
    async function poll() {
      try {
        const res = await getTaskLogs(taskId, lastSeq.current)
        if (stop) return
        lastSeq.current = res.lastSeq
        if (res.chunks.length > 0) {
          setText((cur) => {
            const next = cur + res.chunks.map((c) => c.content).join('')
            // Cap the rendered tail (full log lives server-side).
            if (next.length > 512 * 1024) {
              setTruncated(true)
              return next.slice(next.length - 512 * 1024)
            }
            return next
          })
        }
      } catch {
        // Transient poll errors are ignored; the next tick retries.
      }
    }
    poll()
    const timer = setInterval(poll, 2000)
    return () => {
      stop = true
      clearInterval(timer)
    }
  }, [taskId])

  // Auto-scroll to the bottom on new output while following.
  const stick = useRef(true)
  useEffect(() => {
    const pre = preRef.current
    if (pre && stick.current) {
      pre.scrollTop = pre.scrollHeight
    }
  }, [text])

  return (
    <section>
      <h3 style={{ marginBottom: '0.5rem' }}>
        Log {live && <span className="text-muted">(following…)</span>}
      </h3>
      <pre
        ref={preRef}
        className="task-log"
        onScroll={(e) => {
          const el = e.currentTarget
          stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40
        }}
      >
        {truncated && <span className="text-muted">… earlier output trimmed (full log on the server) …{'\n'}</span>}
        {text || (live ? 'waiting for output…' : '(no output)')}
      </pre>
    </section>
  )
}

function TaskStatusBadge({ status }: { status: TaskStatus }) {
  return <span className={'task-badge task-badge-' + status}>{status}</span>
}

function statusGlyph(status: TaskStatus): string {
  switch (status) {
    case 'done':
      return '✓'
    case 'failed':
      return '✗'
    case 'skipped':
      return '⤼'
    case 'running':
      return '▶'
    default:
      return '·'
  }
}
