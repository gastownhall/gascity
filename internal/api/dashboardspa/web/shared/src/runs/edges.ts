import type { RunSnapshotDep, RunSnapshot } from '../run-snapshot.js';
import type { RunDisplayEdge, RunDisplayNode } from '../run-detail.js';
import { externalizeId, nonEmpty } from './bead-fields.js';

export function buildRunDisplayEdges(
  raw: RunSnapshot,
  physicalToSemantic: Map<string, string>,
  nodes: RunDisplayNode[],
): RunDisplayEdge[] {
  const logicalEdges = projectEdges(raw.logical_edges ?? [], physicalToSemantic, nodes);
  if (logicalEdges.length > 0) return logicalEdges;
  return projectEdges(raw.deps ?? [], physicalToSemantic, nodes, bridgeableScopeCheckIds(raw));
}

function projectEdges(
  deps: RunSnapshotDep[],
  physicalToSemantic: Map<string, string>,
  nodes: RunDisplayNode[],
  bridgeableHiddenIds = new Set<string>(),
): RunDisplayEdge[] {
  const visible = new Set(
    nodes.filter((node) => node.visibleInGraph !== false).map((node) => node.id),
  );
  const outgoing = outgoingDeps(deps);
  const seen = new Set<string>();
  const edges: RunDisplayEdge[] = [];
  for (const dep of deps) {
    const rawFrom = nonEmpty(dep.from);
    const rawTo = nonEmpty(dep.to);
    if (!rawFrom || !rawTo) continue;
    if (nonEmpty(dep.kind) === 'tracks') continue;
    const from = physicalToSemantic.get(rawFrom) ?? externalizeId(rawFrom);
    const to = physicalToSemantic.get(rawTo) ?? externalizeId(rawTo);
    const kind = nonEmpty(dep.kind);
    if (visible.has(from) && visible.has(to)) {
      pushEdge(edges, seen, from, to, kind);
      continue;
    }
    if (visible.has(from) && bridgeableHiddenIds.has(rawTo)) {
      bridgeHiddenEdges({
        edges,
        seen,
        source: from,
        currentRawId: rawTo,
        outgoing,
        visible,
        bridgeableHiddenIds,
        physicalToSemantic,
        ...(kind !== undefined ? { inheritedKind: kind } : {}),
      });
    }
  }
  return edges;
}

function bridgeHiddenEdges({
  edges,
  seen,
  source,
  currentRawId,
  outgoing,
  visible,
  bridgeableHiddenIds,
  physicalToSemantic,
  inheritedKind,
  visited = new Set<string>(),
}: {
  edges: RunDisplayEdge[];
  seen: Set<string>;
  source: string;
  currentRawId: string;
  outgoing: Map<string, RunSnapshotDep[]>;
  visible: Set<string>;
  bridgeableHiddenIds: Set<string>;
  physicalToSemantic: Map<string, string>;
  inheritedKind?: string;
  visited?: Set<string>;
}): void {
  if (visited.has(currentRawId)) return;
  visited.add(currentRawId);
  for (const dep of outgoing.get(currentRawId) ?? []) {
    const rawTo = nonEmpty(dep.to);
    if (!rawTo) continue;
    const kind = nonEmpty(dep.kind);
    if (kind === 'tracks') continue;
    const target = physicalToSemantic.get(rawTo) ?? externalizeId(rawTo);
    const edgeKind = kind ?? inheritedKind;
    if (visible.has(target)) {
      pushEdge(edges, seen, source, target, edgeKind);
    } else if (bridgeableHiddenIds.has(rawTo)) {
      bridgeHiddenEdges({
        edges,
        seen,
        source,
        currentRawId: rawTo,
        outgoing,
        visible,
        bridgeableHiddenIds,
        physicalToSemantic,
        ...(edgeKind !== undefined ? { inheritedKind: edgeKind } : {}),
        visited,
      });
    }
  }
}

function pushEdge(
  edges: RunDisplayEdge[],
  seen: Set<string>,
  from: string,
  to: string,
  kind?: string,
): void {
  if (from === to) return;
  const edgeKind = kind ?? 'dependency';
  const key = `${from}->${to}:${edgeKind}`;
  if (seen.has(key)) return;
  seen.add(key);
  edges.push({ from, to, kind: edgeKind });
}

function outgoingDeps(deps: RunSnapshotDep[]): Map<string, RunSnapshotDep[]> {
  const out = new Map<string, RunSnapshotDep[]>();
  for (const dep of deps) {
    const from = nonEmpty(dep.from);
    const to = nonEmpty(dep.to);
    if (!from || !to) continue;
    out.set(from, [...(out.get(from) ?? []), dep]);
  }
  return out;
}

function bridgeableScopeCheckIds(raw: RunSnapshot): Set<string> {
  const ids = new Set<string>();
  for (const bead of raw.beads ?? []) {
    const id = nonEmpty(bead.id);
    if (!id) continue;
    const kind = nonEmpty(bead.metadata?.['gc.kind']) ?? nonEmpty(bead.kind);
    if (kind === 'scope-check') ids.add(id);
  }
  return ids;
}
