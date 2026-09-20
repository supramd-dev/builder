import { useEffect, useState } from 'react'
import { useLocation } from 'react-router'
import { api, getGitLabEnabled, gitlabStartURL, type Me } from './api'

interface Props {
  onLogin: (me: Me) => void
}

// gitlabNotice turns the status the GitLab callback redirected back with
// (?gitlab=…) into something to show the user. The list mirrors the statuses
// the server sends, and every one of them is a refusal: a sign-in that
// succeeds lands on the dashboard instead of coming back here. Anything else
// in the parameter — a word from an older version, or something a visitor
// typed — shows nothing. Only the two alert styles the stylesheet defines are
// used.
function gitlabNotice(status: string): { kind: 'danger' | 'warning'; text: string } | null {
  switch (status) {
    case 'pending':
      return {
        kind: 'warning',
        text:
          'Your GitLab account was registered, but it is waiting for an ' +
          'administrator to approve it. You will be able to sign in with ' +
          'GitLab once it is approved.',
      }
    case 'disabled':
      return { kind: 'danger', text: 'This account has been disabled.' }
    case 'email_taken':
      return {
        kind: 'danger',
        text:
          'An account with this email address already exists. Sign in with ' +
          'that account’s password instead, or ask an administrator to ' +
          'change its email address first.',
      }
    case 'no_email':
      return {
        kind: 'danger',
        text:
          'GitLab did not return an email address for your account, so it ' +
          'cannot be registered. Set a public or verified email on GitLab ' +
          'and try again.',
      }
    case 'denied':
      return { kind: 'warning', text: 'GitLab sign-in was cancelled.' }
    case 'unavailable':
      return {
        kind: 'danger',
        text: 'GitLab sign-in is not available on this site right now.',
      }
    case 'error':
      return {
        kind: 'danger',
        text: 'GitLab sign-in failed. Please try again.',
      }
    default:
      return null
  }
}

export default function LoginPage({ onLogin }: Props) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [gitlabEnabled, setGitlabEnabled] = useState(false)

  // The status a GitLab sign-in redirected back with. It arrives in the URL
  // the server sends the browser to (/ #/?gitlab=…), so the message survives
  // the round trip through GitLab — there is no local state to carry it.
  const { search } = useLocation()
  const notice = gitlabNotice(new URLSearchParams(search).get('gitlab') ?? '')

  useEffect(() => {
    let cancelled = false
    getGitLabEnabled().then((enabled) => {
      if (!cancelled) setGitlabEnabled(enabled)
    })
    return () => {
      cancelled = true
    }
  }, [])

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
    <form onSubmit={handleSubmit} style={{ maxWidth: '28rem' }} noValidate>
      <h3>Log in</h3>

      {notice && (
        <div className={`alert alert-${notice.kind}`} role="alert">
          {notice.text}
        </div>
      )}

      <div className="form-group">
        <label htmlFor="login-username">Username</label>
        <input
          id="login-username"
          type="text"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          autoComplete="username"
        />
      </div>

      <div className="form-group">
        <label htmlFor="login-password">Password</label>
        <input
          id="login-password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
        />
      </div>

      {error && (
        <div className="alert alert-danger" role="alert">
          {error}
        </div>
      )}

      <button type="submit" className="btn btn-primary" disabled={submitting}>
        {submitting ? 'Logging in…' : 'Log in'}
      </button>

      {gitlabEnabled && (
        <>
          <p className="text-muted" style={{ margin: '1rem 0 0.5rem' }}>
            No password, or a GitLab account? Sign in with GitLab — a new
            account is registered on first use, and an administrator has to
            approve it before it can sign in.
          </p>
          {/* A full-page navigation, not a fetch: the server answers with a
              redirect to GitLab, which the browser itself must follow. */}
          <a href={gitlabStartURL} className="btn btn-default">
            Sign in with GitLab
          </a>
        </>
      )}

      <p className="text-muted" style={{ marginTop: '1rem' }}>
        A test execution and results platform for scientific computing software
        such as molecular dynamics.
      </p>
    </form>
  )
}
