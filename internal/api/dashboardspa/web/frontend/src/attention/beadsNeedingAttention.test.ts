import { describe, expect, it } from 'vitest';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import {
  inProgressCardNote,
  selectBeadsNeedingAttention,
  type BeadAttentionSession,
} from './beadsNeedingAttention';

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

function session(overrides: Partial<BeadAttentionSession>): BeadAttentionSession {
  return {
    id: 'ci-1',
    session_name: 'worker-ci-1',
    state: 'active',
    last_active: '2026-06-07T11:58:00.000Z',
    ...overrides,
  };
}

function select(
  inputs: {
    beads?: readonly Bead[];
    escalations?: readonly Bead[];
    sessions?: readonly BeadAttentionSession[];
  },
  now = NOW,
) {
  return selectBeadsNeedingAttention(
    {
      beads: inputs.beads ?? [],
      escalations: inputs.escalations ?? [],
      ...(inputs.sessions === undefined ? {} : { sessions: inputs.sessions }),
    },
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

describe('stalled in-progress detection (gp-6xd)', () => {
  const inProgress = (overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-doing',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T10:00:00.000Z',
      ...overrides,
    });

  it('marks an in-progress bead with no assignee as stalled', () => {
    const rows = select({
      beads: [
        bead({ id: 'B-doing', status: 'in_progress', updated_at: '2026-06-07T10:00:00.000Z' }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-doing',
        reason: 'stalled',
        severity: 'attention',
        summary: expect.stringContaining('no assignee'),
      }),
    ]);
  });

  it('marks an in-progress bead as stalled when no session resolves to the assignee', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ session_name: 'someone-else' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no live session for worker-ci-1'),
      }),
    );
  });

  it('marks an in-progress bead as stalled when its session is not in a live state', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ state: 'dead' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('session dead'),
      }),
    );
  });

  it('marks an in-progress bead as stalled when activity is older than an hour', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ last_active: '2026-06-07T10:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('no activity'),
      }),
    );
  });

  it('does not mark a bead with a live, recently-active session', () => {
    const rows = select({ beads: [inProgress()], sessions: [session({})] });
    expect(rows).toEqual([]);
  });

  it('a fresh bead heartbeat keeps a quiet session from reading as stalled', () => {
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T11:59:00.000Z' } })],
      sessions: [session({ last_active: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('resolves the session by gc.session_id when the assignee name does not match', () => {
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.session_id': 'ci-9' } })],
      sessions: [session({ id: 'ci-9', session_name: 'renamed' })],
    });
    expect(rows).toEqual([]);
  });

  it('skips session-dependent checks when the session read failed (sessions omitted)', () => {
    const rows = select({ beads: [inProgress()] });
    expect(rows).toEqual([]);
  });

  it('does not guess stalled from a stale heartbeat alone when the session read failed', () => {
    // The worker may be alive but not heartbeating (the common pre-fix state);
    // only session data can distinguish that from a dead worker — do not guess.
    const rows = select({
      beads: [inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T09:00:00.000Z' } })],
    });
    expect(rows).toEqual([]);
  });

  it('prefers a live session over a dead one carrying the same recycled name', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ id: 'ci-old', state: 'dead' }), session({ id: 'ci-new' })],
    });
    expect(rows).toEqual([]);
  });

  it('resolves an assignee that is a bare session id', () => {
    const rows = select({
      beads: [inProgress({ assignee: 'ci-1' })],
      sessions: [session({ session_name: 'unrelated-name' })],
    });
    expect(rows).toEqual([]);
  });
});

describe('waiting-on-human detection (gp-6xd)', () => {
  const held = (overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-held',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T11:30:00.000Z',
      metadata: { 'gc.checkpoint_hold': 'founder design review (Taylor+Afik)' },
      ...overrides,
    });

  it('surfaces a checkpoint-held bead as watch, naming who it waits on', () => {
    const rows = select({ beads: [held()] });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-held',
        reason: 'waiting-human',
        severity: 'watch',
        summary: expect.stringContaining('waiting on Taylor+Afik'),
      }),
    ]);
  });

  it('escalates to attention once the wait passes two hours', () => {
    const rows = select({ beads: [held({ updated_at: '2026-06-07T09:00:00.000Z' })] });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'attention' }));
  });

  it('prefers the stamped gc.waiting_on name over the hold-text parenthetical', () => {
    const rows = select({
      beads: [
        held({
          metadata: {
            'gc.checkpoint_hold': 'review (Taylor+Afik)',
            'gc.waiting_on': 'Afik',
          },
        }),
      ],
    });
    expect(rows[0]?.summary).toContain('waiting on Afik');
  });

  it('falls back to "human" when the hold names nobody', () => {
    const rows = select({
      beads: [held({ metadata: { 'gc.founder_gate': 'design signoff' } })],
    });
    expect(rows[0]?.summary).toContain('waiting on human');
  });

  it('recognizes a hold: label as a waiting marker', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-held',
          status: 'in_progress',
          assignee: 'worker-ci-1',
          updated_at: '2026-06-07T11:30:00.000Z',
          labels: ['hold:founder (Taylor)'],
        }),
      ],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on Taylor'),
      }),
    );
  });

  it('takes precedence over stalled — a parked worker is not lost, it is waiting', () => {
    const rows = select({
      beads: [held()],
      sessions: [session({ state: 'dead' })],
    });
    expect(rows.map((row) => row.reason)).toEqual(['waiting-human']);
  });

  it('takes precedence over ready-unclaimed — an open unassigned gate bead is waiting, not claimable', () => {
    const rows = select({
      beads: [
        bead({
          id: 'B-gate',
          status: 'open',
          created_at: '2026-06-01T11:00:00.000Z',
          updated_at: '2026-06-07T11:30:00.000Z',
          metadata: { 'gc.founder_gate': 'design signoff (Taylor)' },
        }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-gate',
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on Taylor'),
      }),
    ]);
  });
});

describe('inProgressCardNote (gp-6xd)', () => {
  it('shows assignee and activity age for a healthy in-progress bead', () => {
    const note = inProgressCardNote(
      bead({ status: 'in_progress', assignee: 'worker-ci-1' }),
      [session({ last_active: '2026-06-07T11:52:00.000Z' })],
      NOW,
    );
    expect(note).toBe('worker-ci-1 · active 8m ago');
  });

  it('shows the stalled reason when the session is gone', () => {
    const note = inProgressCardNote(
      bead({
        status: 'in_progress',
        assignee: 'worker-ci-1',
        updated_at: '2026-06-06T12:00:00.000Z',
      }),
      [],
      NOW,
    );
    expect(note).toContain('stalled');
    expect(note).toContain('no live session');
  });

  it('shows the waiting reason for a checkpoint-held bead', () => {
    const note = inProgressCardNote(
      bead({
        status: 'in_progress',
        assignee: 'worker-ci-1',
        updated_at: '2026-06-07T11:30:00.000Z',
        metadata: { 'gc.checkpoint_hold': 'gate (Afik)' },
      }),
      [session({})],
      NOW,
    );
    expect(note).toContain('waiting on Afik');
  });

  it('returns null for beads that are not in progress', () => {
    expect(inProgressCardNote(bead({ status: 'open' }), [], NOW)).toBeNull();
  });
});
