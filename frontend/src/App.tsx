import { useEffect, useState } from 'react'
import './App.css'

// Minimal API client. Cookies (session token) are sent automatically since
// /api is same-origin (via the Vite dev proxy or the Go server).
async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const data = (await res.json().catch(() => null)) as (T & { error?: string }) | null
  if (!res.ok) {
    throw new Error(data?.error || `Request failed (${res.status})`)
  }
  return data as T
}

interface Me {
  username: string
  email: string
}

function App() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)

  // Check for an existing session on mount.
  useEffect(() => {
    api<Me>('/api/me')
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setLoadingMe(false))
  }, [])

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setSubmitting(true)
    try {
      const user = await api<Me>('/api/login', {
        method: 'POST',
        body: JSON.stringify({ username, password }),
      })
      setMe(user)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Login failed')
    } finally {
      setSubmitting(false)
    }
  }

  async function handleLogout() {
    try {
      await api('/api/logout', { method: 'POST' })
    } catch {
      // Ignore logout errors; clear local state regardless.
    }
    setMe(null)
    setUsername('')
    setPassword('')
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
            {me ? (
              <>
                <span className="text-ink">{me.username}</span>
                <button type="button" onClick={handleLogout} className="hover:text-accent">
                  Log out
                </button>
              </>
            ) : (
              <a href="#" className="text-accent">
                Log in
              </a>
            )}
          </nav>
        </div>
      </header>

      {/* Main content */}
      <main className="flex flex-1 items-center justify-center px-4 py-12">
        {loadingMe ? (
          <p className="text-sm text-ink-muted">Loading…</p>
        ) : me ? (
          <div className="w-full max-w-sm space-y-4 border border-border bg-surface p-6">
            <h1 className="text-lg font-semibold">Signed in</h1>
            <dl className="space-y-2 text-sm">
              <div className="flex justify-between">
                <dt className="text-ink-muted">Username</dt>
                <dd>{me.username}</dd>
              </div>
              <div className="flex justify-between">
                <dt className="text-ink-muted">Email</dt>
                <dd>{me.email}</dd>
              </div>
            </dl>
            <button
              type="button"
              onClick={handleLogout}
              className="w-full border border-border bg-surface-alt py-1.5 text-sm hover:bg-border"
            >
              Log out
            </button>
          </div>
        ) : (
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
              <p role="alert" className="text-sm text-danger">
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
        )}
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
