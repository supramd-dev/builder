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
    <div className="flex min-h-svh flex-col">
      {/* Top navigation bar */}
      <header className="border-b border-border">
        <div className="mx-auto flex h-12 w-full max-w-4xl items-center justify-between px-4">
          <a href="/" className="font-mono text-sm font-semibold tracking-tight">
            md-builder
          </a>
          <nav className="flex items-center gap-4 text-sm text-ink-muted">
            {me ? (
              <>
                <span className="text-ink">{me.username}</span>
                <button type="button" onClick={handleLogout} className="hover:text-accent">
                  Log out
                </button>
              </>
            ) : (
              <a href="#" className="text-accent">
                Log in
              </a>
            )}
          </nav>
        </div>
      </header>

      {/* Main content */}
      <main className="flex flex-1 items-center justify-center px-4 py-12">
        {loadingMe ? (
          <p className="text-sm text-ink-muted">Loading…</p>
        ) : me ? (
          <div className="w-full">
            <UserCenter me={me} />
          </div>
        ) : (
          <LoginPage onLogin={setMe} />
        )}
      </main>

      {/* Footer */}
      <footer className="border-t border-border">
        <div className="mx-auto flex h-10 w-full max-w-4xl items-center justify-center px-4 text-xs text-ink-muted">
          md-builder · Scientific computing test platform
        </div>
      </footer>
    </div>
  )
}

export default App
