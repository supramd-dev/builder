// Breadcrumbs renders the trail from the dashboard down to the current
// detail page: Dashboard / Task #12 / Run #7 / Case … Each crumb is a real
// router link (history-friendly); the last one is plain text.

import { Link } from 'react-router'

export interface Crumb {
  label: string
  to?: string // omitted = current page (rendered as plain text)
}

export function Breadcrumbs({ trail }: { trail: Crumb[] }) {
  return (
    <nav className="breadcrumbs" aria-label="Breadcrumb">
      {trail.map((c, i) => (
        <span key={i} className="breadcrumb-item">
          {i > 0 && <span className="breadcrumb-sep">/</span>}
          {c.to ? (
            <Link to={c.to}>{c.label}</Link>
          ) : (
            <span className="breadcrumb-current">{c.label}</span>
          )}
        </span>
      ))}
    </nav>
  )
}
