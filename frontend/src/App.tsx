import { useEffect, useState } from 'react'
import { HashRouter, NavLink as RRNavLink, Navigate, Route, Routes, useLocation } from 'react-router'
import './App.css'
import { api, getSiteConfig, type Me } from './api'
import LoginPage from './LoginPage'
import UserCenter from './UserCenter'
import RunPage from './RunPage'
import SettingsPage from './SettingsPage'
import DashboardPage from './DashboardPage'
import TestRunDetailPage from './TestRunDetailPage'
import DocsPage from './DocsPage'
import TaskDetailPage from './TaskDetailPage'
import TaskGraphPage from './TaskGraphPage'
import { applySiteTimezone, subscribeTimezone } from './timezone'

// The route table (hash-based so the docs' #/docs/... anchors keep working
// and the server's SPA fallback stays enough):
//
//   #/                      dashboard (default)
//   #/environments          runner environments
//   #/run                   run command
//   #/settings              site settings
//   #/docs                  documentation
//   #/docs/:section         documentation, a section preselected
//   #/tasks/:taskId         task graph (root)
//   #/tasks/:taskId/log     task log view (root detail + step list)
//   #/runs/:runId           test run detail (a regression case row opens
//                           the child run's own detail page here)
//
// Signed-in pages read their ids from the URL (useParams) and link onward
// with <Link> / useNavigate — no per-page navigation callbacks.

function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)
  // Bumped when the display timezone changes so every page re-renders its
  // timestamps (the formatters read the module-level state directly).
  const [, setTimezoneTick] = useState(0)

  // Check for an existing session on mount.
  useEffect(() => {
    api<Me>('/api/me')
      .then(setMe)
      .catch(() => setMe(null))
      .finally(() => setLoadingMe(false))
  }, [])

  // Load the site timezone (cached in localStorage; a session may not even
  // be established yet — the timezone applies to any rendered times).
  useEffect(() => {
    let cancelled = false
    getSiteConfig()
      .then((cfg) => {
        if (!cancelled) applySiteTimezone(cfg.timezone)
      })
      .catch(() => {
        // Not logged in or offline: the localStorage cache (applied at
        // module load) carries the last known setting.
      })
    return () => {
      cancelled = true
    }
  }, [])

  useEffect(
    () =>
      subscribeTimezone(() => {
        setTimezoneTick((t) => t + 1)
      }),
    [],
  )

  async function handleLogout() {
    try {
      await api('/api/logout', { method: 'POST' })
    } catch {
      // Ignore logout errors; clear local state regardless.
    }
    setMe(null)
  }

  return (
    <HashRouter>
      <ScrollToTop />
      <div className="page">
        <NavBar me={me} onLogout={handleLogout} />
        {/* Main content */}
        <div className="container content" style={{ flexGrow: 1 }}>
          {loadingMe ? (
            <p className="text-muted">Loading…</p>
          ) : me ? (
            <PageRoutes />
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
              <RRNavLink to="/docs">Documentation</RRNavLink>
            </span>
          )}
        </footer>
      </div>
    </HashRouter>
  )
}

// PageRoutes is the signed-in route table.
function PageRoutes() {
  return (
    <Routes>
      <Route path="/" element={<DashboardPage onError={console.warn} />} />
      <Route path="/environments" element={<UserCenter />} />
      <Route path="/run" element={<RunPage onError={console.warn} />} />
      <Route path="/settings" element={<SettingsPage onError={console.warn} />} />
      <Route path="/docs" element={<DocsPage />} />
      <Route path="/docs/:section" element={<DocsPage />} />
      <Route path="/tasks/:taskId" element={<TaskGraphPage />} />
      <Route path="/tasks/:taskId/log" element={<TaskDetailPage />} />
      <Route path="/runs/:runId" element={<TestRunDetailPage onError={console.warn} />} />
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}

// ScrollToTop scrolls the window to the top after every route change.
function ScrollToTop() {
  const { pathname } = useLocation()
  useEffect(() => {
    window.scrollTo(0, 0)
  }, [pathname])
  return null
}

// NavBar is the top navigation; NavLink gives the active link its class.
function NavBar({ me, onLogout }: { me: Me | null; onLogout: () => void }) {
  const navLink = (to: string, label: string) => (
    <RRNavLink to={to} className={({ isActive }) => (isActive ? 'active' : '')}>
      {label}
    </RRNavLink>
  )

  return (
    <nav className="container navbar">
      <RRNavLink to="/" className="brand">
        md-builder
      </RRNavLink>
      {me && (
        <nav className="navbar-nav">
          {navLink('/', 'Dashboard')}
          {navLink('/environments', 'Runner Envs')}
          {navLink('/run', 'Run command')}
          {navLink('/settings', 'Settings')}
        </nav>
      )}
      <div className="navbar-text">
        {me ? (
          <>
            <span className="text-muted">{me.username}</span>
            {' — '}
            <a href="#" onClick={(e) => { e.preventDefault(); onLogout() }}>
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
  )
}

export default App
