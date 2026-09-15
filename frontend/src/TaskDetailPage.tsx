import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { LoaderCircle } from 'lucide-react'
import {
  getTask,
  type SubTask,
  type TaskDetail,
  type TaskStatus,
} from './api'
import TaskLogView from './TaskLogView'
import { TaskStatusText, commitUrl } from './StatusViews'
import { Breadcrumbs } from './Breadcrumbs'

// TaskDetailPage renders one task's pipeline log in the sr.ht build style:
// the title (task number, commit, environment), a summary line (author,
// ref, tags), the job status in color, the pipeline step list and the
// selected step's log, following a running task incrementally.
export default function TaskDetailPage() {
  const taskId = Number(useParams().taskId)
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

  // The graph trail crumb targets the task's root (the graph page), so the
  // log view of any task in a graph links back to the same pipeline.
  const rootId = task ? (task.kind === 'root' ? task.id : task.rootId) : taskId

  return (
    <div>
      <Breadcrumbs
        trail={[
          { label: 'Dashboard', to: '/' },
          { label: `Task #${rootId}`, to: `/tasks/${rootId}` },
          { label: 'Log' },
        ]}
      />

      {error ? (
        <div className="alert alert-danger">{error}</div>
      ) : !task ? (
        <p className="text-muted">Loading…</p>
      ) : (
        <TaskDetail task={task} selected={selected} onSelect={setSelected} />
      )}
    </div>
  )
}

function TaskDetail({
  task,
  selected,
  onSelect,
}: {
  task: TaskDetail
  selected: number | null
  onSelect: (id: number) => void
}) {
  const live = task.status === 'pending' || task.status === 'running'
  return (
    <div>
      {/* Title: task number, commit link, environment. */}
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

      {/* Summary: author, ref, tags — then the job status in color. */}
      <div className="task-summary text-muted">
        {task.commit && (
          <>
            {task.commit.author} pushed to <code>{task.commit.ref}</code> ·{' '}
          </>
        )}
        {task.tags && <>tags: {task.tags} · </>}
        <TaskStatusText status={task.status} />
        {(task.trigger ?? 0) > 0 && (
          <span className="dash-trigger" title="manually triggered from the UI">
            manual
          </span>
        )}
      </div>
      {task.error && <p className="task-error">{task.error}</p>}

      {task.subTasks && task.subTasks.length > 0 && (
        <SubTaskList
          subs={task.subTasks}
          selected={selected}
          onSelect={onSelect}
          rootTaskId={task.kind === 'root' ? task.id : task.rootId}
        />
      )}

      {selected !== null && (
        <section>
          <h3 className="task-section-title">
            Log {live && <span className="text-muted">(following…)</span>}
          </h3>
          <TaskLogView taskId={selected} live={live} />
        </section>
      )}
    </div>
  )
}

// SubTaskList renders the graph's stages as a compact step list in
// dependency (creation) order: glyph + name + kind + status word.
function SubTaskList({
  subs,
  selected,
  onSelect,
  rootTaskId,
}: {
  subs: SubTask[]
  selected: number | null
  onSelect: (id: number) => void
  rootTaskId: number
}) {
  return (
    <section>
      <h3 className="task-section-title">
        Pipeline{' '}
        <Link
          to={`/tasks/${rootTaskId}`}
          className="task-graph-link"
          title="Open the task dependency graph"
        >
          graph →
        </Link>
      </h3>
      <ol className="task-steps">
        {subs.map((sub) => (
          <li key={sub.id}>
            <button
              type="button"
              className={
                'task-step' +
                (selected === sub.id ? ' task-step-selected' : '')
              }
              onClick={() => onSelect(sub.id)}
              title={sub.error || sub.name}
            >
              <span className={
                'task-step-status ' +
                (sub.status === 'done' ? 'text-success'
                  : sub.status === 'failed' ? 'text-danger'
                    : sub.status === 'skipped' ? 'text-warn'
                      : sub.status === 'running' ? 'text-run' : 'text-muted')
              }>
                {sub.status === 'running' ? (
                  <LoaderCircle size={13} className="spin" />
                ) : (
                  statusGlyph(sub.status)
                )}
              </span>
              <span className="task-step-name">{sub.name}</span>
              <span className="text-muted task-step-kind">{sub.kind}</span>
              <TaskStatusText status={sub.status} />
            </button>
          </li>
        ))}
      </ol>
    </section>
  )
}

// TaskLogView lives in TaskLogView.tsx (shared with the run detail page).

function statusGlyph(status: TaskStatus): string {
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
