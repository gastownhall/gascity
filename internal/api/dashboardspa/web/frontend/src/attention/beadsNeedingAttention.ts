import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { elapsedSince, formatElapsed, formatElapsedFine } from './elapsed';

// gascity-dashboard-2j8e.3: the single selector behind the Beads nav badge AND
// the /beads "Needs you" section. It counts beads that genuinely need the
// operator — ready-unclaimed work, abnormally-blocked (escalated /
// help-requested) beads, stalled in-progress work, and work waiting on a named
// human — and EXCLUDES plain dependency-blocked beads. bd defines `blocked` as
// "blocked by a dependency": a bead waiting on its blocker is
// working-as-intended queuing, not attention. The badge (registry
// deriveBeadsAttention) and the page both read this projection, so the nav
// count and the page count cannot disagree — the parity contract the Runs badge
// established (selectBlockedRuns, gascity-dashboard-2j8e.2).
//
// Inputs arrive from separate reads with different filtering:
//  - `beads` is the general engineering-bead list. The dashboard's bead reads
//    drop `gc:`-labelled bookkeeping beads, so escalations never appear here.
//  - `escalations` is the dedicated open-`gc:escalation` queue (the marker the
//    prior `gc dashboard` escalations panel keyed on), fetched separately so the
//    gc:-label filter does not hide it — the same shape as the mayor-decision
//    queue.
//  - `sessions` is the city session list, used to decide whether an
//    in-progress bead's assignee is still alive. Optional: when the session
//    read failed, session-dependent stall checks are skipped rather than
//    marking every in-progress bead stalled during a sessions outage.

// Aging for ready-unclaimed work: a just-filed open bead is normal churn, not
// attention. It enters the badge as `watch` once it has sat unclaimed past the
// watch window, and escalates to `attention` once it is genuinely stale.
const READY_UNCLAIMED_WATCH_MS = 24 * 60 * 60 * 1000;
const READY_UNCLAIMED_STALE_MS = 72 * 60 * 60 * 1000;

// Stalled: an in-progress bead whose assignee shows no sign of life for this
// long. Matches the operator intuition that an agent quiet for an hour mid-task
// has lost its session, not paused for thought.
const STALLED_INACTIVITY_MS = 60 * 60 * 1000;

// Waiting-on-human: a checkpoint hold is expected to clear within a working
// beat; past this it escalates from `watch` to `attention` so a founder gate
// nobody answered stops being invisible.
const WAITING_ATTENTION_MS = 2 * 60 * 60 * 1000;

// Session states that count as "the assignee is alive". Anything else —
// drained, dead, error, unknown — means the session cannot be doing the work.
const LIVE_SESSION_STATES: ReadonlySet<string> = new Set([
  'active',
  'awake',
  'creating',
  'start-pending',
  'draining',
]);

// The gc metadata markers a worker (or the mayor) stamps on a bead that is
// parked on a human checkpoint. `gc.waiting_on` names the human directly; the
// hold/gate text often carries the name in a parenthetical ("founder design
// review (Taylor+Afik)").
const WAITING_META_KEYS = ['gc.checkpoint_hold', 'gc.founder_gate'] as const;
const WAITING_CHECKPOINT_VALUE = 'awaiting-human';
const WAITING_WHO_META_KEY = 'gc.waiting_on';
const HOLD_LABEL_PREFIX = 'hold:';

// Where a live worker's activity shows up: the session's own last_active, and
// the lease heartbeat the worker stamps onto the bead.
const HEARTBEAT_META_KEY = 'gc.last_heartbeat_at';
const SESSION_ID_META_KEY = 'gc.session_id';

/**
 * Why a bead needs the operator. `escalated` is an abnormally-blocked bead that
 * raised the escalation marker (a help-request / escalation); `ready-unclaimed`
 * is open work nobody claimed; `stalled` is in-progress work whose assignee
 * shows no sign of life; `waiting-human` is work parked on a named human
 * checkpoint. Plain dependency-blocked is none of these — excluded.
 */
export type BeadAttentionReason = 'ready-unclaimed' | 'escalated' | 'stalled' | 'waiting-human';

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

/**
 * The slice of a supervisor session the stall check reads. Structural so the
 * selector stays pure and testable without the full generated wire type.
 */
export interface BeadAttentionSession {
  id: string;
  session_name: string;
  alias?: string;
  state: string;
  last_active?: string;
}

export interface BeadAttentionInputs {
  /** The general engineering-bead list (gc:-labelled bookkeeping already dropped). */
  beads: readonly Bead[];
  /** The dedicated open-`gc:escalation` queue (help-request / escalation). */
  escalations: readonly Bead[];
  /** City sessions for liveness checks; omit when the session read failed. */
  sessions?: readonly BeadAttentionSession[];
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
  // waiting-human is checked first: a held bead may also be open+unassigned
  // (a gate bead) or look stalled (a parked worker), and the operator-facing
  // fact in every such case is WHO it waits on.
  for (const bead of inputs.beads) {
    const row =
      waitingHumanRow(bead, nowMs) ??
      readyUnclaimedRow(bead, nowMs) ??
      stalledRow(bead, inputs.sessions, nowMs);
    if (row !== null) rows.push(row);
  }
  return rows;
}

/**
 * The compact status note an in-progress board card carries under its title:
 * the waiting/stalled reason when one fires, otherwise `assignee · active Nm
 * ago`. Lives here so the card, the badge, and the "Needs you" panel read the
 * same rules — tuning a threshold changes all three together.
 */
export function inProgressCardNote(
  bead: Bead,
  sessions: readonly BeadAttentionSession[] | undefined,
  nowMs: number,
): string | null {
  if (bead.status !== 'in_progress') return null;
  const waiting = waitingHumanRow(bead, nowMs);
  if (waiting !== null) return waiting.summary;
  const stalled = stalledRow(bead, sessions, nowMs);
  if (stalled !== null) return stalled.summary;
  const assignee = bead.assignee?.trim() ?? '';
  const activityMs = lastActivityElapsedMs(bead, resolveSession(bead, sessions), nowMs);
  if (activityMs === null) return assignee.length > 0 ? assignee : null;
  const phrase = `active ${formatElapsedFine(activityMs)} ago`;
  return assignee.length > 0 ? `${assignee} · ${phrase}` : phrase;
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

// Waiting-on-human: the bead carries a checkpoint-hold / founder-gate marker.
// Named before stalled — a worker parked on a human gate is doing the right
// thing, and the operator-facing fact is WHO it waits on, not that the session
// went quiet. Waiting-since approximates as the bead's last movement
// (updated_at): the hold markers carry no timestamp of their own.
function waitingHumanRow(bead: Bead, nowMs: number): BeadAttentionRow | null {
  if (bead.status === 'closed') return null;
  const holdText = waitingHoldText(bead);
  if (holdText === null) return null;
  const waitMs = elapsedSince(bead.updated_at ?? bead.created_at, nowMs);
  const who = waitingOn(bead, holdText);
  const phrase =
    waitMs === null ? `waiting on ${who}` : `waiting on ${who} for ${formatElapsed(waitMs)}`;
  return {
    beadId: bead.id,
    reason: 'waiting-human',
    severity: waitMs !== null && waitMs >= WAITING_ATTENTION_MS ? 'attention' : 'watch',
    summary: phrase,
    updatedAt: bead.updated_at ?? bead.created_at,
  };
}

// Stalled: in-progress work whose assignee shows no sign of life — no assignee
// at all, no session resolving to the assignee, a session in a non-live state,
// or no activity (session last_active / bead heartbeat) for over an hour. When
// the session read failed (`sessions` undefined), only the no-assignee check
// runs: without session data we cannot distinguish "worker gone" from "worker
// alive but not heartbeating", so we do not guess — a sessions outage must not
// paint the board stalled.
function stalledRow(
  bead: Bead,
  sessions: readonly BeadAttentionSession[] | undefined,
  nowMs: number,
): BeadAttentionRow | null {
  if (bead.status !== 'in_progress') return null;
  const assignee = bead.assignee?.trim() ?? '';
  const detail = stalledDetail(bead, assignee, sessions, nowMs);
  if (detail === null) return null;
  const ageMs = elapsedSince(bead.updated_at ?? bead.created_at, nowMs);
  const phrase =
    ageMs === null ? `stalled — ${detail}` : `stalled ${formatElapsed(ageMs)} — ${detail}`;
  return {
    beadId: bead.id,
    reason: 'stalled',
    severity: 'attention',
    summary: phrase,
    updatedAt: bead.updated_at ?? bead.created_at,
  };
}

function stalledDetail(
  bead: Bead,
  assignee: string,
  sessions: readonly BeadAttentionSession[] | undefined,
  nowMs: number,
): string | null {
  if (assignee.length === 0) return 'no assignee';
  if (sessions === undefined) return null;
  const session = resolveSession(bead, sessions);
  if (session === undefined) {
    return `no live session for ${assignee}`;
  }
  if (!LIVE_SESSION_STATES.has(session.state)) {
    return `session ${session.state}`;
  }
  const activityMs = lastActivityElapsedMs(bead, session, nowMs);
  if (activityMs !== null && activityMs >= STALLED_INACTIVITY_MS) {
    return `no activity for ${formatElapsed(activityMs)}`;
  }
  return null;
}

// The assignee names a concrete session (session_name), sometimes an alias or
// a bare session id; `gc.session_id` on the bead pins the exact session when
// stamped. Among candidates a LIVE one wins — a recycled session name or a
// stale pinned id must not hide a worker that is genuinely alive under
// another identity of the same bead.
function resolveSession(
  bead: Bead,
  sessions: readonly BeadAttentionSession[] | undefined,
): BeadAttentionSession | undefined {
  if (sessions === undefined) return undefined;
  const assignee = bead.assignee?.trim() ?? '';
  const sessionId = bead.metadata?.[SESSION_ID_META_KEY];
  const candidates = sessions.filter(
    (session) =>
      (sessionId !== undefined && session.id === sessionId) ||
      (assignee.length > 0 &&
        (session.session_name === assignee ||
          session.alias === assignee ||
          session.id === assignee)),
  );
  if (candidates.length === 0) return undefined;
  return candidates.find((session) => LIVE_SESSION_STATES.has(session.state)) ?? candidates[0];
}

// Freshest sign of life: session last_active vs the lease heartbeat the worker
// stamps on the bead. Null when neither is known — unknown is not stale.
function lastActivityElapsedMs(
  bead: Bead,
  session: BeadAttentionSession | undefined,
  nowMs: number,
): number | null {
  const candidates = [
    elapsedSince(session?.last_active, nowMs),
    elapsedSince(bead.metadata?.[HEARTBEAT_META_KEY], nowMs),
  ].filter((value): value is number => value !== null);
  if (candidates.length === 0) return null;
  return Math.min(...candidates);
}

function waitingHoldText(bead: Bead): string | null {
  for (const key of WAITING_META_KEYS) {
    const value = bead.metadata?.[key];
    if (value !== undefined && value.trim().length > 0) return value;
  }
  if (bead.metadata?.['gc.checkpoint'] === WAITING_CHECKPOINT_VALUE)
    return WAITING_CHECKPOINT_VALUE;
  const holdLabel = (bead.labels ?? []).find((label) => label.startsWith(HOLD_LABEL_PREFIX));
  if (holdLabel !== undefined) return holdLabel.slice(HOLD_LABEL_PREFIX.length);
  return null;
}

// WHO the bead waits on — the whole point of the chip (mayor design check,
// gp-6xd): `gc.waiting_on` when stamped, else the parenthetical in the hold
// text ("founder design review (Taylor+Afik)" → "Taylor+Afik"), else "human".
function waitingOn(bead: Bead, holdText: string): string {
  const stamped = bead.metadata?.[WAITING_WHO_META_KEY]?.trim();
  if (stamped !== undefined && stamped.length > 0) return stamped;
  const parenthetical = /\(([^)]+)\)/.exec(holdText);
  const inner = parenthetical?.[1]?.trim();
  if (inner !== undefined && inner.length > 0) return inner;
  return 'human';
}

function hasAssignee(bead: Bead): boolean {
  return bead.assignee !== undefined && bead.assignee.trim().length > 0;
}
