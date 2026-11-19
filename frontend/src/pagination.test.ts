// Paging arithmetic for the dashboard matrix: which slice of the commit
// history a page holds, and how it reads. The server clamps too — what is
// checked here is that the pager agrees with it instead of showing "page 9 of
// 7" or a range that does not add up.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { pageInfo, rangeLabel } from './pagination.ts'

test('pageInfo reports the slice a middle page holds', () => {
  const info = pageInfo(1234, 200, 2)
  assert.deepEqual(info, { page: 2, pages: 7, from: 201, to: 400, total: 1234 })
})

test('the last page stops at the total, not at a full page size', () => {
  assert.deepEqual(pageInfo(1234, 200, 7), { page: 7, pages: 7, from: 1201, to: 1234, total: 1234 })
  // An exact multiple: the last page is full and there is no empty page after it.
  assert.deepEqual(pageInfo(1200, 200, 6), { page: 6, pages: 6, from: 1001, to: 1200, total: 1200 })
})

test('a page past the end is clamped to the last page, never to nothing', () => {
  assert.deepEqual(pageInfo(1234, 200, 99), { page: 7, pages: 7, from: 1201, to: 1234, total: 1234 })
  assert.deepEqual(pageInfo(5, 200, 3), { page: 1, pages: 1, from: 1, to: 5, total: 5 })
})

test('a page below one is the first page', () => {
  assert.deepEqual(pageInfo(500, 100, 0), { page: 1, pages: 5, from: 1, to: 100, total: 500 })
  assert.deepEqual(pageInfo(500, 100, -2), { page: 1, pages: 5, from: 1, to: 100, total: 500 })
})

test('an empty list is one page of nothing', () => {
  assert.deepEqual(pageInfo(0, 200, 1), { page: 1, pages: 1, from: 0, to: 0, total: 0 })
  assert.deepEqual(pageInfo(0, 200, 4), { page: 1, pages: 1, from: 0, to: 0, total: 0 })
})

test('a zero or broken page size does not divide by zero', () => {
  assert.deepEqual(pageInfo(10, 0, 1), { page: 1, pages: 10, from: 1, to: 1, total: 10 })
  assert.deepEqual(pageInfo(10, Number.NaN, 2), { page: 2, pages: 10, from: 2, to: 2, total: 10 })
})

test('a broken total or page number falls back instead of rendering NaN', () => {
  const info = pageInfo(Number.NaN, 200, Number.NaN)
  assert.deepEqual(info, { page: 1, pages: 1, from: 0, to: 0, total: 0 })
})

test('rangeLabel groups thousands so a slice reads at a glance', () => {
  assert.equal(rangeLabel(pageInfo(12345, 200, 7)), '1,201–1,400 of 12,345')
  assert.equal(rangeLabel(pageInfo(5, 200, 1)), '1–5 of 5')
  assert.equal(rangeLabel(pageInfo(0, 200, 1)), 'no rows')
})
