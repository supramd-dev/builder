import { useCallback, useEffect, useState } from 'react'
import {
  deleteEnvironment,
  listEnvironments,
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

  return (
    <div className="mx-auto w-full max-w-4xl px-4 py-8">
      <div className="mb-6 flex items-baseline justify-between">
        <h1 className="text-lg font-semibold">User center</h1>
        <span className="text-sm text-ink-muted">
          {me.username} · {me.email}
        </span>
      </div>

      <section className="space-y-3">
        <div className="flex items-center justify-between">
          <h2 className="text-base font-semibold">Test environments</h2>
          <button
            type="button"
            onClick={() => setEditing({ mode: 'create' })}
            className="border border-accent bg-accent px-3 py-1.5 text-sm text-white hover:bg-accent-hover"
          >
            New environment
          </button>
        </div>
        <p className="text-sm text-ink-muted">
          Environments are the machines where MD programs run: CPU nodes,
          MPI variants, GPU machines. Connectivity is verified over SSH with
          the stored private key.
        </p>

        {loadError && (
          <p role="alert" className="text-sm text-danger">
            {loadError}
          </p>
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
          <p className="text-sm text-ink-muted">Loading…</p>
        ) : envs.length === 0 ? (
          <p className="border border-border bg-surface-alt p-4 text-sm text-ink-muted">
            No environments yet. Create one to get started.
          </p>
        ) : (
          <table className="w-full border border-border text-sm">
            <thead>
              <tr className="border-b border-border bg-surface-alt text-left text-xs uppercase tracking-wide text-ink-muted">
                <th className="px-3 py-2 font-medium">Name</th>
                <th className="px-3 py-2 font-medium">Host</th>
                <th className="px-3 py-2 font-medium">User</th>
                <th className="px-3 py-2 font-medium">Description</th>
                <th className="px-3 py-2 font-medium">Updated</th>
                <th className="px-3 py-2 font-medium">Status</th>
                <th className="px-3 py-2 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {envs.map((env) => {
                const res = testResults[env.id]
                const isTesting = testing.has(env.id)
                return (
                  <tr key={env.id} className="border-b border-border last:border-b-0">
                    <td className="px-3 py-2 font-medium">{env.name}</td>
                    <td className="px-3 py-2 font-mono text-xs">{env.host}</td>
                    <td className="px-3 py-2">{env.username}</td>
                    <td className="px-3 py-2 text-ink-muted">{env.description}</td>
                    <td className="px-3 py-2 text-xs text-ink-muted">
                      {new Date(env.updatedAt).toLocaleString()}
                    </td>
                    <td className="px-3 py-2">
                      {res ? (
                        <span className={res.success ? 'text-success' : 'text-danger'}>
                          {res.success ? '✓ reachable' : '✗ unreachable'}
                        </span>
                      ) : (
                        <span className="text-ink-muted">—</span>
                      )}
                    </td>
                    <td className="px-3 py-2 text-right">
                      <div className="flex justify-end gap-2 text-xs">
                        <button
                          type="button"
                          disabled={isTesting}
                          onClick={() => handleTest(env)}
                          className="border border-border px-2 py-1 hover:border-accent hover:text-accent disabled:opacity-50"
                        >
                          {isTesting ? 'Testing…' : 'Test'}
                        </button>
                        <button
                          type="button"
                          onClick={() => setEditing({ mode: 'edit', env })}
                          className="border border-accent bg-accent px-2 py-1 text-white hover:bg-accent-hover"
                        >
                          Edit
                        </button>
                        <button
                          type="button"
                          onClick={() => handleDelete(env)}
                          className="border border-danger bg-danger px-2 py-1 text-white hover:bg-danger-hover"
                        >
                          Delete
                        </button>
                      </div>
                      {res && !res.success && (
                        <p className="mt-1 max-w-xs truncate text-left text-xs text-danger" title={res.message}>
                          {res.message}
                        </p>
                      )}
                      {res && res.success && (
                        <p className="mt-1 max-w-xs truncate text-left text-xs text-ink-muted" title={res.message}>
                          {res.message}
                        </p>
                      )}
                    </td>
                  </tr>
                )
              })}
            </tbody>
          </table>
        )}
      </section>
    </div>
  )
}
