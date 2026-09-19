import { Suspense, lazy, useEffect, useState } from 'react'
import { HashRouter, NavLink as RRNavLink, Navigate, Route, Routes, useLocation } from 'react-router'
import './App.css'
import { api, cachedSiteConfig, getHealth, getSetupState, type Me } from './api'
import LoginPage from './LoginPage'
import SetupPage from './SetupPage'
import DashboardPage from './DashboardPage'
import TaskPipelinePage from './TaskPipelinePage'
import TestRunDetailPage from './TestRunDetailPage'
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
//   #/tasks/:taskId         pipeline view: dependency graph + selected
//                           stage's live log in one page (a sub-task id is
//                           resolved to its root; the old /log suffix
//                           redirects here)
//   #/runs/:runId           test run detail (a regression case row opens
//                           the child run's own detail page here)
//   #/health                site health board (footer link): backend
//                           liveness, git repository, object storage
//
// Signed-in pages read their ids from the URL (useParams) and link onward
// with <Link> / useNavigate — no per-page navigation callbacks.
//
// The heavier, less-visited pages (Monaco editor, docs viewer) are
// code-split behind React.lazy so the dashboard stays the first paint.

const UserCenter = lazy(() => import('./UserCenter'))
const RunPage = lazy(() => import('./RunPage'))
const SettingsPage = lazy(() => import('./SettingsPage'))
const DocsPage = lazy(() => import('./DocsPage'))
const HealthPage = lazy(() => import('./HealthPage'))

function App() {
  const [me, setMe] = useState<Me | null>(null)
  const [loadingMe, setLoadingMe] = useState(true)
  // Whether the site still needs its first-run setup — a site with no account
  // at all has nothing to log in with, so it gets the setup page instead of
  // the login form. Resolved together with the session: rendering the login
  // page first would flash a form that cannot work.
  const [setupRequired, setSetupRequired] = useState(false)
  // Bumped when the display timezone changes so every page re-renders its
  // timestamps (the formatters read the module-level state directly).
  const [, setTimezoneTick] = useState(0)
  // The served build's source revision, shown in the footer. The health
  // probe is unauthenticated, so the version renders even on the login page.
  const [version, setVersion] = useState('')

  useEffect(() => {
    getHealth()
      .then((h) => setVersion(h.version ?? ''))
      .catch(() => {}) // offline: the footer simply shows no version
  }, [])

  // Check for an existing session on mount; without one, ask whether the site
  // is set up at all. The two are sequential so a signed-in visit costs no
  // extra request.
  useEffect(() => {
    api<Me>('/api/me')
      .then(setMe)
      .catch(async () => {
        setMe(null)
        try {
          setSetupRequired(await getSetupState())
        } catch {
          // Offline or the check failed: fall back to the login page, which
          // is where a configured site wants to go anyway.
        }
      })
      .finally(() => setLoadingMe(false))
  }, [])

  // Load the site timezone (cached in localStorage; a session may not even
  // be established yet — the timezone applies to any rendered times). The
  // module-level site-config cache means this shares one request with other
  // readers (e.g. the Run command page).
  useEffect(() => {
    let cancelled = false
    cachedSiteConfig()
      .then((cfg) => {
        if (!cancelled && cfg) applySiteTimezone(cfg.timezone)
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
            <PageRoutes me={me} onMeChange={setMe} />
          ) : setupRequired ? (
            <SetupPage
              onLogin={setMe}
              onAlreadySetUp={() => setSetupRequired(false)}
            />
          ) : (
            <LoginPage onLogin={setMe} />
          )}
        </div>
        {/* Footer */}
        <footer className="container">
          <span className="text-muted">
            md-builder · Scientific computing test platform
            {version && (
              <>
                {' · '}
                <code style={{ fontSize: '0.85em' }}>{version}</code>
              </>
            )}
          </span>
          {me && (
            <span className="footer-nav">
              <RRNavLink to="/health">Site health</RRNavLink>
              {' · '}
              <RRNavLink to="/docs">Documentation</RRNavLink>
            </span>
          )}
        </footer>
      </div>
    </HashRouter>
  )
}

// PageRoutes is the signed-in route table. Lazy routes share one Suspense
// fallback; the eager pages (dashboard, pipeline, run detail) render without
// it. The signed-in account is passed to the one page that edits it, so a
// change there (a new username, say) reaches the navbar immediately.
function PageRoutes({ me, onMeChange }: { me: Me; onMeChange: (me: Me) => void }) {
  return (
    <Routes>
      <Route path="/" element={<DashboardPage onError={console.warn} />} />
      <Route
        path="/environments"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <UserCenter />
          </Suspense>
        }
      />
      <Route
        path="/run"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <RunPage onError={console.warn} />
          </Suspense>
        }
      />
      <Route
        path="/settings"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <SettingsPage me={me} onMeChange={onMeChange} onError={console.warn} />
          </Suspense>
        }
      />
      <Route
        path="/docs"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <DocsPage />
          </Suspense>
        }
      />
      <Route
        path="/docs/:section"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <DocsPage />
          </Suspense>
        }
      />
      {/* The old split pages (graph at /tasks/:id, log at /tasks/:id/log)
          merged into one pipeline view; /log redirects so old links and
          bookmarks keep working. */}
      <Route path="/tasks/:taskId" element={<TaskPipelinePage />} />
      <Route path="/tasks/:taskId/log" element={<Navigate to="../" replace relative="path" />} />
      <Route path="/runs/:runId" element={<TestRunDetailPage onError={console.warn} />} />
      <Route
        path="/health"
        element={
          <Suspense fallback={<p className="text-muted">Loading…</p>}>
            <HealthPage onError={console.warn} />
          </Suspense>
        }
      />
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
