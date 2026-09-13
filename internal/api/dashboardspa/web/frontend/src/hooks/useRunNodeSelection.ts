import type { RunDisplayNode, FormulaRunDetail } from 'gas-city-dashboard-shared';
import { useCallback, useEffect, useMemo, useState } from 'react';

interface NodeSelection {
  nodeId: string | null;
  routeKey: string;
  source: 'route' | 'user';
}

interface RunNodeSelection {
  selectedNodeId: string | null;
  selectedNode: RunDisplayNode | null;
  toggleNode: (nodeId: string) => void;
  clearSelection: () => void;
}

export function useRunNodeSelection(
  detail: FormulaRunDetail | null,
  routeNodeId: string | null,
  routeKey: string,
): RunNodeSelection {
  const [selection, setSelection] = useState<NodeSelection>({
    nodeId: null,
    routeKey: '',
    source: 'route',
  });

  useEffect(() => {
    if (!detail) return;
    const nextRouteNodeId = selectedRouteNodeId(detail, routeNodeId);
    setSelection((current) => {
      if (current.routeKey === routeKey) {
        if (current.source === 'user') return current;
        if (current.nodeId === nextRouteNodeId) return current;
      }
      return {
        nodeId: nextRouteNodeId,
        routeKey,
        source: 'route',
      };
    });
  }, [detail, routeKey, routeNodeId]);

  const clearSelection = useCallback(() => {
    setSelection((current) => ({
      nodeId: null,
      routeKey: current.routeKey,
      source: 'user',
    }));
  }, []);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') clearSelection();
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [clearSelection]);

  const toggleNode = useCallback(
    (nodeId: string) => {
      setSelection((current) => ({
        nodeId: current.nodeId === nodeId ? null : nodeId,
        routeKey,
        source: 'user',
      }));
    },
    [routeKey],
  );

  const selectedNodeId = selection.nodeId;
  const selectedNode = useMemo<RunDisplayNode | null>(
    () => detail?.nodes.find((node) => node.id === selectedNodeId) ?? null,
    [detail, selectedNodeId],
  );

  return {
    selectedNodeId,
    selectedNode,
    toggleNode,
    clearSelection,
  };
}

function selectedRouteNodeId(detail: FormulaRunDetail, nodeId: string | null): string | null {
  if (!nodeId) return null;
  return detail.nodes.some((node) => node.id === nodeId) ? nodeId : null;
}
