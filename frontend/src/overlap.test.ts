// The repeated-commit policy choices (overlap.ts).
//
// The values are a contract with the server: what the select sends back is
// stored verbatim and read by the dispatch paths (store.CommitOverlap*), so a
// renamed value silently becomes "unknown" — which the server reads as the
// default. The descriptions are what the person choosing reads; the cases
// below only pin that each mode has one and that they differ.
//
// Run with `npm test` (node --test, no dependencies).

import { test } from 'node:test'
import assert from 'node:assert/strict'
import {
  OVERLAP_DEFAULT,
  isOverlapPolicy,
  overlapChoice,
  overlapChoices,
} from './overlap.ts'

test('the choices are exactly the three policies the server knows', () => {
  assert.deepEqual(
    overlapChoices.map((c) => c.value),
    ['requeue', 'fork', 'fork_cancel'],
  )
  for (const choice of overlapChoices) {
    assert.equal(isOverlapPolicy(choice.value), true, choice.value)
  }
})

test('the default is the requeue policy, and it comes first', () => {
  assert.equal(OVERLAP_DEFAULT, 'requeue')
  assert.equal(overlapChoices[0].value, OVERLAP_DEFAULT)
})

test('every choice is labeled and described, in its own words', () => {
  const descriptions = new Set<string>()
  for (const choice of overlapChoices) {
    assert.ok(choice.label.trim().length > 0, choice.value)
    assert.ok(choice.description.length > 40, choice.value)
    assert.ok(!descriptions.has(choice.description), choice.value)
    descriptions.add(choice.description)
  }
})

test('an unknown or empty policy reads as the default', () => {
  // "" is what a configuration written before the option existed reports;
  // anything else is a policy this build does not know. Either way the select
  // shows the default, which is also what the server would do with it.
  for (const value of ['', 'dequeue', 'FORK', 'fork-cancel']) {
    assert.equal(isOverlapPolicy(value), false, value)
    assert.equal(overlapChoice(value).value, OVERLAP_DEFAULT, value)
  }
})

test('a known policy reads back as itself', () => {
  for (const choice of overlapChoices) {
    assert.equal(overlapChoice(choice.value), choice)
  }
})
