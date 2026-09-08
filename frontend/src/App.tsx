import { useEffect, useState } from 'react'
import './App.css'
import { api, type Me } from './api'
import LoginPage from './LoginPage'
import UserCenter from './UserCenter'

function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)

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
  }

  return (
    <div className="page">
      {/* Top navigation bar */}
      <nav className="container navbar">
        <a href="/" className="brand">
          md-builder
        </a>
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
          <UserCenter me={me} />
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
