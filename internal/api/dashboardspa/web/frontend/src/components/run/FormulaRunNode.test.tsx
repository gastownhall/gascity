import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { RunConstructKind, RunDisplayNode } from 'gas-city-dashboard-shared';
import { FormulaRunNode } from './FormulaRunNode';

afterEach(() => cleanup());

describe('FormulaRunNode', () => {
  it('uses distinct shape classes for first-pass graph.v2 constructs', () => {
    const constructs: RunConstructKind[] = [
      'run-root',
      'step',
      'retry',
      'check-loop',
      'scope',
      'condition',
      'fanout',
      'expansion',
    ];
    const classes = new Map<string, string>();

    for (const constructKind of constructs) {
      const { unmount } = render(
        <FormulaRunNode node={nodeFor(constructKind)} selected={false} onToggle={vi.fn()} />,
      );
      const button = screen.getByRole('button', {
        name: new RegExp(`sample ${constructKind.replace(/-/g, ' ')}`, 'i'),
      });
      const shapeClass = [...button.classList].find((className) =>
        className.startsWith('formula-run-node-shape-'),
      );
      expect(shapeClass, constructKind).toBeTruthy();
      classes.set(constructKind, shapeClass ?? '');
      unmount();
    }

    expect(new Set(classes.values()).size).toBe(constructs.length);
    expect(classes.get('run-root')).toBe('formula-run-node-shape-root');
    expect(classes.get('fanout')).not.toBe(classes.get('expansion'));
  });

  it('shows the active retry attempt when the backend marks one running', () => {
    render(
      <FormulaRunNode
        node={{
          ...nodeFor('retry'),
          attemptSummary: {
            kind: 'tracked',
            count: 3,
            badge: { kind: 'bounded', label: '2/3' },
            active: { kind: 'running', value: 2 },
          },
        }}
        selected={false}
        onToggle={vi.fn()}
      />,
    );

    expect(screen.getByText(/retry · attempt 2\/3 · running attempt 2/i)).toBeTruthy();
  });

  it('uses product language for run-level construct labels', () => {
    const { rerender } = render(
      <FormulaRunNode node={nodeFor('run-root')} selected={false} onToggle={vi.fn()} />,
    );

    expect(screen.getByText(/^run root$/i)).toBeTruthy();

    rerender(<FormulaRunNode node={nodeFor('run-finalize')} selected={false} onToggle={vi.fn()} />);

    expect(screen.getByText(/^finalize$/i)).toBeTruthy();
  });

  it('marks selected state visually without rendering selected as node copy', () => {
    render(<FormulaRunNode node={nodeFor('step')} selected onToggle={vi.fn()} />);

    const button = screen.getByRole('button', { name: /sample step/i });
    expect(button.getAttribute('aria-pressed')).toBe('true');
    expect(button.classList.contains('ring-2')).toBe(true);
    expect(screen.queryByText(/^selected$/i)).toBeNull();
  });
});

function nodeFor(constructKind: RunConstructKind): RunDisplayNode {
  return {
    id: `sample-${constructKind}`,
    semanticNodeId: `sample-${constructKind}`,
    title: `Sample ${constructKind.replace(/-/g, ' ')}`,
    kind: constructKind,
    constructKind,
    status: 'pending',
    currentBeadId: `sample-${constructKind}`,
    scope: { kind: 'run' },
    visibleInGraph: true,
    historicalOnly: false,
    iterationSummary: { kind: 'single' },
    attemptSummary: { kind: 'none' },
    visibleExecutionInstanceId: `sample-${constructKind}`,
    executionInstances: [
      {
        id: `sample-${constructKind}`,
        semanticNodeId: `sample-${constructKind}`,
        beadId: `sample-${constructKind}`,
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
