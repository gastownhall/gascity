import { cleanup, render, screen } from '@testing-library/react';
import type {
  RunDisplayNode,
  RunExecutionInstance,
  RunNodeStatus,
} from 'gas-city-dashboard-shared';
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

  it('shows the server-picked visible instance even when the old heuristic would pick another', () => {
    // The retired client heuristic preferred the last attempt in sort order
    // (attempt 2). The server points visibleExecutionInstanceId at attempt 1, so
    // that instance must be shown — proving the server pick wins.
    render(<RunNodeSessionPanel node={multiAttemptNode('review-a1')} visible />);

    expect(screen.getByText('review-bead-a1')).toBeTruthy();
    expect(screen.queryByText('review-bead-a2')).toBeNull();
  });

  it('falls back to the last instance when the server id is absent or unknown', () => {
    // An empty visibleExecutionInstanceId (or one that matches no instance) falls
    // back to the last instance in sort order — attempt 2 here.
    render(<RunNodeSessionPanel node={multiAttemptNode('')} visible />);

    expect(screen.getByText('review-bead-a2')).toBeTruthy();
    expect(screen.queryByText('review-bead-a1')).toBeNull();
  });

  it('does not crash and shows graceful copy for an attached session with no link (public-shield shape)', () => {
    // The public floor emits `session: { kind: 'attached' }` with no link — the
    // shape that previously threw `undefined.sessionId` in the ErrorBoundary.
    // The shared type now models this redacted shape directly, so the fixture is
    // type-correct without casting around the contract.
    const node = attachedNode({ kind: 'attached' });

    expect(() => render(<RunNodeSessionPanel node={node} visible />)).not.toThrow();

    expect(screen.getByText('Session transcript is unavailable for this node.')).toBeTruthy();
    // A null id must never reach the transcript fetch.
    expect(mockFetchSupervisorSessionTranscript).not.toHaveBeenCalled();
  });

  it('renders the transcript path for an attached session that carries a link', () => {
    const node = attachedNode({
      kind: 'attached',
      streamable: false,
      link: { sessionId: 'gc-session-review', sessionName: 'review-pipeline', assignee: 'codex' },
    });

    render(<RunNodeSessionPanel node={node} visible />);

    expect(mockFetchSupervisorSessionTranscript).toHaveBeenCalledWith('gc-session-review');
    expect(screen.getByText('Fetching transcript.')).toBeTruthy();
    expect(screen.queryByText('Session transcript is unavailable for this node.')).toBeNull();
  });
});

function attempt(value: number, status: RunNodeStatus): RunExecutionInstance {
  return {
    id: `review-a${value}`,
    semanticNodeId: 'review',
    beadId: `review-bead-a${value}`,
    iteration: { kind: 'base' },
    attempt: { kind: 'attempt', value },
    label: `attempt ${value}`,
    status,
    session: { kind: 'none', reason: 'not_started' },
    currentIteration: value === 2,
    historical: false,
  };
}

function multiAttemptNode(visibleExecutionInstanceId: string): RunDisplayNode {
  return {
    id: 'review',
    semanticNodeId: 'review',
    title: 'Review',
    kind: 'step',
    constructKind: 'step',
    status: 'active',
    currentBeadId: 'review',
    scope: { kind: 'run' },
    visibleInGraph: true,
    historicalOnly: false,
    iterationSummary: { kind: 'single' },
    attemptSummary: {
      kind: 'tracked',
      count: 2,
      badge: { kind: 'bounded', label: '2/3' },
      active: { kind: 'idle' },
    },
    visibleExecutionInstanceId,
    executionInstances: [attempt(1, 'failed'), attempt(2, 'active')],
    controlBadges: [],
  };
}

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
