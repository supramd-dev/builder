// What a stored artifact's *name* says about how the run page shows it.
//
// The name is the whole test in every case, on purpose. Deciding by content
// would mean fetching every artifact of a run — a results file can be
// megabytes — just to find out it is not a chart, and would pull the 3.5 MB
// Plotly renderer into runs that have no plot at all. So the naming
// conventions are the contract, and they are documented for the people
// writing the pipelines (docs/test-matrix.md: *"Plot artifacts"*, *"HTML
// artifacts"*).

import type { TestArtifactRef } from './api'

// isPlotArtifact reports whether an artifact is a Plotly figure document:
// `xxxx.plot.json` or `xxxx.plotly.json`, case-insensitively.
export function isPlotArtifact(a: TestArtifactRef): boolean {
  return /\.plot(ly)?\.json$/i.test(a.name)
}

// isHtmlArtifact reports whether an artifact is a rendered page:
// `xxxx.html` or `xxxx.htm`, case-insensitively.
export function isHtmlArtifact(a: TestArtifactRef): boolean {
  return /\.html?$/i.test(a.name)
}
