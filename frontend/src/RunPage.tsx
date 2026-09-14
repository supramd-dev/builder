import { useEffect, useRef, useState } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import { defineMonacoTheme } from './monacoTheme'
import { formatDuration } from './gtest'
import {
  execEnvironment,
  execScript,
  getSiteConfig,
  listEnvironments,
  triggerManualTest,
  triggerManualYAML,
  type ExecResult,
  type ScriptLanguage,
  type TestEnvironment,
} from './api'

interface RunPageProps {
  onError: (message: string) => void
  // onOpenTask jumps to a freshly dispatched graph (manual test tab).
  onOpenTask?: (taskId: number) => void
}

// Default script skeletons. The first-line comment names the interpreter
// that consumes the script on the remote host.
const DEFAULT_SCRIPTS: Record<ScriptLanguage, string> = {
  bash: `#!/usr/bin/env bash
set -euo pipefail

echo "hello from $(hostname)"
uname -a
`,
  python: `#!/usr/bin/env python3
import platform

print("hello from", platform.node())
print(platform.platform())
`,
}

// Interpreter comments accepted on the first line, per language.
const INTERPRETERS: Record<ScriptLanguage, string[]> = {
  bash: ['#!/usr/bin/env bash', '#!/bin/bash', '# bash'],
  python: ['#!/usr/bin/env python3', '#!/usr/bin/env python', '# python3'],
}

// RunPage offers two ways to exercise a test environment, side by side as
// tabs: ad-hoc command/script execution, and dispatching a manual test
// graph (clone → build → unit → regression).
export default function RunPage({ onError, onOpenTask }: RunPageProps) {
  const [tab, setTab] = useState<'exec' | 'manual'>('exec')

  return (
    <div>
      <h2>Run command</h2>
      <div className="tabs" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'exec'}
          className={tab === 'exec' ? 'tab active' : 'tab'}
          onClick={() => setTab('exec')}
        >
          Exec / script
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'manual'}
          className={tab === 'manual' ? 'tab active' : 'tab'}
          onClick={() => setTab('manual')}
        >
          Manual test
        </button>
      </div>
      {tab === 'exec' ? (
        <ExecTab onError={onError} />
      ) : (
        <ManualTestTab onError={onError} onOpenTask={onOpenTask} />
      )}
    </div>
  )
}

// --- Exec / script tab (ad-hoc command & script execution) ------------------

function ExecTab({ onError }: RunPageProps) {
  const [environments, setEnvironments] = useState<TestEnvironment[]>([])
  const [loading, setLoading] = useState(true)
  const [envId, setEnvId] = useState('')
  const [language, setLanguage] = useState<ScriptLanguage>('bash')
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<ExecResult | null>(null)
  const [error, setError] = useState('')
  const editorRef = useRef<Parameters<OnMount>[0] | null>(null)

  useEffect(() => {
    let cancelled = false
    listEnvironments()
      .then((envs) => {
        if (cancelled) return
        const enabled = envs.filter((e) => e.enabled)
        setEnvironments(enabled)
        if (enabled.length > 0) {
          setEnvId((current) =>
            current && enabled.some((e) => String(e.id) === current)
              ? current
              : String(enabled[0].id),
          )
        }
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : String(err)
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  // Configure the editor theme once Monaco loads.
  const handleEditorMount: OnMount = (editor, monaco) => {
    editorRef.current = editor
    defineMonacoTheme(monaco)
  }

  const run = async () => {
    if (!envId) return
    const script = editorRef.current?.getValue() ?? ''
    if (!script.trim()) return
    setRunning(true)
    setError('')
    setResult(null)
    try {
      // Scripts run through the script pipeline (first-line interpreter
      // comment); plain one-liners still go through the command endpoint.
      const firstLine = script.trimStart().split('\n', 1)[0].trim()
      const isScript = /^#!|^#\s/.test(firstLine)
      const res = isScript
        ? await execScript(Number(envId), language, script)
        : await execEnvironment(Number(envId), script)
      setResult(res)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
    } finally {
      setRunning(false)
    }
  }

  const switchLanguage = (lang: ScriptLanguage) => {
    setLanguage(lang)
    setResult(null)
    const editor = editorRef.current
    if (!editor) return
    const current = editor.getValue()
    // Replace the content with the new skeleton only when the editor is
    // empty or still holds an untouched default of the other language.
    const isDefault = Object.values(DEFAULT_SCRIPTS).includes(current)
    if (!current.trim() || isDefault) {
      editor.setValue(DEFAULT_SCRIPTS[lang])
    }
  }

  return (
    <div>
      <p className="text-muted">
        Execute a bash command or Python script on a remote test environment
        over SSH. Name the interpreter in the first-line comment
        (e.g. <code>#!/usr/bin/env python3</code>).
      </p>

      {loading ? (
        <p>Loading environments…</p>
      ) : environments.length === 0 ? (
        <div className="alert alert-danger">
          No enabled environments available. Enable one in the
          Runner Envs page first.
        </div>
      ) : (
        <>
          <form
            onSubmit={(e) => {
              e.preventDefault()
              if (!running) void run()
            }}
          >
            <div className="run-controls">
              <div className="form-group">
                <label htmlFor="run-env">Environment</label>
                <select
                  id="run-env"
                  value={envId}
                  onChange={(e) => {
                    setEnvId(e.target.value)
                    setResult(null)
                  }}
                >
                  {environments.map((env) => (
                    <option key={env.id} value={env.id}>
                      {env.name} ({env.host})
                    </option>
                  ))}
                </select>
              </div>

              <fieldset className="form-group run-language">
                <label>Language</label>
                <div className="run-language-buttons">
                  {(Object.keys(DEFAULT_SCRIPTS) as ScriptLanguage[]).map(
                    (lang) => (
                      <label key={lang} className="run-lang-option">
                        <input
                          type="radio"
                          name="language"
                          value={lang}
                          checked={language === lang}
                          onChange={() => switchLanguage(lang)}
                        />{' '}
                        {lang === 'python' ? 'Python' : 'Bash'}
                      </label>
                    ),
                  )}
                </div>
              </fieldset>
            </div>

            <div className="form-group">
              <label htmlFor="run-editor">
                Command or script (first-line comment selects the interpreter:
                {INTERPRETERS[language].map((c) => (
                  <code key={c}> {c}</code>
                ))}
                )
              </label>
              <div className="editor-shell">
                <Editor
                  height="320px"
                  language={language === 'python' ? 'python' : 'shell'}
                  value={DEFAULT_SCRIPTS[language]}
                  onMount={handleEditorMount}
                  theme="md-builder"
                  options={{
                    minimap: { enabled: false },
                    fontSize: 13,
                    lineNumbersMinChars: 3,
                    scrollBeyondLastLine: false,
                    renderLineHighlight: 'line',
                    padding: { top: 8, bottom: 8 },
                    automaticLayout: true,
                  }}
                />
              </div>
            </div>

            <button
              type="submit"
              className="btn btn-primary"
              disabled={running}
            >
              {running ? 'Running…' : 'Run'}
            </button>
          </form>
        </>
      )}

      {error && <div className="alert alert-danger">{error}</div>}

      {result && (
        <div style={{ marginTop: '1rem' }}>
          <h3>Result</h3>
          <p>
            <strong>Status:</strong>{' '}
            {result.success ? (
              <span style={{ color: 'var(--success)' }}>
                Success (exit code {result.exitCode})
              </span>
            ) : (
              <span style={{ color: 'var(--danger)' }}>
                Failed (exit code {result.exitCode})
              </span>
            )}{' '}
            · {formatDuration(result.durationMilliSeconds)}
          </p>
          {result.stdout && (
            <>
              <h4>stdout</h4>
              <pre className="output">{result.stdout}</pre>
            </>
          )}
          {result.stderr && (
            <>
              <h4>stderr</h4>
              <pre className="output">{result.stderr}</pre>
            </>
          )}
        </div>
      )}
    </div>
  )
}

// --- Manual test tab (dispatch build/unit/regression graphs) ---------------

const DEFAULT_MANUAL_BUILD = 'cmake . && cmake --build . -j8' // placeholder example only

// splitPaths turns the comma/space-separated results-file input into a list
// for the API (a run can produce several results files); a single entry is
// sent as a scalar to keep the request readable.
function splitPaths(input: string): string | string[] | undefined {
  const parts = input
    .split(/[,\s]+/)
    .map((p) => p.trim())
    .filter((p) => p !== '')
  if (parts.length === 0) return undefined
  return parts.length === 1 ? parts[0] : parts
}

// YAMLTriggerSection dispatches the md-builder.yaml matrix of one ref:
// a text input (branch / tag / commit id, empty = HEAD) and a button — the
// webhook flow (clone, yaml parse, environment matching, graphs) on demand.
function YAMLTriggerSection({
  repo,
  onError,
}: {
  repo: string
  onError: (message: string) => void
}) {
  const [ref, setRef] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [result, setResult] = useState<Awaited<ReturnType<typeof triggerManualYAML>> | null>(null)
  const [error, setError] = useState('')

  const submit = async () => {
    if (submitting) return
    setSubmitting(true)
    setError('')
    setResult(null)
    try {
      const res = await triggerManualYAML(ref.trim())
      setResult(res)
      if (res.dispatchError) {
        setError(res.dispatchError)
        onError(res.dispatchError)
      }
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form
      className="yaml-trigger"
      onSubmit={(e) => {
        e.preventDefault()
        if (!submitting) void submit()
      }}
    >
      <fieldset className="form-group">
        <label>Run the md-builder.yaml matrix of a branch / tag / commit</label>
        <div className="yaml-trigger-row">
          <input
            type="text"
            value={ref}
            onChange={(e) => setRef(e.target.value)}
            placeholder="main / v1.2.0 / a commit id — empty = HEAD"
            aria-label="Branch, tag or commit id"
          />
          <button type="submit" className="btn btn-primary" disabled={submitting}>
            {submitting ? 'Dispatching…' : 'Test yaml matrix'}
          </button>
        </div>
        <small className="text-muted">
          Resolves the ref on{' '}
          {repo ? <code>{repo}</code> : 'the site-configured code repository'}, reads the
          md-builder.yaml at it and dispatches one task graph per matching
          environment — exactly like a webhook push (the same commit requeues
          the same graphs). Results appear on the dashboard.
        </small>
      </fieldset>

      {error && <div className="alert alert-danger">{error}</div>}

      {result && !error && (
        <p>
          Dispatched {result.jobsCreated} task graph{result.jobsCreated === 1 ? '' : 's'} for{' '}
          <code>{result.commitSha.slice(0, 12)}</code>
          {result.entriesSkipped > 0 && <> ({result.entriesSkipped} entr{result.entriesSkipped === 1 ? 'y' : 'ies'} matched no environment)</>}
          .
        </p>
      )}
    </form>
  )
}

// ManualTestTab dispatches a user-configured test: one repository (default:
// the site config's code repository), an optional ref and the three stage
// commands, run as task graphs (clone → build → unit → regression) by the
// scheduler on the selected environments.
function ManualTestTab({ onError, onOpenTask }: RunPageProps) {
  const [environments, setEnvironments] = useState<TestEnvironment[]>([])
  const [selected, setSelected] = useState<Set<number>>(new Set())
  const [loading, setLoading] = useState(true)
  const [repo, setRepo] = useState('')
  const [ref, setRef] = useState('')
  const [buildCommand, setBuildCommand] = useState('')
  const [unitCommand, setUnitCommand] = useState('')
  const [unitResults, setUnitResults] = useState('')
  const [regressionCommand, setRegressionCommand] = useState('')
  const [regressionResults, setRegressionResults] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [error, setError] = useState('')
  const [dispatched, setDispatched] = useState<number[]>([])

  useEffect(() => {
    let cancelled = false
    Promise.all([listEnvironments(), getSiteConfig().catch(() => null)])
      .then(([envs, cfg]) => {
        if (cancelled) return
        const enabled = envs.filter((e) => e.enabled)
        setEnvironments(enabled)
        setSelected(new Set(enabled.map((e) => e.id))) // default: all
        if (cfg) setRepo(cfg.codeRepo)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : String(err)
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  const toggleEnv = (id: number) => {
    setSelected((current) => {
      const next = new Set(current)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const submit = async () => {
    if (submitting || selected.size === 0) return
    setSubmitting(true)
    setError('')
    setDispatched([])
    try {
      const res = await triggerManualTest({
        repo,
        ref,
        buildCommand,
        unitCommand,
        unitResults: splitPaths(unitResults),
        regressionCommand,
        regressionResults: splitPaths(regressionResults),
        environmentIds: [...selected],
      })
      setDispatched(res.roots.map((r) => r.taskId))
      if (res.roots.length > 0 && onOpenTask) onOpenTask(res.roots[0].taskId)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <div>
      <YAMLTriggerSection repo={repo} onError={onError} />

      <p className="text-muted">
        Or dispatch a test of one repository on the selected environments with
        custom stage commands: the task graph (clone → build → unit test →
        regression test) is queued and run by the scheduler. It shows up on
        the dashboard marked <code>manual</code>.{' '}
        {repo ? (
          <>
            Default repository: <code>{repo}</code>
          </>
        ) : (
          <em>No code repository configured in the site settings — enter one below.</em>
        )}
      </p>

      {loading ? (
        <p>Loading environments…</p>
      ) : environments.length === 0 ? (
        <div className="alert alert-danger">
          No enabled environments available. Enable one in the
          Runner Envs page first.
        </div>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (!submitting) void submit()
          }}
        >
          <div className="run-controls">
            <div className="form-group">
              <label htmlFor="manual-repo">Repository (empty = site default)</label>
              <input
                id="manual-repo"
                type="text"
                value={repo}
                onChange={(e) => setRepo(e.target.value)}
                placeholder="https://gitlab.com/group/code"
              />
            </div>

            <div className="form-group">
              <label htmlFor="manual-ref">Branch, tag or commit (optional)</label>
              <input
                id="manual-ref"
                type="text"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="main / v1.2.0 / a commit id — empty = HEAD"
              />
            </div>
          </div>

          <fieldset className="form-group">
            <label>Stages — an empty stage is skipped</label>
            <div className="manual-stages">
              <div className="form-group">
                <label htmlFor="manual-build">Build command</label>
                <textarea
                  id="manual-build"
                  className="form-control"
                  rows={2}
                  value={buildCommand}
                  onChange={(e) => setBuildCommand(e.target.value)}
                  placeholder={DEFAULT_MANUAL_BUILD}
                  style={{
                    fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                    fontSize: '0.875rem',
                  }}
                />
                <small className="text-muted">Empty skips the build stage.</small>
              </div>
              <div className="form-group">
                <label htmlFor="manual-unit">Unit test command</label>
                <textarea
                  id="manual-unit"
                  className="form-control"
                  rows={2}
                  value={unitCommand}
                  onChange={(e) => setUnitCommand(e.target.value)}
                  placeholder="ctest -L unit"
                  style={{
                    fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                    fontSize: '0.875rem',
                  }}
                />
                <input
                  id="manual-unit-results"
                  type="text"
                  value={unitResults}
                  onChange={(e) => setUnitResults(e.target.value)}
                  placeholder="results file(s), e.g. build/test_detail.xml, build/extra.json (optional)"
                  style={{ marginTop: '0.375rem' }}
                />
              </div>
              <div className="form-group">
                <label htmlFor="manual-reg">Regression test command</label>
                <textarea
                  id="manual-reg"
                  className="form-control"
                  rows={2}
                  value={regressionCommand}
                  onChange={(e) => setRegressionCommand(e.target.value)}
                  placeholder="python3 run.py"
                  style={{
                    fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                    fontSize: '0.875rem',
                  }}
                />
                <input
                  id="manual-reg-results"
                  type="text"
                  value={regressionResults}
                  onChange={(e) => setRegressionResults(e.target.value)}
                  placeholder="results file(s), e.g. regression_results.json (optional)"
                  style={{ marginTop: '0.375rem' }}
                />
              </div>
            </div>
          </fieldset>

          <fieldset className="form-group">
            <label>Environments (all enabled selected by default)</label>
            <div className="manual-envs">
              {environments.map((env) => (
                <label key={env.id} className="run-lang-option">
                  <input
                    type="checkbox"
                    checked={selected.has(env.id)}
                    onChange={() => toggleEnv(env.id)}
                  />{' '}
                  {env.name}
                  {env.tags && <span className="text-muted"> ({env.tags})</span>}
                </label>
              ))}
            </div>
          </fieldset>

          <button
            type="submit"
            className="btn btn-primary"
            disabled={submitting || selected.size === 0}
          >
            {submitting ? 'Dispatching…' : `Dispatch test (${selected.size} environment${selected.size === 1 ? '' : 's'})`}
          </button>
        </form>
      )}

      {error && <div className="alert alert-danger">{error}</div>}

      {dispatched.length > 0 && !error && (
        <p>
          Dispatched {dispatched.length} task graph{dispatched.length === 1 ? '' : 's'}:{' '}
          {dispatched.map((id, i) => (
            <span key={id}>
              {i > 0 && ', '}
              <a
                href="#"
                onClick={(e) => {
                  e.preventDefault()
                  if (onOpenTask) onOpenTask(id)
                }}
              >
                task #{id}
              </a>
            </span>
          ))}
        </p>
      )}
    </div>
  )
}

// (The Monaco theme is defined in monacoTheme.ts, shared with the message
// dialog's read-only viewer.)
