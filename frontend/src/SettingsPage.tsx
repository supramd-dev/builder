import { useEffect, useState } from 'react'
import {
  getSiteConfig,
  updateSiteConfig,
  type SiteConfig,
} from './api'

interface SettingsPageProps {
  onError: (message: string) => void
}

// SettingsPage lets logged-in users edit the site-wide repository
// configuration: the code repository under test, the test input repository
// and the branch/commit of the test inputs to run against.
export default function SettingsPage({ onError }: SettingsPageProps) {
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')
  const [codeRepo, setCodeRepo] = useState('')
  const [testInputRepo, setTestInputRepo] = useState('')
  const [testRepoRef, setTestRepoRef] = useState('')
  const [updatedAt, setUpdatedAt] = useState('')

  useEffect(() => {
    let cancelled = false
    getSiteConfig()
      .then((cfg: SiteConfig) => {
        if (cancelled) return
        setCodeRepo(cfg.codeRepo)
        setTestInputRepo(cfg.testInputRepo)
        setTestRepoRef(cfg.testRepoRef)
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
      const cfg = await updateSiteConfig({ codeRepo, testInputRepo, testRepoRef })
      setCodeRepo(cfg.codeRepo)
      setTestInputRepo(cfg.testInputRepo)
      setTestRepoRef(cfg.testRepoRef)
      setUpdatedAt(cfg.updatedAt)
      setSaved(true)
    } catch (err: unknown) {
      setSaveError(err instanceof Error ? err.message : String(err))
    } finally {
      setSaving(false)
    }
  }

  if (loading) {
    return (
      <div>
        <h2>Settings</h2>
        <p className="text-muted">Loading…</p>
      </div>
    )
  }

  return (
    <div>
      <h2>Settings</h2>
      {saveError && <div className="alert alert-danger">{saveError}</div>}

      <h3>Repository configuration</h3>

      <div className="alert alert-warning" role="alert">
        <strong>Note:</strong> currently only <strong>GitLab</strong>{' '}
        repositories are expected — gitlab.com or any self-hosted GitLab
        instance (repository locations are not validated, so use the correct
        URL for your instance).
      </div>

      <p className="text-muted">
        The <strong>code repository</strong> holds the code under test. The{' '}
        <strong>test input repository</strong> holds the inputs used to
        exercise it; tests run against the branch or commit id configured
        below.
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

        <div className="form-group">
          <label htmlFor="cfg-input-repo">Test input repository</label>
          <input
            id="cfg-input-repo"
            type="text"
            value={testInputRepo}
            onChange={(e) => {
              setTestInputRepo(e.target.value)
              setSaved(false)
            }}
            placeholder="https://gitlab.com/group/test-inputs"
          />
        </div>

        <div className="form-group">
          <label htmlFor="cfg-test-repo-ref">
            Test input branch or commit id
          </label>
          <input
            id="cfg-test-repo-ref"
            type="text"
            value={testRepoRef}
            onChange={(e) => {
              setTestRepoRef(e.target.value)
              setSaved(false)
            }}
            placeholder="main, v1.2.0 or a commit id"
          />
          <small className="text-muted">
            Tests use this version of the test input repository.
          </small>
        </div>

        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? 'Saving…' : 'Save'}
        </button>{' '}
        {saved && (
          <span style={{ color: 'var(--success)' }}>Saved.</span>
        )}
        {updatedAt && (
          <span className="text-muted" style={{ marginLeft: '0.75rem' }}>
            Last updated {updatedAt}
          </span>
        )}
      </form>

      <h3>GitLab webhook</h3>
      <p className="text-muted">
        Configure a GitLab webhook (Settings → Webhooks) pointing at{' '}
        <code>/api/webhooks/gitlab</code> with the <em>Push events</em>{' '}
        trigger. Push events are received and logged; automatic test runs are
        not wired up yet.
      </p>
    </div>
  )
}
