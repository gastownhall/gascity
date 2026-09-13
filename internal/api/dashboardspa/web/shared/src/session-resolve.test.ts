// Run with: npx tsx --test shared/src/session-resolve.test.ts
//
// gascity-dashboard-3ax: the single role/assignee → session resolution shared
// by frontend Maintainer sling recording and run/bead session joins.

import { test, describe } from 'node:test';
import assert from 'node:assert/strict';
import { resolveSessionForTarget, lastSegment } from './session-resolve.js';
import type { DashboardSession } from './dashboard-sessions.js';

function sess(partial: Partial<DashboardSession> & { id: string }): DashboardSession {
  return {
    template: 't',
    state: 'active',
    created_at: '2026-05-24T00:00:00Z',
    attached: false,
    ...partial,
  } as DashboardSession;
}

describe('resolveSessionForTarget', () => {
  test('null on empty target or empty list', () => {
    assert.equal(resolveSessionForTarget('', [sess({ id: 'a', alias: 'x' })]), null);
    assert.equal(resolveSessionForTarget('x', []), null);
  });

  test('resolves by exact alias, returning the session object', () => {
    const s = sess({ id: 'gc-1', alias: 'chief-of-staff', last_active: '2026-05-24T01:00:00Z' });
    const got = resolveSessionForTarget('chief-of-staff', [sess({ id: 'gc-0' }), s]);
    assert.equal(got?.id, 'gc-1');
    assert.equal(got?.last_active, '2026-05-24T01:00:00Z');
  });

  test('resolves by pool', () => {
    const s = sess({ id: 'gc-2', pool: 'mayor' });
    assert.equal(resolveSessionForTarget('mayor', [s])?.id, 'gc-2');
  });

  test('resolves by last segment of alias (split on . and /)', () => {
    const s = sess({ id: 'gc-3', alias: 'oversight-rig.chief-of-staff' });
    assert.equal(resolveSessionForTarget('chief-of-staff', [s])?.id, 'gc-3');
  });

  test('resolves by last segment of session_name (split on __ before -)', () => {
    const s = sess({ id: 'gc-4', session_name: 'oversight-rig__chief-of-staff' });
    assert.equal(resolveSessionForTarget('chief-of-staff', [s])?.id, 'gc-4');
  });

  test('active sessions outrank non-active on a tie', () => {
    const asleep = sess({ id: 'old', pool: 'mayor', state: 'asleep' });
    const live = sess({ id: 'live', pool: 'mayor', state: 'active' });
    // order puts asleep first; active-first pass must still pick the live one.
    assert.equal(resolveSessionForTarget('mayor', [asleep, live])?.id, 'live');
  });

  test('unresolvable role (role-pool dispatch miss) returns null — the R2 lossy case', () => {
    const s = sess({ id: 'gc-5', alias: 'some-rig.worker', pool: 'worker-pool' });
    assert.equal(resolveSessionForTarget('chief-of-staff', [s]), null);
  });
});

describe('lastSegment', () => {
  test('returns value unchanged when no separator present', () => {
    assert.equal(lastSegment('chief-of-staff', ['/', '.']), 'chief-of-staff');
  });
  test('splits on the rightmost separator across the set', () => {
    assert.equal(lastSegment('a/b.c', ['/', '.']), 'c');
    assert.equal(lastSegment('rig__role', ['__', '--']), 'role');
  });
});
