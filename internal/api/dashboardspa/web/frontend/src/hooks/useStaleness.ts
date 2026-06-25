import { useMemo } from 'react';
import type { RunLane } from 'gas-city-dashboard-shared';
import { useNow } from '../contexts/NowContext';

// gascity-dashboard-kb3: client-side staleness derivation. The server
// emits NO stalenessTier and NO byStalenessTier census (R9-strict
// contract from gascity-dashboard-3ax); the threshold crossing happens
// here on a 1s tick driven by NowContext.
//
// The "age" feeding the tier is derived from max(lane.updatedAt,
// session.lastActive) — i.e. the MOST-RECENT operator-visible fact.
// A fresh session masks an old bead write because what the operator
// can see (the session activity) is what should drive the tier.

export type StalenessTier = 'fresh' | 'warning' | 'stalled' | 'unknown';

/**
 * Tier boundaries (ageMs lower-bounds). Tunable in one place so the PRD
 * §12 acceptance tests can pin them via fixtures without touching the
 * renderer. The `fresh: 0` entry exists only to keep the satisfies-Record
 * exhaustive over `Exclude<StalenessTier, 'unknown'>`; the derivation
 * reads only `warning` and `stalled` as thresholds.
 */
export const STALENESS_TIER_MS = {
  fresh: 0,
  warning: 5 * 60_000,
  stalled: 30 * 60_000,
} as const satisfies Record<Exclude<StalenessTier, 'unknown'>, number>;

/** Threshold at which a lane crosses from warning -> stalled and joins the failing count. */
export const STALENESS_THRESHOLD_MS = STALENESS_TIER_MS.stalled;

interface LaneStaleness {
  tier: StalenessTier;
  /** Milliseconds since the most-recent operator-visible fact. */
  ageMs: number;
  /**
   * True iff (tier === 'stalled') AND lane.health.phaseConfidence === 'known'.
   * Used to gate the maroon One Mark + the failing-count contribution.
   * An inferred lane is NEVER stalled-by-this-hook (R2).
   */
  isStalled: boolean;
}

export interface StalenessResult {
  byLane: ReadonlyMap<string, LaneStaleness>;
  /** Order-preserving, oldest-first ranking of `isStalled === true` lane ids. */
  clientStalledLaneIds: readonly string[];
}

function pickMostRecentFact(lane: RunLane): number | null {
  const candidates: number[] = [];
  if (lane.updatedAt.status === 'available') {
    const t = Date.parse(lane.updatedAt.at);
    if (!Number.isNaN(t)) candidates.push(t);
  }
  if (lane.health.status === 'available') {
    const session = lane.health.data.session;
    if (session.status === 'resolved' && session.lastActive.status === 'available') {
      const t = Date.parse(session.lastActive.at);
      if (!Number.isNaN(t)) candidates.push(t);
    }
  }
  if (candidates.length === 0) return null;
  // Most-recent fact wins: a fresh session masks an old bead write
  // (operator visibility, per PRD R9).
  return Math.max(...candidates);
}

function tierFromAge(ageMs: number): Exclude<StalenessTier, 'unknown'> {
  if (ageMs >= STALENESS_TIER_MS.stalled) return 'stalled';
  if (ageMs >= STALENESS_TIER_MS.warning) return 'warning';
  return 'fresh';
}

export function useStaleness(lanes: readonly RunLane[]): StalenessResult {
  const now = useNow();

  return useMemo(() => {
    const byLane = new Map<string, LaneStaleness>();
    const stalled: { id: string; ageMs: number }[] = [];

    for (const lane of lanes) {
      const factAt = pickMostRecentFact(lane);
      const isKnown =
        lane.health.status === 'available' && lane.health.data.phaseConfidence === 'known';
      if (factAt === null) {
        byLane.set(lane.id, { tier: 'unknown', ageMs: 0, isStalled: false });
        continue;
      }
      const ageMs = Math.max(0, now - factAt);
      // R2: an inferred lane has no honest tier. We don't trust the
      // phase-resolution that would tell us whether the operator should
      // care about the age, so we report 'unknown' and isStalled=false —
      // the same shape as "no fact at all". The renderer treats both as
      // explicit absences; neither drives the maroon One Mark.
      if (!isKnown) {
        byLane.set(lane.id, { tier: 'unknown', ageMs, isStalled: false });
        continue;
      }
      const tier = tierFromAge(ageMs);
      const isStalled = tier === 'stalled';
      byLane.set(lane.id, { tier, ageMs, isStalled });
      if (isStalled) stalled.push({ id: lane.id, ageMs });
    }

    stalled.sort((a, b) => b.ageMs - a.ageMs);
    return { byLane, clientStalledLaneIds: stalled.map((s) => s.id) };
  }, [lanes, now]);
}
