import { useState } from 'react'
import { Markdown } from './markdown'

// The documentation lives in standalone .md files under frontend/docs and
// is embedded into the bundle at build time via Vite's ?raw import.
import overview from '../docs/overview.md?raw'
import gettingStarted from '../docs/getting-started.md?raw'
import siteConfiguration from '../docs/site-configuration.md?raw'
import environments from '../docs/environments.md?raw'
import testMatrix from '../docs/test-matrix.md?raw'
import runnerStrategy from '../docs/runner-strategy.md?raw'
import webhooks from '../docs/webhooks.md?raw'
import api from '../docs/api.md?raw'

// DOCS is the ordered table of contents; ids anchor the section headers.
const DOCS: { id: string; label: string; source: string }[] = [
  { id: 'overview', label: 'Overview', source: overview },
  { id: 'getting-started', label: 'Getting started', source: gettingStarted },
  { id: 'site-configuration', label: 'Site configuration', source: siteConfiguration },
  { id: 'environments', label: 'Test environments', source: environments },
  { id: 'test-matrix', label: 'Test matrix (YAML)', source: testMatrix },
  { id: 'runner-strategy', label: 'Runner strategy', source: runnerStrategy },
  { id: 'webhooks', label: 'GitLab webhooks', source: webhooks },
  { id: 'dashboard', label: 'Dashboard & API', source: api },
]

// DocsPage renders the platform documentation with a table of contents
// linking to each section.
export default function DocsPage() {
  const [active, setActive] = useState(DOCS[0].id)
  const current = DOCS.find((d) => d.id === active) ?? DOCS[0]

  return (
    <div className="docs-page">
      <div className="docs-toc">
        <h3>Contents</h3>
        <nav>
          {DOCS.map((d) => (
            <a
              key={d.id}
              href={`#${d.id}`}
              className={d.id === active ? 'active' : ''}
              onClick={(e) => {
                e.preventDefault()
                setActive(d.id)
              }}
            >
              {d.label}
            </a>
          ))}
        </nav>
      </div>
      <div className="docs-body" id={current.id}>
        <Markdown source={current.source} />
        <div className="docs-pager">
          {DOCS.map((d, i) =>
            d.id === current.id ? (
              <span key={d.id}>
                {i > 0 && (
                  <a
                    href={`#${DOCS[i - 1].id}`}
                    onClick={(e) => {
                      e.preventDefault()
                      setActive(DOCS[i - 1].id)
                    }}
                  >
                    ← {DOCS[i - 1].label}
                  </a>
                )}
                {i > 0 && i < DOCS.length - 1 && ' · '}
                {i < DOCS.length - 1 && (
                  <a
                    href={`#${DOCS[i + 1].id}`}
                    onClick={(e) => {
                      e.preventDefault()
                      setActive(DOCS[i + 1].id)
                    }}
                  >
                    {DOCS[i + 1].label} →
                  </a>
                )}
              </span>
            ) : null,
          )}
        </div>
      </div>
    </div>
  )
}
