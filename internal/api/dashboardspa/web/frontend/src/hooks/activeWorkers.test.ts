import { describe, expect, it } from 'vitest';
import type { SupervisorBead } from '../supervisor/beadReads';
import type { SupervisorSession } from '../supervisor/sessionReads';
import { deriveActiveWorkers, summarizeActiveWorkers } from './activeWorkers';

// "Workers active" is SESSION-driven: count the live worker sessions, group by
// rig, and best-effort attach an in-progress bead when one is captured. The
// orchestration sessions (mayor, dispatchers, project-leads) are excluded.

function session(partial: Partial<SupervisorSession> & { id: string }): SupervisorSession {
  return {
    template: 'polecat',
    session_name: partial.id,
    title: partial.id,
    state: 'active',
    created_at: '2026-06-03T00:00:00Z',
    attached: false,
    running: true,
    provider: 'claude',
    ...partial,
  } as SupervisorSession;
}

function bead(partial: Partial<SupervisorBead> & { id: string }): SupervisorBead {
  return {
    title: `title for ${partial.id}`,
    status: 'in_progress',
    issue_type: 'task',
    created_at: '2026-06-03T00:00:00Z',
    ...partial,
  } as SupervisorBead;
}

describe('deriveActiveWorkers', () => {
  it('counts active worker sessions grouped by rig, most workers first', () => {
    const sessions = [
      session({ id: 'gc-1', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-2', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-3', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-4', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-5', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-6', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-7', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-8', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-9', template: 'worker', rig: 'zeldascension' }),
      // Orchestration — excluded.
      session({ id: 'gc-m', template: 'mayor', rig: '' }),
      session({ id: 'gc-pl', template: 'gascity.project-lead', rig: 'gascity' }),
    ];
    const result = deriveActiveWorkers(sessions, []);
    expect(result.total).toBe(9);
    expect(result.byRig).toEqual([
      { rig: 'gascity', count: 3 },
      { rig: 'scix_experiments', count: 3 },
      { rig: 'gascity-packs', count: 2 },
      { rig: 'zeldascension', count: 1 },
    ]);
  });

  it('excludes suspended / non-active sessions', () => {
    const sessions = [
      session({ id: 'gc-1', template: 'polecat', rig: 'gascity' }),
      session({ id: 'gc-2', template: 'polecat', rig: 'gascity', state: 'asleep' }),
      session({ id: 'gc-3', template: 'polecat', rig: 'gascity', state: 'closed' }),
    ];
    const result = deriveActiveWorkers(sessions, []);
    expect(result.total).toBe(1);
    expect(result.workers[0]?.session.id).toBe('gc-1');
  });

  it('attaches an in-progress bead when its assignee embeds the worker session id', () => {
    const sessions = [session({ id: 'gc-335825', template: 'polecat', rig: '/home/ds/gascity' })];
    const beads = [bead({ id: 'gc-5rarj', title: 'fix the thing', assignee: 'polecat-gc-335825' })];
    const result = deriveActiveWorkers(sessions, beads);
    expect(result.workers[0]?.bead?.id).toBe('gc-5rarj');
    expect(result.workers[0]?.rig).toBe('gascity');
    expect(result.workers[0]?.worker).toBe('polecat');
  });

  it('leaves bead undefined when no in-progress bead matches the worker (the common case)', () => {
    const sessions = [session({ id: 'gc-1', template: 'polecat', rig: 'gascity' })];
    // A closed bead must NOT attach, and an unassigned in-progress bead must not
    // surface anywhere — the worker being active is the signal.
    const beads = [
      bead({ id: 'gc-closed', status: 'closed', assignee: 'polecat-gc-1' }),
      bead({ id: 'gc-stalled' }), // in_progress, unassigned
    ];
    const result = deriveActiveWorkers(sessions, beads);
    expect(result.total).toBe(1);
    expect(result.workers[0]?.bead).toBeUndefined();
  });

  it('orders worker rows by most-recent activity first', () => {
    const sessions = [
      session({ id: 'gc-1', rig: 'gascity', last_active: '2026-06-03T10:00:00Z' }),
      session({ id: 'gc-2', rig: 'gascity', last_active: '2026-06-03T11:00:00Z' }),
    ];
    const result = deriveActiveWorkers(sessions, []);
    expect(result.workers.map((w) => w.session.id)).toEqual(['gc-2', 'gc-1']);
  });
});

describe('summarizeActiveWorkers', () => {
  it('produces the calm grouped summary line', () => {
    const sessions = [
      session({ id: 'gc-1', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-2', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-3', template: 'polecat', rig: '/home/ds/gascity' }),
      session({ id: 'gc-4', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-5', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-6', template: 'scix-worker', rig: 'scix_experiments' }),
      session({ id: 'gc-7', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-8', template: 'polecat', rig: '/home/ds/gascity-packs-main' }),
      session({ id: 'gc-9', template: 'worker', rig: 'zeldascension' }),
    ];
    const result = deriveActiveWorkers(sessions, []);
    expect(summarizeActiveWorkers(result)).toBe(
      '9 workers active across gascity (3), scix_experiments (3), gascity-packs (2), zeldascension (1).',
    );
  });

  it('uses the singular noun for one worker', () => {
    const result = deriveActiveWorkers(
      [session({ id: 'gc-1', template: 'polecat', rig: 'gascity' })],
      [],
    );
    expect(summarizeActiveWorkers(result)).toBe('1 worker active across gascity (1).');
  });

  it('returns the calm empty-state sentence when no workers are active', () => {
    expect(summarizeActiveWorkers(deriveActiveWorkers([], []))).toBe(
      'No workers active right now.',
    );
  });
});
