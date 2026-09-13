import type { Monaco } from '@monaco-editor/react'

// defineMonacoTheme registers the sourcehut-flavored Monaco theme for light
// and dark mode. Monaco themes cannot react to media queries, so the base is
// derived from the current color scheme at mount. Shared by every editor
// instance (the script runner, the message dialog, ...).
export function defineMonacoTheme(monaco: Monaco) {
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
