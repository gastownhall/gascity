import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { FormulaRunDetail } from 'gas-city-dashboard-shared';
import { FormulaRunDiagram } from './FormulaRunDiagram';

afterEach(() => cleanup());

describe('FormulaRunDiagram', () => {
  it('keeps the supervisor node order even when lanes group later nodes together', () => {
    render(
      <FormulaRunDiagram
        detail={detailWithInterleavedLaneNodes()}
        selectedNodeId={null}
        onToggleNode={vi.fn()}
      />,
    );

    const labels = screen.getAllByRole('button').map((button) => button.textContent ?? '');

    expect(labels[0]).toContain('Root');
    expect(labels[1]).toContain('Rig A setup');
    expect(labels[2]).toContain('Rig B review');
    expect(labels[3]).toContain('Rig A finalize');
    expect(screen.getAllByText('rig-a')).toHaveLength(2);
    expect(screen.getByText('rig-b')).toBeTruthy();
  });

  it('omits historical-only loop nodes from the left graph', () => {
    render(
      <FormulaRunDiagram
        detail={detailWithHistoricalOnlyNode()}
        selectedNodeId={null}
        onToggleNode={vi.fn()}
      />,
    );

    expect(screen.queryByRole('button', { name: /old-only review/i })).toBeNull();
    expect(screen.getByRole('button', { name: /current review/i })).toBeTruthy();
  });

  it('keeps dependency edges out of the primary graph chrome', () => {
    const detail = detailWithInterleavedLaneNodes();
    detail.edges = [
      { from: 'root', to: 'rig-a-setup', kind: 'blocks' },
      { from: 'rig-a-setup', to: 'rig-b-review', kind: 'blocks' },
    ];
    render(<FormulaRunDiagram detail={detail} selectedNodeId={null} onToggleNode={vi.fn()} />);

    expect(screen.queryByText(/step dependencies/i)).toBeNull();
    expect(screen.queryByText(/Root -> Rig A setup · blocks/i)).toBeNull();
    expect(screen.queryByText(/Rig A setup -> Rig B review · blocks/i)).toBeNull();
  });
});

function detailWithInterleavedLaneNodes(): FormulaRunDetail {
  return {
    runId: 'gc-root',
    rootBeadId: 'gc-root',
    rootStoreRef: 'city:racoon-city',
    resolvedRootStore: 'city:racoon-city',
    scopeKind: 'city',
    scopeRef: 'racoon-city',
    title: 'Interleaved run',
    formula: { kind: 'known', name: 'mol-test', source: 'metadata' },
    formulaDetail: { kind: 'available', name: 'mol-test', target: 'racoon-city/codex' },
    executionPath: { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' },
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 1 },
    completeness: { kind: 'complete' },
    phase: 'active',
    stages: [],
    progress: progress(4, 4, { ready: 4 }),
    nodes: [
      node('root', 'Root'),
      node('rig-a-setup', 'Rig A setup', 'rig-a'),
      node('rig-b-review', 'Rig B review', 'rig-b'),
      node('rig-a-finalize', 'Rig A finalize', 'rig-a'),
    ],
    edges: [],
    lanes: [
      { id: '__run', label: 'Run', nodeIds: ['root'] },
      { id: 'rig-a', label: 'rig-a', nodeIds: ['rig-a-setup', 'rig-a-finalize'] },
      { id: 'rig-b', label: 'rig-b', nodeIds: ['rig-b-review'] },
    ],
  };
}

function detailWithHistoricalOnlyNode(): FormulaRunDetail {
  return {
    runId: 'gc-root',
    rootBeadId: 'gc-root',
    rootStoreRef: 'city:racoon-city',
    resolvedRootStore: 'city:racoon-city',
    scopeKind: 'city',
    scopeRef: 'racoon-city',
    title: 'Historical loop run',
    formula: { kind: 'known', name: 'mol-test', source: 'metadata' },
    formulaDetail: { kind: 'available', name: 'mol-test', target: 'racoon-city/codex' },
    executionPath: { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' },
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 1 },
    completeness: { kind: 'complete' },
    phase: 'active',
    stages: [],
    progress: progress(2, 1, { active: 1 }, { active: 1, completed: 1 }, 2),
    nodes: [
      {
        id: 'old-only-review',
        semanticNodeId: 'old-only-review',
        title: 'Old-only review',
        kind: 'task',
        constructKind: 'step',
        status: 'completed',
        currentBeadId: 'gc-old-only-review-i1',
        scope: { kind: 'run' },
        historicalOnly: true,
        visibleInGraph: false,
        iterationSummary: {
          kind: 'stacked',
          visibleIteration: 1,
          iterationCount: 2,
          control: { kind: 'unknown' },
        },
        attemptSummary: { kind: 'none' },
        visibleExecutionInstanceId: 'gc-old-only-review-i1',
        executionInstances: [
          {
            id: 'gc-old-only-review-i1',
            semanticNodeId: 'old-only-review',
            beadId: 'gc-old-only-review-i1',
            iteration: { kind: 'loop', value: 1 },
            attempt: { kind: 'untracked' },
            label: 'iteration 1',
            status: 'completed',
            session: { kind: 'none', reason: 'session_unresolved' },
            historical: true,
            currentIteration: false,
          },
        ],
        controlBadges: [],
      },
      {
        id: 'current-review',
        semanticNodeId: 'current-review',
        title: 'Current review',
        kind: 'task',
        constructKind: 'step',
        status: 'active',
        currentBeadId: 'gc-current-review-i2',
        scope: { kind: 'run' },
        historicalOnly: false,
        visibleInGraph: true,
        iterationSummary: {
          kind: 'stacked',
          visibleIteration: 2,
          iterationCount: 2,
          control: { kind: 'unknown' },
        },
        attemptSummary: { kind: 'none' },
        visibleExecutionInstanceId: 'gc-current-review-i2',
        executionInstances: [
          {
            id: 'gc-current-review-i2',
            semanticNodeId: 'current-review',
            beadId: 'gc-current-review-i2',
            iteration: { kind: 'loop', value: 2 },
            attempt: { kind: 'untracked' },
            label: 'iteration 2',
            status: 'active',
            session: { kind: 'none', reason: 'session_unresolved' },
            historical: false,
            currentIteration: true,
          },
        ],
        controlBadges: [],
      },
    ],
    edges: [],
    lanes: [
      {
        id: '__run',
        label: 'Run',
        nodeIds: ['old-only-review', 'current-review'],
      },
    ],
  };
}

function progress(
  totalNodeCount: number,
  visibleNodeCount: number,
  statusCounts: FormulaRunDetail['progress']['statusCounts'],
  allStatusCounts = statusCounts,
  executionInstanceCount = totalNodeCount,
): FormulaRunDetail['progress'] {
  return {
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 1 },
    snapshotPartial: false,
    terminal: false,
    totalNodeCount,
    visibleNodeCount,
    edgeCount: 0,
    executionInstanceCount,
    sessionLinkCount: 0,
    streamableSessionCount: 0,
    streamableSessionIds: [],
    statusCounts,
    allStatusCounts,
  };
}

function node(id: string, title: string, scopeRef?: string): FormulaRunDetail['nodes'][number] {
  const displayNode: FormulaRunDetail['nodes'][number] = {
    id,
    semanticNodeId: id,
    title,
    kind: 'step',
    constructKind: 'step',
    status: 'ready',
    currentBeadId: id,
    scope: scopeRef === undefined ? { kind: 'run' } : { kind: 'scoped', ref: scopeRef },
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
        status: 'ready',
        session: { kind: 'none', reason: 'not_started' },
        currentIteration: true,
        historical: false,
      },
    ],
    controlBadges: [],
  };
  return displayNode;
}
