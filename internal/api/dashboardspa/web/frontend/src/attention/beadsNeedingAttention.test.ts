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
  },
  now = NOW,
) {
  return selectBeadsNeedingAttention(
    {
      beads: inputs.beads ?? [],
      escalations: inputs.escalations ?? [],
    },
    now,
  );
}

describe('selectBeadsNeedingAttention (gascity-dashboard-2j8e.3)', () => {
  it('excludes an unassigned open bead regardless of age — unclaimed ready work is machine-operable', () => {
    const rows = select({
      beads: [bead({ id: 'B-ready', status: 'open', created_at: '2026-05-01T11:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('excludes an open bead assigned to an agent session (not the reserved human alias)', () => {
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

  it('includes a nonclosed open bead explicitly assigned to the reserved human alias', () => {
    const rows = select({
      beads: [bead({ id: 'B-human', status: 'open', assignee: 'human' })],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'B-human',
        reason: 'human-assigned',
        severity: 'attention',
        summary: expect.stringContaining('assigned to human'),
      }),
    ]);
  });

  it('includes a human-assigned bead in any nonclosed status', () => {
    const rows = select({
      beads: [
        bead({ id: 'B-human-blocked', status: 'blocked', assignee: 'human' }),
        bead({ id: 'B-human-doing', status: 'in_progress', assignee: 'human' }),
      ],
    });
    expect(rows.map((row) => row.beadId).sort()).toEqual(['B-human-blocked', 'B-human-doing']);
  });

  it('excludes a closed bead even when assigned to human', () => {
    const rows = select({
      beads: [bead({ id: 'B-human-done', status: 'closed', assignee: 'human' })],
    });
    expect(rows).toEqual([]);
  });

  it('includes an open escalation because the queue records its human escalation', () => {
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

  it('includes an open escalation even while an agent remains assigned', () => {
    const rows = select({
      escalations: [
        bead({
          id: 'B-esc-agent',
          status: 'blocked',
          assignee: 'worker-1',
          labels: ['gc:escalation'],
        }),
      ],
    });
    expect(rows.map((row) => row.beadId)).toEqual(['B-esc-agent']);
  });

  it('excludes a plain dependency-blocked bead with no human assignee (working-as-intended queuing)', () => {
    const rows = select({
      beads: [bead({ id: 'B-dep', status: 'blocked', created_at: '2026-06-01T11:00:00.000Z' })],
    });
    expect(rows).toEqual([]);
  });

  it('excludes a closed escalation', () => {
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

  // gascity-sc-4oix8: the regression this fix exists for. A stale unassigned
  // workflow root is machine-claimable however long it has aged, so it must
  // not count as "needs you"; a bead explicitly handed to the reserved
  // `human` alias must, every time. Both together must surface only the
  // human-owned bead.
  it('surfaces only the human-assigned bead when a stale unassigned root and a human-assigned bead are both present', () => {
    const rows = select({
      beads: [
        bead({ id: 'B-stale-root', status: 'open', created_at: '2026-05-01T11:00:00.000Z' }),
        bead({ id: 'B-human', status: 'open', assignee: 'human' }),
      ],
    });
    expect(rows.map((row) => row.beadId)).toEqual(['B-human']);
  });
});

// gascity-sc-4oix8: stalled in-progress detection is no longer part of
// selectBeadsNeedingAttention membership (a stalled agent is machine work,
// not a human need) — it now surfaces only through inProgressCardNote's board
// card, so these cases exercise that entry point directly. The underlying
// stalledRow/stalledDetail logic is unchanged.
describe('stalled in-progress detection (gp-6xd)', () => {
  const inProgress = (overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-doing',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T10:00:00.000Z',
      ...overrides,
    });

  const cardNote = (b: Bead, sessions?: readonly BeadAttentionSession[]) =>
    inProgressCardNote(b, sessions, NOW);

  it('does not surface stalled membership on the "Needs you" selector — stalled is machine work', () => {
    const rows = select({ beads: [inProgress()] });
    expect(rows).toEqual([]);
  });

  it('marks an in-progress bead with no assignee as stalled', () => {
    const note = cardNote(
      bead({ id: 'B-doing', status: 'in_progress', updated_at: '2026-06-07T10:00:00.000Z' }),
    );
    expect(note).toContain('stalled');
    expect(note).toContain('no assignee');
  });

  it('marks an in-progress bead as stalled when no session resolves to the assignee', () => {
    const note = cardNote(inProgress(), [session({ session_name: 'someone-else' })]);
    expect(note).toContain('no live session for worker-ci-1');
  });

  it('marks an in-progress bead as stalled when its session is not in a live state', () => {
    const note = cardNote(inProgress(), [session({ state: 'dead' })]);
    expect(note).toContain('session dead');
  });

  // A closed session decodes to the empty state ("closed beads have no runtime
  // state", internal/session/info_codec.go), which must not render as the
  // dangling "stalled 3h — session ".
  it('names an ended session rather than emitting a dangling empty state', () => {
    const note = cardNote(inProgress(), [session({ state: '' })]);
    expect(note).toContain('session ended');
    expect(note).not.toMatch(/session\s*$/);
  });

  // `state` is free-form with no enum upstream, and the sibling reader
  // (shared/src/agents/needsYou.ts) matches it case-insensitively — a differently
  // cased spelling must not paint a healthy worker stalled.
  it('treats a live state as live regardless of case', () => {
    const note = cardNote(inProgress(), [session({ state: 'Active' })]);
    expect(note).not.toContain('stalled');
  });

  it('marks an in-progress bead as stalled when activity is older than an hour', () => {
    const note = cardNote(inProgress(), [session({ last_active: '2026-06-07T10:00:00.000Z' })]);
    expect(note).toContain('no activity');
  });

  it('does not mark a bead with a live, recently-active session', () => {
    const note = cardNote(inProgress(), [session({})]);
    expect(note).not.toContain('stalled');
  });

  it('a fresh bead heartbeat keeps a quiet session from reading as stalled', () => {
    const note = cardNote(
      inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T11:59:00.000Z' } }),
      [session({ last_active: '2026-06-07T09:00:00.000Z' })],
    );
    expect(note).not.toContain('stalled');
  });

  it('resolves the session by gc.session_id when the assignee name does not match', () => {
    const note = cardNote(inProgress({ metadata: { 'gc.session_id': 'ci-9' } }), [
      session({ id: 'ci-9', session_name: 'renamed' }),
    ]);
    expect(note).not.toContain('stalled');
  });

  it('skips session-dependent checks when the session read failed (sessions omitted)', () => {
    const note = cardNote(inProgress());
    expect(note).not.toContain('stalled');
  });

  it('does not guess stalled from a stale heartbeat alone when the session read failed', () => {
    // The worker may be alive but not heartbeating (the common pre-fix state);
    // only session data can distinguish that from a dead worker — do not guess.
    const note = cardNote(
      inProgress({ metadata: { 'gc.last_heartbeat_at': '2026-06-07T09:00:00.000Z' } }),
    );
    expect(note).not.toContain('stalled');
  });

  it('prefers a live session over a dead one carrying the same recycled name', () => {
    const note = cardNote(inProgress(), [
      session({ id: 'ci-old', state: 'dead' }),
      session({ id: 'ci-new' }),
    ]);
    expect(note).not.toContain('stalled');
  });

  it('resolves an assignee that is a bare session id', () => {
    const note = cardNote(inProgress({ assignee: 'ci-1' }), [
      session({ session_name: 'unrelated-name' }),
    ]);
    expect(note).not.toContain('stalled');
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

  // The two canonical hold values (engdocs/contributors/hold-label-conventions.md,
  // internal/beadmeta/hold_labels.go). Neither carries a parenthetical, so
  // without an explicit mapping both would render the actorless "waiting on
  // human" on the most common real input.
  const heldByLabel = (label: string, overrides: Partial<Bead> = {}) =>
    bead({
      id: 'B-held',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      updated_at: '2026-06-07T11:30:00.000Z',
      labels: [label],
      ...overrides,
    });

  it('names the mayor for the canonical hold:mayor label', () => {
    const rows = select({ beads: [heldByLabel('hold:mayor')] });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'waiting-human',
        summary: expect.stringContaining('waiting on mayor'),
      }),
    );
  });

  it('names external for the canonical hold:external label', () => {
    const rows = select({ beads: [heldByLabel('hold:external')] });
    expect(rows[0]?.summary).toContain('waiting on external');
  });

  it('never escalates hold:external — the next actor is outside this bd instance', () => {
    const rows = select({
      beads: [heldByLabel('hold:external', { updated_at: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'watch' }));
  });

  it('still escalates hold:mayor past two hours — the mayor is here to answer', () => {
    const rows = select({
      beads: [heldByLabel('hold:mayor', { updated_at: '2026-06-07T09:00:00.000Z' })],
    });
    expect(rows[0]).toEqual(expect.objectContaining({ severity: 'attention' }));
  });

  it('surfaces a waiting hold without treating a parked worker as lost', () => {
    const rows = select({ beads: [held()] });
    expect(rows.map((row) => row.reason)).toEqual(['waiting-human']);
  });

  it('excludes a closed waiting-human bead', () => {
    const rows = select({ beads: [held({ status: 'closed' })] });
    expect(rows).toEqual([]);
  });

  it('emits one waiting row when a held bead is also assigned to human', () => {
    const rows = select({ beads: [held({ assignee: 'human' })] });
    expect(rows.map((row) => row.reason)).toEqual(['waiting-human']);
  });

  it('surfaces an open, unassigned gate bead as waiting-human', () => {
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
