import { useEffect, useState } from 'react'
import { Check, Copy, ExternalLink } from 'lucide-react'
import { completeSetup, getSetupState, type Me } from './api'
import { gitlabWebhooksURL } from './gitlab'

interface Props {
  onLogin: (me: Me) => void
  // Called when the site turns out to be set up after all — the login form is
  // the only thing left to show then.
  onAlreadySetUp: () => void
}

// SetupPage is the first-run guide: it replaces the login page while the
// database holds no account at all, and never appears again afterwards (the
// server decides — see GET /api/setup). Three blocks — the code repository,
// the first administrator, and where the GitLab webhook goes — then one
// confirm that stores all of it and signs the new administrator in.
//
// It is imported eagerly (like the login page) rather than lazily: on a fresh
// install it *is* the first paint, and a code-split chunk would only add a
// round trip to it.
export default function SetupPage({ onLogin, onAlreadySetUp }: Props) {
  const [codeRepo, setCodeRepo] = useState('')
  const [accessToken, setAccessToken] = useState('')
  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [copied, setCopied] = useState(false)
  // The site's own address is what GitLab has to post to; captured on mount,
  // since window is not available while the module is evaluated.
  const [origin, setOrigin] = useState('')

  useEffect(() => setOrigin(window.location.origin), [])

  const webhookURL = origin
    ? `${origin}/api/webhooks/gitlab`
    : '/api/webhooks/gitlab'
  // Live while the repository is typed: the link is only useful once the
  // address is an http(s) GitLab URL.
  const gitlabURL = gitlabWebhooksURL(codeRepo)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(webhookURL)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // Clipboard API unavailable (insecure context): the field is
      // read-selectable, so it can still be copied by hand.
    }
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    // The two passwords are compared here and nowhere else: there is no
    // password-reset path, so a typo would lock the only administrator out.
    if (password !== confirm) {
      setError('The two passwords do not match.')
      return
    }
    setError('')
    setSubmitting(true)
    try {
      // The response is the signed-in administrator: the server creates the
      // account, stores the repository and opens the session in one call.
      onLogin(
        await completeSetup({
          codeRepo,
          accessToken,
          username,
          email,
          password,
        }),
      )
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Setup failed')
      // The site may have been set up elsewhere since this page was opened —
      // another tab, another person: ask again, and hand over to the login
      // form instead of leaving up a form that can never succeed.
      try {
        if (!(await getSetupState())) onAlreadySetUp()
      } catch {
        // The check itself failed; keep the error already shown.
      }
    } finally {
      setSubmitting(false)
    }
  }

  return (
    <form className="setup" onSubmit={handleSubmit}>
      <h3>Welcome to md-builder</h3>
      <p className="text-muted">
        This site has no account yet, so there is nothing to log in with. The
        three blocks below set it up: the code repository md-builder tests, the
        administrator account that manages it, and where GitLab should send
        push events. Confirming stores all of it and signs you in — this page
        does not appear again.
      </p>

      {/* --- 1. Code repository --- */}
      <section className="setup-step">
        <h4>1. Code repository</h4>
        <p className="text-muted">
          The GitLab repository under test. md-builder clones it on the server
          for every dispatch, so the test environments need no access to it.
        </p>

        <div className="form-group">
          <label htmlFor="setup-repo">Repository</label>
          <input
            id="setup-repo"
            type="text"
            value={codeRepo}
            onChange={(e) => setCodeRepo(e.target.value)}
            placeholder="https://gitlab.example.com/group/code"
            autoComplete="off"
            required
          />
        </div>

        <div className="form-group">
          <label htmlFor="setup-token">Access token (optional)</label>
          <input
            id="setup-token"
            type="password"
            value={accessToken}
            onChange={(e) => setAccessToken(e.target.value)}
            placeholder="glpat-… — leave empty for a public repository"
            autoComplete="off"
          />
          <small className="text-muted">
            A GitLab Project Access Token with the <code>read_repository</code>{' '}
            scope, used for every git operation (clone, ref resolution,
            md-builder.yaml). It is stored write-only: no API returns it, and
            the server redacts it from logs. It can be set or replaced later in
            Settings → Repository.
          </small>
        </div>
      </section>

      {/* --- 2. Administrator account --- */}
      <section className="setup-step">
        <h4>2. Administrator account</h4>
        <p className="text-muted">
          The first account of a site is an <strong>administrator</strong>: it
          manages the other accounts, the site configuration and the
          environments. Every account after it is created with the{' '}
          <code>adduser</code> command on the server.
        </p>

        <div className="form-group">
          <label htmlFor="setup-username">Username</label>
          <input
            id="setup-username"
            type="text"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoComplete="username"
            required
          />
        </div>

        <div className="form-group">
          <label htmlFor="setup-email">Email</label>
          <input
            id="setup-email"
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="email"
            required
          />
        </div>

        <div className="form-group">
          <label htmlFor="setup-password">Password</label>
          <input
            id="setup-password"
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="new-password"
            minLength={8}
            required
          />
          <small className="text-muted">At least 8 characters.</small>
        </div>

        <div className="form-group">
          <label htmlFor="setup-confirm">Password again</label>
          <input
            id="setup-confirm"
            type="password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            autoComplete="new-password"
            minLength={8}
            required
          />
        </div>
      </section>

      {/* --- 3. GitLab webhook (a hint, nothing to fill in) --- */}
      <section className="setup-step">
        <h4>3. GitLab webhook</h4>
        <p className="text-muted">
          Nothing to fill in here — this is where the webhook goes. Adding it
          makes md-builder dispatch a test run whenever code is pushed, instead
          of only when a run is started by hand.
        </p>

        <div className="form-group">
          <label>Webhook URL</label>
          <div className="webhook-copy-row">
            <input
              type="text"
              readOnly
              value={webhookURL}
              onFocus={(e) => e.target.select()}
            />
            <button
              type="button"
              className="btn"
              onClick={copy}
              title="Copy to clipboard"
            >
              {copied ? <Check size={14} /> : <Copy size={14} />}
              {copied ? 'Copied' : 'Copy'}
            </button>
          </div>
          <small className="text-muted">
            In GitLab: <em>project → Settings → Webhooks</em>. Paste this URL,
            enable the <em>Push events</em>, <em>Tag push events</em> and{' '}
            <em>Merge request events</em> triggers, and paste the{' '}
            <em>Secret token</em> shown in Settings → Webhook once you are
            signed in (administrators only — it authenticates GitLab, since a
            webhook cannot carry a session cookie).
          </small>
        </div>

        {gitlabURL ? (
          <p>
            <a href={gitlabURL} target="_blank" rel="noreferrer">
              <ExternalLink size={14} style={{ verticalAlign: '-2px' }} /> Open
              the GitLab webhook settings of this repository
            </a>
          </p>
        ) : (
          <p className="text-muted">
            Fill in the repository above (an http(s) GitLab address) to link
            straight to its webhook settings.
          </p>
        )}
      </section>

      {error && (
        <div className="alert alert-danger" role="alert">
          {error}
        </div>
      )}

      <div className="setup-actions">
        <button type="submit" className="btn btn-primary" disabled={submitting}>
          {submitting ? 'Setting up…' : 'Finish setup and sign in'}
        </button>
        <small className="text-muted">
          The repository and the account are stored together: if anything is
          rejected, nothing is written.
        </small>
      </div>
    </form>
  )
}
