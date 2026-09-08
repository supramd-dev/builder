import { useEffect, useRef, useState } from 'react'
import Editor, { type Monaco, type OnMount } from '@monaco-editor/react'
import {
  execEnvironment,
  execScript,
  listEnvironments,
  type ExecResult,
  type ScriptLanguage,
  type TestEnvironment,
} from './api'

interface RunPageProps {
  onError: (message: string) => void
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

export default function RunPage({ onError }: RunPageProps) {
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
      <h2>Run command</h2>
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
