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
import { ApiError, cancelTask, testArtifactDownloadUrl, testArtifactRawUrl } from './api.ts'

test('artifact URLs name the route the server serves', () => {
  assert.equal(testArtifactDownloadUrl(25), '/api/test-artifacts/25/download')
  assert.equal(testArtifactRawUrl(25), '/api/test-artifacts/25/raw')
})

test('the download and view URLs are different routes', () => {
  // A download is an attachment; the preview needs the same bytes served
  // as a document, so one must not be used in place of the other.
  assert.notEqual(testArtifactDownloadUrl(7), testArtifactRawUrl(7))
})

// withFetch runs body with fetch stubbed to answer one request, and returns
// what the call asked for: the URL and the init, so a test can assert the route
// and the method the server routes have to match.
async function withFetch(
  status: number,
  body: unknown,
  call: () => Promise<unknown>,
): Promise<{ url: string; init: RequestInit | undefined }> {
  const real = globalThis.fetch
  const seen: { url: string; init: RequestInit | undefined }[] = []
  globalThis.fetch = (async (url: string | URL | Request, init?: RequestInit) => {
    seen.push({ url: String(url), init })
    return new Response(JSON.stringify(body), {
      status,
      headers: { 'Content-Type': 'application/json' },
    })
  }) as typeof fetch
  try {
    await call()
  } finally {
    globalThis.fetch = real
  }
  assert.equal(seen.length, 1)
  return seen[0]
}

test('cancelling a task POSTs to the cancellation route', async () => {
  const result = { taskId: 12, cancelled: 2, aborted: 1, summary: 'cancelled by alice' }
  let answer: unknown
  const req = await withFetch(200, result, async () => {
    answer = await cancelTask(12)
  })
  // Stopping a run is a write, not a read of its log or runs: the route is the
  // one /api/tasks/{id}/cancel, and it is reached with a POST.
  assert.equal(req.url, '/api/tasks/12/cancel')
  assert.equal(req.init?.method, 'POST')
  assert.deepEqual(answer, result)
})

test('a cancellation with nothing to stop reports the refusal', async () => {
  // The server answers 409 when the work finished first; the caller has to see
  // that as a failure rather than as a cancellation that happened.
  await assert.rejects(
    () => withFetch(409, { error: 'nothing to cancel: this task has no unfinished work' }, () => cancelTask(12)),
    (err: unknown) =>
      err instanceof ApiError &&
      err.status === 409 &&
      err.message === 'nothing to cancel: this task has no unfinished work',
  )
})
