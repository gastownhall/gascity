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

  // A closed session decodes to the empty state ("closed beads have no runtime
  // state", internal/session/info_codec.go), which must not render as the
  // dangling "stalled 3h — session ".
  it('names an ended session rather than emitting a dangling empty state', () => {
    const rows = select({
      beads: [inProgress()],
      sessions: [session({ state: '' })],
    });
    expect(rows[0]).toEqual(
      expect.objectContaining({
        reason: 'stalled',
        summary: expect.stringContaining('session ended'),
      }),
    );
    expect(rows[0]?.summary).not.toMatch(/session\s*$/);
  });

  // `state` is free-form with no enum upstream, and the sibling reader
  // (shared/src/agents/needsYou.ts) matches it case-insensitively — a differently
  // cased spelling must not paint a healthy worker stalled.
  it('treats a live state as live regardless of case', () => {
    const rows = select({ beads: [inProgress()], sessions: [session({ state: 'Active' })] });
    expect(rows).toEqual([]);
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

describe('non-executable workflow topology beads are exempt from the claim rules (#7232)', () => {
  // A graph.v2 workflow root with real child steps is a controller-owned latch:
  // instantiation stamps it gc.workflow_expanded=true, and it sits in_progress
  // (sling) or open (order dispatch) with no assignee while its steps run.
  const EXPANDED_ROOT_METADATA: Readonly<Record<string, string>> = {
    'gc.kind': 'workflow',
    'gc.formula_contract': 'graph.v2',
    'gc.workflow_expanded': 'true',
  };
  // A root-only graph.v2 root never gets the stamp: the root itself is the
  // unit of work an agent claims, so a dead worker on it is a real stall.
  const ROOT_ONLY_METADATA: Readonly<Record<string, string>> = {
    'gc.kind': 'workflow',
    'gc.formula_contract': 'graph.v2',
  };
  const SIX_DAYS_AGO = '2026-06-01T11:00:00.000Z';

  function workflowRoot(overrides: Partial<Bead> = {}): Bead {
    return bead({
      id: 'R-wf',
      status: 'in_progress',
      updated_at: '2026-06-07T10:00:00.000Z',
      metadata: { ...EXPANDED_ROOT_METADATA },
      ...overrides,
    });
  }

  it('does not flag a running workflow root with or without a sessions list (AC-1)', () => {
    expect(select({ beads: [workflowRoot()], sessions: [session({})] })).toEqual([]);
    expect(select({ beads: [workflowRoot()] })).toEqual([]);
  });

  it('gives a running workflow root no board card note (AC-2)', () => {
    expect(inProgressCardNote(workflowRoot(), [session({})], NOW)).toBeNull();
  });

  // Each shape names the stall detail an ordinary in-progress bead gets.
  const assigneeShapes: ReadonlyArray<{
    name: string;
    assignee?: string;
    sessions: readonly BeadAttentionSession[];
    detail: string;
  }> = [
    { name: 'no assignee', sessions: [session({})], detail: 'no assignee' },
    {
      name: 'assignee on a drained session',
      assignee: 'worker-ci-1',
      sessions: [session({ state: 'drained' })],
      detail: 'session drained',
    },
    {
      name: 'assignee with no session',
      assignee: 'worker-ci-1',
      sessions: [],
      detail: 'no live session for worker-ci-1',
    },
  ];
  // Scope and spec beads are exempt whatever they carry; a workflow root only
  // once it is expanded.
  const nonExecutableTopology: ReadonlyArray<{
    topology: string;
    metadata: Readonly<Record<string, string>>;
  }> = [
    { topology: 'expanded gc.kind=workflow root', metadata: EXPANDED_ROOT_METADATA },
    { topology: 'gc.kind=scope bead', metadata: { 'gc.kind': 'scope' } },
    { topology: 'gc.kind=spec bead', metadata: { 'gc.kind': 'spec' } },
  ];
  const topologyCases = nonExecutableTopology.flatMap((topology) =>
    assigneeShapes.map((shape) => ({ ...topology, ...shape })),
  );

  it.each(topologyCases)(
    'does not flag an in-progress $topology with $name (AC-3)',
    ({ metadata, assignee, sessions }) => {
      const topology = workflowRoot({
        ...(assignee === undefined ? {} : { assignee }),
        metadata: { ...metadata },
      });
      expect(select({ beads: [topology], sessions })).toEqual([]);
      expect(inProgressCardNote(topology, sessions, NOW) ?? '').not.toContain('stalled');
    },
  );

  it.each(assigneeShapes)(
    'still flags an in-progress never-expanded workflow root with $name as stalled',
    ({ assignee, sessions, detail }) => {
      const root = workflowRoot({
        ...(assignee === undefined ? {} : { assignee }),
        metadata: { ...ROOT_ONLY_METADATA },
      });
      expect(select({ beads: [root], sessions })).toEqual([
        expect.objectContaining({
          beadId: 'R-wf',
          reason: 'stalled',
          severity: 'attention',
          summary: expect.stringContaining(detail),
        }),
      ]);
      expect(inProgressCardNote(root, sessions, NOW)).toBe(`stalled 2h — ${detail}`);
    },
  );

  it.each(['check', 'workflow-finalize', 'wisp', undefined])(
    'still flags an unassigned in-progress bead with gc.kind=%s as stalled (AC-4)',
    (kind) => {
      const rows = select({
        beads: [
          bead({
            id: 'B-ctl',
            status: 'in_progress',
            updated_at: '2026-06-07T10:00:00.000Z',
            ...(kind === undefined ? {} : { metadata: { 'gc.kind': kind } }),
          }),
        ],
        sessions: [session({})],
      });
      expect(rows).toEqual([
        expect.objectContaining({
          beadId: 'B-ctl',
          reason: 'stalled',
          severity: 'attention',
          summary: expect.stringContaining('no assignee'),
        }),
      ]);
    },
  );

  it('surfaces a stuck workflow through its step, not its root (AC-5)', () => {
    const step = bead({
      id: 'S-impl',
      status: 'in_progress',
      assignee: 'worker-ci-1',
      metadata: { 'gc.root_bead_id': 'R-wf' },
    });
    const rows = select({ beads: [workflowRoot(), step], sessions: [session({ state: 'dead' })] });
    expect(rows.map((row) => `${row.beadId}:${row.reason}`)).toEqual(['S-impl:stalled']);
  });

  it('matches kind and expansion stamp trimmed and case-sensitive, like the Go claim gate', () => {
    const padded = workflowRoot({
      metadata: { 'gc.kind': ' workflow ', 'gc.workflow_expanded': ' true ' },
    });
    expect(select({ beads: [padded], sessions: [session({})] })).toEqual([]);
    // The claim gate compares the kind exactly, so a `Workflow` bead is
    // claimable work there, and a stall on it must still surface.
    const mixedCase = workflowRoot({
      metadata: { 'gc.kind': 'Workflow', 'gc.workflow_expanded': 'true' },
    });
    expect(select({ beads: [mixedCase], sessions: [session({})] })).toEqual([
      expect.objectContaining({ beadId: 'R-wf', reason: 'stalled' }),
    ]);
  });

  it('still reports a held workflow root as waiting-human', () => {
    const rows = select({
      beads: [workflowRoot({ labels: ['hold:mayor'] })],
      sessions: [session({})],
    });
    expect(rows.map((row) => `${row.beadId}:${row.reason}`)).toEqual(['R-wf:waiting-human']);
  });

  it('does not flag an open expanded workflow root or scope as ready-unclaimed', () => {
    // Order dispatch leaves a graph.v2 root open for its whole run, and no
    // worker ever claims it: its open steps carry the unclaimed signal.
    const rows = select({
      beads: [
        workflowRoot({ status: 'open', created_at: SIX_DAYS_AGO }),
        bead({
          id: 'S-scope',
          status: 'open',
          created_at: SIX_DAYS_AGO,
          metadata: { 'gc.kind': 'scope' },
        }),
      ],
    });
    expect(rows).toEqual([]);
  });

  it('still flags an open never-expanded workflow root as ready-unclaimed', () => {
    const rows = select({
      beads: [
        workflowRoot({
          status: 'open',
          created_at: SIX_DAYS_AGO,
          metadata: { ...ROOT_ONLY_METADATA },
        }),
      ],
    });
    expect(rows).toEqual([
      expect.objectContaining({
        beadId: 'R-wf',
        reason: 'ready-unclaimed',
        severity: 'attention',
      }),
    ]);
  });
});
