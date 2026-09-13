// Run with: npx tsx --test shared/src/alert.test.ts
//
// Contract tests for the AlertItem DTO (gascity-dashboard-4s07, PRD R1).
// The home-view alert system unifies typed actionable signals into one
// ranked attention queue; this DTO is the single source of truth both
// backend (snapshot read path) and frontend (SSE pending layer) import,
// so a contract mismatch is a compile error, not a runtime undefined.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  ALERT_KINDS,
  ALERT_SOURCES,
  ALERT_SEVERITIES,
  ALERT_SEVERITY_RANK,
  makeAlertDedupKey,
} from './alert.js';
import type { AlertItem, AlertRef } from './alert.js';
import {
  ALERT_KINDS as barrelAlertKinds,
  makeAlertDedupKey as barrelMakeAlertDedupKey,
} from './index.js';

test('ALERT_KINDS is the closed union the PRD defines (R1)', () => {
  assert.deepEqual([...ALERT_KINDS].sort(), [
    'operator-mail',
    'pending-decision',
    'run-needs-operator',
    'run-thrashing',
  ]);
});

test('ALERT_SOURCES groups by data plane', () => {
  assert.deepEqual([...ALERT_SOURCES].sort(), ['mail', 'pending', 'runs']);
});

test('ALERT_SEVERITIES + rank order place failing above attention (R5/R7)', () => {
  assert.deepEqual([...ALERT_SEVERITIES].sort(), ['attention', 'failing']);
  assert.ok(ALERT_SEVERITY_RANK.failing > ALERT_SEVERITY_RANK.attention);
});

test('makeAlertDedupKey is deterministic and stable across calls', () => {
  const ref: AlertRef = { runId: 'r1' };
  assert.equal(
    makeAlertDedupKey('run-needs-operator', ref),
    makeAlertDedupKey('run-needs-operator', ref),
  );
  assert.equal(makeAlertDedupKey('run-needs-operator', ref), 'run-needs-operator:r1');
});

test('makeAlertDedupKey prefers requestId (the pending idempotency key) over other ids', () => {
  const key = makeAlertDedupKey('pending-decision', { requestId: 'req-9', sessionId: 's1' });
  assert.equal(key, 'pending-decision:req-9');
});

test('makeAlertDedupKey throws on a ref with no identifying id (fail fast, no silent empty key)', () => {
  assert.throws(() => makeAlertDedupKey('operator-mail', {}), /identif/i);
});

test('barrel (index.js) re-exports the alert module as value exports', () => {
  assert.equal(barrelMakeAlertDedupKey, makeAlertDedupKey);
  assert.equal(barrelAlertKinds, ALERT_KINDS);
});

test('AlertItem carries provenance and the R17 monotonic version for last-write-wins', () => {
  const item: AlertItem = {
    kind: 'pending-decision',
    source: 'pending',
    ref: { requestId: 'req-1', sessionId: 's1' },
    href: '/agents/s1',
    title: 'agent awaiting your decision',
    reason: 'tool approval',
    severity: 'failing',
    occurredAt: '2026-06-02T00:00:00.000Z',
    dedupKey: makeAlertDedupKey('pending-decision', { requestId: 'req-1' }),
    version: 3,
    provenance: 'fresh',
  };
  assert.equal(item.provenance, 'fresh');
  assert.equal(item.version, 3);
  // foldedCount is optional: absent means this row is not a fold parent (R8).
  assert.equal(item.foldedCount, undefined);
});

test('a fold-parent AlertItem carries a visible foldedCount (R8: no silent fold)', () => {
  const parent: AlertItem = {
    kind: 'run-thrashing',
    source: 'runs',
    ref: { runId: 'rig-a' },
    href: '/runs/rig-a',
    title: 'rig-a has a failed agent',
    reason: 'failed run',
    severity: 'failing',
    occurredAt: '2026-06-02T00:00:00.000Z',
    dedupKey: makeAlertDedupKey('run-thrashing', { runId: 'rig-a' }),
    version: 1,
    provenance: 'fresh',
    foldedCount: 3,
  };
  assert.equal(parent.foldedCount, 3);
});
