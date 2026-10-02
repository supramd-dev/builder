import { useState } from 'react'
import {
  createEnvironment,
  updateEnvironment,
  type EnvironmentInput,
  type TestEnvironment,
} from './api'

interface Props {
  existing?: TestEnvironment | null
  onSaved: () => void
  onCancel: () => void
}

export default function EnvironmentForm({ existing, onSaved, onCancel }: Props) {
  const [form, setForm] = useState<EnvironmentInput>({
    name: existing?.name ?? '',
    host: existing?.host ?? '',
    username: existing?.username ?? '',
    tags: existing?.tags ?? [],
    description: existing?.description ?? '',
    envScript: existing?.envScript ?? '',
    privateKey: '',
    enabled: existing?.enabled ?? true,
  })
  // The whitelist is edited as one block of text (commas, spaces or newlines
  // separate names) because that is the shape the server stores it in. It is
  // kept outside `form` because it is optional there: an untouched empty box
  // on a new environment means "start me on the built-in default list", which
  // the server applies when the field is absent.
  const [allowedEnvVars, setAllowedEnvVars] = useState(
    existing?.allowedEnvVars.join(', ') ?? '',
  )
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)

  const isEdit = !!existing
  // The built-in list the server would fall back to, known here only from a
  // row on this page — it is offered as a hint, never sent as a value.
  const defaults = existing?.allowedEnvVarsDefault ?? []

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setSaving(true)
    try {
      const payload: EnvironmentInput = { ...form }
      if (isEdit && existing) {
        // The box holds this environment's effective list, so sending it
        // pins that list as the environment's own — including the built-in
        // default, when that is what it was showing.
        payload.allowedEnvVars = allowedEnvVars
        await updateEnvironment(existing.id, payload)
      } else if (allowedEnvVars.trim() === '') {
        await createEnvironment(payload)
      } else {
        payload.allowedEnvVars = allowedEnvVars
        await createEnvironment(payload)
      }
      onSaved()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Save failed')
    } finally {
      setSaving(false)
    }
  }

  return (
    <form onSubmit={handleSubmit}>
      <h3>{isEdit ? 'Edit environment' : 'New environment'}</h3>

      <div className="form-group">
        <label htmlFor="env-name">Name</label>
        <input
          id="env-name"
          type="text"
          value={form.name}
          onChange={(e) => setForm({ ...form, name: e.target.value })}
          placeholder="cpu-node-1"
        />
      </div>

      <div className="form-group">
        <label htmlFor="env-host">Host</label>
        <input
          id="env-host"
          type="text"
          value={form.host}
          onChange={(e) => setForm({ ...form, host: e.target.value })}
          placeholder="192.168.1.10 or host:22"
        />
      </div>

      <div className="form-group">
        <label htmlFor="env-username">Login username</label>
        <input
          id="env-username"
          type="text"
          value={form.username}
          onChange={(e) => setForm({ ...form, username: e.target.value })}
          placeholder="runner"
        />
      </div>

      <div className="form-group">
        <label htmlFor="env-key">
          Private key {isEdit && '(leave blank to keep current)'}
        </label>
        <textarea
          id="env-key"
          className="form-control"
          rows={3}
          value={form.privateKey}
          onChange={(e) => setForm({ ...form, privateKey: e.target.value })}
          placeholder="-----BEGIN OPENSSH PRIVATE KEY----- ..."
          style={{ fontFamily: 'monospace', fontSize: '0.875rem' }}
        />
      </div>

      <div className="form-group">
        <label htmlFor="env-tags">Tags</label>
        <input
          id="env-tags"
          type="text"
          value={form.tags.join(', ')}
          onChange={(e) =>
            setForm({
              ...form,
              tags: e.target.value
                .split(/[\s,]+/)
                .filter((t) => t.length > 0)
                .map((t) => t.toLowerCase()),
            })
          }
          placeholder="cpu, mpi, cuda"
        />
        <span className="text-muted">
          Comma or space separated, lowercase. Matrix entries in
          md-builder.yaml select environments by these tags.
        </span>
      </div>

      <div className="form-group">
        <label htmlFor="env-desc">Description</label>
        <input
          id="env-desc"
          type="text"
          value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })}
          placeholder="CPU pool / MPI / GPU environment notes"
        />
      </div>

      <div className="form-group">
        <label htmlFor="env-script">
          Environment setup script (md-builder-env-*.sh)
        </label>
        <textarea
          id="env-script"
          className="form-control"
          rows={5}
          value={form.envScript}
          onChange={(e) => setForm({ ...form, envScript: e.target.value })}
          placeholder={'module load gcc/13\nexport CXX=g++'}
          style={{ fontFamily: 'monospace', fontSize: '0.875rem' }}
        />
        <span className="text-muted">
          Sourced before every build / unit / regression command on this
          environment. Leave empty to skip (a warning is logged). The script
          is written to the task dir on the remote host.
        </span>
      </div>

      <div className="form-group">
        <label htmlFor="env-allowed-vars">Expandable host variables</label>
        <textarea
          id="env-allowed-vars"
          className="form-control"
          rows={2}
          value={allowedEnvVars}
          onChange={(e) => setAllowedEnvVars(e.target.value)}
          placeholder={defaults.length > 0 ? defaults.join(', ') : 'HOME, USER, PATH'}
          style={{ fontFamily: 'monospace', fontSize: '0.875rem' }}
        />
        <span className="text-muted">
          The host environment variables a <code>variables:</code> value in
          md-builder.yaml may expand on this machine — names only, separated by
          commas, spaces or newlines. Anything else is kept as written and
          reported in the task log.{' '}
          {isEdit
            ? 'Saving pins this list to this environment.'
            : 'Leave empty to start from the built-in default list.'}
        </span>
      </div>

      <div className="form-group">
        <label style={{ fontWeight: 'normal' }}>
          <input
            type="checkbox"
            checked={form.enabled ?? true}
            onChange={(e) => setForm({ ...form, enabled: e.target.checked })}
            style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
          />
          Enabled (jobs may be dispatched to this environment)
        </label>
      </div>

      {error && (
        <div className="alert alert-danger" role="alert">
          {error}
        </div>
      )}

      <div style={{ display: 'flex', gap: '0.25rem' }}>
        <button type="button" className="btn btn-default" onClick={onCancel}>
          Cancel
        </button>
        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? 'Saving…' : isEdit ? 'Save changes' : 'Create'}
        </button>
      </div>
    </form>
  )
}
