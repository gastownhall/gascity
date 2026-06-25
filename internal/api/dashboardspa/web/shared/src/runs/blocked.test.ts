import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { blockedRunReason, blockedRunRemedy, selectBlockedRuns } from './blocked.js';
import type { RunLane } from '../snapshot/types.js';

// gascity-dashboard-2j8e.2: the single genuinely-blocked-runs selector. Both
// the Runs nav badge and the /runs page read it, so the badge count and the
// page count cannot disagree. Input is buildRunSummary's blockedLanes, which is
// already dangling-root + non-graph.v2 suppressed upstream — the gc-1920-class
// phantom never reaches here.

function lane(overrides: Partial<RunLane> = {}): RunLane {
  return {
    id: 'gc-100',
    title: 'mol-focus-review',
    formula: { status: 'known', name: 'mol-focus-review' },
    scope: { status: 'available', kind: 'rig', ref: 'app', rootStoreRef: 'rig:app' },
    external: { status: 'unavailable', error: 'external reference unavailable' },
    phase: 'blocked',
    phaseLabel: 'blocked',
    statusCounts: { blocked: 1, open: 2 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-06-01T00:00:00.000Z' },
    stages: [],
    progress: { status: 'unavailable', error: 'run progress unavailable' },
    formulaStageResolved: false,
    health: { status: 'unavailable', error: 'run health has not been derived' },
    ...overrides,
  };
}

describe('selectBlockedRuns — gascity-dashboard-2j8e.2', () => {
  test('projects one BlockedRun per blocked lane, preserving id/title/scope', () => {
    const scope = {
      status: 'available',
      kind: 'rig',
      ref: 'app',
      rootStoreRef: 'rig:app',
    } as const;
    const runs = selectBlockedRuns([
      lane({ id: 'gc-1', title: 'Run one', scope }),
      lane({ id: 'gc-2', title: 'Run two', scope }),
    ]);
    assert.equal(runs.length, 2);
    assert.deepEqual(
      runs.map((r) => r.id),
      ['gc-1', 'gc-2'],
    );
    assert.equal(runs[0]?.title, 'Run one');
    assert.deepEqual(runs[0]?.scope, scope);
  });

  test('selects only blocked lanes — active and complete are excluded', () => {
    const runs = selectBlockedRuns([
      lane({ id: 'active', phase: 'implementation' }),
      lane({ id: 'done', phase: 'complete' }),
      lane({ id: 'blocked', phase: 'blocked' }),
    ]);
    assert.deepEqual(
      runs.map((r) => r.id),
      ['blocked'],
    );
  });

  test('empty input yields no blocked runs', () => {
    assert.deepEqual(selectBlockedRuns([]), []);
  });
});

describe('blockedRunReason — why-blocked', () => {
  test('names the blocked stage when the active stage resolved', () => {
    const reason = blockedRunReason(
      lane({
        progress: {
          status: 'active_step',
          stepId: 'review.adopt',
          stage: { status: 'available', index: 2, key: 'review', label: 'Review' },
          attempt: { status: 'unavailable', error: 'run step attempt unavailable' },
        },
      }),
    );
    assert.equal(reason, 'Blocked at Review');
  });

  test('falls back to the blocked-step count when no stage resolved', () => {
    assert.equal(blockedRunReason(lane({ statusCounts: { blocked: 2 } })), '2 blocked steps');
    assert.equal(blockedRunReason(lane({ statusCounts: { blocked: 1 } })), '1 blocked step');
  });

  test('uses a generic phrase when neither stage nor a blocked step is known', () => {
    assert.equal(
      blockedRunReason(lane({ statusCounts: { open: 1 } })),
      'Blocked, awaiting operator',
    );
  });
});

describe('blockedRunRemedy — how-to-unblock', () => {
  test('tells the operator to dispatch when no worker is assigned', () => {
    assert.equal(
      blockedRunRemedy(lane({ activeAssignees: [] })),
      'No worker assigned. Claim or dispatch one.',
    );
  });

  test('points to the run detail when a worker is assigned', () => {
    assert.equal(
      blockedRunRemedy(lane({ activeAssignees: ['claude-1'] })),
      'Open run detail to review the blocked step.',
    );
  });
});
