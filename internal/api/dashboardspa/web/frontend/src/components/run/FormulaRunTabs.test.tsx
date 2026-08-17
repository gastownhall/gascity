import { cleanup, render, screen } from '@testing-library/react';
import type { RunDisplayNode } from 'gas-city-dashboard-shared';
import { afterEach, describe, expect, it } from 'vitest';
import { FormulaRunTabs } from './FormulaRunTabs';

afterEach(() => cleanup());

describe('FormulaRunTabs', () => {
  it('renders the Session transcript view with no Diff tab', () => {
    render(<FormulaRunTabs selectedNode={nodeWithoutSession()} />);

    const sessionTab = screen.getByRole('tab', { name: 'Session' });
    expect(sessionTab.hasAttribute('disabled')).toBe(false);
    expect(screen.queryByRole('tab', { name: 'Diff' })).toBeNull();
    expect(screen.getByRole('tabpanel').getAttribute('aria-labelledby')).toBe(
      'run-evidence-tab-session',
    );
    expect(screen.getByText('Session unresolved for the current running node.')).toBeTruthy();
  });

  it('prompts for a node before selection', () => {
    render(<FormulaRunTabs selectedNode={null} />);

    expect(screen.getByRole('tab', { name: 'Session' })).toBeTruthy();
    expect(screen.getByText(/select a node to inspect its session/i)).toBeTruthy();
  });
});

function nodeWithoutSession(): RunDisplayNode {
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
    attemptSummary: { kind: 'none' },
    visibleExecutionInstanceId: 'review',
    executionInstances: [
      {
        id: 'review',
        semanticNodeId: 'review',
        beadId: 'review',
        iteration: { kind: 'base' },
        attempt: { kind: 'untracked' },
        label: 'base',
        status: 'active',
        session: { kind: 'none', reason: 'session_unresolved' },
        currentIteration: true,
        historical: false,
      },
    ],
    controlBadges: [],
  };
}
