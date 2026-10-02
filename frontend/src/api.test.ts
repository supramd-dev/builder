// The artifact URLs the run page links to.
//
// They are strings the server routes have to match exactly — /download
// saves the bytes, /raw hands them to the browser to render (the sandboxed
// HTML preview and its "open in a new tab" both point there) — and a typo
// in either one shows up as a 404 in a link nobody clicked in a test.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { testArtifactDownloadUrl, testArtifactRawUrl } from './api.ts'

test('artifact URLs name the route the server serves', () => {
  assert.equal(testArtifactDownloadUrl(25), '/api/test-artifacts/25/download')
  assert.equal(testArtifactRawUrl(25), '/api/test-artifacts/25/raw')
})

test('the download and view URLs are different routes', () => {
  // A download is an attachment; the preview needs the same bytes served
  // as a document, so one must not be used in place of the other.
  assert.notEqual(testArtifactDownloadUrl(7), testArtifactRawUrl(7))
})
