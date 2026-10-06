import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { LoaderCircle, Maximize2 } from 'lucide-react'
import {
  cancelTask,
  getTestArtifact,
  getTestRun,
  isTerminalStatus,
  runArtifactsZipUrl,
  testArtifactDownloadUrl,
  type Run,
  type TaskKind,
  type TestArtifactRef,
  type TestRunDetail,
} from './api'
import { formatDuration, parseGTestResults, type GTestCase } from './gtest'
import MessageDialog from './MessageDialog'
import ArtifactPreviewDialog from './ArtifactPreviewDialog'
import TaskLogView from './TaskLogView'
import { StatusText } from './StatusViews'
import { statusView } from './status'
import { formatTime } from './timezone'
import { Breadcrumbs } from './Breadcrumbs'
import PlotSection from './plot/PlotSection'

interface Props {
  onError: (message: string) => void
}

// TestRunDetailPage shows one attempt of one task in the sr.ht build style:
// the title (the test's name and kind, the attempt, status in color), a
// summary block (commit, environment, results, time), the attempt's
// conclusion, every other attempt of the same task (a retry history), the
// stored artifacts — results files get parsed in the browser — and the
// attempt's stdout log.
//
// A run is one attempt, so it has no children of its own: the regression
// cases are tasks beside it, reached through the task page (the breadcrumb
// crumb, or the link next to the log).
export default function TestRunDetailPage({ onError }: Props) {
  const runId = Number(useParams().runId)
  const [run, setRun] = useState<TestRunDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  // Cancelling this attempt's stage, and what went wrong when it did not
  // happen. Kept apart from `error`, which is the page failing to load: a
  // refusal (409) must not replace the run being read with an error view.
  const [cancelling, setCancelling] = useState(false)
  const [cancelError, setCancelError] = useState('')

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    // A new run id is a new page: a failure recorded for the previous one must
    // not outlive it and hide the run that did load.
    setError('')
    getTestRun(runId)
      .then((r) => {
        if (!cancelled) setRun(r)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load'
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [runId, onError])

  // An attempt that has not finished follows its stage live: poll until the
  // real outcome lands. The question is asked as "has it finished?" rather than
  // "is it pending or running?" so that a status this build does not recognise
  // keeps the page polling instead of freezing it — a frozen page has no
  // request left to notice the outcome. (A terminal run needs no such care:
  // the store never reopens one — a re-run opens a new attempt, and so a new
  // run with a page of its own.)
  const live = run !== null && !isTerminalStatus(run.status)
  // Cancelling this stage. A run is one attempt of one task, so the store's
  // cancellation of that task is what stops it — the stage is a leaf of the
  // graph, and the work waiting on it is skipped (see cancelTask on the server
  // side). The run is read again right after, so the page shows what the store
  // did rather than what the press assumed; a refusal (409: the stage finished
  // first) is said out loud instead of looking like a press that did nothing.
  const cancel = async () => {
    if (run === null || run.taskId <= 0) return
    setCancelling(true)
    setCancelError('')
    try {
      await cancelTask(run.taskId)
      setRun(await getTestRun(runId))
    } catch (err) {
      setCancelError(err instanceof Error ? err.message : 'Failed to cancel')
    } finally {
      setCancelling(false)
    }
  }
  useEffect(() => {
    if (!live) return
    // The interval is cleared on navigation/unmount, but a request already on
    // the wire is not: without this flag its reply would land afterwards and
    // put the run just left (or unmounted) back on the page.
    let cancelled = false
    const timer = setInterval(() => {
      getTestRun(runId)
        .then((r) => {
          if (!cancelled) setRun(r)
        })
        .catch(() => {}) // transient poll errors keep the last good view
    }, 3000)
    return () => {
      cancelled = true
      clearInterval(timer)
    }
  }, [live, runId])

  // The breadcrumb trail's task crumb needs the task id, which arrives with
  // the run; before that the trail ends at Dashboard.
  const trail = [
    { label: 'Dashboard', to: '/' },
    ...(run && run.taskId > 0
      ? [{ label: `Task #${run.taskId}`, to: `/tasks/${run.taskId}` }]
      : []),
    { label: `Run #${runId}` },
  ]

  if (loading) {
    return (
      <div>
        <Breadcrumbs trail={trail} />
        <p className="text-muted">Loading…</p>
      </div>
    )
  }
  if (error || !run) {
    return (
      <div>
        <Breadcrumbs trail={trail} />
        {error && <div className="alert alert-danger">{error}</div>}
      </div>
    )
  }

  const failed = run.status === 'failed'
  const skipped = run.status === 'skipped'
  const inFlight = run.status === 'pending' || run.status === 'running'
  // The status' wording and color come from the shared table (status.ts), so
  // this page, the task graph and the dashboard cannot drift apart. An open
  // attempt reads as the state it is in ("⏳ running"), not as a verdict.
  const view = statusView(run.status)
  const statusCls = view.textCls
  const statusText = view.live ? '⏳ ' + run.status : view.text

  return (
    <div>
      <Breadcrumbs trail={trail} />

      {/* Title: the test's name + status in color, and the attempt when the
          task ran more than once (a retry). */}
      <h2 className="task-title">
        {run.taskName ?? kindLabel(run.taskKind ?? run.kind)} ·{' '}
        <span className={statusCls}>{statusText}</span>
        {inFlight && <LoaderCircle size={15} className="spin" style={{ verticalAlign: '-2px' }} />}
        {run.attempt > 1 && (
          <span className="text-muted" style={{ fontWeight: 400 }}>
            {' '}
            · attempt {run.attempt}
          </span>
        )}
        {run.commitShortSha && (
          <>
            {' · '}
            <code>{run.commitShortSha}</code>
          </>
        )}
        {run.environmentName && <span className="text-muted"> on {run.environmentName}</span>}
      </h2>

      {/* The test's own label from md-builder.yaml (stored per trigger, so it
          reflects the yaml at dispatch time). */}
      {run.taskDescription && <p className="dash-run-description">{run.taskDescription}</p>}

      {/* Summary: commit, author, results, time. */}
      <div className="task-summary text-muted">
        {run.commitMessage && <>{run.commitMessage} · </>}
        {run.commitAuthor && <>{run.commitAuthor} · </>}
        {skipped ? (
          <span className="text-warn">not executed (upstream failure)</span>
        ) : inFlight ? (
          <span className="text-run">
            {run.status === 'running' ? 'running — following the stage live' : 'queued — waiting for upstream stages'}
          </span>
        ) : (
          <>
            <span className={failed ? 'text-danger' : 'text-success'}>
              {run.passed}/{run.total} passed
            </span>
            {run.failed > 0 && <span className="text-danger"> ({run.failed} failed)</span>}
            {run.skipped > 0 && <span className="text-warn"> ({run.skipped} skipped)</span>}
          </>
        )}
        {run.durationMillis > 0 && <> · {formatDuration(run.durationMillis)}</>}
        {run.startedAt && (
          <>
            {' · '}
            {formatTime(run.startedAt)}
            {run.finishedAt ? ` → ${formatTime(run.finishedAt)}` : ''}
          </>
        )}
      </div>

      {skipped && (
        <p className="dash-skip-note">
          This stage was skipped: an upstream task failed before it could run,
          so no tests were executed. The reason is in the summary below.
        </p>
      )}

      {run.summary && <pre className="dash-run-summary">{run.summary}</pre>}

      {/* The stage is still in flight: it can be stopped from here. */}
      {live && run.taskId > 0 && (
        <p style={{ marginBottom: '0.5rem' }}>
          <button
            type="button"
            className="btn btn-danger btn-sm"
            disabled={cancelling}
            onClick={() => {
              const name = run.taskName ? `"${run.taskName}"` : 'this stage'
              if (
                window.confirm(
                  `Cancel ${name}? The stage is stopped and marked cancelled, and the stages ` +
                    'waiting on it are skipped — what has already finished keeps its results.',
                )
              ) {
                void cancel()
              }
            }}
          >
            {cancelling ? 'Cancelling…' : 'Cancel this stage'}
          </button>
        </p>
      )}
      {cancelError && <div className="alert alert-danger">{cancelError}</div>}

      {/* The task page is where the graph, the sibling stages and — for a
          regression stage — the cases with their own runs live. */}
      {run.taskId > 0 && (
        <p>
          <Link to={`/tasks/${run.taskId}`}>Open the task: graph and stage log →</Link>
        </p>
      )}

      <AttemptsSection run={run} />

      {/* Every stored artifact of the attempt, whatever its kind: build files
          (never parsed), results files (parsed in the browser below) — each
          downloadable, the whole bundle as one zip. */}
      {!inFlight && run.artifacts.length > 0 && (
        <ArtifactsSection run={run} onError={onError} />
      )}

      {/* Plot artifacts (*.plot.json / *.plotly.json): one interactive
          Plotly chart per file, fetched and rendered client-side like the
          results files. */}
      {!inFlight && <PlotSection artifacts={run.artifacts} onError={onError} />}

      {/* Results files are parsed in the browser (nothing is stored while the
          stage is still executing). */}
      {!inFlight && <ResultsFileSection run={run} onError={onError} />}

      {/* A run with no results file and no per-test counts has nothing else
          to show: say so rather than leaving the page half empty. */}
      {!inFlight &&
        !run.artifacts.some((a) => a.kind === 'results') &&
        run.total === 0 && (
          <p className="text-muted">
            This attempt reported no test cases — see the summary above.
          </p>
        )}

      <LogSection run={run} />
    </div>
  )
}

// kindLabel names a task kind for the page title, for a run whose task was
// deleted (or which carries no name of its own).
function kindLabel(kind: TaskKind | string): string {
  switch (kind) {
    case 'build':
      return 'Build'
    case 'unit':
      return 'Unit tests'
    case 'clone':
      return 'Clone'
    case 'regression':
      return 'Regression tests'
    case 'regression_case':
      return 'Regression case'
    default:
      return 'Task'
  }
}

// AttemptsSection lists every attempt of the run's task, newest first — a
// task is re-run on retry, so the list is how an earlier attempt stays
// reachable. Each row is a link to that attempt's own page (this one is
// marked); the row's counts and timings come from the stored runs.
function AttemptsSection({ run }: { run: TestRunDetail }) {
  const attempts = run.attempts ?? []
  if (attempts.length <= 1) return null
  return (
    <section>
      <h3 className="task-section-title">
        Attempts{' '}
        <span className="text-muted" style={{ fontWeight: 400 }}>
          ({attempts.length} runs of this task, newest first)
        </span>
      </h3>
      <table className="table" style={{ marginBottom: '1rem' }}>
        <thead>
          <tr>
            <th>Attempt</th>
            <th>Status</th>
            <th>Results</th>
            <th>Duration</th>
            <th>Started</th>
          </tr>
        </thead>
        <tbody>
          {attempts.map((a: Run) => (
            <tr key={a.id} className="dash-attempt-row">
              <td>
                {a.id === run.id ? (
                  <span>
                    #{a.attempt} <span className="text-muted">(this run)</span>
                  </span>
                ) : (
                  <Link to={`/runs/${a.id}`}>#{a.attempt}</Link>
                )}
              </td>
              <td>
                <StatusText status={a.status} />
              </td>
              <td className="text-muted">
                {a.total > 0 ? `${a.passed}/${a.total} passed` : '—'}
              </td>
              <td className="text-muted">{a.durationMillis > 0 ? formatDuration(a.durationMillis) : '—'}</td>
              <td className="text-muted">{a.startedAt ? formatTime(a.startedAt) : '—'}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </section>
  )
}

// LogSection shows the attempt's stdout: the task's log at this run's
// attempt, so an earlier attempt's page shows that attempt's output. The view
// follows the run's own status, so an attempt that may still write is not read
// as if its output were complete.
function LogSection({ run }: { run: TestRunDetail }) {
  if (run.taskId === 0) return null
  return (
    <section>
      <h3 className="task-section-title">Log</h3>
      <TaskLogView taskId={run.taskId} attempt={run.attempt} status={run.status} />
    </section>
  )
}

// ArtifactsSection lists every stored artifact of the attempt (build files,
// results files, future logs/series) with a per-file preview (Monaco editor
// dialog), a per-file download link and one zip bundling them — only the
// attempt's own files: a run is one attempt of one task, and the task's whole
// subtree is zipped from the task page.
function ArtifactsSection({
  run,
  onError,
}: {
  run: TestRunDetail
  onError: (message: string) => void
}) {
  const [preview, setPreview] = useState<TestArtifactRef | null>(null)
  if (run.artifacts.length === 0) return null
  return (
    <section>
      <h3 className="task-section-title">
        Artifacts{' '}
        <span className="text-muted" style={{ fontWeight: 400 }}>
          ({run.artifacts.length} file{run.artifacts.length === 1 ? '' : 's'})
        </span>
      </h3>
      <table className="table" style={{ marginBottom: '0.5rem' }}>
        <thead>
          <tr>
            <th>File</th>
            <th>Kind</th>
            <th>Size</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {run.artifacts.map((a) => (
            <tr key={a.id}>
              <td>
                <code>{a.name}</code>
              </td>
              <td className="text-muted">{a.kind}</td>
              <td className="text-muted">{formatBytes(a.size)}</td>
              <td style={{ textAlign: 'right' }}>
                <a
                  href=""
                  onClick={(e) => {
                    e.preventDefault()
                    setPreview(a)
                  }}
                  title="Preview the file in a dialog — a page or a Markdown document renders, anything else opens as source"
                >
                  View
                </a>
                {' · '}
                <a href={testArtifactDownloadUrl(a.id)}>Download</a>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {run.artifacts.length > 1 && (
        <p style={{ marginBottom: '1rem' }}>
          <a href={runArtifactsZipUrl(run.id)}>Download all as zip</a>
        </p>
      )}
      {preview !== null && (
        // Keyed by artifact: the dialog's mode (rendered or source) is
        // decided per file and must not carry over from the last one.
        <ArtifactPreviewDialog
          key={preview.id}
          artifact={preview}
          onClose={() => setPreview(null)}
          onError={onError}
        />
      )}
    </section>
  )
}

// formatBytes renders a byte count the way file listings do.
function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KiB`
  return `${(n / (1024 * 1024)).toFixed(1)} MiB`
}

// ResultsFileSection fetches every stored results file of the run and
// renders the merged per-case list parsed in the browser (the backend keeps
// only the aggregate counts). Files whose format is not recognized are
// skipped — the parse follows the default names/formats, no file picker.
function ResultsFileSection({
  run,
  onError,
}: {
  run: TestRunDetail
  onError: (message: string) => void
}) {
  const refs = run.artifacts.filter((a) => a.kind === 'results')
  const [parsed, setParsed] = useState<GTestCase[] | null>(null)
  const [badFiles, setBadFiles] = useState<string[]>([])
  const [raw, setRaw] = useState<{ name: string; content: string }[]>([])
  const [loadError, setLoadError] = useState('')

  useEffect(() => {
    if (refs.length === 0) {
      setParsed(null)
      setBadFiles([])
      setRaw([])
      setLoadError('')
      return
    }
    let cancelled = false
    Promise.all(
      refs.map((ref) =>
        getTestArtifact(ref.id).then((a) => ({
          name: a.name,
          content: a.content,
        })),
      ),
    )
      .then((files) => {
        if (cancelled) return
        setRaw(files)
        const cases: GTestCase[] = []
        const bad: string[] = []
        for (const f of files) {
          try {
            cases.push(...parseGTestResults(f.content))
          } catch {
            bad.push(f.name)
          }
        }
        setParsed(cases)
        setBadFiles(bad)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load results files'
        setLoadError(msg)
        onError(msg)
      })
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refs.map((r) => r.id).join(',')])

  if (refs.length === 0) {
    return null
  }

  return (
    <section>
      <h3 className="task-section-title">
        Test cases{' '}
        <span className="text-muted" style={{ fontWeight: 400 }}>
          (parsed in the browser from {refs.length} file
          {refs.length === 1 ? '' : 's'})
        </span>
      </h3>
      {loadError && <div className="alert alert-danger">{loadError}</div>}
      {badFiles.length > 0 && (
        <p className="text-muted">
          Unrecognized format (stored below, not parsed):{' '}
          {badFiles.map((n, i) => (
            <span key={n}>
              {i > 0 && ', '}
              <code>{n}</code>
            </span>
          ))}
          .
        </p>
      )}
      {parsed && parsed.length > 0 && <BrowserCaseTable cases={parsed} />}
      {raw.length > 0 && (
        <details style={{ marginBottom: '1rem' }}>
          <summary className="text-muted">
            Raw results file{raw.length === 1 ? '' : 's'}
          </summary>
          {raw.map((f) => (
            <div key={f.name}>
              <p className="text-muted" style={{ margin: '0.5rem 0 0.25rem' }}>
                <code>{f.name}</code>
              </p>
              <pre className="task-log">{f.content}</pre>
            </div>
          ))}
        </details>
      )}
    </section>
  )
}

// BrowserCaseTable renders the browser-parsed case list (no database rows
// behind it — no case-detail links).
function BrowserCaseTable({ cases }: { cases: GTestCase[] }) {
  const [note, setNote] = useState<{ name: string; message: string } | null>(null)
  return (
    <>
      <table className="table" style={{ marginBottom: '1rem' }}>
        <thead>
          <tr>
            <th>Case</th>
            <th>Status</th>
            <th>Duration</th>
            <th>Note</th>
          </tr>
        </thead>
        <tbody>
          {cases.map((c, i) => (
            <tr key={`${c.name}-${i}`}>
              <td>{c.name}</td>
              <td>
                <StatusText status={c.status} />
              </td>
              <td>{formatDuration(c.durationMs)}</td>
              <td className="text-muted">
                <NoteCell name={c.name} message={c.message} onExpand={setNote} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {note && (
        <MessageDialog
          title={note.name}
          message={note.message}
          onClose={() => setNote(null)}
        />
      )}
    </>
  )
}

// NoteCell shows the message's truncated first line plus an expand icon
// that opens the full text in a read-only dialog.
function NoteCell({
  name,
  message,
  onExpand,
}: {
  name: string
  message: string
  onExpand: (note: { name: string; message: string }) => void
}) {
  if (!message) {
    return <>—</>
  }
  return (
    <>
      <span title={message}>{truncateNote(message)}</span>{' '}
      <button
        type="button"
        className="note-expand"
        aria-label="Show full message"
        title="Show full message"
        onClick={(e) => {
          e.stopPropagation()
          onExpand({ name, message })
        }}
      >
        <Maximize2 size={12} />
      </button>
    </>
  )
}

function truncateNote(message: string): string {
  if (message.length <= 80) {
    return message
  }
  const flat = message.split('\n')[0]
  return flat.length > 80 ? flat.slice(0, 77) + '…' : flat
}
