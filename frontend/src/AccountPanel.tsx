import { useEffect, useState } from 'react'
import { X } from 'lucide-react'
import {
  listAccounts,
  updateAccount,
  type Account,
  type Me,
} from './api'
import { formatTime } from './timezone'

interface AccountPanelProps {
  me: Me
  onMeChange: (me: Me) => void
  onError: (message: string) => void
}

// AccountPanel is the Settings → Account tab: everyone edits their own
// account, and an administrator also gets the list of accounts to manage.
// Hiding the list from a regular user is presentation only — the API refuses
// the request either way.
export default function AccountPanel({ me, onMeChange, onError }: AccountPanelProps) {
  return (
    <div>
      <MyAccountForm me={me} onMeChange={onMeChange} onError={onError} />
      {me.role === 'admin' && (
        <AccountsTable me={me} onMeChange={onMeChange} onError={onError} />
      )}
    </div>
  )
}

// toMe narrows an account row to the signed-in user's shape, so an
// administrator editing their own row in the table updates the navbar too.
function toMe(a: Account): Me {
  return { id: a.id, username: a.username, email: a.email, role: a.role }
}

// --- my account --------------------------------------------------------------

function MyAccountForm({
  me,
  onMeChange,
  onError,
}: {
  me: Me
  onMeChange: (me: Me) => void
  onError: (message: string) => void
}) {
  const [username, setUsername] = useState(me.username)
  const [email, setEmail] = useState(me.email)
  const [password, setPassword] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState(false)
  const [saveError, setSaveError] = useState('')

  // The signed-in account can change under us (an administrator editing their
  // own row in the table below), so keep the form in step with it.
  useEffect(() => {
    setUsername(me.username)
    setEmail(me.email)
  }, [me.username, me.email])

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setSaveError('')
    setSaved(false)
    try {
      const updated = await updateAccount(me.id, {
        username,
        email,
        // An empty field keeps the stored password.
        password: password || undefined,
      })
      onMeChange({ id: updated.id, username: updated.username, email: updated.email, role: updated.role })
      setPassword('')
      setSaved(true)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setSaveError(msg)
      onError(msg)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div>
      {saveError && <div className="alert alert-danger">{saveError}</div>}

      <h3>My account</h3>
      <p className="text-muted">
        Your username is how you sign in; the email address identifies you in
        the account list.
      </p>

      <form onSubmit={save} style={{ maxWidth: '36rem' }}>
        <div className="form-group">
          <label htmlFor="acct-username">Username</label>
          <input
            id="acct-username"
            value={username}
            onChange={(e) => {
              setUsername(e.target.value)
              setSaved(false)
            }}
            autoComplete="username"
          />
        </div>

        <div className="form-group">
          <label htmlFor="acct-email">Email</label>
          <input
            id="acct-email"
            type="email"
            value={email}
            onChange={(e) => {
              setEmail(e.target.value)
              setSaved(false)
            }}
            autoComplete="email"
          />
        </div>

        <div className="form-group">
          <label htmlFor="acct-password">
            New password (leave blank to keep the current one)
          </label>
          <input
            id="acct-password"
            type="password"
            value={password}
            onChange={(e) => {
              setPassword(e.target.value)
              setSaved(false)
            }}
            placeholder="••••••••"
            autoComplete="new-password"
          />
          <small className="text-muted">
            At least 8 characters. Changing it signs out your other browsers,
            not this one.
          </small>
        </div>

        <button type="submit" className="btn btn-primary" disabled={saving}>
          {saving ? 'Saving…' : 'Save'}
        </button>{' '}
        {saved && <span style={{ color: 'var(--success)' }}>Saved.</span>}
      </form>
    </div>
  )
}

// --- the account list (administrators only) ----------------------------------

function AccountsTable({
  me,
  onMeChange,
  onError,
}: {
  me: Me
  onMeChange: (me: Me) => void
  onError: (message: string) => void
}) {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [busyID, setBusyID] = useState(0)
  const [editing, setEditing] = useState<Account | null>(null)

  useEffect(() => {
    let cancelled = false
    listAccounts()
      .then((list) => {
        if (!cancelled) setAccounts(list)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        const msg = err instanceof Error ? err.message : String(err)
        setError(msg)
        onError(msg)
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [onError])

  // replace swaps one row in place, so a change does not reload the list.
  const replace = (updated: Account) => {
    setAccounts((list) => list.map((a) => (a.id === updated.id ? updated : a)))
  }

  const toggleDisabled = async (account: Account) => {
    setBusyID(account.id)
    setError('')
    try {
      replace(
        await updateAccount(account.id, {
          username: account.username,
          email: account.email,
          disabled: !account.disabled,
        }),
      )
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
      onError(msg)
    } finally {
      setBusyID(0)
    }
  }

  if (loading) {
    return <p className="text-muted">Loading…</p>
  }

  return (
    <div>
      <h3 style={{ marginTop: '2rem' }}>Accounts</h3>
      <p className="text-muted">
        Administrators are created on the server with{' '}
        <code>md-builder adduser -admin</code>; a role cannot be changed here.
        Disabling an account signs it out immediately and blocks further
        logins.
      </p>

      {error && <div className="alert alert-danger">{error}</div>}

      <table className="table">
        <thead>
          <tr>
            <th>Username</th>
            <th>Email</th>
            <th>Role</th>
            <th>Status</th>
            <th>Created</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {accounts.map((a) => {
            const isSelf = a.id === me.id
            // The server refuses both; not offering the button keeps the
            // reason out of the user's way.
            const canDisable = !isSelf && a.role !== 'admin'
            return (
              <tr key={a.id}>
                <td>
                  {a.username}
                  {isSelf && <span className="text-muted"> (you)</span>}
                </td>
                <td>{a.email}</td>
                <td>{a.role === 'admin' ? 'Administrator' : 'User'}</td>
                <td>
                  {a.disabled ? (
                    <span className="text-danger">Disabled</span>
                  ) : (
                    <span className="text-muted">Active</span>
                  )}
                </td>
                <td className="text-muted">{formatTime(a.createdAt)}</td>
                <td className="text-right">
                  <button
                    type="button"
                    className="btn btn-sm"
                    onClick={() => setEditing(a)}
                  >
                    Edit
                  </button>{' '}
                  {canDisable && (
                    <button
                      type="button"
                      className={a.disabled ? 'btn btn-sm btn-default' : 'btn btn-sm btn-danger'}
                      disabled={busyID === a.id}
                      onClick={() => toggleDisabled(a)}
                    >
                      {a.disabled ? 'Enable' : 'Disable'}
                    </button>
                  )}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>

      {editing && (
        <EditAccountDialog
          account={editing}
          isSelf={editing.id === me.id}
          onClose={() => setEditing(null)}
          onSaved={(updated) => {
            replace(updated)
            setEditing(null)
            // Editing your own row here must move the navbar too, so the
            // signed-in user stays the same object as the table's row.
            if (updated.id === me.id) onMeChange(toMe(updated))
          }}
          onError={onError}
        />
      )}
    </div>
  )
}

// EditAccountDialog edits one account. The disabled checkbox is offered only
// where the server would accept it: for a regular user's account, and never
// for your own.
function EditAccountDialog({
  account,
  isSelf,
  onClose,
  onSaved,
  onError,
}: {
  account: Account
  isSelf: boolean
  onClose: () => void
  onSaved: (updated: Account) => void
  onError: (message: string) => void
}) {
  const [username, setUsername] = useState(account.username)
  const [email, setEmail] = useState(account.email)
  const [password, setPassword] = useState('')
  const [disabled, setDisabled] = useState(account.disabled)
  const [saving, setSaving] = useState(false)
  const [error, setError] = useState('')

  const canDisable = !isSelf && account.role !== 'admin'

  // Close on Escape, as the other dialogs do.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const save = async (e: React.FormEvent) => {
    e.preventDefault()
    setSaving(true)
    setError('')
    try {
      const updated = await updateAccount(account.id, {
        username,
        email,
        password: password || undefined,
        ...(canDisable ? { disabled } : {}),
      })
      onSaved(updated)
    } catch (err: unknown) {
      const msg = err instanceof Error ? err.message : String(err)
      setError(msg)
      onError(msg)
    } finally {
      setSaving(false)
    }
  }

  return (
    <div
      className="dialog-backdrop"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose()
      }}
    >
      <div className="dialog" role="dialog" aria-modal="true" aria-label={`Edit ${account.username}`}>
        <div className="dialog-head">
          <strong className="dialog-title">Edit {account.username}</strong>
          <button type="button" className="dialog-close" aria-label="Close" onClick={onClose}>
            <X size={14} />
          </button>
        </div>
        <form onSubmit={save} style={{ padding: '0.75rem', overflowY: 'auto' }}>
          {error && <div className="alert alert-danger">{error}</div>}

          <div className="form-group">
            <label htmlFor="edit-username">Username</label>
            <input
              id="edit-username"
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              autoComplete="off"
            />
          </div>

          <div className="form-group">
            <label htmlFor="edit-email">Email</label>
            <input
              id="edit-email"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              autoComplete="off"
            />
          </div>

          <div className="form-group">
            <label htmlFor="edit-password">
              New password (leave blank to keep the current one)
            </label>
            <input
              id="edit-password"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="••••••••"
              autoComplete="new-password"
            />
            <small className="text-muted">
              At least 8 characters. Setting one signs the account out
              everywhere.
            </small>
          </div>

          {canDisable && (
            <div className="form-group">
              <label style={{ fontWeight: 'normal' }}>
                <input
                  type="checkbox"
                  checked={disabled}
                  onChange={(e) => setDisabled(e.target.checked)}
                  style={{ marginRight: '0.35rem', position: 'relative', top: '2px' }}
                />
                Disabled — cannot sign in
              </label>
            </div>
          )}

          <button type="submit" className="btn btn-primary" disabled={saving}>
            {saving ? 'Saving…' : 'Save'}
          </button>{' '}
          <button type="button" className="btn btn-default" onClick={onClose} disabled={saving}>
            Cancel
          </button>
        </form>
      </div>
    </div>
  )
}
