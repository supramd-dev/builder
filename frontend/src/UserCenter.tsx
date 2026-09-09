import { useCallback, useEffect, useState } from 'react'
import {
  deleteEnvironment,
  listEnvironments,
  setEnvironmentEnabled,
  testEnvironment,
  type ConnectivityResult,
  type Me,
  type TestEnvironment,
} from './api'
import EnvironmentForm from './EnvironmentForm'

interface Props {
  me: Me
}

type Editing =
  | { mode: 'none' }
  | { mode: 'create' }
  | { mode: 'edit'; env: TestEnvironment }

export default function UserCenter({ me }: Props) {
  const [envs, setEnvs] = useState<TestEnvironment[] | null>(null)
  const [loadError, setLoadError] = useState('')
  const [editing, setEditing] = useState<Editing>({ mode: 'none' })
  const [testResults, setTestResults] = useState<Record<number, ConnectivityResult>>({})
  const [testing, setTesting] = useState<Set<number>>(new Set())
  const [toggling, setToggling] = useState<Set<number>>(new Set())

  const refresh = useCallback(async () => {
    try {
      const list = await listEnvironments()
      setEnvs(list)
      setLoadError('')
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : 'Failed to load')
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  async function handleDelete(env: TestEnvironment) {
    if (!window.confirm(`Delete environment "${env.name}"?`)) return
    try {
      await deleteEnvironment(env.id)
      await refresh()
    } catch (err) {
      setLoadError(err instanceof Error ? err.message : 'Delete failed')
    }
  }

  async function handleTest(env: TestEnvironment) {
    setTesting((s) => new Set(s).add(env.id))
    try {
      const res = await testEnvironment(env.id)
      setTestResults((r) => ({ ...r, [env.id]: res }))
    } catch (err) {
      setTestResults((r) => ({
        ...r,
        [env.id]: {
          success: false,
          message: err instanceof Error ? err.message : 'Test failed',
        },
      }))
    } finally {
      setTesting((s) => {
        const next = new Set(s)
        next.delete(env.id)
        return next
      })
    }
  }

  async function handleToggleEnabled(env: TestEnvironment) {
    const next = !env.enabled
    setToggling((s) => new Set(s).add(env.id))
    // Optimistically update; revert on failure.
    setEnvs((list) =>
      list ? list.map((e) => (e.id === env.id ? { ...e, enabled: next } : e)) : list,
    )
    try {
      await setEnvironmentEnabled(env.id, next)
    } catch (err) {
      setEnvs((list) =>
        list ? list.map((e) => (e.id === env.id ? { ...e, enabled: !next } : e)) : list,
      )
      setLoadError(err instanceof Error ? err.message : 'Toggle failed')
    } finally {
      setToggling((s) => {
        const copy = new Set(s)
        copy.delete(env.id)
        return copy
      })
    }
  }

  return (
    <div>
      <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline' }}>
        <h2>User center</h2>
        <span className="text-muted">
          {me.username} · {me.email}
        </span>
      </div>

      <h3>Test environments</h3>
      <p>
        Environments are the machines where MD programs run: CPU nodes, MPI
        variants, GPU machines. Connectivity is verified over SSH with the
        stored private key.
      </p>

      {editing.mode === 'none' && (
        <p>
          <button
            type="button"
            className="btn btn-primary"
            onClick={() => setEditing({ mode: 'create' })}
          >
            New environment
          </button>
        </p>
      )}

      {loadError && (
        <div className="alert alert-danger" role="alert">
          {loadError}
        </div>
      )}

      {editing.mode !== 'none' && (
        <EnvironmentForm
          existing={editing.mode === 'edit' ? editing.env : null}
          onSaved={() => {
            setEditing({ mode: 'none' })
            refresh()
          }}
          onCancel={() => setEditing({ mode: 'none' })}
        />
      )}

      {envs === null ? (
        <p className="text-muted">Loading…</p>
      ) : envs.length === 0 ? (
        <div className="event">
          <p className="text-muted" style={{ margin: 0 }}>
            No environments yet. Create one to get started.
          </p>
        </div>
      ) : (
        <table className="table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Host</th>
              <th>User</th>
              <th>Tags</th>
              <th>Description</th>
              <th>Enabled</th>
              <th>Updated</th>
              <th>Status</th>
              <th className="text-right">Actions</th>
            </tr>
          </thead>
          <tbody>
            {envs.map((env) => {
              const res = testResults[env.id]
              const isTesting = testing.has(env.id)
              return (
                <tr key={env.id}>
                  <td>{env.name}</td>
                  <td>
                    <code>{env.host}</code>
                  </td>
                  <td>{env.username}</td>
                  <td>
                    {env.tags.length > 0 ? (
                      <span className="env-tags">
                        {env.tags.map((tag) => (
                          <span key={tag} className="env-tag">
                            {tag}
                          </span>
                        ))}
                      </span>
                    ) : (
                      <span className="text-muted">—</span>
                    )}
                  </td>
                  <td className="text-muted">{env.description}</td>
                  <td>
                    <button
                      type="button"
                      className="btn btn-sm"
                      disabled={toggling.has(env.id)}
                      onClick={() => handleToggleEnabled(env)}
                      style={{
                        background: env.enabled ? 'var(--success)' : 'var(--btn-default-bg)',
                        color: env.enabled ? '#fff' : 'var(--muted)',
                        borderColor: env.enabled ? 'var(--success)' : 'var(--border)',
                      }}
                      title={env.enabled ? 'Disable this environment' : 'Enable this environment'}
                    >
                      {toggling.has(env.id) ? '…' : env.enabled ? 'Enabled' : 'Disabled'}
                    </button>
                  </td>
                  <td className="text-muted">
                    {new Date(env.updatedAt).toLocaleString()}
                  </td>
                  <td>
                    {res ? (
                      res.success ? (
                        <span className="text-success">✓ reachable</span>
                      ) : (
                        <span className="text-danger">✗ unreachable</span>
                      )
                    ) : (
                      <span className="text-muted">—</span>
                    )}
                  </td>
                  <td className="text-right">
                    <div style={{ display: 'flex', gap: '0.25rem', justifyContent: 'flex-end' }}>
                      <button
                        type="button"
                        className="btn btn-default btn-sm"
                        disabled={isTesting}
                        onClick={() => handleTest(env)}
                      >
                        {isTesting ? 'Testing…' : 'Test'}
                      </button>
                      <button
                        type="button"
                        className="btn btn-primary btn-sm"
                        onClick={() => setEditing({ mode: 'edit', env })}
                      >
                        Edit
                      </button>
                      <button
                        type="button"
                        className="btn btn-danger btn-sm"
                        onClick={() => handleDelete(env)}
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      )}

      {envs !== null && envs.length > 0 && (
        <p className="text-muted" style={{ fontSize: '0.8rem' }}>
          {envs.map((env) => {
            const res = testResults[env.id]
            return res ? (
              <span key={env.id} style={{ display: 'block' }}>
                <strong>{env.name}</strong>: {res.message}
              </span>
            ) : null
          })}
        </p>
      )}
    </div>
  )
}
