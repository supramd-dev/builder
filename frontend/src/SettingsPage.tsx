import { useEffect, useState } from 'react'
import {
  getSiteConfig,
  updateSiteConfig,
  type SiteConfig,
} from './api'
import {
  allTimezones,
  applySiteTimezone,
  browserTimezone,
  commonTimezones,
  formatTime,
  siteTimezone,
} from './timezone'

interface SettingsPageProps {
  onError: (message: string) => void
}

// SettingsPage organizes the site-wide configuration into tabs: the code
// repository (and its credentials), the display settings (timezone), and
// the GitLab webhook reference. Test inputs live inside the code repository
// itself, so there is no separate test-input tab.
export default function SettingsPage({ onError }: SettingsPageProps) {
  const [tab, setTab] = useState<'repo' | 'display' | 'webhook'>('repo')

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
          aria-selected={tab === 'display'}
          className={tab === 'display' ? 'tab active' : 'tab'}
          onClick={() => setTab('display')}
        >
          Display
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
      </div>
      {tab === 'repo' ? (
        <RepositoryTab onError={onError} />
      ) : tab === 'display' ? (
        <DisplayTab onError={onError} />
      ) : (
        <WebhookTab />
      )}
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
  const [deployKey, setDeployKey] = useState('')
  const [deployToken, setDeployToken] = useState('')
  const [deployTokenUser, setDeployTokenUser] = useState('')
  const [clearDeployKey, setClearDeployKey] = useState(false)
  const [clearDeployToken, setClearDeployToken] = useState(false)
  const [deployKeySet, setDeployKeySet] = useState(false)
  const [deployTokenSet, setDeployTokenSet] = useState(false)
  const [timezone, setTimezone] = useState('')
  const [updatedAt, setUpdatedAt] = useState('')

  useEffect(() => {
    let cancelled = false
    getSiteConfig()
      .then((cfg: SiteConfig) => {
        if (cancelled) return
        setCodeRepo(cfg.codeRepo)
        setDeployTokenUser(cfg.deployTokenUser)
        setDeployKeySet(cfg.deployKeySet)
        setDeployTokenSet(cfg.deployTokenSet)
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
        deployKey: clearDeployKey ? '' : deployKey,
        deployToken: clearDeployToken ? '' : deployToken,
        deployTokenUser,
        // Keep the timezone as-is from this tab (the Display tab owns it).
        timezone,
        clearDeployKey,
        clearDeployToken,
      })
      setCodeRepo(cfg.codeRepo)
      setDeployTokenUser(cfg.deployTokenUser)
      setDeployKeySet(cfg.deployKeySet)
      setDeployTokenSet(cfg.deployTokenSet)
      setTimezone(cfg.timezone)
      setDeployKey('')
      setDeployToken('')
      setClearDeployKey(false)
      setClearDeployToken(false)
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
          <strong>deploy token</strong> or a{' '}
          <strong>deploy key</strong> — used both by the server (to read the
          test matrix) and by the test environments (to clone the
          repositories). For public repositories leave both empty. The token
          applies to https repository URLs; the key converts them to SSH.
          Secrets are stored server-side and never shown again.
        </p>

        <div className="form-group">
          <label htmlFor="cfg-deploy-token">
            Deploy token{' '}
            {deployTokenSet &&
              !clearDeployToken &&
              '(configured — leave blank to keep)'}
          </label>
          <input
            id="cfg-deploy-token"
            type="password"
            value={deployToken}
            onChange={(e) => {
              setDeployToken(e.target.value)
              if (e.target.value) setClearDeployToken(false)
              setSaved(false)
            }}
            placeholder={
              deployTokenSet ? '••••••••' : 'glpat-… or the token value'
            }
            autoComplete="new-password"
          />
          <small className="text-muted">
            A GitLab deploy token or personal/group access token with{' '}
            <code>read_repository</code> scope.
          </small>
        </div>

        <div className="form-group">
          <label htmlFor="cfg-deploy-token-user">
            Deploy token username (optional)
          </label>
          <input
            id="cfg-deploy-token-user"
            type="text"
            value={deployTokenUser}
            onChange={(e) => {
              setDeployTokenUser(e.target.value)
              setSaved(false)
            }}
            placeholder="oauth2 (default for access tokens)"
          />
          <small className="text-muted">
            GitLab shows this next to the token, e.g.{' '}
            <code>gitlab+deploy-token-42</code>. Leave empty for{' '}
            <code>oauth2</code>.
          </small>
        </div>

        <div className="form-group">
          <label htmlFor="cfg-deploy-key">
            Deploy key (SSH private key){' '}
            {deployKeySet &&
              !clearDeployKey &&
              '(configured — leave blank to keep)'}
          </label>
          <textarea
            id="cfg-deploy-key"
            className="form-control"
            rows={3}
            value={deployKey}
            onChange={(e) => {
              setDeployKey(e.target.value)
              if (e.target.value) setClearDeployKey(false)
              setSaved(false)
            }}
            placeholder="-----BEGIN OPENSSH PRIVATE KEY----- ..."
            style={{ fontFamily: 'monospace', fontSize: '0.875rem' }}
          />
          <small className="text-muted">
            PEM-encoded SSH private key of a GitLab deploy key (granted read
            access to the repository).
          </small>
        </div>

        {(deployKeySet || deployTokenSet) && (
          <div className="form-group">
            <label style={{ fontWeight: 'normal' }}>
              <input
                type="checkbox"
                checked={clearDeployKey}
                onChange={(e) => {
                  setClearDeployKey(e.target.checked)
                  if (e.target.checked) setDeployKey('')
                  setSaved(false)
                }}
                style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
              />
              Remove stored deploy key
            </label>
            <label style={{ fontWeight: 'normal' }}>
              <input
                type="checkbox"
                checked={clearDeployToken}
                onChange={(e) => {
                  setClearDeployToken(e.target.checked)
                  if (e.target.checked) setDeployToken('')
                  setSaved(false)
                }}
                style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
              />
              Remove stored deploy token
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

function WebhookTab() {
  return (
    <div>
      <h3>GitLab webhook</h3>
      <p className="text-muted">
        Configure a GitLab webhook (Settings → Webhooks) pointing at{' '}
        <code>/api/webhooks/gitlab</code> with the <em>Push events</em>{' '}
        trigger. Pushes to the configured code repository dispatch test jobs
        automatically.
      </p>
    </div>
  )
}
