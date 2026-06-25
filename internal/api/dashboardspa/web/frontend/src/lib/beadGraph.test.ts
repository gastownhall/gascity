import { describe, expect, it } from 'vitest';
import type { BeadStatus } from 'gas-city-dashboard-shared';
import type { SupervisorBead } from '../supervisor/beadReads';
import { BOARD_COLUMNS, buildBeadGraph, selectColumns, type BoardColumnId } from './beadGraph';

// Minimal bead factory — only the fields beadGraph reads. Everything else
// is filled with inert defaults so the wire shape stays satisfied.
function bead(id: string, status: BeadStatus, extra: Partial<SupervisorBead> = {}): SupervisorBead {
  return {
    id,
    title: `bead ${id}`,
    status,
    issue_type: 'task',
    created_at: '2026-05-01T00:00:00Z',
    ...extra,
  };
}

describe('buildBeadGraph — forward / inverse edges', () => {
  it('builds forward edges from the needs[] field', () => {
    const graph = buildBeadGraph([bead('A', 'open'), bead('B', 'open', { needs: ['A'] })]);
    const b = graph.nodes.get('B');
    expect(b?.deps.map((d) => d.id)).toEqual(['A']);
    expect(b?.deps[0]?.bead?.id).toBe('A');
    expect(b?.deps[0]?.kind).toBe('needs');
  });

  it('builds forward edges from dependencies[].depends_on_id with the dep type as kind', () => {
    const graph = buildBeadGraph([
      bead('A', 'closed'),
      bead('B', 'open', {
        dependencies: [{ depends_on_id: 'A', issue_id: 'B', type: 'blocks' }],
      }),
    ]);
    const b = graph.nodes.get('B');
    expect(b?.deps.map((d) => d.id)).toEqual(['A']);
    expect(b?.deps[0]?.kind).toBe('blocks');
  });

  it('dedups an id that appears in both needs[] and dependencies[]', () => {
    const graph = buildBeadGraph([
      bead('A', 'closed'),
      bead('B', 'open', {
        needs: ['A'],
        dependencies: [{ depends_on_id: 'A', issue_id: 'B', type: 'blocks' }],
      }),
    ]);
    expect(graph.nodes.get('B')?.deps).toHaveLength(1);
  });

  it('computes the inverse blocks[] edge (in-set only)', () => {
    const graph = buildBeadGraph([bead('A', 'open'), bead('B', 'open', { needs: ['A'] })]);
    expect(graph.nodes.get('A')?.blocks.map((x) => x.id)).toEqual(['B']);
  });

  it('marks a dependency that points outside the fetched set as unresolved', () => {
    const graph = buildBeadGraph([bead('B', 'open', { needs: ['GHOST'] })]);
    const b = graph.nodes.get('B');
    expect(b?.deps[0]?.bead).toBeNull();
    expect(b?.hasUnresolvedDeps).toBe(true);
  });
});

describe('buildBeadGraph — column placement', () => {
  function columnOf(beads: SupervisorBead[], id: string): BoardColumnId | undefined {
    return buildBeadGraph(beads).nodes.get(id)?.column;
  }

  it('maps statuses to columns', () => {
    expect(columnOf([bead('A', 'in_progress')], 'A')).toBe('in_progress');
    expect(columnOf([bead('A', 'blocked')], 'A')).toBe('blocked');
    expect(columnOf([bead('A', 'closed')], 'A')).toBe('done');
  });

  it('places an open bead with no needs in the ready column', () => {
    const graph = buildBeadGraph([bead('A', 'open')]);
    expect(graph.nodes.get('A')?.column).toBe('ready');
    expect(graph.nodes.get('A')?.ready).toBe(true);
  });

  it('places an open bead whose needs are all closed in ready', () => {
    expect(columnOf([bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })], 'B')).toBe('ready');
  });

  it('keeps an open bead with an unmet need in the open column, not ready', () => {
    const graph = buildBeadGraph([bead('A', 'open'), bead('B', 'open', { needs: ['A'] })]);
    expect(graph.nodes.get('B')?.column).toBe('open');
    expect(graph.nodes.get('B')?.ready).toBe(false);
  });

  it('does not call a bead ready when a need is unresolved (honest, not optimistic)', () => {
    const graph = buildBeadGraph([bead('B', 'open', { needs: ['GHOST'] })]);
    expect(graph.nodes.get('B')?.ready).toBe(false);
    expect(graph.nodes.get('B')?.column).toBe('open');
  });
});

describe('buildBeadGraph — column contents', () => {
  it('exposes every column key even when empty', () => {
    const graph = buildBeadGraph([]);
    for (const col of BOARD_COLUMNS) {
      expect(graph.columns[col.id]).toEqual([]);
    }
  });

  it('sorts within a column by priority (missing last), then id', () => {
    const graph = buildBeadGraph([
      bead('Z', 'in_progress', { priority: 2 }),
      bead('A', 'in_progress'),
      bead('M', 'in_progress', { priority: 0 }),
    ]);
    expect(graph.columns.in_progress.map((n) => n.bead.id)).toEqual(['M', 'Z', 'A']);
  });
});

describe('selectColumns — per-rig projection', () => {
  it('keeps only the given ids, preserving each column order', () => {
    const graph = buildBeadGraph([
      bead('A', 'in_progress', { priority: 0 }),
      bead('B', 'in_progress', { priority: 1 }),
      bead('C', 'in_progress', { priority: 2 }),
    ]);
    const cols = selectColumns(graph, new Set(['A', 'C']));
    expect(cols.in_progress.map((n) => n.bead.id)).toEqual(['A', 'C']);
  });

  it('resolves a cross-rig dependency because the graph is built over all beads', () => {
    // A (rig 1) is closed; B (rig 2) needs A. Build the graph over both,
    // then project to rig 2 only — B is still ready (its need resolved),
    // proving the edge was not lost by the per-rig split.
    const graph = buildBeadGraph([bead('A', 'closed'), bead('B', 'open', { needs: ['A'] })]);
    const rig2 = selectColumns(graph, new Set(['B']));
    expect(rig2.ready.map((n) => n.bead.id)).toEqual(['B']);
    expect(rig2.open).toEqual([]);
  });

  it('returns empty columns for an id set that matches nothing', () => {
    const graph = buildBeadGraph([bead('A', 'open')]);
    const cols = selectColumns(graph, new Set(['nope']));
    for (const col of BOARD_COLUMNS) expect(cols[col.id]).toEqual([]);
  });
});
