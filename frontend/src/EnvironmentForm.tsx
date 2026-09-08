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

const empty: EnvironmentInput = {
  name: '',
  host: '',
  username: '',
  privateKey: '',
  description: '',
}

export default function EnvironmentForm({ existing, onSaved, onCancel }: Props) {
  const [form, setForm] = useState<EnvironmentInput>({
    ...empty,
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
    <form
      onSubmit={handleSubmit}
      className="space-y-4 border border-border bg-surface p-5"
    >
      <h2 className="text-base font-semibold">
        {isEdit ? 'Edit environment' : 'New environment'}
      </h2>

      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <label className="block space-y-1">
          <span className="text-xs text-ink-muted">Name</span>
          <input
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            placeholder="cpu-node-1"
            className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
          />
        </label>

        <label className="block space-y-1">
          <span className="text-xs text-ink-muted">Host</span>
          <input
            value={form.host}
            onChange={(e) => setForm({ ...form, host: e.target.value })}
            placeholder="192.168.1.10 or host:22"
            className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
          />
        </label>

        <label className="block space-y-1">
          <span className="text-xs text-ink-muted">Login username</span>
          <input
            value={form.username}
            onChange={(e) => setForm({ ...form, username: e.target.value })}
            placeholder="runner"
            className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
          />
        </label>

        <label className="block space-y-1">
          <span className="text-xs text-ink-muted">
            Private key {isEdit && '(leave blank to keep current)'}
          </span>
          <textarea
            value={form.privateKey}
            onChange={(e) => setForm({ ...form, privateKey: e.target.value })}
            placeholder="-----BEGIN OPENSSH PRIVATE KEY----- ..."
            rows={3}
            className="w-full resize-y border border-border bg-surface px-2 py-1.5 font-mono text-xs outline-none focus:border-accent"
          />
        </label>
      </div>

      <label className="block space-y-1">
        <span className="text-xs text-ink-muted">Description</span>
        <input
          value={form.description}
          onChange={(e) => setForm({ ...form, description: e.target.value })}
          placeholder="CPU pool / MPI / GPU environment notes"
          className="w-full border border-border bg-surface px-2 py-1.5 text-sm outline-none focus:border-accent"
        />
      </label>

      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}

      <div className="flex justify-end gap-2">
        <button
          type="button"
          onClick={onCancel}
          className="border border-border bg-surface-alt px-3 py-1.5 text-sm hover:bg-border"
        >
          Cancel
        </button>
        <button
          type="submit"
          disabled={saving}
          className="border border-accent bg-accent px-3 py-1.5 text-sm text-white hover:bg-accent-hover disabled:opacity-50"
        >
          {saving ? 'Saving…' : isEdit ? 'Save changes' : 'Create'}
        </button>
      </div>
    </form>
  )
}
