import type { CommitOverlapPolicy } from './api'

// The repeated-commit policies, as the settings page presents them. A revision
// can reach the matrix more than once — a branch is pushed and then opened as
// a merge request, a tag is pushed for a commit that was already built, the
// same ref is dispatched twice by hand — and this option decides what the
// second event does to the work the first one dispatched.
//
// Plain TypeScript (no JSX) so the wording and the fallback are testable under
// `node --test`. The values are the server's own (store.CommitOverlap*): they
// are what the API stores, so they must not be translated.

export interface OverlapChoice {
  value: CommitOverlapPolicy
  // label is the option text in the select.
  label: string
  // description is the paragraph under the select: what this mode does, in
  // the terms the person choosing cares about (which rows appear, which runs
  // keep going).
  description: string
}

// OVERLAP_DEFAULT is the policy a configuration that was never asked reports,
// and the one the select falls back to for a value this build does not know.
// It is the server's own default (store.CommitOverlapRequeue).
export const OVERLAP_DEFAULT: CommitOverlapPolicy = 'requeue'

// overlapChoices is the full list, in the order the select shows it: the
// default first (what most sites want, and what an existing configuration is
// already doing), then the two that change it.
export const overlapChoices: readonly OverlapChoice[] = [
  {
    value: 'requeue',
    label: 'Requeue the existing task (default)',
    description:
      'The commit keeps one row on the dashboard and one task graph per environment. The second event ' +
      'restarts that graph from the beginning: a stage still running is closed as superseded, and its ' +
      'attempt is kept as history. Nothing is duplicated — you see the newest run of the revision.',
  },
  {
    value: 'fork',
    label: 'Run the new event as its own task',
    description:
      'Each event gets its own row and its own task graph, and they run independently. The earlier ' +
      'task is left exactly as it was — still queued, still running, still holding its result — so the ' +
      'revision can be tested twice at the same time, on the same environment.',
  },
  {
    value: 'fork_cancel',
    label: 'Run the new event as its own task and cancel the older one',
    description:
      'Each event gets its own row and its own task graph, and the unfinished work of the earlier ' +
      'ones is cancelled: a stage that is still running is aborted. Work that had already finished ' +
      'keeps its result. Use this when only the newest run of a revision matters and the older one ' +
      'should not keep an environment busy.',
  },
]

// isOverlapPolicy reports whether a value is one of the policies this build
// knows. Used to pin a string (what the API returns) to the select's type.
export function isOverlapPolicy(value: string): value is CommitOverlapPolicy {
  return overlapChoices.some((choice) => choice.value === value)
}

// overlapChoice returns the choice to present for a policy value. An
// unrecognized value — a policy a newer server added, or an empty string from
// a row written before the option existed — reads as the default, which is
// what the server would do with it: the select still shows something, and the
// save button sends back a value the server accepts.
export function overlapChoice(value: string): OverlapChoice {
  return (
    overlapChoices.find((choice) => choice.value === value) ??
    overlapChoices.find((choice) => choice.value === OVERLAP_DEFAULT)!
  )
}
