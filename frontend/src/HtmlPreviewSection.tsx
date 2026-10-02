// Inline preview for HTML artifacts.
//
// A run's artifacts may include rendered pages: a Plotly HTML export, a
// coverage report, whatever a test script writes. They are shown the way they
// are meant to be read — framed in the run page, and openable on their own in
// a new tab. Both point at GET /api/test-artifacts/{id}/raw, the artifact
// endpoint's view-don't-save form.
//
// The page is a build product, so it is *not* trusted like the app's own
// pages: the server serves HTML under `Content-Security-Policy: sandbox`, and
// the frame repeats the same list in its `sandbox` attribute. Scripts run
// (that is what makes a Plotly export worth framing), but from an opaque
// origin that can read no cookie or storage and whose requests carry no
// credentials. A page that needs more than that can still be downloaded and
// opened locally.
//
// The name decides, as with plots: only *.html / *.htm are framed, so a build
// that emits JSON or a log never gets a frame it could not use.

import { testArtifactRawUrl, type TestArtifactRef } from './api'
import { isHtmlArtifact } from './artifacts'

// sandbox is the policy a framed page runs under — the list the server sets
// in the response's Content-Security-Policy, repeated on the frame so the
// page stays sandboxed whatever the response headers turn out to be.
const sandbox = 'allow-scripts allow-popups allow-downloads allow-forms allow-modals'

// HtmlPreviewSection renders every HTML artifact of a run: one frame per
// file, each with a link to open it on its own. Runs without HTML artifacts
// render nothing.
export default function HtmlPreviewSection({ artifacts }: { artifacts: TestArtifactRef[] }) {
  const pages = artifacts.filter(isHtmlArtifact)
  if (pages.length === 0) return null

  return (
    <section>
      <h3 className="task-section-title">
        HTML pages{' '}
        <span className="text-muted" style={{ fontWeight: 400 }}>
          ({pages.length} page{pages.length === 1 ? '' : 's'} from{' '}
          <code>*.html</code> artifacts)
        </span>
      </h3>
      <p className="text-muted" style={{ marginBottom: '0.75rem' }}>
        Framed in a sandbox: the page's own scripts run, but it cannot reach
        your session. Download the file to open it without one.
      </p>
      {pages.map((a) => (
        <div className="html-preview" key={a.id}>
          <p style={{ marginBottom: '0.25rem' }}>
            <code>{a.name}</code>{' '}
            <a
              href={testArtifactRawUrl(a.id)}
              target="_blank"
              rel="noopener noreferrer"
              title="Open the rendered page in a new tab"
            >
              Open in a new tab ↗
            </a>
          </p>
          {/* Lazy on purpose: a Plotly export carries its own 3.5 MB of
              plotly.js, and a frame below the fold should not fetch that
              before anyone scrolls to it. */}
          <iframe
            src={testArtifactRawUrl(a.id)}
            title={a.name}
            sandbox={sandbox}
            loading="lazy"
          />
        </div>
      ))}
    </section>
  )
}
