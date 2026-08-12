import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { DashboardBead, BeadStatus } from 'gas-city-dashboard-shared';
import { buildBeadGraph } from '../../lib/beadGraph';
import { BeadDependencies } from './BeadDependencies';

// gascity-dashboard-14s1: the needs/blocks dependency view moved off the
// cramped board row into the detail modal. These pin the navigation and
// unresolved-edge behaviour that previously lived in BeadBoard.test.

afterEach(() => cleanup());

function bead(id: string, status: BeadStatus, extra: Partial<DashboardBead> = {}): DashboardBead {
  return {
    id,
    title: `bead ${id}`,
    status,
    issue_type: 'task',
    priority: null,
    created_at: '2026-05-01T00:00:00Z',
    ...extra,
  };
}

function nodeFor(id: string, beads: DashboardBead[]) {
  // buildBeadGraph consumes SupervisorBead (priority: number), while these
  // fixtures model DashboardBead (priority: number | null). Normalize the
  // non-priority rows to 0 so the graph input matches the supervisor shape.
  const graph = buildBeadGraph(beads.map((b) => ({ ...b, priority: b.priority ?? 0 })));
  const node = graph.nodes.get(id);
  if (!node) throw new Error(`no node for ${id}`);
  return node;
}

describe('BeadDependencies', () => {
  it('renders an upstream need and re-centres on click', () => {
    const onOpenBead = vi.fn();
    // B needs A; both inside the window, so A resolves to a navigable row.
    const node = nodeFor('B', [bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })]);
    render(<BeadDependencies node={node} onOpenBead={onOpenBead} />);
    fireEvent.click(screen.getByTitle('Open A'));
    expect(onOpenBead).toHaveBeenCalledWith('A');
  });

  it('renders a downstream blocker (B blocks the beads that need it)', () => {
    const onOpenBead = vi.fn();
    const node = nodeFor('A', [bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })]);
    render(<BeadDependencies node={node} onOpenBead={onOpenBead} />);
    // A's downstream "blocks" set contains B.
    fireEvent.click(screen.getByTitle('Open B'));
    expect(onOpenBead).toHaveBeenCalledWith('B');
  });

  it('marks an unresolved upstream edge with no navigation target', () => {
    const node = nodeFor('B', [bead('B', 'open', { needs: ['GHOST'] })]);
    render(<BeadDependencies node={node} onOpenBead={vi.fn()} />);
    expect(screen.queryByTitle('Open GHOST')).toBeNull();
    expect(screen.getByText('unresolved')).toBeTruthy();
  });

  it('states plainly when there are no dependencies', () => {
    const node = nodeFor('A', [bead('A', 'open')]);
    render(<BeadDependencies node={node} />);
    expect(screen.getByText(/no dependencies/i)).toBeTruthy();
  });
});
