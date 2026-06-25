import { cleanup, render, screen } from '@testing-library/react';
import type { RunDisplayNode, RunNodeStatus } from 'gas-city-dashboard-shared';
import { afterEach, describe, expect, it } from 'vitest';
import { RunNodeSessionPanel } from './RunNodeSessionPanel';

afterEach(() => cleanup());

describe('RunNodeSessionPanel', () => {
  it('distinguishes a running node with unresolved session metadata', () => {
    render(<RunNodeSessionPanel node={node('active', 'session_unresolved')} visible />);

    expect(screen.getByText('Session unresolved for the current running node.')).toBeTruthy();
  });

  it('distinguishes work that has not started a session yet', () => {
    render(<RunNodeSessionPanel node={node('ready', 'not_started')} visible />);

    expect(screen.getByText('This node has not started a session yet.')).toBeTruthy();
  });

  it('exposes selected execution instance identity for operator inspection', () => {
    render(<RunNodeSessionPanel node={node('ready', 'not_started')} visible />);

    expect(screen.getByText('Execution instance')).toBeTruthy();
    expect(screen.getByText('review-exec')).toBeTruthy();
    expect(screen.getByText('Bead')).toBeTruthy();
    expect(screen.getByText('review-bead')).toBeTruthy();
  });
});

function node(status: RunNodeStatus, reason: 'not_started' | 'session_unresolved'): RunDisplayNode {
  return {
    id: 'review',
    semanticNodeId: 'review',
    title: 'Review',
    kind: 'step',
    constructKind: 'step',
    status,
    currentBeadId: 'review',
    scope: { kind: 'run' },
    visibleInGraph: true,
    historicalOnly: false,
    iterationSummary: { kind: 'single' },
    attemptSummary: { kind: 'none' },
    visibleExecutionInstanceId: 'review',
    executionInstances: [
      {
        id: 'review-exec',
        semanticNodeId: 'review',
        beadId: 'review-bead',
        iteration: { kind: 'base' },
        attempt: { kind: 'untracked' },
        label: 'base',
        status,
        session: { kind: 'none', reason },
        currentIteration: true,
        historical: false,
      },
    ],
    controlBadges: [],
  };
}
