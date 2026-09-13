import { test, describe } from 'node:test';
import assert from 'node:assert/strict';

import {
  selectAgentsNeedingYou,
  type AgentNeedsYouReason,
  type AgentPendingSignal,
} from './needsYou.js';
import type { AgentResponse } from '../generated/gc-supervisor-client/index.js';

// gascity-dashboard-2j8e.4: the single agents "needs you" selector. Both the
// Agents nav badge and the /agents page read it, so the badge count and the
// page count cannot disagree. Actively-running, idle, asleep, and suspended
// agents are ambient roster state, never counted.

function agent(overrides: Partial<AgentResponse>): AgentResponse {
  return {
    available: true,
    name: 'agent',
    running: false,
    state: 'active',
    suspended: false,
    ...overrides,
  };
}

const liveSession = { attached: true, last_activity: '2026-06-01T11:59:00.000Z', name: 'agent' };

describe('selectAgentsNeedingYou', () => {
  test('does NOT count actively-running, idle, asleep, or suspended agents', () => {
    const result = selectAgentsNeedingYou(
      [
        agent({ name: 'running', running: true, state: 'active', session: liveSession }),
        agent({ name: 'idle', state: 'idle', session: liveSession }),
        agent({ name: 'asleep', running: true, state: 'asleep', session: liveSession }),
        agent({ name: 'suspended', suspended: true, state: 'active', session: liveSession }),
        agent({ name: 'unavailable', available: false, state: 'idle' }),
      ],
      [],
    );
    assert.deepEqual(result, []);
  });

  test('counts each needs-you reason: awaiting-input, errored, rate-limited, stalled', () => {
    const pending: AgentPendingSignal[] = [{ agentName: 'mayor', prompt: 'Approve deployment?' }];
    const result = selectAgentsNeedingYou(
      [
        agent({ name: 'mayor', running: true, state: 'active', session: liveSession }),
        agent({ name: 'crashed', state: 'failed' }),
        agent({ name: 'throttled', running: true, state: 'rate-limited', session: liveSession }),
        agent({ name: 'ghost', running: true, state: 'active' }),
      ],
      pending,
    );

    const byName = new Map<string, AgentNeedsYouReason>(result.map((r) => [r.name, r.reason]));
    assert.equal(byName.get('mayor'), 'awaiting-input');
    assert.equal(byName.get('crashed'), 'errored');
    assert.equal(byName.get('throttled'), 'rate-limited');
    assert.equal(byName.get('ghost'), 'stalled');
    assert.equal(result.length, 4);
  });

  test('treats a detached agent as stalled', () => {
    const result = selectAgentsNeedingYou([agent({ name: 'lost', state: 'detached' })], []);
    assert.deepEqual(result, [
      { name: 'lost', reason: 'stalled', detail: 'Detached from its session.', action: 'nudge' },
    ]);
  });

  test('ranks an awaiting-input ask above a coincident failure state', () => {
    const result = selectAgentsNeedingYou(
      [agent({ name: 'mayor', state: 'stuck' })],
      [{ agentName: 'mayor', prompt: 'Approve?' }],
    );
    assert.deepEqual(result, [
      { name: 'mayor', reason: 'awaiting-input', detail: 'Approve?', action: 'respond' },
    ]);
  });

  test('carries the action verb per reason and a structural detail phrase', () => {
    const result = selectAgentsNeedingYou(
      [
        agent({ name: 'crashed', state: 'errored' }),
        agent({ name: 'throttled', running: true, state: 'waiting', session: liveSession }),
        agent({ name: 'ghost', running: true, state: 'running' }),
      ],
      [],
    );
    assert.deepEqual(result, [
      { name: 'crashed', reason: 'errored', detail: 'Exited errored.', action: 'reset' },
      {
        name: 'throttled',
        reason: 'rate-limited',
        detail: 'Throttled by a provider limit.',
        action: 'nudge',
      },
      {
        name: 'ghost',
        reason: 'stalled',
        detail: 'Running with no live session.',
        action: 'nudge',
      },
    ]);
  });

  test('falls back to a generic awaiting phrase when the prompt is absent', () => {
    const result = selectAgentsNeedingYou(
      [agent({ name: 'mayor', state: 'active' })],
      [{ agentName: 'mayor' }],
    );
    assert.equal(result[0]?.detail, 'Awaiting your decision.');
  });
});
