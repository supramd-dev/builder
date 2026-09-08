import { useState } from 'react'
import './App.css'

function App() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  // Static placeholder: connect to real backend auth later
  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!username || !password) {
      setError('Please enter username and password')
      return
    }
    setError('')
    setSubmitting(true)
    // TODO: POST /api/login
    await new Promise((r) => setTimeout(r, 600))
    setSubmitting(false)
    setError('Demo mode: backend not connected')
  }

  return (
    <div className="flex min-h-svh flex-col">
      {/* Top navigation bar */}
      <header className="border-b border-border">
        <div className="mx-auto flex h-12 w-full max-w-4xl items-center justify-between px-4">
          <a href="/" className="font-mono text-sm font-semibold tracking-tight">
            md-builder
          </a>
          <nav className="flex items-center gap-4 text-sm text-ink-muted">
            <a href="#" className="hover:text-accent">
              Register
            </a>
            <a href="#" className="text-accent">
              Log in
            </a>
          </nav>
        </div>
      </header>

      {/* Login form */}
      <main className="flex flex-1 items-center justify-center px-4 py-12">
        <form
          onSubmit={handleSubmit}
          className="w-full max-w-sm space-y-5 border border-border bg-surface p-6"
          noValidate
        >
          <h1 className="text-lg font-semibold">Log in</h1>

          <label className="block space-y-1">
            <span className="text-sm text-ink-muted">Username</span>
            <input
              type="text"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="username"
              className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>

          <label className="block space-y-1">
            <span className="text-sm text-ink-muted">Password</span>
            <input
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoComplete="current-password"
              className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
            />
          </label>

          {error && (
            <p role="alert" className="text-sm text-accent">
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={submitting}
            className="w-full border border-accent bg-accent py-1.5 text-sm text-white hover:bg-accent-hover disabled:opacity-50"
          >
            {submitting ? 'Logging in…' : 'Log in'}
          </button>

          <p className="text-xs text-ink-muted">
            A test execution and results platform for scientific computing
            software such as molecular dynamics.
          </p>
        </form>
      </main>

      {/* Footer */}
      <footer className="border-t border-border">
        <div className="mx-auto flex h-10 w-full max-w-4xl items-center justify-center px-4 text-xs text-ink-muted">
          md-builder · Scientific computing test platform
        </div>
      </footer>
    </div>
  )
}

export default App
