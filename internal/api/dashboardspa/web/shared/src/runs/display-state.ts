import type { RunDisplayEdge, RunDisplayNode, RunNodeStatus } from '../run-detail.js';

const TERMINAL_STATUSES = new Set<RunNodeStatus>(['completed', 'done', 'failed', 'skipped']);

/**
 * Convert raw bead state into graph presentation state. The supervisor exposes
 * durable bead status, while the dashboard needs to show whether a waiting node
 * is actually ready to be claimed or still blocked by upstream work.
 */
export function applyDisplayNodeStates(
  nodes: readonly RunDisplayNode[],
  edges: readonly RunDisplayEdge[],
): RunDisplayNode[] {
  const byId = new Map(nodes.map((node) => [node.id, node]));
  const inbound = buildInboundEdges(edges, byId);
  const statusById = new Map<string, RunNodeStatus>();

  for (const node of nodes) {
    statusById.set(node.id, displayStatusFor(node, inbound.get(node.id) ?? [], byId));
  }

  return nodes.map((node) => {
    const status = statusById.get(node.id) ?? node.status;
    if (status === node.status) return node;
    return {
      ...node,
      status,
      executionInstances: node.executionInstances.map((instance) =>
        instance.currentIteration !== false && instance.status === 'pending'
          ? { ...instance, status }
          : instance,
      ),
    };
  });
}

function displayStatusFor(
  node: RunDisplayNode,
  blockers: readonly string[],
  byId: Map<string, RunDisplayNode>,
): RunNodeStatus {
  if (node.status !== 'pending') return node.status;
  if (blockers.length === 0) return 'ready';
  const allDone = blockers.every((blockerId) => {
    const blocker = byId.get(blockerId);
    return blocker ? TERMINAL_STATUSES.has(blocker.status) : false;
  });
  return allDone ? 'ready' : 'blocked';
}

function buildInboundEdges(
  edges: readonly RunDisplayEdge[],
  byId: Map<string, RunDisplayNode>,
): Map<string, string[]> {
  const inbound = new Map<string, string[]>();
  for (const edge of edges) {
    if (!byId.has(edge.from) || !byId.has(edge.to)) continue;
    inbound.set(edge.to, [...(inbound.get(edge.to) ?? []), edge.from]);
  }
  return inbound;
}
