import { useState } from 'react'
import { api, type Me } from './api'

interface Props {
  onLogin: (me: Me) => void
}

export default function LoginPage({ onLogin }: Props) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setSubmitting(true)
    try {
      const me = await api<Me>('/api/login', {
        method: 'POST',
        body: JSON.stringify({ username, password }),
      })
      onLogin(me)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Login failed')
    } finally {
      setSubmitting(false)
    }
  }

  return (
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
        A test execution and results platform for scientific computing software
        such as molecular dynamics.
      </p>
    </form>
  )
}
