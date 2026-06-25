import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { RunLane, RunLaneHealth } from 'gas-city-dashboard-shared';
import { NowProvider } from '../contexts/NowContext';
import { STALENESS_TIER_MS, STALENESS_THRESHOLD_MS, useStaleness } from './useStaleness';

// gascity-dashboard-kb3: useStaleness owns the R9-strict client-side
// staleness derivation. The server emits NO stalenessTier enum; it ships
// the raw lane.updatedAt + lane.health.session.lastActive facts, and the
// client crosses the threshold on its 1s clock.
//
// CRITICAL invariant (R2): a lane with phaseConfidence='inferred' must
// NEVER appear as `isStalled=true`. The server's own thrashing count
// already excludes inferred; the client-derived stalled count must do
// the same or the headline "failing" overcounts a phantom.

afterEach(() => {
  vi.useRealTimers();
});

function laneFixture(overrides: {
  id: string;
  updatedAt?: string | { error: string };
  health: RunLaneHealth | { error: string };
}): RunLane {
  const health =
    'error' in overrides.health
      ? ({ status: 'unavailable', error: overrides.health.error } as const)
      : ({ status: 'available', data: overrides.health } as const);
  const updatedAt =
    typeof overrides.updatedAt === 'string'
      ? ({ status: 'available', at: overrides.updatedAt } as const)
      : overrides.updatedAt !== undefined
        ? ({ status: 'unavailable', error: overrides.updatedAt.error } as const)
        : ({ status: 'unavailable', error: 'updatedAt missing' } as const);

  return {
    id: overrides.id,
    title: overrides.id,
    formula: { status: 'unavailable', error: 'unused' },
    scope: { status: 'unavailable', error: 'unused' },
    external: { status: 'unavailable', error: 'unused' },
    phase: 'implementation',
    phaseLabel: 'Implementation',
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt,
    stages: [],
    progress: { status: 'unavailable', error: 'unused' },
    formulaStageResolved: false,
    health,
  };
}

function knownHealth(sessionLastActive?: string): RunLaneHealth {
  return {
    phaseConfidence: 'known',
    needsOperator: false,
    stuckNode: { status: 'unavailable', error: 'no active step' },
    thrashingDetected: false,
    session: {
      status: 'resolved',
      lastActive:
        sessionLastActive !== undefined
          ? { status: 'available', at: sessionLastActive }
          : { status: 'unavailable', error: 'no session lastActive' },
      running: { status: 'unavailable', error: 'unused' },
      activity: { status: 'unavailable', error: 'unused' },
    },
  };
}

function inferredHealth(sessionLastActive?: string): RunLaneHealth {
  return { ...knownHealth(sessionLastActive), phaseConfidence: 'inferred' };
}

const T0 = Date.parse('2026-05-29T20:00:00.000Z');

function wrapper({ children }: { children: React.ReactNode }) {
  return <NowProvider intervalMs={1000}>{children}</NowProvider>;
}

function renderStaleness(lanes: RunLane[]) {
  return renderHook(() => useStaleness(lanes), { wrapper });
}

describe('useStaleness', () => {
  it('derives age from min(lane.updatedAt, session.lastActive) so a fresh session masks stale bead writes', () => {
    // Per PRD R9: tier = "what does the operator actually see" — the more
    // recent of the two facts. A 30-minute-old bead with a 30-second-old
    // session is NOT stalled; we believe the session.
    vi.useFakeTimers().setSystemTime(new Date(T0));
    const tenMinAgo = new Date(T0 - 10 * 60_000).toISOString();
    const tenSecAgo = new Date(T0 - 10_000).toISOString();
    const lane = laneFixture({
      id: 'fresh-session',
      updatedAt: tenMinAgo,
      health: knownHealth(tenSecAgo),
    });
    const { result } = renderStaleness([lane]);
    const tier = result.current.byLane.get('fresh-session');
    expect(tier).toBeDefined();
    expect(tier?.ageMs).toBe(10_000);
    expect(tier?.tier).toBe('fresh');
    expect(tier?.isStalled).toBe(false);
  });

  it('crosses the stalled threshold purely from the wall-clock tick (no server hint)', () => {
    // Server emits no stalenessTier — purely client-side. Set the lane's
    // most-recent fact past the threshold; assert isStalled=true.
    vi.useFakeTimers().setSystemTime(new Date(T0));
    const wayBack = new Date(T0 - STALENESS_THRESHOLD_MS - 1_000).toISOString();
    const lane = laneFixture({
      id: 'old',
      updatedAt: wayBack,
      health: knownHealth(wayBack),
    });
    const { result } = renderStaleness([lane]);
    const tier = result.current.byLane.get('old');
    expect(tier?.isStalled).toBe(true);
    expect(tier?.tier).toBe('stalled');
  });

  it('NEVER marks an inferred lane as stalled (R2 footgun gate)', () => {
    // The single most important invariant in this hook. Inferred lanes
    // must be excluded from the failing count — they cannot drive the
    // maroon One Mark. The same lane with phaseConfidence='known' is
    // tested elsewhere; here we flip ONLY that bit and assert the gate.
    vi.useFakeTimers().setSystemTime(new Date(T0));
    const wayBack = new Date(T0 - STALENESS_THRESHOLD_MS - 60_000).toISOString();
    const lane = laneFixture({
      id: 'inferred-stale',
      updatedAt: wayBack,
      health: inferredHealth(wayBack),
    });
    const { result } = renderStaleness([lane]);
    const tier = result.current.byLane.get('inferred-stale');
    expect(tier?.isStalled).toBe(false);
    expect(tier?.tier).toBe('unknown');
    // And the headline-failing list excludes it.
    expect(result.current.clientStalledLaneIds).toEqual([]);
  });

  it('emits tier=unknown when the lane has no usable timestamp (R9 explicit-absence floor)', () => {
    // session.lastActive.unavailable + updatedAt.unavailable -> the
    // operator has no honest fact to read from. The hook MUST NOT silently
    // imply freshness; emit 'unknown' so the renderer can show a
    // explicit-absence marker.
    vi.useFakeTimers().setSystemTime(new Date(T0));
    const lane = laneFixture({
      id: 'no-facts',
      updatedAt: { error: 'absent' },
      health: knownHealth(undefined),
    });
    const { result } = renderStaleness([lane]);
    const tier = result.current.byLane.get('no-facts');
    expect(tier?.tier).toBe('unknown');
    expect(tier?.isStalled).toBe(false);
  });

  it('emits ageMs and tier through the 1s tick boundary as the clock advances', () => {
    // The render only changes when the tier crosses a boundary — but the
    // ageMs MUST update every tick because the rendered "waited N min"
    // string depends on it. (Phase 1 architect finding C3.)
    vi.useFakeTimers().setSystemTime(new Date(T0));
    // Lane just past the warning tier boundary at T0.
    const justWarning = new Date(T0 - STALENESS_TIER_MS.warning - 100).toISOString();
    const lane = laneFixture({
      id: 'ticking',
      updatedAt: justWarning,
      health: knownHealth(justWarning),
    });
    const { result } = renderStaleness([lane]);
    const initial = result.current.byLane.get('ticking');
    expect(initial?.tier).toBe('warning');

    // Advance enough to cross the stalled threshold and let React
    // commit the resulting state update from the interval tick.
    // act() flushes the interval's setNow call so the next read of
    // result.current sees the new tier + age, exactly as it would
    // in production. (Phase 4 code-review M4.)
    act(() => {
      vi.advanceTimersByTime(STALENESS_THRESHOLD_MS - STALENESS_TIER_MS.warning + 1_000);
    });
    const later = result.current.byLane.get('ticking');
    expect(later?.tier).toBe('stalled');
    expect(later?.ageMs).toBeGreaterThan(initial?.ageMs ?? 0);
  });
});
