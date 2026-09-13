import { useEffect } from 'react'
import Editor, { type OnMount } from '@monaco-editor/react'
import { X } from 'lucide-react'
import { defineMonacoTheme } from './monacoTheme'

interface Props {
  title: string
  message: string
  onClose: () => void
}

// MessageDialog is a modal that shows a long text (a test case's failure
// message) in a read-only Monaco editor — the table cell only carries the
// truncated first line.
export default function MessageDialog({ title, message, onClose }: Props) {
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
      <div className="dialog" role="dialog" aria-modal="true" aria-label={title}>
        <div className="dialog-head">
          <strong className="dialog-title" title={title}>
            {title}
          </strong>
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
          <Editor
            height="100%"
            language="plaintext"
            value={message}
            theme="md-builder"
            onMount={handleMount}
            options={{
              readOnly: true,
              minimap: { enabled: false },
              fontSize: 13,
              lineNumbersMinChars: 3,
              scrollBeyondLastLine: true,
              renderLineHighlight: 'line',
              padding: { top: 8, bottom: 8 },
              automaticLayout: true,
              wordWrap: 'on',
            }}
          />
        </div>
      </div>
    </div>
  )
}
