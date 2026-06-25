import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import { deriveRunHealth } from './health.js';
import type { RunLane } from '../snapshot/types.js';

// gascity-dashboard (0gww): the run-lane health-unavailable attention emitter
// guards on `lane.health.status === 'available'`. deriveRunHealth used to wrap
// EVERY lane in status:'available' — even when the session list was unavailable
// and health could not actually be derived — so the guard always continued and
// the emitter was structurally dead. These tests pin the contract that drives
// the emitter: no session list ⇒ health.status === 'unavailable'.

function lane(overrides: Partial<RunLane> = {}): RunLane {
  return {
    id: 'run-1',
    title: 'a run',
    formula: { status: 'known', name: 'mol-test' },
    scope: { status: 'available', kind: 'rig', ref: 'app', rootStoreRef: 'rig:app' },
    external: { status: 'unavailable', error: 'external reference unavailable' },
    phase: 'implementation',
    phaseLabel: 'implementation',
    statusCounts: { in_progress: 1 },
    activeAssignees: ['app/codex'],
    updatedAt: { status: 'available', at: '2026-06-08T00:00:00.000Z' },
    stages: [],
    progress: { status: 'unavailable', error: 'run progress unavailable' },
    formulaStageResolved: false,
    health: { status: 'unavailable', error: 'run health has not been derived' },
    ...overrides,
  };
}

describe('deriveRunHealth — session-list unavailability (0gww)', () => {
  test('reports health.status unavailable for every lane when the session list is unavailable', () => {
    const { lanes } = deriveRunHealth({
      lanes: [lane({ id: 'run-a' }), lane({ id: 'run-b' })],
      sessions: [],
      sessionsAvailable: false,
      marks: new Map(),
    });

    for (const enriched of lanes) {
      assert.equal(enriched.health.status, 'unavailable');
      if (enriched.health.status !== 'unavailable') continue;
      assert.equal(enriched.health.error, 'run session list unavailable');
    }
  });

  test('still derives available health when the session list is available', () => {
    const { lanes } = deriveRunHealth({
      lanes: [lane()],
      sessions: [],
      sessionsAvailable: true,
      marks: new Map(),
    });

    assert.equal(lanes[0]?.health.status, 'available');
  });
});
