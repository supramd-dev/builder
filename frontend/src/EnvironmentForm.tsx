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
    description: existing?.description ?? '',
    privateKey: '',
  })
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)

  const isEdit = !!existing

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError('')
    setSaving(true)
    try {
      if (isEdit && existing) {
        await updateEnvironment(existing.id, form)
      } else {
        await createEnvironment(form)
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
        <label htmlFor="env-desc">Description</label>
        <input
          id="env-desc"
          type="text"
          value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })}
          placeholder="CPU pool / MPI / GPU environment notes"
        />
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
