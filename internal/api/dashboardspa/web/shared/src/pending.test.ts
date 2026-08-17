// Run with: npx tsx --test shared/src/pending.test.ts
//
// PendingInteraction boundary parse + AlertItem mapping (gascity-dashboard-8167,
// PRD R3). The supervisor emits this only on the per-session SSE; the parse is
// the edge that keeps untyped frames from flowing in as a known shape.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  parsePendingInteraction,
  pendingInteractionToAlert,
  type PendingAlertContext,
} from './pending.js';
import { parsePendingInteraction as barrelParse } from './index.js';

const CTX: PendingAlertContext = {
  sessionId: 's1',
  occurredAt: '2026-06-02T12:00:00.000Z',
  version: 7,
  provenance: 'fresh',
};

test('parses a valid pending frame (required request_id + kind)', () => {
  const pi = parsePendingInteraction({ request_id: 'req-1', kind: 'tool_approval' });
  assert.notEqual(pi, null);
  assert.equal(pi!.request_id, 'req-1');
  assert.equal(pi!.kind, 'tool_approval');
});

test('carries optional prompt/options/metadata when present and well-typed', () => {
  const pi = parsePendingInteraction({
    request_id: 'req-1',
    kind: 'prompt_for_input',
    prompt: 'Approve push?',
    options: ['allow', 'deny'],
    metadata: { tool: 'git' },
  });
  assert.deepEqual(pi!.options, ['allow', 'deny']);
  assert.equal(pi!.prompt, 'Approve push?');
  assert.deepEqual(pi!.metadata, { tool: 'git' });
});

test('rejects frames missing request_id or kind (boundary validation)', () => {
  assert.equal(parsePendingInteraction({ kind: 'x' }), null);
  assert.equal(parsePendingInteraction({ request_id: 'r' }), null);
  assert.equal(parsePendingInteraction({ request_id: '', kind: 'x' }), null);
  assert.equal(parsePendingInteraction('not an object'), null);
  assert.equal(parsePendingInteraction(null), null);
});

test('drops malformed options rather than passing a wrong-typed array through', () => {
  const pi = parsePendingInteraction({ request_id: 'r', kind: 'k', options: ['ok', 3] });
  assert.equal(pi!.options, undefined);
});

test('maps to a pending-decision AlertItem with requestId-keyed dedup', () => {
  const pi = parsePendingInteraction({
    request_id: 'req-9',
    kind: 'tool_approval',
    prompt: 'Approve push?\nmore',
  })!;
  const alert = pendingInteractionToAlert(pi, CTX);
  assert.equal(alert.kind, 'pending-decision');
  assert.equal(alert.source, 'pending');
  assert.equal(alert.dedupKey, 'pending-decision:req-9');
  assert.equal(alert.ref.requestId, 'req-9');
  assert.equal(alert.ref.sessionId, 's1');
  assert.equal(alert.href, '/agents/s1');
  assert.equal(alert.title, 'Approve push?'); // first line only
  assert.equal(alert.reason, 'tool_approval'); // kind carried verbatim
  assert.equal(alert.version, 7);
  assert.equal(alert.provenance, 'fresh');
});

test('falls back to a generic title when the prompt is absent', () => {
  const pi = parsePendingInteraction({ request_id: 'r', kind: 'k' })!;
  assert.equal(pendingInteractionToAlert(pi, CTX).title, 'agent awaiting your decision');
});

test('barrel re-exports the pending module', () => {
  assert.equal(barrelParse, parsePendingInteraction);
});
