import { useEffect, useState } from 'react'
import { Maximize2 } from 'lucide-react'
import {
  getTestArtifact,
  getTestRun,
  type CaseResult,
  type TestRunDetail,
} from './api'
import { formatDuration, parseGTestResults, type GTestCase } from './gtest'
import MessageDialog from './MessageDialog'
import TaskLogView from './TaskLogView'
import { formatTime } from './timezone'

interface Props {
  runId: number
  onBack: () => void
  onOpenCase: (runId: number, caseResult: CaseResult) => void
  onError: (message: string) => void
}

// TestRunDetailPage shows one test run in the sr.ht build style: the title
// (kind, status in color), a summary block (environment, commit, results,
// time), the run's one-paragraph conclusion, the per-case results (from the
// stored results file, parsed in the browser) and the stage's stdout log.
export default function TestRunDetailPage({ runId, onBack, onOpenCase, onError }: Props) {
  const [run, setRun] = useState<TestRunDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
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

  if (loading) {
    return (
      <div>
        <BackLink onBack={onBack} />
        <p className="text-muted">Loading…</p>
      </div>
    )
  }
  if (error || !run) {
    return (
      <div>
        <BackLink onBack={onBack} />
        {error && <div className="alert alert-danger">{error}</div>}
      </div>
    )
  }

  const failed = run.status === 'failed'
  const skipped = run.status === 'skipped'
  const statusCls = failed ? 'text-danger' : skipped ? 'text-warn' : 'text-success'
  const statusText = failed ? '✗ failed' : skipped ? '⤼ skipped' : '✓ passed'
  const kindLabel =
    run.kind === 'regression' ? 'Regression tests' : run.kind === 'build' ? 'Build' : 'Unit tests'

  return (
    <div>
      <BackLink onBack={onBack} />

      {/* Title: kind + status in color. */}
      <h2 className="task-title">
        {kindLabel} · <span className={statusCls}>{statusText}</span>
        {run.commitShortSha && (
          <>
            {' · '}
            <code>{run.commitShortSha}</code>
          </>
        )}
        {run.environmentName && <span className="text-muted"> on {run.environmentName}</span>}
      </h2>

      {/* Summary: commit, author, results, time. */}
      <div className="task-summary text-muted">
        {run.commitMessage && <>{run.commitMessage} · </>}
        {run.commitAuthor && <>{run.commitAuthor} · </>}
        {skipped ? (
          <span className="text-warn">not executed (upstream failure)</span>
        ) : (
          <>
            <span className={failed ? 'text-danger' : 'text-success'}>
              {run.passed}/{run.total} passed
            </span>
            {run.failed > 0 && <span className="text-danger"> ({run.failed} failed)</span>}
            {run.skipped > 0 && <span className="text-warn"> ({run.skipped} skipped)</span>}
          </>
        )}
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
          so no tests were executed. See the summary below for the upstream
          failure.
        </p>
      )}

      {run.summary && <pre className="dash-run-summary">{run.summary}</pre>}

      {/* Unit runs parse their results file in the browser. */}
      <ResultsFileSection run={run} onError={onError} />

      {/* Cases reported through the database (regression reports). */}
      {run.cases.length > 0 && (
        <>
          <h3 className="task-section-title">Test cases</h3>
          <CaseTable
            cases={run.cases.map((c) => ({
              name: c.name,
              status: c.status,
              durationMs: c.durationMillis,
              message: c.message,
            }))}
            onOpenCase={(name) => {
              const c = run.cases.find((x) => x.name === name)
              if (c) onOpenCase(runId, c)
            }}
          />
        </>
      )}
      {run.cases.length === 0 && !run.artifacts.some((a) => a.kind === 'results') && (
        <p className="text-muted">
          No per-case results were reported for this run — see the summary
          above.
        </p>
      )}

      {/* The stage's stdout (task log). */}
      {run.taskId !== 0 && (
        <section>
          <h3 className="task-section-title">Log</h3>
          <TaskLogView taskId={run.taskId} live={false} />
        </section>
      )}
    </div>
  )
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
                {c.status === 'passed' ? (
                  <span className="text-success">✓ passed</span>
                ) : c.status === 'skipped' ? (
                  <span className="text-warn">⤼ skipped</span>
                ) : (
                  <span className="text-danger">✗ failed</span>
                )}
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

// CaseTable renders database-backed case results (regression reports), with
// row links into the case detail page.
function CaseTable({
  cases,
  onOpenCase,
}: {
  cases: GTestCase[]
  onOpenCase: (name: string) => void
}) {
  const [note, setNote] = useState<{ name: string; message: string } | null>(null)
  return (
    <>
      <table className="table">
        <thead>
          <tr>
            <th>Case</th>
            <th>Status</th>
            <th>Duration</th>
            <th>Note</th>
          </tr>
        </thead>
        <tbody>
          {cases.map((c) => (
            <tr
              key={c.name}
              className="dash-case-row"
              onClick={() => onOpenCase(c.name)}
              title="Click for case details"
            >
              <td>{c.name}</td>
              <td>
                {c.status === 'passed' ? (
                  <span className="text-success">✓ passed</span>
                ) : (
                  <span className="text-danger">✗ failed</span>
                )}
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
// that opens the full text in a read-only dialog. stopPropagation keeps the
// expand click from triggering the surrounding row link (CaseTable).
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

// BackLink is the "← back to dashboard" navigation.
function BackLink({ onBack }: { onBack: () => void }) {
  return (
    <p style={{ marginBottom: '0.5rem' }}>
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
  )
}
