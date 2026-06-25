import type { RunIssue } from './phaseMapping.js';
import type { RunLane } from '../snapshot/types.js';

// gascity-dashboard-s4rp: a run can be echoed by the supervisor long after it
// has stopped progressing. The operator repro is gc-1920 — an ancient
// (id ~1920 vs a current store at ~346k) mol-focus-review approval-gate latch
// with NO live session, ~4 days since its last bead write, and no in_progress
// step. It was counted as Active:1 and flickered in and out of the lane set on
// every refresh. Half (b) of gascity-dashboard-4xcv asked for these
// session-less latches to be demoted out of Active; the blocked half (a) was
// already handled by the blockedLanes split.
//
// The sharp predicate has to demote the dead latch WITHOUT demoting a run that
// is legitimately session-less for a benign reason — a freshly queued run that
// has not yet been picked up, or an approval gate genuinely waiting on a human.
// Those are RECENT; the dead latch is days old. Staleness is therefore the
// distinguishing axis, with a floor well clear of any human-in-the-loop
// turnaround so a real approval gate is never demoted while it is still being
// waited on.

/**
 * A run is treated as a stale session-less latch once its most-recent bead
 * write is older than this. Deliberately far above the live-attention staleness
 * tiers (frontend `STALENESS_TIER_MS.stalled` = 30m) — that tier flags a run
 * the operator should look at NOW; this floor decides a run is abandoned and
 * should leave the Active set entirely, so it must clear normal queue and
 * approval-gate dwell times (hours) with room to spare.
 */
export const STALE_LATCH_AFTER_MS = 24 * 60 * 60 * 1000;

/**
 * True when `lane` is an open run that is no longer progressing and should be
 * demoted out of the Active set: the session list resolved, no session maps to
 * the lane, no primary step is in_progress, and the lane's last write is older
 * than {@link STALE_LATCH_AFTER_MS}. complete/blocked lanes are already
 * partitioned out upstream and are never considered here.
 *
 * `nowMs` is the snapshot generation time (the caller's fetch timestamp), not a
 * live clock read — staleness is judged against the data's own generation so
 * the result is deterministic for a given snapshot (no wall-clock test flake).
 */
export function isStaleSessionlessLatch(
  lane: RunLane,
  nowMs: number,
  sessionsAvailable: boolean,
): boolean {
  // Without a session list we cannot trust the "no session" signal — a failed
  // session read must not demote every lane.
  if (!sessionsAvailable) return false;
  if (lane.phase === 'complete' || lane.phase === 'blocked') return false;
  // A live in_progress primary step means the run IS progressing.
  if (lane.progress.status === 'active_step') return false;
  // A resolved session means a worker is on it (even if parked at a gate).
  if (laneSessionResolved(lane)) return false;
  // Needs a known age to judge staleness; absent that we do not demote.
  if (lane.updatedAt.status !== 'available') return false;
  const ageMs = nowMs - Date.parse(lane.updatedAt.at);
  return Number.isFinite(ageMs) && ageMs >= STALE_LATCH_AFTER_MS;
}

function laneSessionResolved(lane: RunLane): boolean {
  return lane.health.status === 'available' && lane.health.data.session.status === 'resolved';
}

/**
 * True when a run group's root bead is absent from the group's issues — the run
 * is rooted at a bead that no longer exists in the store (gascity-dashboard-s4rp
 * dangling root). Such a group has no authoritative root metadata; its title is
 * inferred from a child and its scope is unresolvable, so it must not be
 * surfaced as a live run.
 */
export function isDanglingRootGroup(rootId: string, issues: readonly RunIssue[]): boolean {
  return !issues.some((issue) => issue.id === rootId);
}
