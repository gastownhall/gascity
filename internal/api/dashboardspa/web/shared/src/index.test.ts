// Run with: npx tsx --test shared/src/index.test.ts
//
// Regression tests for gascity-dashboard-wj8: gc supervisor emits
// context_pct against a hardcoded context_window of 200_000 even for
// sessions running with the [1m] extended-context beta header (true
// window 1_000_000). The dashboard must scale gc's value back to the
// true window so the displayed % matches what the CLI/tmux session
// reports.

import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import {
  CITY_NAME_RE,
  effectiveContextPct,
  errorMessage,
  GC_EVENT_PREFIX,
  makeNodeKey,
  SCOPE_REF_RE,
  TRUE_CONTEXT_WINDOWS,
} from './index.js';
import { CITY_NAME_RE as leafCityNameRe } from './city.js';
import {
  effectiveContextPct as leafEffectiveContextPct,
  TRUE_CONTEXT_WINDOWS as leafTrueContextWindows,
} from './context-window.js';
import { makeNodeKey as leafMakeNodeKey } from './links.js';
import {
  errorMessage as leafErrorMessage,
  GC_EVENT_PREFIX as leafGcEventPrefix,
} from './operator.js';
import { SCOPE_REF_RE as leafScopeRefRe } from './run-detail.js';
import type {
  Avail,
  ClientErrorReport,
  CountedList,
  PartialAwareList,
  RequiredPartialList,
  DashboardSession,
  SlingIntent,
  SlingKind,
  RunCensus,
  RunCensusState,
  RunLaneHealth,
  RunLaneHealthState,
  RunLaneScope,
  RunLaneSessionActivity,
  RunLaneSessionLastActive,
  RunLaneSessionRunning,
  RunLaneStagePosition,
  RunLaneStepAttempt,
  RunLaneStuckNode,
  RunLaneUpdatedAt,
} from './index.js';
import type {
  Avail as LeafAvail,
  CountedList as LeafCountedList,
  PartialAwareList as LeafPartialAwareList,
  RequiredPartialList as LeafRequiredPartialList,
} from './lists.js';
import './lists.js';
import './viewing-as.js';
import './dashboard-beads.js';
import './activity.js';
import './dashboard-health.js';
import './transcript.js';
import './api-error.js';

function sess(partial: Partial<DashboardSession>): DashboardSession {
  return {
    id: 'gc-test',
    template: 'mayor',
    state: 'active',
    created_at: '2026-05-19T00:00:00-04:00',
    attached: true,
    // 6bv7 F10: session_name/title/running/provider tightened to required
    // in the shared DashboardSession projection contract.
    session_name: 'gc-test',
    title: 'gc-test',
    running: true,
    provider: 'claude',
    ...partial,
  };
}

test('mayor fixture: gc reports 89% against 200k, true window is 1M, effective is ~18%', () => {
  // Real shape pulled from http://127.0.0.1:8372/v0/city/ds-research/sessions
  // for the mayor session, the case the bead was filed against.
  const mayor = sess({
    id: 'gc-2568',
    title: 'mayor',
    alias: 'mayor',
    model: 'claude-opus-4-7',
    context_pct: 89,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(mayor), 18);
});

test('bead-report fixture: gc 75% against 200k -> 15% against 1M', () => {
  // The exact ratio from the bug report.
  const s = sess({
    model: 'claude-opus-4-7',
    context_pct: 75,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 15);
});

test('claude-sonnet-4-5 also gets 1M scaling', () => {
  const s = sess({
    model: 'claude-sonnet-4-5',
    context_pct: 50,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 10);
});

test('claude-sonnet-4-6 also gets 1M scaling', () => {
  const s = sess({
    model: 'claude-sonnet-4-6',
    context_pct: 100,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 20);
});

test('unknown model falls back to gc-reported value', () => {
  const s = sess({
    model: 'some-other-model',
    context_pct: 42,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 42);
});

test('missing model falls back to gc-reported value', () => {
  const s = sess({
    context_pct: 42,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 42);
});

test('missing context_window falls back to gc-reported value', () => {
  // Can't compute a scale factor without knowing what gc divided by.
  // Preserve the reported value rather than guessing.
  const s = sess({
    model: 'claude-opus-4-7',
    context_pct: 89,
  });
  assert.equal(effectiveContextPct(s), 89);
});

test('missing context_pct returns undefined', () => {
  const s = sess({ model: 'claude-opus-4-7' });
  assert.equal(effectiveContextPct(s), undefined);
});

test('gc-reported window matches true window: no scaling', () => {
  // If gc upstream fixes itself and starts emitting context_window=1_000_000
  // for [1m] sessions, our scaling becomes a no-op.
  const s = sess({
    model: 'claude-opus-4-7',
    context_pct: 18,
    context_window: 1000000,
  });
  assert.equal(effectiveContextPct(s), 18);
});

test('result is capped at 100', () => {
  // Defensive: if gc ever reports >100 due to its own bug, we still
  // render a sane value.
  const s = sess({
    model: 'claude-opus-4-7',
    context_pct: 600,
    context_window: 200000,
  });
  assert.equal(effectiveContextPct(s), 100);
});

test('result rounds to integer', () => {
  // Display layer renders integers; the helper rounds so thresholds
  // and display agree.
  const s = sess({
    model: 'claude-opus-4-7',
    context_pct: 73,
    context_window: 200000,
  });
  // 73 * 200000 / 1000000 = 14.6 -> 15
  assert.equal(effectiveContextPct(s), 15);
});

test('TRUE_CONTEXT_WINDOWS includes the deployed Claude models', () => {
  // Smoke test on the model registry. If a new Claude generation lands,
  // adding it here should be the only change needed.
  assert.equal(TRUE_CONTEXT_WINDOWS['claude-opus-4-7'], 1_000_000);
  assert.equal(TRUE_CONTEXT_WINDOWS['claude-sonnet-4-5'], 1_000_000);
  assert.equal(TRUE_CONTEXT_WINDOWS['claude-sonnet-4-6'], 1_000_000);
});

test('runtime helpers live in domain leaves and remain re-exported by the barrel', () => {
  assert.equal(leafGcEventPrefix, GC_EVENT_PREFIX);
  assert.equal(leafErrorMessage, errorMessage);
  assert.equal(leafTrueContextWindows, TRUE_CONTEXT_WINDOWS);
  assert.equal(leafEffectiveContextPct, effectiveContextPct);
});

test('leaf-owned runtime exports stay available through the barrel', () => {
  assert.equal(leafScopeRefRe, SCOPE_REF_RE);
  assert.equal(leafCityNameRe, CITY_NAME_RE);
  assert.equal(leafMakeNodeKey, makeNodeKey);
  assert.equal(SCOPE_REF_RE.test('rig:demo-app'), true);
  assert.equal(CITY_NAME_RE.test('demo-city'), true);
  assert.equal(makeNodeKey('bead', 'b-1', 'rig:demo-app'), 'bead:rig:demo-app:b-1');
});

test('shared barrel does not expose dashboard mirror DTOs for direct supervisor surfaces', () => {
  const source = readFileSync(new URL('./index.ts', import.meta.url), 'utf8');
  const healthSource = readFileSync(new URL('./dashboard-health.ts', import.meta.url), 'utf8');

  assert.doesNotMatch(source, /gc-agents/);
  assert.doesNotMatch(source, /gc-rigs/);
  assert.doesNotMatch(source, /gc-mail/);
  assert.doesNotMatch(source, /gc-events/);
  assert.doesNotMatch(source, /formula-runs/);
  assert.doesNotMatch(healthSource, /\binterface\s+GcStatus\b/);
  assert.doesNotMatch(healthSource, /\bHealthDiagnostics\b/);
  assert.doesNotMatch(healthSource, /\bSupervisorHealth\b/);
  assert.doesNotMatch(healthSource, /\bDoltUsage\b/);
  assert.doesNotMatch(healthSource, /\bBeadsUsage\b/);
  assert.doesNotMatch(healthSource, /\bConfigComparisonRow\b/);
  assert.equal(exists(new URL('./formula-runs.ts', import.meta.url)), false);
});

test('errorMessage normalizes unknown error values for shared client/server reporting', () => {
  assert.equal(errorMessage(new Error('boom')), 'boom');
  assert.equal(errorMessage('plain failure'), 'plain failure');
  assert.equal(errorMessage({ status: 500 }), 'unknown error');
});

test('shared client-error and maintainer intent types compile as cross-workspace contracts', () => {
  const report: ClientErrorReport = {
    component: 'AgentDetail',
    operation: 'refreshBeads',
    message: 'failed',
  };
  const intent: SlingIntent = 'triage';
  const kind: SlingKind = 'issue';

  assert.equal(report.component, 'AgentDetail');
  assert.equal(intent, 'triage');
  assert.equal(kind, 'issue');
});

test('shared list envelope generics compile from leaves and barrel', () => {
  const available: Avail<{ data: string }> = { status: 'available', data: 'ok' };
  const unavailable: LeafAvail<{ data: string }> = {
    status: 'unavailable',
    error: 'missing supervisor data',
  };
  const list: PartialAwareList<{ id: string }> = { items: [{ id: 'a' }] };
  const counted: CountedList<{ id: string }> = {
    items: [{ id: 'a' }],
    total: 1,
  };
  const requiredPartial: RequiredPartialList<{ id: string }> = {
    items: [],
    partial: false,
  };

  const leafList: LeafPartialAwareList<{ id: string }> = list;
  const leafCounted: LeafCountedList<{ id: string }> = counted;
  const leafRequiredPartial: LeafRequiredPartialList<{ id: string }> = requiredPartial;

  assert.equal(available.data, 'ok');
  assert.equal(unavailable.error, 'missing supervisor data');
  assert.equal(leafList.items[0]?.id, 'a');
  assert.equal(leafCounted.total, 1);
  assert.equal(leafRequiredPartial.partial, false);
});

test('snapshot availability states use the shared Avail<T> generic', () => {
  const source = readFileSync(new URL('./snapshot/types.ts', import.meta.url), 'utf8');
  const aliases: Array<[string, RegExp]> = [
    ['RunCensusState', /export type RunCensusState = Avail<\{\s*data: RunCensus;\s*\}>;/],
    [
      'RunLaneHealthState',
      /export type RunLaneHealthState = Avail<\{\s*data: RunLaneHealth;\s*\}>;/,
    ],
    ['RunLaneUpdatedAt', /export type RunLaneUpdatedAt = Avail<\{\s*at: string;\s*\}>;/],
    [
      'RunLaneStagePosition',
      /export type RunLaneStagePosition = Avail<\{\s*index: number;\s*key: string;\s*label: string;\s*\}>;/,
    ],
    ['RunLaneStepAttempt', /export type RunLaneStepAttempt = Avail<\{\s*value: number;\s*\}>;/],
    ['RunLaneStuckNode', /export type RunLaneStuckNode = Avail<\{\s*id: string;\s*\}>;/],
    [
      'RunLaneSessionLastActive',
      /export type RunLaneSessionLastActive = Avail<\{\s*at: string;\s*\}>;/,
    ],
    [
      'RunLaneSessionRunning',
      /export type RunLaneSessionRunning = Avail<\{\s*value: boolean;\s*\}>;/,
    ],
    [
      'RunLaneSessionActivity',
      /export type RunLaneSessionActivity = Avail<\{\s*value: string;\s*\}>;/,
    ],
    [
      'RunLaneScope',
      /export type RunLaneScope = Avail<\{\s*kind: 'city' \| 'rig';\s*ref: string;\s*rootStoreRef: string;\s*\}>;/,
    ],
  ];

  for (const [alias, pattern] of aliases) {
    assert.match(source, pattern, `${alias} should be declared with Avail<T>`);
  }
});

test('snapshot availability states compile as Avail<T> aliases', () => {
  const census: Avail<{ data: RunCensus }> = {
    status: 'unavailable',
    error: 'census unavailable',
  } satisfies RunCensusState;
  const health: Avail<{ data: RunLaneHealth }> = {
    status: 'unavailable',
    error: 'health unavailable',
  } satisfies RunLaneHealthState;
  const updatedAt: Avail<{ at: string }> = {
    status: 'available',
    at: '2026-05-31T00:00:00Z',
  } satisfies RunLaneUpdatedAt;
  const stage: Avail<{ index: number; key: string; label: string }> = {
    status: 'available',
    index: 1,
    key: 'review',
    label: 'Review',
  } satisfies RunLaneStagePosition;
  const attempt: Avail<{ value: number }> = {
    status: 'available',
    value: 2,
  } satisfies RunLaneStepAttempt;
  const stuckNode: Avail<{ id: string }> = {
    status: 'available',
    id: 'node-1',
  } satisfies RunLaneStuckNode;
  const lastActive: Avail<{ at: string }> = {
    status: 'available',
    at: '2026-05-31T00:00:00Z',
  } satisfies RunLaneSessionLastActive;
  const running: Avail<{ value: boolean }> = {
    status: 'available',
    value: true,
  } satisfies RunLaneSessionRunning;
  const activity: Avail<{ value: string }> = {
    status: 'available',
    value: 'active',
  } satisfies RunLaneSessionActivity;
  const scope: Avail<{ kind: 'city' | 'rig'; ref: string; rootStoreRef: string }> = {
    status: 'available',
    kind: 'rig',
    ref: 'demo-app',
    rootStoreRef: 'rig:demo-app',
  } satisfies RunLaneScope;

  assert.equal(census.status, 'unavailable');
  assert.equal(health.status, 'unavailable');
  assert.equal(updatedAt.at, '2026-05-31T00:00:00Z');
  assert.equal(stage.key, 'review');
  assert.equal(attempt.value, 2);
  assert.equal(stuckNode.id, 'node-1');
  assert.equal(lastActive.at, '2026-05-31T00:00:00Z');
  assert.equal(running.value, true);
  assert.equal(activity.value, 'active');
  assert.equal(scope.rootStoreRef, 'rig:demo-app');
});

function exists(url: URL): boolean {
  try {
    readFileSync(url);
    return true;
  } catch {
    return false;
  }
}
