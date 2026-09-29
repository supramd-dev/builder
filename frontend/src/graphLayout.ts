import type { SubTask, TaskDetail } from './api'

// NODE_W/NODE_H size the graph nodes; GAP_X/GAP_Y the layer spacing.
export const NODE_W = 180
export const NODE_H = 44
export const GAP_X = 56
export const GAP_Y = 22

// The drawn graph has two kinds of node. Real nodes are the sub-tasks the
// scheduler claims and runs (clone/build/unit, and one per regression case);
// derived nodes are containers whose status comes from the nodes they group
// — the root, and the "reg test" node that summarizes the stage's cases.
// A derived node has no task behind it, so it is never scheduled and
// nothing depends on it.
export const ROOT_KEY = 'root'
export const GROUP_KEY = 'regression'

export type NodeKey = number | typeof ROOT_KEY | typeof GROUP_KEY

// LayoutNode is one drawn node. derived marks the synthetic regression
// group; the root is a container too, but the page draws it by its own key.
export interface LayoutNode {
  key: NodeKey
  name: string
  status: string
  sub: SubTask | null // null on a synthetic node (root, regression group)
  derived: boolean
}

// RegressionGroup is the synthetic node summarizing a regression stage whose
// cases each run as their own sub-task.
export interface RegressionGroup {
  cases: SubTask[]
  status: string
}

// derivedStatus derives a container's status from the nodes it groups. The
// rules mirror the server's aggregation of a regression run
// (recomputeParent): a failed case fails the group even while the others
// still run, an all-skipped group reads as skipped (what the dashboard shows
// for a stage no upstream failure let run), and the group stays running
// until every case reported.
export function derivedStatus(nodes: { status: string }[]): string {
  if (nodes.length === 0) return 'pending'
  const count = (s: string) => nodes.filter((n) => n.status === s).length
  if (count('failed') > 0) return 'failed'
  if (count('running') > 0) return 'running'
  // Every case is queued: the stage has not been claimed yet (its
  // dependency is still running), so the group is pending too.
  if (count('pending') === nodes.length) return 'pending'
  // Some cases reported, the rest still queued: in flight.
  if (count('pending') > 0) return 'running'
  // Nothing failed: an all-skipped stage (an upstream failure stopped it
  // from running) reads as skipped, anything else as done.
  if (count('skipped') === nodes.length) return 'skipped'
  return 'done'
}

// regressionGroup collects the regression case sub-tasks to summarize under
// one derived node. The node is drawn for a single case too: the cases are
// what the graph links to their own runs, so without it a one-case stage
// would have no way into its stage-wide regression run.
export function regressionGroup(subs: SubTask[]): RegressionGroup | null {
  const cases = subs.filter((s) => s.kind === 'regression')
  if (cases.length === 0) return null
  return { cases, status: derivedStatus(cases) }
}

// layerNodes assigns each sub-task a dependency layer (longest path from a
// source) and orders nodes within a layer by id, producing a left-to-right
// DAG layout: clone in layer 0, build in layer 1, tests in layer 2. The root
// takes a leftmost column of its own.
//
// With a regression group the cases move one column right and the derived
// node takes the column they had, so the stage reads as one unit: the group
// is its entry point and the cases hang off it.
export function layerNodes(
  subs: SubTask[],
  root: TaskDetail | null,
  group: RegressionGroup | null,
): LayoutNode[][] {
  const asNode = (s: SubTask): LayoutNode => ({
    key: s.id,
    name: s.name,
    status: s.status,
    sub: s,
    derived: false,
  })
  const layers: LayoutNode[][] = []
  if (root) {
    layers.push([{ key: ROOT_KEY, name: 'task', status: root.status, sub: null, derived: false }])
  }
  const byDepth = depthLayers(subs)
  if (!group) {
    for (const layer of byDepth) layers.push(layer.map(asNode))
    return layers
  }
  const grouped = new Set(group.cases.map((c) => c.id))
  const groupNode: LayoutNode = {
    key: GROUP_KEY,
    name: 'reg test',
    status: group.status,
    sub: null,
    derived: true,
  }
  // Every case depends on the same stage entry point, so they share one
  // layer; the group node takes that column and the cases move one right.
  const caseLayer = byDepth.findIndex((layer) => layer.some((s) => grouped.has(s.id)))
  byDepth.forEach((layer, li) => {
    if (li !== caseLayer) {
      layers.push(layer.map(asNode))
      return
    }
    // Siblings that are not cases (unit tests) keep the column; the group
    // joins them below it.
    layers.push([...layer.filter((s) => !grouped.has(s.id)).map(asNode), groupNode])
    layers.push(layer.filter((s) => grouped.has(s.id)).map(asNode))
  })
  return layers
}

// depthLayers groups the sub-tasks by their dependency depth (longest path
// from a source), dropping the empty layers a cycle would leave.
function depthLayers(subs: SubTask[]): SubTask[][] {
  const byId = new Map(subs.map((s) => [s.id, s]))
  const depth = new Map<number, number>()
  function d(s: SubTask): number {
    const cached = depth.get(s.id)
    if (cached !== undefined) return cached
    const deps = (s.dependsOn ?? []).filter((dep) => byId.has(dep))
    depth.set(s.id, 0) // cycle guard
    const v = deps.length === 0 ? 0 : 1 + Math.max(...deps.map((dep) => d(byId.get(dep)!)))
    depth.set(s.id, v)
    return v
  }
  for (const s of subs) d(s)

  const layers: SubTask[][] = []
  for (const s of subs) {
    const l = depth.get(s.id) ?? 0
    ;(layers[l] ??= []).push(s)
  }
  return layers.filter((l) => l && l.length > 0)
}

// nodePositions computes each node's pixel position: x by column, y by row
// within the column.
export function nodePositions(layers: LayoutNode[][]): Map<NodeKey, { x: number; y: number }> {
  const pos = new Map<NodeKey, { x: number; y: number }>()
  layers.forEach((layer, li) => {
    layer.forEach((n, i) => {
      pos.set(n.key, { x: li * (NODE_W + GAP_X), y: i * (NODE_H + GAP_Y) })
    })
  })
  return pos
}

// graphEdges builds the bezier path between every node and each of its
// dependencies. The cases of a regression group do not draw their own
// dependency edges: the group carries them (dependency → group → case), so
// the stage reads as one unit instead of a fan from the build node. An edge
// is "done" once its source node finished, for the animated draw-in.
export function graphEdges(
  subs: SubTask[],
  root: TaskDetail | null,
  group: RegressionGroup | null,
  pos: Map<NodeKey, { x: number; y: number }>,
): { d: string; done: boolean }[] {
  const out: { d: string; done: boolean }[] = []
  const status = new Map<NodeKey, string>()
  if (root) status.set(ROOT_KEY, root.status)
  for (const s of subs) status.set(s.id, s.status)
  if (group) status.set(GROUP_KEY, group.status)

  const link = (from: NodeKey, to: NodeKey) => {
    const a = pos.get(from)
    const b = pos.get(to)
    if (!a || !b) return
    out.push({ d: bezier(a, b), done: status.get(from) === 'done' })
  }
  // A sub-task's dependency is a node id, except the root's, which is drawn
  // in its own leftmost column.
  const keyOf = (id: number): NodeKey => (id === root?.id ? ROOT_KEY : id)

  const grouped = new Set(group?.cases.map((c) => c.id) ?? [])
  for (const s of subs) {
    if (grouped.has(s.id)) continue // the group carries these edges
    for (const dep of s.dependsOn ?? []) {
      const from = keyOf(dep)
      if (!pos.has(from)) continue
      link(from, s.id)
    }
  }
  if (group) {
    // The cases share the stage's entry point, so one edge per distinct
    // dependency is enough.
    const seen = new Set<NodeKey>()
    for (const c of group.cases) {
      for (const dep of c.dependsOn ?? []) {
        const from = keyOf(dep)
        if (seen.has(from) || !pos.has(from)) continue
        seen.add(from)
        link(from, GROUP_KEY)
      }
    }
    for (const c of group.cases) link(GROUP_KEY, c.id)
  }
  return out
}

// bezier is the S-curve from one node's right edge to the next node's left
// edge, both anchored on the nodes' vertical centers.
function bezier(
  from: { x: number; y: number },
  to: { x: number; y: number },
): string {
  const x1 = from.x + NODE_W / 2
  const y1 = from.y + NODE_H / 2
  const x2 = to.x + NODE_W / 2
  const y2 = to.y + NODE_H / 2
  const dx = Math.max(30, (x2 - x1) / 2)
  return `M ${x1} ${y1} C ${x1 + dx} ${y1}, ${x2 - dx} ${y2}, ${x2} ${y2}`
}
