import { useEffect, useState } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import { X, LoaderCircle, Download } from 'lucide-react'
import { defineMonacoTheme } from './monacoTheme'
import { getTestArtifact, type TestArtifactContent } from './api'

interface Props {
  artifactId: number
  onClose: () => void
  onError: (message: string) => void
}

// ArtifactPreviewDialog shows one stored artifact in a read-only Monaco
// editor: a "view" click in the artifacts table fetches the content and
// renders it with the language picked from the file name (gtest XML,
// JSON — including *.plot.json figures — yaml, logs, plain text), so
// result files and figure sources can be inspected without downloading.
export default function ArtifactPreviewDialog({ artifactId, onClose, onError }: Props) {
  const [artifact, setArtifact] = useState<TestArtifactContent | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  useEffect(() => {
    let cancelled = false
    setLoading(true)
    setError('')
    getTestArtifact(artifactId)
      .then((a) => {
        if (!cancelled) setArtifact(a)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : 'Failed to load artifact'
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [artifactId, onError])

  // Close on Escape.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const handleMount: OnMount = (_editor, monaco) => {
    defineMonacoTheme(monaco)
  }

  return (
    <div
      className="dialog-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div
        className="dialog dialog-wide"
        role="dialog"
        aria-modal="true"
        aria-label={artifact ? `Preview ${artifact.name}` : 'Artifact preview'}
      >
        <div className="dialog-head">
          <strong className="dialog-title" title={artifact?.name ?? ''}>
            {artifact?.name ?? 'Artifact'}
          </strong>
          {artifact && (
            <a
              className="dialog-action"
              href={`/api/test-artifacts/${artifact.id}/download`}
              title="Download the file"
            >
              <Download size={14} />
            </a>
          )}
          <button
            type="button"
            className="dialog-close"
            aria-label="Close"
            onClick={onClose}
          >
            <X size={14} />
          </button>
        </div>
        <div className="dialog-body">
          {loading && (
            <p className="text-muted">
              <LoaderCircle size={14} className="spin" /> Loading…
            </p>
          )}
          {error && <div className="alert alert-danger">{error}</div>}
          {!loading && !error && artifact && (
            <Editor
              height="100%"
              language={languageOf(artifact.name)}
              value={artifact.content}
              theme="md-builder"
              onMount={handleMount}
              options={{
                readOnly: true,
                minimap: { enabled: artifact.content.length > 8000 },
                fontSize: 13,
                lineNumbersMinChars: 3,
                scrollBeyondLastLine: true,
                renderLineHighlight: 'line',
                padding: { top: 8, bottom: 8 },
                automaticLayout: true,
                wordWrap: 'on',
              }}
            />
          )}
        </div>
      </div>
    </div>
  )
}

// languageOf picks the Monaco language from the artifact's file name.
// Unknown extensions fall back to plaintext — Monaco renders anything, but
// the matched languages get syntax highlighting and folding.
function languageOf(name: string): string {
  const lower = name.toLowerCase()
  if (lower.endsWith('.json')) return 'json'
  if (lower.endsWith('.xml')) return 'xml'
  if (lower.endsWith('.yaml') || lower.endsWith('.yml')) return 'yaml'
  if (lower.endsWith('.py')) return 'python'
  if (lower.endsWith('.sh') || lower.endsWith('.bash')) return 'shell'
  if (lower.endsWith('.md')) return 'markdown'
  if (lower.endsWith('.html')) return 'html'
  if (lower.endsWith('.js') || lower.endsWith('.ts')) return 'javascript'
  if (lower.endsWith('.c') || lower.endsWith('.h')) return 'c'
  if (lower.endsWith('.cpp') || lower.endsWith('.cc') || lower.endsWith('.hpp')) return 'cpp'
  return 'plaintext'
}
