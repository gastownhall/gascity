import type { RunDisplayNode, FormulaRunDetail } from 'gas-city-dashboard-shared';
import { FormulaRunNode } from './FormulaRunNode';

interface FormulaRunDiagramProps {
  detail: FormulaRunDetail;
  selectedNodeId: string | null;
  onToggleNode: (nodeId: string) => void;
}

export function FormulaRunDiagram({
  detail,
  selectedNodeId,
  onToggleNode,
}: FormulaRunDiagramProps) {
  const nodes = orderedNodes(detail);
  const laneLabels = nodeLaneLabels(detail);

  if (nodes.length === 0) {
    return (
      <p className="text-body text-fg-muted italic">
        No graph nodes have materialized for this formula run.
      </p>
    );
  }

  return (
    <section aria-label="Formula run graph">
      <div className="flex items-baseline justify-between gap-4">
        <h2 className="text-title text-fg">Formula Graph</h2>
      </div>
      <ol className="mt-5 space-y-3 relative">
        {nodes.map((node, index) => {
          const laneLabel = laneLabels.get(node.id);
          const previousLaneLabel =
            index > 0 ? laneLabels.get(nodes[index - 1]?.id ?? '') : undefined;
          const showLaneLabel = laneLabel !== undefined && laneLabel !== previousLaneLabel;
          return (
            <li key={node.id} className="relative pl-6">
              {showLaneLabel && (
                <p className="mb-1 text-label uppercase tracking-wider text-fg-faint">
                  {laneLabel}
                </p>
              )}
              {index < nodes.length - 1 && (
                <span
                  aria-hidden="true"
                  className="absolute left-2 top-10 bottom-[-0.75rem] border-l border-rule"
                />
              )}
              <span
                aria-hidden="true"
                className="absolute left-[0.3125rem] top-7 h-2 w-2 rounded-full bg-fg-faint"
              />
              <FormulaRunNode
                node={node}
                selected={selectedNodeId === node.id}
                onToggle={onToggleNode}
              />
            </li>
          );
        })}
      </ol>
    </section>
  );
}

function orderedNodes(detail: FormulaRunDetail): RunDisplayNode[] {
  return detail.nodes.filter((node) => node.visibleInGraph !== false);
}

function nodeLaneLabels(detail: FormulaRunDetail): Map<string, string> {
  const labels = new Map<string, string>();
  for (const lane of detail.lanes) {
    for (const nodeId of lane.nodeIds) {
      labels.set(nodeId, lane.label);
    }
  }
  return labels;
}
