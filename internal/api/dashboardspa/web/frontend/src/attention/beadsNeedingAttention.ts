import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { elapsedSince, formatElapsed } from './elapsed';

// gascity-dashboard-2j8e.3: the single selector behind the Beads nav badge AND
// the /beads "Needs you" section. It counts beads that genuinely need the
// operator — ready-unclaimed work and abnormally-blocked (escalated /
// help-requested) beads — and EXCLUDES plain dependency-blocked beads. bd
// defines `blocked` as "blocked by a dependency": a bead waiting on its blocker
// is working-as-intended queuing, not attention. The badge (registry
// deriveBeadsAttention) and the page both read this projection, so the nav
// count and the page count cannot disagree — the parity contract the Runs badge
// established (selectBlockedRuns, gascity-dashboard-2j8e.2).
//
// Two inputs because they arrive from two reads with opposite filtering:
//  - `beads` is the general engineering-bead list. The dashboard's bead reads
//    drop `gc:`-labelled bookkeeping beads, so escalations never appear here.
//  - `escalations` is the dedicated open-`gc:escalation` queue (the marker the
//    prior `gc dashboard` escalations panel keyed on), fetched separately so the
//    gc:-label filter does not hide it — the same shape as the mayor-decision
//    queue.

// Aging for ready-unclaimed work: a just-filed open bead is normal churn, not
// attention. It enters the badge as `watch` once it has sat unclaimed past the
// watch window, and escalates to `attention` once it is genuinely stale.
const READY_UNCLAIMED_WATCH_MS = 24 * 60 * 60 * 1000;
const READY_UNCLAIMED_STALE_MS = 72 * 60 * 60 * 1000;

/**
 * Why a bead needs the operator. `escalated` is an abnormally-blocked bead that
 * raised the escalation marker (a help-request / escalation); `ready-unclaimed`
 * is open work nobody claimed. Plain dependency-blocked is neither — excluded.
 */
export type BeadAttentionReason = 'ready-unclaimed' | 'escalated';

/** The badge-driving severities — escalation acts now, stale unclaimed escalates. */
export type BeadAttentionSeverity = 'attention' | 'watch';

export interface BeadAttentionRow {
  beadId: string;
  reason: BeadAttentionReason;
  severity: BeadAttentionSeverity;
  /** Operator-facing one-line context, leading with the bead title (why it is here). */
  summary: string;
  /** Movement timestamp used for ordering and aging. */
  updatedAt: string;
}

export interface BeadAttentionInputs {
  /** The general engineering-bead list (gc:-labelled bookkeeping already dropped). */
  beads: readonly Bead[];
  /** The dedicated open-`gc:escalation` queue (help-request / escalation). */
  escalations: readonly Bead[];
}

/**
 * Project the bead reads into the operator-actionable attention set. Pure and
 * deterministic given (inputs, nowMs) — the badge and the page read the same
 * output, so their counts agree by construction.
 */
export function selectBeadsNeedingAttention(
  inputs: BeadAttentionInputs,
  nowMs: number,
): BeadAttentionRow[] {
  const rows: BeadAttentionRow[] = [];
  for (const bead of inputs.escalations) {
    const row = escalatedRow(bead);
    if (row !== null) rows.push(row);
  }
  for (const bead of inputs.beads) {
    const row = readyUnclaimedRow(bead, nowMs);
    if (row !== null) rows.push(row);
  }
  return rows;
}

// Escalated / help-requested: an open escalation bead is abnormal blocking —
// counted immediately, regardless of age. A resolved (closed) escalation is not.
function escalatedRow(bead: Bead): BeadAttentionRow | null {
  if (bead.status === 'closed') return null;
  return {
    beadId: bead.id,
    reason: 'escalated',
    severity: 'attention',
    summary: `${bead.title} — escalation raised`,
    updatedAt: bead.updated_at ?? bead.created_at,
  };
}

// Ready-unclaimed: open work with no assignee, aged past the watch window so
// normal churn does not inflate the badge. Plain dependency-blocked (bd
// `blocked` = "blocked by a dependency") and in-progress/closed work are not
// surfaced — only genuinely-claimable open beads.
function readyUnclaimedRow(bead: Bead, nowMs: number): BeadAttentionRow | null {
  if (bead.status !== 'open' || hasAssignee(bead)) return null;
  const ageMs = elapsedSince(bead.created_at, nowMs);
  if (ageMs === null || ageMs < READY_UNCLAIMED_WATCH_MS) return null;
  const stale = ageMs >= READY_UNCLAIMED_STALE_MS;
  return {
    beadId: bead.id,
    reason: 'ready-unclaimed',
    severity: stale ? 'attention' : 'watch',
    summary: `${bead.title} opened ${formatElapsed(ageMs)} ago`,
    updatedAt: bead.created_at,
  };
}

function hasAssignee(bead: Bead): boolean {
  return bead.assignee !== undefined && bead.assignee.trim().length > 0;
}
