import { useEffect, useState } from 'react'
import './App.css'
import { api, type Me } from './api'
import LoginPage from './LoginPage'
import UserCenter from './UserCenter'
import RunPage from './RunPage'
import SettingsPage from './SettingsPage'
import DashboardPage from './DashboardPage'
import TestRunDetailPage from './TestRunDetailPage'
import CaseDetailPage from './CaseDetailPage'
import DocsPage from './DocsPage'
import TaskDetailPage from './TaskDetailPage'
import type { CaseResult } from './api'

// Page is the client-side routing state. The dashboard hierarchy is
// dashboard → run detail → case detail, each with a back link; a live task
// graph (no run reported yet) opens the task detail instead.
type Page =
  | { view: 'dashboard' }
  | { view: 'environments' }
  | { view: 'run' }
  | { view: 'settings' }
  | { view: 'docs' }
  | { view: 'run-detail'; runId: number }
  | { view: 'case-detail'; runId: number; caseResult: CaseResult }
  | { view: 'task-detail'; taskId: number }

function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)
  const [page, setPage] = useState<Page>({ view: 'dashboard' })

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
    setPage({ view: 'dashboard' })
  }

  const navLink = (p: Page, label: string) => (
    <a
      href="#"
      className={page.view === p.view ? 'active' : ''}
      onClick={(e) => {
        e.preventDefault()
        setPage(p)
      }}
    >
      {label}
    </a>
  )

  return (
    <div className="page">
      {/* Top navigation bar */}
      <nav className="container navbar">
        <a href="/" className="brand">
          md-builder
        </a>
        {me && (
          <nav className="navbar-nav">
            {navLink({ view: 'dashboard' }, 'Dashboard')}
            {navLink({ view: 'environments' }, 'User center')}
            {navLink({ view: 'run' }, 'Run command')}
            {navLink({ view: 'settings' }, 'Settings')}
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
          page.view === 'dashboard' ? (
            <DashboardPage
              onOpenRun={(runId) => setPage({ view: 'run-detail', runId })}
              onOpenTask={(taskId) => setPage({ view: 'task-detail', taskId })}
              onError={console.warn}
            />
          ) : page.view === 'task-detail' ? (
            <TaskDetailPage
              taskId={page.taskId}
              onBack={() => setPage({ view: 'dashboard' })}
            />
          ) : page.view === 'run-detail' ? (
            <TestRunDetailPage
              runId={page.runId}
              onBack={() => setPage({ view: 'dashboard' })}
              onOpenCase={(runId, caseResult) =>
                setPage({ view: 'case-detail', runId, caseResult })
              }
              onError={console.warn}
            />
          ) : page.view === 'case-detail' ? (
            <CaseDetailPage
              runId={page.runId}
              caseResult={page.caseResult}
              onBack={() => setPage({ view: 'run-detail', runId: page.runId })}
            />
          ) : page.view === 'run' ? (
            <RunPage onError={console.warn} />
          ) : page.view === 'settings' ? (
            <SettingsPage onError={console.warn} />
          ) : page.view === 'docs' ? (
            <DocsPage />
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
        {me && (
          <span className="footer-nav">
            {navLink({ view: 'docs' }, 'Documentation')}
          </span>
        )}
      </footer>
    </div>
  )
}

export default App
