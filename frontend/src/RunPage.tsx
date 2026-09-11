import { useEffect, useRef, useState } from 'react'
import Editor, { type Monaco, type OnMount } from '@monaco-editor/react'
import {
  buildTest,
  execEnvironment,
  execScript,
  getSiteConfig,
  listEnvironments,
  triggerManualTest,
  type BuildTestResult,
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
// tabs: ad-hoc command/script execution, and an interactive build test
// (clone the site-configured code repository on the environment and run a
// build command).
export default function RunPage({ onError, onOpenTask }: RunPageProps) {
  const [tab, setTab] = useState<'exec' | 'build' | 'manual'>('exec')

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
          aria-selected={tab === 'build'}
          className={tab === 'build' ? 'tab active' : 'tab'}
          onClick={() => setTab('build')}
        >
          Build test
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
      ) : tab === 'build' ? (
        <BuildTestTab onError={onError} />
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
    defineTheme(monaco)
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
          No enabled environments available. Enable one in the user center
          first.
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
            · {result.durationMilliSeconds} ms
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

// --- Build test tab (clone + build on the environment) ----------------------

function BuildTestTab({ onError }: RunPageProps) {
  const [environments, setEnvironments] = useState<TestEnvironment[]>([])
  const [loading, setLoading] = useState(true)
  const [envId, setEnvId] = useState('')
  const [ref, setRef] = useState('')
  const [buildCommand, setBuildCommand] = useState('')
  const [repo, setRepo] = useState('')
  const [running, setRunning] = useState(false)
  const [result, setResult] = useState<BuildTestResult | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    Promise.all([listEnvironments(), getSiteConfig().catch(() => null)])
      .then(([envs, cfg]) => {
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

  const run = async () => {
    if (!envId) return
    setRunning(true)
    setError('')
    setResult(null)
    try {
      const res = await buildTest(Number(envId), buildCommand, ref)
      setResult(res)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
    } finally {
      setRunning(false)
    }
  }

  return (
    <div>
      <p className="text-muted">
        Clone the site-configured code repository on the selected environment
        (via the deploy key / token from the site settings) and run a build
        command inside it — a dry run of a matrix build before wiring it into{' '}
        <code>md-builder.yaml</code>.{' '}
        {repo ? (
          <>
            Repository: <code>{repo}</code>
          </>
        ) : (
          <em>No code repository configured in the site settings.</em>
        )}
      </p>

      {loading ? (
        <p>Loading environments…</p>
      ) : environments.length === 0 ? (
        <div className="alert alert-danger">
          No enabled environments available. Enable one in the user center
          first.
        </div>
      ) : (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            if (!running) void run()
          }}
        >
          <div className="run-controls">
            <div className="form-group">
              <label htmlFor="build-env">Environment</label>
              <select
                id="build-env"
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

            <div className="form-group">
              <label htmlFor="build-ref">Branch, tag or commit (optional)</label>
              <input
                id="build-ref"
                type="text"
                value={ref}
                onChange={(e) => setRef(e.target.value)}
                placeholder="main / v1.2.0 / a commit id"
              />
            </div>
          </div>

          <div className="form-group">
            <label htmlFor="build-command">Build command</label>
            <textarea
              id="build-command"
              className="form-control"
              rows={4}
              value={buildCommand}
              onChange={(e) => setBuildCommand(e.target.value)}
              placeholder="cmake . && cmake --build . -j8"
              style={{
                fontFamily: 'ui-monospace, Menlo, Consolas, monospace',
                fontSize: '0.875rem',
              }}
            />
            <small className="text-muted">
              Runs inside the cloned repository directory with the same
              environment as the scheduled jobs. Empty uses the CMake default.
            </small>
          </div>

          <button
            type="submit"
            className="btn btn-primary"
            disabled={running || !repo}
          >
            {running ? 'Building…' : 'Build'}
          </button>
        </form>
      )}

      {error && <div className="alert alert-danger">{error}</div>}

      {result && (
        <div style={{ marginTop: '1rem' }}>
          <h3>Result</h3>
          <p>
            <strong>Status:</strong>{' '}
            {result.success ? (
              <span style={{ color: 'var(--success)' }}>
                Build succeeded (exit code {result.exitCode})
              </span>
            ) : (
              <span style={{ color: 'var(--danger)' }}>
                Build failed (exit code {result.exitCode})
              </span>
            )}{' '}
            · {result.durationMilliSeconds} ms
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

const DEFAULT_MANUAL_BUILD = 'cmake . && cmake --build . -j8'

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
  const [regressionCommand, setRegressionCommand] = useState('')
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
        regressionCommand,
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
      <p className="text-muted">
        Dispatch a test of one repository on the selected environments: the
        task graph (clone → build → unit test → regression test) is queued
        and run by the scheduler, like a webhook-triggered push. It shows up
        on the dashboard marked <code>manual</code>.{' '}
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
          No enabled environments available. Enable one in the user center
          first.
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
                <small className="text-muted">Empty uses the CMake default.</small>
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

// defineTheme registers a sourcehut-flavored Monaco theme for light and
// dark mode. Monaco themes cannot react to media queries, so both are
// derived from the current color scheme at mount.
function defineTheme(monaco: Monaco) {
  const dark = window.matchMedia('(prefers-color-scheme: dark)').matches
  monaco.editor.defineTheme('md-builder', {
    base: dark ? 'vs-dark' : 'vs',
    inherit: true,
    rules: [
      { token: 'comment', foreground: dark ? '6a9955' : '6a737d' },
      { token: 'keyword', foreground: dark ? '3395ff' : '0640e0' },
      { token: 'string', foreground: dark ? '2bb34b' : '1a7f37' },
    ],
    colors: {
      'editor.background': dark ? '#131618' : '#ffffff',
      'editorLineNumber.foreground': dark ? '#495057' : '#adb5bd',
      'editor.lineHighlightBackground': dark ? '#1c1f23' : '#f8f9fa',
      'editorGutter.background': dark ? '#131618' : '#ffffff',
    },
  })
}
