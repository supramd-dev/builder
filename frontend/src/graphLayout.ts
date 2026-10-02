import type { TaskDetail, TaskNode } from './api'

// NODE_W/NODE_H size the graph nodes; GAP_X/GAP_Y the layer spacing.
export const NODE_W = 180
export const NODE_H = 44
export const GAP_X = 56
export const GAP_Y = 22

// LayoutNode is one drawn node. node is the task node behind it; the root is
// drawn as a node too, with node null — nothing depends on it, which is why
// one graph edge is never drawn to it.
export interface LayoutNode {
  key: number // the task id
  name: string
  status: string
  node: TaskNode | null
  virtual: boolean
}

// RegressionStage groups a virtual regression container with the case tasks
// under it. The container runs nothing: its status, counts and times are the
// cases' aggregate, computed by the server (that is the whole point of the
// rollup), and each case is a task of its own with its own log and runs.
export interface RegressionStage {
  stage: TaskNode
  cases: TaskNode[]
}

// regressionStage finds the graph's regression container and the cases nested
// under it (parentId), or null when the entry had no regression stage.
export function regressionStage(nodes: TaskNode[]): RegressionStage | null {
  const stage = nodes.find((n) => n.kind === 'regression')
  if (!stage) return null
  return { stage, cases: nodes.filter((n) => n.parentId === stage.id) }
}

// layerNodes assigns each node a dependency layer (longest path from a
// source) and orders nodes within a layer by id, producing a left-to-right
// DAG layout: clone in layer 0, build in layer 1, the tests in layer 2. The
// root takes a leftmost column of its own.
//
// The regression container has no dependency of its own — it is virtual, so
// nothing can wait on it — and it is drawn in the column its cases would
// occupy, with the cases one column further right: the stage reads as one
// unit, entered at the container, and the container is what links to the
// cases.
export function layerNodes(
  root: TaskDetail,
  nodes: TaskNode[],
  stage: RegressionStage | null,
): LayoutNode[][] {
  const asNode = (n: TaskNode): LayoutNode => ({
    key: n.id,
    name: n.name,
    status: n.status,
    node: n,
    virtual: !!n.virtual,
  })
  const rootNode: LayoutNode = {
    key: root.id,
    name: root.name || 'task',
    status: root.status,
    node: null,
    virtual: true,
  }
  const layers: LayoutNode[][] = [[rootNode]]

  // The container is placed from its cases and the cases from the container,
  // so neither takes part in the dependency layering: that is computed from
  // the rest of the graph (the clone → build → unit chain).
  const cases = stage?.cases ?? []
  const caseIDs = new Set(cases.map((c) => c.id))
  const layouted = nodes.filter((n) => n.id !== stage?.stage.id && !caseIDs.has(n.id))
  const byDepth = depthLayers(layouted)
  if (!stage) {
    for (const layer of byDepth) layers.push(layer.map(asNode))
    return layers
  }

  // The cases hang off the stage's entry point (the build task, or the clone
  // when the entry has no build stage): the container is drawn in that
  // entry's next column and the cases follow one column further right.
  const depthOf = new Map<number, number>()
  byDepth.forEach((layer, li) => layer.forEach((n) => depthOf.set(n.id, li)))
  let insertAt = byDepth.length
  for (const c of cases) {
    for (const dep of c.dependsOn ?? []) {
      const d = depthOf.get(dep)
      if (d !== undefined && d + 1 < insertAt) insertAt = d + 1
    }
  }
  byDepth.forEach((layer, li) => {
    // Siblings that are not cases (the unit stage, usually) keep the column;
    // the container joins them below it, and the cases follow one column
    // right.
    layers.push(li === insertAt ? [...layer.map(asNode), asNode(stage.stage)] : layer.map(asNode))
  })
  if (insertAt >= byDepth.length) layers.push([asNode(stage.stage)])
  if (cases.length > 0) layers.push(cases.map(asNode))
  return layers
}

// depthLayers groups the nodes by their dependency depth (longest path from a
// source), dropping the empty layers a cycle would leave.
function depthLayers(nodes: TaskNode[]): TaskNode[][] {
  const byId = new Map(nodes.map((n) => [n.id, n]))
  const depth = new Map<number, number>()
  function d(n: TaskNode): number {
    const cached = depth.get(n.id)
    if (cached !== undefined) return cached
    const deps = (n.dependsOn ?? []).filter((dep) => byId.has(dep))
    depth.set(n.id, 0) // cycle guard
    const v = deps.length === 0 ? 0 : 1 + Math.max(...deps.map((dep) => d(byId.get(dep)!)))
    depth.set(n.id, v)
    return v
  }
  for (const n of nodes) d(n)

  const layers: TaskNode[][] = []
  for (const n of nodes) {
    const l = depth.get(n.id) ?? 0
    ;(layers[l] ??= []).push(n)
  }
  return layers.filter((l) => l && l.length > 0)
}

// nodePositions computes each node's pixel position: x by column, y by row
// within the column.
export function nodePositions(layers: LayoutNode[][]): Map<number, { x: number; y: number }> {
  const pos = new Map<number, { x: number; y: number }>()
  layers.forEach((layer, li) => {
    layer.forEach((n, i) => {
      pos.set(n.key, { x: li * (NODE_W + GAP_X), y: i * (NODE_H + GAP_Y) })
    })
  })
  return pos
}

// graphEdges builds the bezier path between every node and each of its
// dependencies. The cases of a regression stage do not draw their own
// dependency edges: the container carries them (dependency → container →
// case), so the stage reads as one unit instead of a fan from the build node.
// An edge is "done" once its source node passed, for the animated draw-in.
export function graphEdges(
  nodes: TaskNode[],
  stage: RegressionStage | null,
  pos: Map<number, { x: number; y: number }>,
): { d: string; done: boolean }[] {
  const out: { d: string; done: boolean }[] = []
  const done = new Map<number, boolean>()
  for (const n of nodes) done.set(n.id, n.status === 'passed')
  const cases = stage?.cases ?? []
  const caseIDs = new Set(cases.map((c) => c.id))
  if (stage) done.set(stage.stage.id, stage.stage.status === 'passed')

  const link = (from: number, to: number) => {
    const a = pos.get(from)
    const b = pos.get(to)
    if (!a || !b) return
    out.push({ d: bezier(a, b), done: done.get(from) === true })
  }

  for (const n of nodes) {
    if (caseIDs.has(n.id) || n.id === stage?.stage.id) continue // drawn below
    for (const dep of n.dependsOn ?? []) {
      if (pos.has(dep)) link(dep, n.id)
    }
  }
  if (stage) {
    // The cases share the stage's entry point (the build task), so one edge
    // per distinct dependency is enough.
    const seen = new Set<number>()
    for (const c of cases) {
      for (const dep of c.dependsOn ?? []) {
        if (seen.has(dep) || !pos.has(dep) || caseIDs.has(dep)) continue
        seen.add(dep)
        link(dep, stage.stage.id)
      }
    }
    for (const c of cases) link(stage.stage.id, c.id)
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
