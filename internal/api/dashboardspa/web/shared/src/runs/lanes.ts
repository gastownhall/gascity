import type { RunDisplayLane, RunDisplayNode } from '../run-detail.js';

const RUN_SCOPE = '__run';

export function buildRunDisplayLanes(nodes: RunDisplayNode[]): RunDisplayLane[] {
  const byScope = new Map<string, RunDisplayLane>();
  for (const node of nodes) {
    const scope = node.scope.kind === 'scoped' ? node.scope.ref : RUN_SCOPE;
    const existing = byScope.get(scope) ?? {
      id: scope,
      label: scope === RUN_SCOPE ? 'Run' : scope,
      nodeIds: [],
    };
    existing.nodeIds.push(node.id);
    byScope.set(scope, existing);
  }
  return [...byScope.values()];
}
