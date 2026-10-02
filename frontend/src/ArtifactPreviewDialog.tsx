// The artifact preview: one stored file, as source or as the thing it is.
//
// A "View" click in the artifacts table opens this. Most files have one
// form — the read-only Monaco editor, with the language picked from the
// name (gtest XML, JSON including plot figures, yaml, logs, plain text).
// Two kinds have a second: an HTML page renders in a sandboxed frame, and a
// Markdown document renders with the app's own renderer. Those open in the
// rendered form, and a Preview | Source switch in the head goes back to the
// bytes.
//
// The rendered HTML frame points at GET /api/test-artifacts/{id}/raw, the
// artifact endpoint's view-don't-save form, and is sandboxed twice: the
// server sends `Content-Security-Policy: sandbox` (no allow-same-origin)
// and the frame repeats the same list. The page is a build product, so its
// scripts are not trusted with this session — they run (that is what makes a
// Plotly export worth framing), but from an opaque origin that reads no
// cookie or storage and whose requests carry no credentials.
//
// Markdown needs no sandbox: markdown.tsx builds React nodes, so nothing in
// the file is ever interpreted as markup.

import { useEffect, useState } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import { X, LoaderCircle, Download, ExternalLink } from 'lucide-react'
import { defineMonacoTheme } from './monacoTheme'
import { getTestArtifact, testArtifactDownloadUrl, testArtifactRawUrl, type TestArtifactRef } from './api'
import { isHtmlArtifact, isMarkdownArtifact } from './artifacts'
import { Markdown } from './markdown'

interface Props {
  artifact: TestArtifactRef
  onClose: () => void
  onError: (message: string) => void
}

// sandbox is the policy a framed page runs under — the list the server sets
// in the response's Content-Security-Policy, repeated on the frame so the
// page stays sandboxed whatever the response headers turn out to be.
const sandbox = 'allow-scripts allow-popups allow-downloads allow-forms allow-modals'

type Mode = 'rendered' | 'source'

export default function ArtifactPreviewDialog({ artifact, onClose, onError }: Props) {
  const page = isHtmlArtifact(artifact)
  const markdown = isMarkdownArtifact(artifact)
  const [mode, setMode] = useState<Mode>(page || markdown ? 'rendered' : 'source')

  // The bytes are fetched only for the views that read them: the editor,
  // and the Markdown renderer. A framed page streams straight from /raw, so
  // previewing an HTML artifact costs no fetch at all.
  const needContent = mode === 'source' || markdown
  const [content, setContent] = useState('')
  const [loading, setLoading] = useState(needContent)
  const [error, setError] = useState('')

  useEffect(() => {
    if (!needContent) return
    let cancelled = false
    setLoading(true)
    setError('')
    getTestArtifact(artifact.id)
      .then((a) => {
        if (!cancelled) setContent(a.content)
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
  }, [artifact.id, needContent, onError])

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
        aria-label={`Preview ${artifact.name}`}
      >
        <div className="dialog-head">
          <strong className="dialog-title" title={artifact.name}>
            {artifact.name}
          </strong>
          {(page || markdown) && (
            <div className="dialog-modes">
              <button
                type="button"
                aria-pressed={mode === 'rendered'}
                onClick={() => setMode('rendered')}
              >
                Preview
              </button>
              <button
                type="button"
                aria-pressed={mode === 'source'}
                onClick={() => setMode('source')}
              >
                Source
              </button>
            </div>
          )}
          {page && (
            <a
              className="dialog-action"
              href={testArtifactRawUrl(artifact.id)}
              target="_blank"
              rel="noopener noreferrer"
              title="Open the rendered page in a new tab (sandboxed too)"
            >
              <ExternalLink size={14} />
            </a>
          )}
          <a
            className="dialog-action"
            href={testArtifactDownloadUrl(artifact.id)}
            title="Download the file"
          >
            <Download size={14} />
          </a>
          <button
            type="button"
            className="dialog-close"
            aria-label="Close"
            onClick={onClose}
          >
            <X size={14} />
          </button>
        </div>
        {page && mode === 'rendered' && (
          <p className="dialog-note">
            Framed in a sandbox: the page's own scripts run, but it reads no
            cookie or storage and its requests carry no credentials. Download
            the file to open it without one.
          </p>
        )}
        <div className="dialog-body">
          {needContent && loading && (
            <p className="text-muted">
              <LoaderCircle size={14} className="spin" /> Loading…
            </p>
          )}
          {needContent && error && <div className="alert alert-danger">{error}</div>}
          {mode === 'source' && !loading && !error && (
            <Editor
              height="100%"
              language={languageOf(artifact.name)}
              value={content}
              theme="md-builder"
              onMount={handleMount}
              options={{
                readOnly: true,
                minimap: { enabled: content.length > 8000 },
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
          {mode === 'rendered' && page && (
            <iframe
              className="artifact-frame"
              src={testArtifactRawUrl(artifact.id)}
              title={artifact.name}
              sandbox={sandbox}
            />
          )}
          {mode === 'rendered' && markdown && !loading && !error && (
            <div className="artifact-doc">
              <Markdown source={content} />
            </div>
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
  if (lower.endsWith('.md') || lower.endsWith('.markdown')) return 'markdown'
  if (lower.endsWith('.html')) return 'html'
  if (lower.endsWith('.js') || lower.endsWith('.ts')) return 'javascript'
  if (lower.endsWith('.c') || lower.endsWith('.h')) return 'c'
  if (lower.endsWith('.cpp') || lower.endsWith('.cc') || lower.endsWith('.hpp')) return 'cpp'
  return 'plaintext'
}
