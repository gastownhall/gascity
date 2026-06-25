import { describe, expect, it } from 'vitest';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { selectBeadsNeedingAttention } from './beadsNeedingAttention';

const NOW = Date.parse('2026-06-07T12:00:00.000Z');

function bead(overrides: Partial<Bead>): Bead {
  return {
    created_at: '2026-06-07T11:00:00.000Z',
    id: 'B-0',
    issue_type: 'task',
    status: 'open',
    title: 'Bead',
    ...overrides,
  };
}

function select(inputs: { beads?: readonly Bead[]; escalations?: readonly Bead[] }, now = NOW) {
  return selectBeadsNeedingAttention(
    { beads: inputs.beads ?? [], escalations: inputs.escalations ?? [] },
    now,
  );
}

describe('selectBeadsNeedingAttention (gascity-dashboard-2j8e.3)', () => {
  it('includes a ready-unclaimed bead once it has aged past the watch window', () => {
    const rows = select({
      beads: [bead({ id: 'B-ready', status: 'open', created_at: '2026-06-05T11:00:00.000Z' })],
    });
    expect(rows).toEqual([
      expect.objectContaining({ beadId: 'B-ready', reason: 'ready-unclaimed', severity: 'watch' }),
    ]);
  });

  it('escalates a long-stale ready-unclaimed bead to attention', () => {
    const rows = select({
      beads: [bead({ id: 'B-stale', status: 'open', created_at: '2026-06-01T11:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({ reason: 'ready-unclaimed', severity: 'attention' }),
    );
  });

  it('does not surface a freshly-filed open bead as noise', () => {
    const rows = select({
      beads: [bead({ id: 'B-fresh', status: 'open', created_at: '2026-06-07T11:30:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('does not surface an assigned open bead as ready-unclaimed', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-assigned',
          status: 'open',
          assignee: 'worker-1',
          created_at: '2026-06-01T11:00:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([]);
  });

  it('includes an abnormally-blocked (escalated) bead immediately, regardless of age', () => {
    const rows = select({
      escalations: [
        bead({
          id: 'B-esc',
          status: 'blocked',
          labels: ['gc:escalation'],
          created_at: '2026-06-07T11:55:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({ beadId: 'B-esc', reason: 'escalated', severity: 'attention' }),
    ]);
  });

  it('excludes a plain dependency-blocked bead (working-as-intended queuing)', () => {
    const rows = select({
      beads: [bead({ id: 'B-dep', status: 'blocked', created_at: '2026-06-01T11:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('excludes a closed (resolved) escalation', () => {
    const rows = select({
      escalations: [bead({ id: 'B-done', status: 'closed', labels: ['gc:escalation'] })],
    });
    expect(rows).toEqual([]);
  });

  it('does not count a P1 high-priority open bead just for its priority', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-p1',
          status: 'open',
          priority: 1,
          assignee: 'worker-1',
          created_at: '2026-06-07T11:55:00.000Z',
        }),
      ],
    });
    expect(rows).toEqual([]);
  });

  it('combines ready-unclaimed and escalated across both inputs', () => {
    const rows = select({
      beads: [bead({ id: 'B-ready', status: 'open', created_at: '2026-06-05T11:00:00.000Z' })],
      escalations: [bead({ id: 'B-esc', status: 'blocked', labels: ['gc:escalation'] })],
    });
    expect(rows.map((row) => `${row.beadId}:${row.reason}`)).toEqual([
      'B-esc:escalated',
      'B-ready:ready-unclaimed',
    ]);
  });
});
