import { describe, expect, it } from 'vitest';
import type { SupervisorBead } from '../supervisor/beadReads';
import {
  buildCanvasEdges,
  computeCanvasLayout,
  fitCanvas,
  type CanvasLayoutItem,
} from './beadsCanvasLayout';

describe('beadsCanvasLayout', () => {
  it('keeps every issue type in a deterministic cluster territory', () => {
    const items = [
      item('ga-bug', 'bug'),
      item('ga-feature', 'feature'),
      item('ga-epic', 'epic'),
      item('ga-decision', 'decision'),
      item('ga-custom', 'investigation'),
    ];

    const first = computeCanvasLayout(items, 'clusters', 16 / 9);
    const second = computeCanvasLayout([...items].reverse(), 'clusters', 16 / 9);

    expect(first.territories.map((territory) => territory.label)).toEqual([
      'bugs',
      'features',
      'epics',
      'decisions',
      'investigations',
    ]);
    expect([...first.positions.entries()]).toEqual([...second.positions.entries()]);
    expect(first.positions.size).toBe(items.length);
  });

  it('places priority lanes in P0 to P4 order and defaults malformed priorities to P3', () => {
    const layout = computeCanvasLayout(
      [item('ga-p4', 'task', 4), item('ga-p0', 'task', 0), item('ga-bad', 'task', 99)],
      'lanes',
      1,
    );

    expect(layout.territories.map((territory) => territory.label)).toEqual([
      'P0 · critical',
      'P1 · high',
      'P2 · medium',
      'P3 · low',
      'P4 · backlog',
    ]);
    expect(
      layout.territories.find((territory) => territory.label.startsWith('P3'))?.members,
    ).toEqual(['ga-bad']);
  });

  it('projects needs, typed dependencies, and parent relations as directed edges', () => {
    const beads: SupervisorBead[] = [
      bead('ga-root'),
      bead('ga-child', {
        parent: 'ga-root',
        needs: ['ga-blocker'],
        dependencies: [{ issue_id: 'ga-child', depends_on_id: 'ga-related', type: 'references' }],
      }),
      bead('ga-blocker'),
      bead('ga-related'),
    ];

    expect(buildCanvasEdges(beads)).toEqual([
      { from: 'ga-blocker', to: 'ga-child', relation: 'blocks' },
      { from: 'ga-related', to: 'ga-child', relation: 'references' },
      { from: 'ga-root', to: 'ga-child', relation: 'contains' },
    ]);
  });

  it('fits world bounds inside the usable viewport', () => {
    const fitted = fitCanvas(
      { x: 100, y: 200, width: 1_000, height: 500 },
      { width: 800, height: 600 },
      40,
    );

    expect(fitted.zoom).toBeCloseTo(0.72);
    expect(fitted.pan.x).toBeCloseTo(-32);
    expect(fitted.pan.y).toBeCloseTo(-24);
  });
});

function item(id: string, issueType: string, priority = 2): CanvasLayoutItem {
  return { id, issueType, priority, width: 220, height: 112 };
}

function bead(id: string, overrides: Partial<SupervisorBead> = {}): SupervisorBead {
  return {
    id,
    title: id,
    status: 'open',
    priority: 2,
    issue_type: 'task',
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  };
}
