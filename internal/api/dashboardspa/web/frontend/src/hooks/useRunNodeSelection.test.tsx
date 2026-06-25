import { act, cleanup, fireEvent, renderHook, waitFor } from '@testing-library/react';
import type { RunDisplayNode, FormulaRunDetail } from 'gas-city-dashboard-shared';
import { afterEach, describe, expect, it } from 'vitest';
import { useRunNodeSelection } from './useRunNodeSelection';

afterEach(() => {
  cleanup();
});

describe('useRunNodeSelection', () => {
  it('starts empty and applies a valid route node when detail is available', async () => {
    const { result, rerender } = renderSelectionHook({
      detail: null,
      routeNodeId: 'review',
      routeKey: routeKey('review'),
    });

    expect(result.current.selectedNodeId).toBeNull();

    rerender({
      detail: detailWithNodes(['design', 'review']),
      routeNodeId: 'review',
      routeKey: routeKey('review'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBe('review'));
    expect(result.current.selectedNode?.title).toBe('Review');
  });

  it('waits for a query-selected node to appear in a refreshed detail payload', async () => {
    const { result, rerender } = renderSelectionHook({
      detail: detailWithNodes(['design']),
      routeNodeId: 'review',
      routeKey: routeKey('review'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBeNull());

    rerender({
      detail: detailWithNodes(['design', 'review']),
      routeNodeId: 'review',
      routeKey: routeKey('review'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBe('review'));
  });

  it('keeps user selection for the same route but reapplies selection when the route key changes', async () => {
    const detail = detailWithNodes(['design', 'review']);
    const { result, rerender } = renderSelectionHook({
      detail,
      routeNodeId: 'design',
      routeKey: routeKey('design'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBe('design'));

    act(() => result.current.toggleNode('review'));
    expect(result.current.selectedNodeId).toBe('review');

    rerender({
      detail,
      routeNodeId: 'design',
      routeKey: routeKey('design'),
    });
    expect(result.current.selectedNodeId).toBe('review');

    rerender({
      detail,
      routeNodeId: 'design',
      routeKey: routeKey('design', 'refreshed-route'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBe('design'));
  });

  it('clears route-driven selection when the node query is removed', async () => {
    const detail = detailWithNodes(['design', 'review']);
    const { result, rerender } = renderSelectionHook({
      detail,
      routeNodeId: 'review',
      routeKey: routeKey('review'),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBe('review'));

    rerender({
      detail,
      routeNodeId: null,
      routeKey: routeKey(null),
    });

    await waitFor(() => expect(result.current.selectedNodeId).toBeNull());
  });

  it('toggles one selected node and clears it on Escape', () => {
    const { result } = renderHook(() =>
      useRunNodeSelection(detailWithNodes(['design', 'review']), null, routeKey(null)),
    );

    act(() => result.current.toggleNode('design'));
    expect(result.current.selectedNodeId).toBe('design');

    act(() => result.current.toggleNode('review'));
    expect(result.current.selectedNodeId).toBe('review');

    act(() => result.current.toggleNode('review'));
    expect(result.current.selectedNodeId).toBeNull();

    act(() => result.current.toggleNode('design'));
    expect(result.current.selectedNodeId).toBe('design');

    fireEvent.keyDown(window, { key: 'Escape' });
    expect(result.current.selectedNodeId).toBeNull();
  });
});

interface HookProps {
  detail: FormulaRunDetail | null;
  routeNodeId: string | null;
  routeKey: string;
}

function renderSelectionHook(initialProps: HookProps) {
  return renderHook(
    ({ detail, routeNodeId, routeKey }: HookProps) =>
      useRunNodeSelection(detail, routeNodeId, routeKey),
    { initialProps },
  );
}

function routeKey(nodeId: string | null, suffix = 'route'): string {
  return ['wf-1', 'city', 'racoon-city', nodeId ?? '', suffix].join('\u0000');
}

function detailWithNodes(nodeIds: string[]): FormulaRunDetail {
  return {
    runId: 'wf-1',
    rootBeadId: 'wf-1',
    rootStoreRef: 'city:racoon-city',
    resolvedRootStore: 'city:racoon-city',
    scopeKind: 'city',
    scopeRef: 'racoon-city',
    title: 'Adopt PR',
    formula: { kind: 'known', name: 'mol-adopt-pr-v2', source: 'metadata' },
    formulaDetail: {
      kind: 'available',
      name: 'mol-adopt-pr-v2',
      target: 'racoon-city/codex',
    },
    executionPath: { kind: 'known', path: '/tmp/rig' },
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 1 },
    completeness: { kind: 'complete' },
    phase: 'active',
    stages: [],
    progress: {
      snapshotVersion: 1,
      snapshotEventSeq: { kind: 'known', seq: 1 },
      snapshotPartial: false,
      totalNodeCount: nodeIds.length,
      visibleNodeCount: nodeIds.length,
      edgeCount: 0,
      executionInstanceCount: 0,
      sessionLinkCount: 0,
      streamableSessionCount: 0,
      streamableSessionIds: [],
      statusCounts: { pending: nodeIds.length },
      allStatusCounts: { pending: nodeIds.length },
    },
    nodes: nodeIds.map((id) => displayNode(id)),
    edges: [],
    lanes: [{ id: 'default', label: 'Run', nodeIds }],
  };
}

function displayNode(id: string): RunDisplayNode {
  return {
    id,
    semanticNodeId: id,
    title: titleFor(id),
    kind: 'task',
    constructKind: 'step',
    status: 'pending',
    currentBeadId: id,
    scope: { kind: 'run' },
    visibleInGraph: true,
    historicalOnly: false,
    iterationSummary: { kind: 'single' },
    attemptSummary: { kind: 'none' },
    visibleExecutionInstanceId: id,
    executionInstances: [
      {
        id,
        semanticNodeId: id,
        beadId: id,
        iteration: { kind: 'base' },
        attempt: { kind: 'untracked' },
        label: 'base',
        status: 'pending',
        session: { kind: 'none', reason: 'not_started' },
        currentIteration: true,
        historical: false,
      },
    ],
    controlBadges: [],
  };
}

function titleFor(id: string): string {
  return id
    .split('-')
    .map((part) => part.charAt(0).toUpperCase() + part.slice(1))
    .join(' ');
}
