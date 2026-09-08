import { useEffect, useState } from 'react'
import './App.css'
import { api, type Me } from './api'
import LoginPage from './LoginPage'
import UserCenter from './UserCenter'
import RunPage from './RunPage'
import SettingsPage from './SettingsPage'

type View = 'environments' | 'run' | 'settings'

function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)
  const [view, setView] = useState<View>('environments')

  // Check for an existing session on mount.
  useEffect(() => {
    api<Me>('/api/me')
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setLoadingMe(false))
  }, [])

  async function handleLogout() {
    try {
      await api('/api/logout', { method: 'POST' })
    } catch {
      // Ignore logout errors; clear local state regardless.
    }
    setMe(null)
    setView('environments')
  }

  return (
    <div className="page">
      {/* Top navigation bar */}
      <nav className="container navbar">
        <a href="/" className="brand">
          md-builder
        </a>
        {me && (
          <nav className="navbar-nav">
            <a
              href="#"
              className={view === 'environments' ? 'active' : ''}
              onClick={(e) => {
                e.preventDefault()
                setView('environments')
              }}
            >
              User center
            </a>
            <a
              href="#"
              className={view === 'run' ? 'active' : ''}
              onClick={(e) => {
                e.preventDefault()
                setView('run')
              }}
            >
              Run command
            </a>
            <a
              href="#"
              className={view === 'settings' ? 'active' : ''}
              onClick={(e) => {
                e.preventDefault()
                setView('settings')
              }}
            >
              Settings
            </a>
          </nav>
        )}
        <div className="navbar-text">
          {me ? (
            <>
              <span className="text-muted">{me.username}</span>
              {' — '}
              <a href="#" onClick={(e) => { e.preventDefault(); handleLogout() }}>
                Log out
              </a>
            </>
          ) : (
            <>
              <a href="#">Log in</a>
              {' — '}
              <a href="#">Register</a>
            </>
          )}
        </div>
      </nav>

      {/* Main content */}
      <div className="container content" style={{ flexGrow: 1 }}>
        {loadingMe ? (
          <p className="text-muted">Loading…</p>
        ) : me ? (
          view === 'run' ? (
            <RunPage onError={console.warn} />
          ) : view === 'settings' ? (
            <SettingsPage onError={console.warn} />
          ) : (
            <UserCenter me={me} />
          )
        ) : (
          <LoginPage onLogin={setMe} />
        )}
      </div>

      {/* Footer */}
      <footer className="container">
        <span className="text-muted">
          md-builder · Scientific computing test platform
        </span>
      </footer>
    </div>
  )
}

export default App
