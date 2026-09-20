import { useEffect, useState } from 'react'
import { Check, Copy, ExternalLink, RefreshCw } from 'lucide-react'
import {
  cachedSiteConfig,
  getSiteConfig,
  rotateWebhookToken,
  updateSiteConfig,
  type Me,
  type SiteConfig,
} from './api'
import AccountPanel from './AccountPanel'
import { gitlabWebhooksURL } from './gitlab'
import {
  allTimezones,
  applySiteTimezone,
  browserTimezone,
  commonTimezones,
  formatTime,
  siteTimezone,
} from './timezone'

interface SettingsPageProps {
  me: Me
  onMeChange: (me: Me) => void
  onError: (message: string) => void
}

// SettingsPage organizes the site-wide configuration into tabs, in this
// order: the code repository (and its credentials), the GitLab webhook
// reference, the GitLab sign-in integration (a preview, administrators
// only), the account tab — everyone edits their own account there, and an
// administrator also manages the other accounts — and the display settings
// (timezone). Test inputs live inside the code repository itself, so there
// is no separate test-input tab.
export default function SettingsPage({ me, onMeChange, onError }: SettingsPageProps) {
  const [tab, setTab] = useState<
    'repo' | 'webhook' | 'gitlab' | 'account' | 'display'
  >('repo')
  const isAdmin = me.role === 'admin'

  return (
    <div>
      <h2>Settings</h2>
      <div className="tabs" role="tablist">
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'repo'}
          className={tab === 'repo' ? 'tab active' : 'tab'}
          onClick={() => setTab('repo')}
        >
          Repository
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'webhook'}
          className={tab === 'webhook' ? 'tab active' : 'tab'}
          onClick={() => setTab('webhook')}
        >
          Webhook
        </button>
        {/* The GitLab sign-in integration holds credentials, so the tab is
            rendered only for an administrator. (The Repository tab, which
            also holds a token, is currently visible to everyone — that is a
            separate question; this one does not have to inherit it.) */}
        {isAdmin && (
          <button
            type="button"
            role="tab"
            aria-selected={tab === 'gitlab'}
            className={tab === 'gitlab' ? 'tab active' : 'tab'}
            onClick={() => setTab('gitlab')}
          >
            GitLab
          </button>
        )}
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'account'}
          className={tab === 'account' ? 'tab active' : 'tab'}
          onClick={() => setTab('account')}
        >
          {/* The same tab, named for what it holds: only an administrator
              gets the account list, so only an administrator sees "Users". */}
          {isAdmin ? 'Users' : 'Account'}
        </button>
        <button
          type="button"
          role="tab"
          aria-selected={tab === 'display'}
          className={tab === 'display' ? 'tab active' : 'tab'}
          onClick={() => setTab('display')}
        >
          Display
        </button>
      </div>
      {tab === 'repo' ? (
        <RepositoryTab onError={onError} />
      ) : tab === 'webhook' ? (
        <WebhookTab me={me} />
      ) : tab === 'gitlab' ? (
        <GitLabTab />
      ) : tab === 'account' ? (
        <AccountPanel me={me} onMeChange={onMeChange} onError={onError} />
      ) : (
        <DisplayTab onError={onError} />
      )}
    </div>
  )
}

// --- GitLab integration tab (preview) ----------------------------------------

// GitLabTab is a placeholder for the planned GitLab sign-in integration: it
// shows the shape of the configuration that will be needed (the instance
// address, an API token, and the OAuth application a future "Sign in with
// GitLab" button will use), with every control disabled.
//
// Nothing here is wired up: there is no store column, no API field and no
// request, so the fields are deliberately disabled rather than merely
// unsaved — an editable form that silently drops its input would read as a
// configuration that works. Wiring it up is a separate change (store
// columns, a migration, and write-only token handling on the API, following
// the same rules as the Repository tab's credentials).
function GitLabTab() {
  return (
    <div>
      <h3>GitLab integration</h3>

      <div className="alert alert-warning" role="alert">
        <strong>Not implemented yet.</strong> This tab is a preview of the
        planned <strong>GitLab sign-in</strong> integration — the fields below
        are shown disabled because nothing reads or stores them. To sign in
        today, use the account created by the server administrator.
      </div>

      <p className="text-muted">
        Once implemented, this configuration will let users authenticate with
        their GitLab account instead of a local password. It is separate from
        the Repository tab: that token reads the code under test, whereas this
        one identifies <em>people</em>.
      </p>

      {/* No onSubmit: the form is inert, and the Save button is disabled, so
          submitting is not reachable. */}
      <form style={{ maxWidth: '36rem' }}>
        <div className="form-group">
          <label htmlFor="gl-site-address">Site address</label>
          <input
            id="gl-site-address"
            type="text"
            defaultValue="https://gitlab.com"
            placeholder="https://gitlab.example.com"
            disabled
          />
          <small className="text-muted">
            The GitLab instance users sign in against — gitlab.com or a
            self-hosted instance.
          </small>
        </div>

        <div className="form-group">
          <label htmlFor="gl-api-token">API token</label>
          <input
            id="gl-api-token"
            type="password"
            placeholder="glpat-… (the token value)"
            autoComplete="off"
            disabled
          />
          <small className="text-muted">
            Used to look up GitLab users (and, optionally, their group
            membership) when someone signs in.
          </small>
        </div>

        <h4>OAuth application (for sign-in)</h4>
        <p className="text-muted">
          Create an application in GitLab under{' '}
          <em>Admin Area → Applications</em> with the <code>read_user</code>{' '}
          scope, then copy its credentials here.
        </p>

        <div className="form-group">
          <label htmlFor="gl-app-id">Application ID</label>
          <input
            id="gl-app-id"
            type="text"
            placeholder="The application's Client ID"
            disabled
          />
        </div>

        <div className="form-group">
          <label htmlFor="gl-app-secret">Application secret</label>
          <input
            id="gl-app-secret"
            type="password"
            placeholder="••••••••"
            autoComplete="off"
            disabled
          />
          <small className="text-muted">
            Stored server-side and never shown again, like the Repository
            tab's token.
          </small>
        </div>

        <div className="form-group">
          <label
            style={{ display: 'flex', alignItems: 'center', gap: '0.5rem' }}
          >
            <input type="checkbox" disabled />
            Allow sign-in with GitLab
          </label>
          <small className="text-muted">
            When off, only local accounts can sign in.
          </small>
        </div>

        <button type="submit" className="btn btn-primary" disabled>
          Save
        </button>
      </form>
    </div>
  )
}

// --- Repository tab ----------------------------------------------------------

function RepositoryTab({ onError }: { onError: (message: string) => void }) {
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const [codeRepo, setCodeRepo] = useState('')
  const [accessToken, setAccessToken] = useState('')
  const [clearAccessToken, setClearAccessToken] = useState(false)
  const [accessTokenSet, setAccessTokenSet] = useState(false)
  const [secretToken, setSecretToken] = useState('')
  const [clearSecretToken, setClearSecretToken] = useState(false)
  const [secretTokenSet, setSecretTokenSet] = useState(false)
  const [timezone, setTimezone] = useState('')
  const [updatedAt, setUpdatedAt] = useState('')

  useEffect(() => {
    let cancelled = false
    getSiteConfig()
      .then((cfg: SiteConfig) => {
        if (cancelled) return
        setCodeRepo(cfg.codeRepo)
        setAccessTokenSet(cfg.accessTokenSet)
        setSecretTokenSet(cfg.secretTokenSet)
        setTimezone(cfg.timezone)
        setUpdatedAt(cfg.updatedAt)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : String(err)
        setSaveError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setSaveError('')
    setSaved(false)
    try {
      const cfg = await updateSiteConfig({
        codeRepo,
        accessToken: clearAccessToken ? '' : accessToken,
        // Keep the timezone as-is from this tab (the Display tab owns it).
        timezone,
        clearAccessToken,
        secretToken: clearSecretToken ? '' : secretToken,
        clearSecretToken,
      })
      setCodeRepo(cfg.codeRepo)
      setAccessTokenSet(cfg.accessTokenSet)
      setSecretTokenSet(cfg.secretTokenSet)
      setTimezone(cfg.timezone)
      setAccessToken('')
      setClearAccessToken(false)
      setSecretToken('')
      setClearSecretToken(false)
      setUpdatedAt(cfg.updatedAt)
      setSaved(true)
    } catch (err: unknown) {
      setSaveError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) {
    return <p className="text-muted">Loading…</p>
  }

  return (
    <div>
      {saveError && <div className="alert alert-danger">{saveError}</div>}

      <h3>Repository configuration</h3>

      <div className="alert alert-warning" role="alert">
        <strong>Note:</strong> currently only <strong>GitLab</strong>{' '}
        repositories are expected — gitlab.com or any self-hosted GitLab
        instance (repository locations are not validated, so use the correct
        URL for your instance).
      </div>

      <p className="text-muted">
        The <strong>code repository</strong> holds the code under test and
        its test inputs (they live inside the repository, or the repository
        fetches them itself).
      </p>

      <form onSubmit={save} style={{ maxWidth: '36rem' }}>
        <div className="form-group">
          <label htmlFor="cfg-code-repo">Code repository</label>
          <input
            id="cfg-code-repo"
            type="text"
            value={codeRepo}
            onChange={(e) => {
              setCodeRepo(e.target.value)
              setSaved(false)
            }}
            placeholder="https://gitlab.com/group/code"
          />
        </div>

        <h4>Repository credentials (optional)</h4>
        <p className="text-muted">
          For <strong>private</strong> repositories, configure a GitLab{' '}
          <strong>Project Access Token</strong> with the{' '}
          <code>read_repository</code> scope. It is used by the server only
          (to read the test matrix and to clone the repository before
          uploading it to the test environments — the environments
          themselves need no repository access). For public repositories
          leave it empty. SSH repository locations are cloned over https
          with the token. The secret is stored server-side and never shown
          again.
        </p>

        <div className="form-group">
          <label htmlFor="cfg-access-token">
            Project Access Token{' '}
            {accessTokenSet &&
              !clearAccessToken &&
              '(configured — leave blank to keep)'}
          </label>
          <input
            id="cfg-access-token"
            type="password"
            value={accessToken}
            onChange={(e) => {
              setAccessToken(e.target.value)
              if (e.target.value) setClearAccessToken(false)
              setSaved(false)
            }}
            placeholder={
              accessTokenSet ? '••••••••' : 'glpat-… (the token value)'
            }
            autoComplete="new-password"
          />
          <small className="text-muted">
            Create one in GitLab under <em>Settings → Access Tokens</em>{' '}
            with the <code>read_repository</code> scope.
          </small>
        </div>

        {accessTokenSet && (
          <div className="form-group">
            <label style={{ fontWeight: 'normal' }}>
              <input
                type="checkbox"
                checked={clearAccessToken}
                onChange={(e) => {
                  setClearAccessToken(e.target.checked)
                  if (e.target.checked) setAccessToken('')
                  setSaved(false)
                }}
                style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
              />
              Remove stored access token
            </label>
          </div>
        )}

        <h4>Secret token for commands (optional)</h4>
        <p className="text-muted">
          A site-wide secret exported to <strong>every</strong> stage command
          (build, unit, regression cases) as the environment variable{' '}
          <code>MD_SECRET_TOKEN</code>. Use it in{' '}
          <code>md-builder.yaml</code> commands to authenticate against
          internal services — package mirrors, artifact stores, licensed
          software servers — without hardcoding credentials in the
          repository. The value is stored server-side, never shown again,
          and scrubbed (<code>REDACTED</code>) from task logs if a command
          echoes it.
        </p>

        <div className="form-group">
          <label htmlFor="cfg-secret-token">
            Secret token{' '}
            {secretTokenSet &&
              !clearSecretToken &&
              '(configured — leave blank to keep)'}
          </label>
          <input
            id="cfg-secret-token"
            type="password"
            value={secretToken}
            onChange={(e) => {
              setSecretToken(e.target.value)
              if (e.target.value) setClearSecretToken(false)
              setSaved(false)
            }}
            placeholder={
              secretTokenSet ? '••••••••' : 'any secret string'
            }
            autoComplete="new-password"
          />
          <small className="text-muted">
            Referenced as{' '}
            <code>
              $MD_SECRET_TOKEN
            </code>{' '}
            in md-builder.yaml commands, e.g.{' '}
            <code>curl -H &quot;Authorization: Bearer $MD_SECRET_TOKEN&quot; …</code>
          </small>
        </div>

        {secretTokenSet && (
          <div className="form-group">
            <label style={{ fontWeight: 'normal' }}>
              <input
                type="checkbox"
                checked={clearSecretToken}
                onChange={(e) => {
                  setClearSecretToken(e.target.checked)
                  if (e.target.checked) setSecretToken('')
                  setSaved(false)
                }}
                style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
              />
              Remove stored secret token
            </label>
          </div>
        )}

        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? 'Saving…' : 'Save'}
        </button>{' '}
        {saved && (
          <span style={{ color: 'var(--success)' }}>Saved.</span>
        )}
        {updatedAt && (
          <span className="text-muted" style={{ marginLeft: '0.75rem' }}>
            Last updated {formatTime(updatedAt)}
          </span>
        )}
      </form>
    </div>
  )
}

// --- Display tab (timezone) ---------------------------------------------------

function DisplayTab({ onError }: { onError: (message: string) => void }) {
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const [codeRepo, setCodeRepo] = useState('')
  const [timezone, setTimezone] = useState('')
  const [updatedAt, setUpdatedAt] = useState('')
  const browser = browserTimezone()

  useEffect(() => {
    let cancelled = false
    getSiteConfig()
      .then((cfg: SiteConfig) => {
        if (cancelled) return
        setCodeRepo(cfg.codeRepo)
        setTimezone(cfg.timezone)
        setUpdatedAt(cfg.updatedAt)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : String(err)
        setSaveError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setSaveError('')
    setSaved(false)
    try {
      const cfg = await updateSiteConfig({ codeRepo, timezone })
      setCodeRepo(cfg.codeRepo)
      setTimezone(cfg.timezone)
      setUpdatedAt(cfg.updatedAt)
      // All pages re-render their timestamps through the subscription.
      applySiteTimezone(cfg.timezone)
      setSaved(true)
    } catch (err: unknown) {
      setSaveError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) {
    return <p className="text-muted">Loading…</p>
  }

  // The picker: browser-local first, then the curated common zones, then
  // every zone the runtime knows.
  const full = allTimezones()
  const extras = full.filter((tz) => tz !== 'UTC' && !commonTimezones.includes(tz))
  const sample = formatTime(new Date().toISOString())

  return (
    <div>
      {saveError && <div className="alert alert-danger">{saveError}</div>}

      <h3>Display timezone</h3>
      <p className="text-muted">
        All times shown in md-builder (dashboards, task and run pages) are
        rendered in this timezone. It is a display setting only — logs and
        stored data keep their original timestamps.
      </p>

      <form onSubmit={save} style={{ maxWidth: '36rem' }}>
        <div className="form-group">
          <label htmlFor="cfg-timezone">Timezone</label>
          <select
            id="cfg-timezone"
            value={timezone}
            onChange={(e) => {
              setTimezone(e.target.value)
              setSaved(false)
            }}
          >
            <option value="">
              Browser local{browser ? ` (${browser})` : ''}
            </option>
            {commonTimezones.map((tz) => (
              <option key={tz} value={tz}>
                {tz}
              </option>
            ))}
            {extras.length > 0 && (
              <>
                <option disabled value="">
                  ─────────
                </option>
                {extras.map((tz) => (
                  <option key={tz} value={tz}>
                    {tz}
                  </option>
                ))}
              </>
            )}
          </select>
          <small className="text-muted">
            Currently active:{' '}
            <code>{siteTimezone() || `${browser || 'browser local'}`}</code>{' '}
            — times render as {sample}.
          </small>
        </div>

        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? 'Saving…' : 'Save'}
        </button>{' '}
        {saved && <span style={{ color: 'var(--success)' }}>Saved.</span>}
        {updatedAt && (
          <span className="text-muted" style={{ marginLeft: '0.75rem' }}>
            Last updated {formatTime(updatedAt)}
          </span>
        )}
      </form>
    </div>
  )
}

// --- Webhook tab --------------------------------------------------------------

// WebhookTab shows the webhook endpoint as a copy-ready absolute URL, the
// shared secret GitLab has to send back with every event, and a link
// straight into the GitLab project's webhook settings (the configured code
// repository, when it is an http(s) GitLab URL). GitLab should be pointed at
// it with the Push events, Tag push events and Merge request events
// triggers.
//
// The secret is generated with the site configuration and can be rotated
// here; both reading and rotating it are administrator-only, so a regular
// user sees just the URL and a note to ask one.
function WebhookTab({ me }: { me: Me }) {
  const [cfg, setCfg] = useState<SiteConfig | null>(null)
  const [origin, setOrigin] = useState('')
  const [copied, setCopied] = useState<'url' | 'token' | ''>('')
  const [rotating, setRotating] = useState(false)
  const [error, setError] = useState('')

  useEffect(() => {
    // The origin is stable for a page load; capture it once the tab mounts
    // (window is not available during module init on SSR-style tests).
    setOrigin(window.location.origin)
    let cancelled = false
    cachedSiteConfig()
      .then((loaded) => {
        if (!cancelled && loaded) setCfg(loaded)
      })
      .catch(() => {
        // The tab degrades to the path-only URL without the repo link.
      })
    return () => {
      cancelled = true
    }
  }, [])

  const webhookURL = origin ? `${origin}/api/webhooks/gitlab` : '/api/webhooks/gitlab'
  const gitlabURL = gitlabWebhooksURL(cfg?.codeRepo ?? '')
  const isAdmin = me.role === 'admin'
  const token = cfg?.webhookToken ?? ''

  const copy = async (what: 'url' | 'token', value: string) => {
    try {
      await navigator.clipboard.writeText(value)
      setCopied(what)
      window.setTimeout(() => setCopied(''), 2000)
    } catch {
      // Clipboard API unavailable (insecure context): select-then-copy is
      // the fallback users can do by hand; keep the fields read-selectable.
    }
  }

  const rotate = async () => {
    if (
      !window.confirm(
        'Generate a new webhook token? The GitLab webhook keeps failing until ' +
          'you paste the new token into its Secret token field.',
      )
    ) {
      return
    }
    setRotating(true)
    setError('')
    try {
      setCfg(await rotateWebhookToken())
    } catch (err: unknown) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setRotating(false)
    }
  }

  return (
    <div>
      <h3>GitLab webhook</h3>
      <p className="text-muted">
        Configure a GitLab webhook pointing at the URL below with the{' '}
        <em>Push events</em>, <em>Tag push events</em> and{' '}
        <em>Merge request events</em> triggers. Events targeting the
        configured code repository dispatch test jobs automatically.
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
            onClick={() => copy('url', webhookURL)}
            title="Copy to clipboard"
          >
            {copied === 'url' ? <Check size={14} /> : <Copy size={14} />}
            {copied === 'url' ? 'Copied' : 'Copy'}
          </button>
        </div>
        <small className="text-muted">
          GitLab posts to it over the network, so it cannot use a session
          cookie: it authenticates with the token below instead.
        </small>
      </div>

      {isAdmin ? (
        <div className="form-group">
          <label>Secret token</label>
          <div className="webhook-copy-row">
            <input
              type="text"
              readOnly
              value={token}
              onFocus={(e) => e.target.select()}
            />
            <button
              type="button"
              className="btn"
              onClick={() => copy('token', token)}
              title="Copy to clipboard"
            >
              {copied === 'token' ? <Check size={14} /> : <Copy size={14} />}
              {copied === 'token' ? 'Copied' : 'Copy'}
            </button>
            <button
              type="button"
              className="btn"
              onClick={rotate}
              disabled={rotating}
              title="Generate a new token"
            >
              <RefreshCw size={14} />
              {rotating ? 'Generating…' : 'Regenerate'}
            </button>
          </div>
          <small className="text-muted">
            Paste this into the webhook's <em>Secret token</em> field in
            GitLab; every event must carry it back in{' '}
            <code>X-Gitlab-Token</code>, and events without it are rejected.
            This is not the secret token of the Repository tab: that one is
            handed to your build scripts as <code>MD_SECRET_TOKEN</code> and
            is never shown here. Regenerating breaks the GitLab webhook until
            the new value is saved there.
          </small>
          {error && <div className="alert alert-danger">{error}</div>}
        </div>
      ) : (
        <p className="text-muted">
          The webhook secret is shown to administrators only. Ask an
          administrator to copy it into the GitLab webhook's{' '}
          <em>Secret token</em> field.
        </p>
      )}

      {gitlabURL ? (
        <p>
          <a href={gitlabURL} target="_blank" rel="noreferrer">
            <ExternalLink size={14} style={{ verticalAlign: '-2px' }} />{' '}
            Open the GitLab webhook settings of the configured repository
          </a>
        </p>
      ) : (
        <p className="text-muted">
          Set the code repository (Repository tab) to link directly to its
          GitLab webhook settings.
        </p>
      )}
    </div>
  )
}
