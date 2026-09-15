import { useEffect, useState } from 'react'
import { Link, useParams } from 'react-router'
import { Markdown } from './markdown'

// The documentation lives in standalone .md files under frontend/docs and
// is embedded into the bundle at build time via Vite's ?raw import. Each
// section exists in English and Chinese (<id>.md / <id>.zh.md).
import overview from '../docs/overview.md?raw'
import overviewZh from '../docs/overview.zh.md?raw'
import gettingStarted from '../docs/getting-started.md?raw'
import gettingStartedZh from '../docs/getting-started.zh.md?raw'
import siteConfiguration from '../docs/site-configuration.md?raw'
import siteConfigurationZh from '../docs/site-configuration.zh.md?raw'
import environments from '../docs/environments.md?raw'
import environmentsZh from '../docs/environments.zh.md?raw'
import testMatrix from '../docs/test-matrix.md?raw'
import testMatrixZh from '../docs/test-matrix.zh.md?raw'
import runnerStrategy from '../docs/runner-strategy.md?raw'
import runnerStrategyZh from '../docs/runner-strategy.zh.md?raw'
import webhooks from '../docs/webhooks.md?raw'
import webhooksZh from '../docs/webhooks.zh.md?raw'
import api from '../docs/api.md?raw'
import apiZh from '../docs/api.zh.md?raw'

// UI strings for the docs chrome (TOC, pager, language switch).
const STRINGS = {
  en: {
    contents: 'Contents',
    labels: {
      overview: 'Overview',
      'getting-started': 'Getting started',
      'site-configuration': 'Site configuration',
      environments: 'Test environments',
      'test-matrix': 'Test matrix (YAML)',
      'runner-strategy': 'Runner and tasks',
      webhooks: 'GitLab webhooks',
      dashboard: 'Dashboard & API',
    } as Record<string, string>,
  },
  zh: {
    contents: '目录',
    labels: {
      overview: '概述',
      'getting-started': '快速上手',
      'site-configuration': '站点配置',
      environments: '测试环境',
      'test-matrix': '测试矩阵(YAML)',
      'runner-strategy': 'Runner 与任务',
      webhooks: 'GitLab webhooks',
      dashboard: '仪表板与 API',
    } as Record<string, string>,
  },
} as const

type Lang = keyof typeof STRINGS

// DOCS is the ordered table of contents; ids anchor the section headers.
const DOCS: { id: string; source: Record<Lang, string> }[] = [
  { id: 'overview', source: { en: overview, zh: overviewZh } },
  { id: 'getting-started', source: { en: gettingStarted, zh: gettingStartedZh } },
  { id: 'site-configuration', source: { en: siteConfiguration, zh: siteConfigurationZh } },
  { id: 'environments', source: { en: environments, zh: environmentsZh } },
  { id: 'test-matrix', source: { en: testMatrix, zh: testMatrixZh } },
  { id: 'runner-strategy', source: { en: runnerStrategy, zh: runnerStrategyZh } },
  { id: 'webhooks', source: { en: webhooks, zh: webhooksZh } },
  { id: 'dashboard', source: { en: api, zh: apiZh } },
]

// initialLang picks the browser language when it is Chinese, English
// otherwise (all docs are authored in English).
function initialLang(): Lang {
  if (typeof navigator !== 'undefined' && /^zh/i.test(navigator.language)) {
    return 'zh'
  }
  return 'en'
}

// DocsPage renders the platform documentation with a table of contents
// linking to each section and an EN/中文 language switch (persisted in
// localStorage). Routed as /docs and /docs/:section — the section comes
// straight from the URL, so every in-docs `#/docs/<section>` link (TOC,
// pager, cross-references in the markdown) navigates react-router-style and
// the section follows. Unknown section ids fall back to the first section.
export default function DocsPage() {
  const section = useParams().section
  const active = DOCS.some((d) => d.id === section) ? (section as string) : DOCS[0].id
  const [lang, setLang] = useState<Lang>(initialLang)
  const current = DOCS.find((d) => d.id === active) ?? DOCS[0]
  const t = STRINGS[lang]

  // Remember the choice across visits.
  useEffect(() => {
    try {
      const saved = localStorage.getItem('md-builder-docs-lang')
      if (saved === 'en' || saved === 'zh') setLang(saved)
    } catch {
      // localStorage unavailable (private mode): keep the default.
    }
  }, [])

  function switchLang(next: Lang) {
    setLang(next)
    try {
      localStorage.setItem('md-builder-docs-lang', next)
    } catch {
      // Ignore persistence errors.
    }
  }

  const label = (id: string) => t.labels[id] ?? id

  return (
    <div className="docs-page">
      <div className="docs-toc">
        <h3>{t.contents}</h3>
        <nav>
          {DOCS.map((d) => (
            <Link
              key={d.id}
              to={`/docs/${d.id}`}
              className={d.id === active ? 'active' : ''}
            >
              {label(d.id)}
            </Link>
          ))}
        </nav>
        <div className="docs-lang" role="group" aria-label="Language">
          <button
            type="button"
            className={'btn btn-sm docs-lang-btn' + (lang === 'en' ? ' docs-lang-active' : '')}
            onClick={() => switchLang('en')}
          >
            EN
          </button>
          <button
            type="button"
            className={'btn btn-sm docs-lang-btn' + (lang === 'zh' ? ' docs-lang-active' : '')}
            onClick={() => switchLang('zh')}
          >
            中文
          </button>
        </div>
      </div>
      <div className="docs-body" id={current.id} lang={lang === 'zh' ? 'zh-CN' : 'en'}>
        <Markdown source={current.source[lang]} />
        <div className="docs-pager">
          {DOCS.map((d, i) =>
            d.id === current.id ? (
              <span key={d.id}>
                {i > 0 && (
                  <Link to={`/docs/${DOCS[i - 1].id}`}>
                    ← {label(DOCS[i - 1].id)}
                  </Link>
                )}
                {i > 0 && i < DOCS.length - 1 && ' · '}
                {i < DOCS.length - 1 && (
                  <Link to={`/docs/${DOCS[i + 1].id}`}>
                    {label(DOCS[i + 1].id)} →
                  </Link>
                )}
              </span>
            ) : null,
          )}
        </div>
      </div>
    </div>
  )
}
