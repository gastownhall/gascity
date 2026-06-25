import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { isDanglingRootGroup, isStaleSessionlessLatch, STALE_LATCH_AFTER_MS } from './liveness.js';
import type { RunIssue } from './phaseMapping.js';
import type { RunLane, RunLaneHealth } from '../snapshot/types.js';

// gascity-dashboard-s4rp: the sharp session-less demotion predicate. Operator
// repro gc-1920 — an ancient approval-gate latch with no live session, no
// in_progress step, ~4d stale — inflated Active:1 and flickered in/out. It must
// be demoted, while a freshly-queued run and an approval gate genuinely waiting
// on a human (both legitimately session-less) must NOT be.

const NOW_MS = Date.parse('2026-06-07T00:00:00.000Z');

function health(sessionStatus: 'resolved' | 'unresolved'): RunLane['health'] {
  const session: RunLaneHealth['session'] =
    sessionStatus === 'resolved'
      ? {
          status: 'resolved',
          lastActive: { status: 'available', at: new Date(NOW_MS).toISOString() },
          running: { status: 'available', value: true },
          activity: { status: 'available', value: 'working' },
        }
      : { status: 'unresolved', error: 'run session unresolved' };

  return {
    status: 'available',
    data: {
      phaseConfidence: 'inferred',
      needsOperator: false,
      stuckNode: { status: 'unavailable', error: 'active run step unavailable' },
      thrashingDetected: false,
      session,
    },
  };
}

function lane(overrides: Partial<RunLane> = {}): RunLane {
  return {
    id: 'gc-1920',
    title: 'mol-focus-review',
    formula: { status: 'known', name: 'mol-focus-review' },
    scope: { status: 'unavailable', error: 'run scope metadata unavailable' },
    external: { status: 'unavailable', error: 'external reference unavailable' },
    phase: 'approval',
    phaseLabel: 'approval',
    statusCounts: { open: 1 },
    activeAssignees: [],
    updatedAt: {
      status: 'available',
      at: new Date(NOW_MS - 4 * 24 * 60 * 60 * 1000).toISOString(),
    },
    stages: [],
    progress: { status: 'unavailable', error: 'run progress unavailable' },
    formulaStageResolved: false,
    health: health('unresolved'),
    ...overrides,
  };
}

describe('isStaleSessionlessLatch — gascity-dashboard-s4rp', () => {
  test('demotes a session-less, step-less, stale approval latch (gc-1920)', () => {
    assert.equal(isStaleSessionlessLatch(lane(), NOW_MS, true), true);
  });

  test('keeps a freshly-queued session-less run (recent updatedAt)', () => {
    const queued = lane({
      phase: 'intake',
      updatedAt: { status: 'available', at: new Date(NOW_MS - 60_000).toISOString() },
    });
    assert.equal(isStaleSessionlessLatch(queued, NOW_MS, true), false);
  });

  test('keeps an approval gate waiting on a human while still recent', () => {
    const waiting = lane({
      updatedAt: { status: 'available', at: new Date(NOW_MS - 30 * 60_000).toISOString() },
    });
    assert.equal(isStaleSessionlessLatch(waiting, NOW_MS, true), false);
  });

  test('keeps a stale run that still has a resolved live session', () => {
    assert.equal(
      isStaleSessionlessLatch(lane({ health: health('resolved') }), NOW_MS, true),
      false,
    );
  });

  test('keeps a stale run that still has an in_progress primary step', () => {
    const active = lane({
      progress: {
        status: 'active_step',
        stepId: 'implementation.patch',
        stage: { status: 'unavailable', error: 'active run stage unavailable' },
        attempt: { status: 'unavailable', error: 'run step attempt unavailable' },
      },
    });
    assert.equal(isStaleSessionlessLatch(active, NOW_MS, true), false);
  });

  test('does not demote when the session list is unavailable', () => {
    assert.equal(isStaleSessionlessLatch(lane(), NOW_MS, false), false);
  });

  test('never demotes complete or blocked lanes (already partitioned upstream)', () => {
    assert.equal(isStaleSessionlessLatch(lane({ phase: 'complete' }), NOW_MS, true), false);
    assert.equal(isStaleSessionlessLatch(lane({ phase: 'blocked' }), NOW_MS, true), false);
  });

  test('does not demote without a known age', () => {
    const noAge = lane({
      updatedAt: { status: 'unavailable', error: 'run update time unavailable' },
    });
    assert.equal(isStaleSessionlessLatch(noAge, NOW_MS, true), false);
  });

  test('the staleness boundary is exclusive below the floor', () => {
    const justUnder = lane({
      updatedAt: {
        status: 'available',
        at: new Date(NOW_MS - (STALE_LATCH_AFTER_MS - 1_000)).toISOString(),
      },
    });
    assert.equal(isStaleSessionlessLatch(justUnder, NOW_MS, true), false);
    const atFloor = lane({
      updatedAt: { status: 'available', at: new Date(NOW_MS - STALE_LATCH_AFTER_MS).toISOString() },
    });
    assert.equal(isStaleSessionlessLatch(atFloor, NOW_MS, true), true);
  });
});

describe('isDanglingRootGroup — gascity-dashboard-s4rp', () => {
  function issue(id: string): RunIssue {
    return {
      id,
      title: id,
      status: 'open',
      issue_type: 'task',
      updated_at: '2026-06-01T00:00:00Z',
    };
  }

  test('flags a group whose root bead is absent from its issues', () => {
    assert.equal(isDanglingRootGroup('gc-1920', [issue('gc-1920-step-1')]), true);
  });

  test('does not flag a group whose root bead is present', () => {
    assert.equal(
      isDanglingRootGroup('gc-1920', [issue('gc-1920'), issue('gc-1920-step-1')]),
      false,
    );
  });
});
