import type { RunLane } from '../snapshot/types.js';

// The run-health derivation (deriveRunHealth / buildCensus / advanceProgressMarks)
// moved to Go (internal/runproj); the dashboard reads health and census off the
// server-computed RunSummary DTO. This pure accessor is the one piece that stays
// client-side, because the attention layer (AmbientHome, StatusSentence) reads
// it during a session-list outage when per-lane health degrades to unavailable.

/**
 * Structural needs-operator signal for a lane: true when the lane's phase is a
 * human-gate phase ('approval' or 'blocked'). This is derived from lane.phase
 * alone — a structural bead-state fact, NOT a session-derived health
 * conclusion. It therefore stays valid even when the session list is
 * unavailable and per-lane health degrades to status:'unavailable'. Consumers
 * must read needsOperator through this accessor rather than gating it behind
 * health.status === 'available', or a human-gate decision vanishes from the
 * home concern region during a session-list outage.
 */
export function laneNeedsOperator(lane: RunLane): boolean {
  return lane.phase === 'approval' || lane.phase === 'blocked';
}
