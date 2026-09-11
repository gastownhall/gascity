// Dashboard-owned DTO for the home-view alert system (gascity-dashboard-4s07,
// PRD R1). This is the single source of truth for one ranked "actionable
// work" item; both the backend snapshot read path and the frontend SSE
// pending layer import it, so a shape mismatch is a compile error rather
// than a runtime undefined.
//
// Design constraints carried from the converged PRD + premortem:
//  - Closed unions (no stringly-typed logic): the `as const` arrays below are
//    the single source for both the runtime list and the derived type.
//  - Provenance travels with every item (R6/R15): `provenance` is the source's
//    SourceStatus, so a stale/error/fixture-derived alert can never be rendered
//    as live truth and never drives the One Mark.
//  - Last-write-wins dedup (R17): `dedupKey` + a monotonic upstream `version`
//    let a newer live (SSE) row supersede a stale (snapshot-envelope) row with
//    the same key, closing the resolved-while-stale self-contradiction window.
//  - Ranking, eligibility predicates, and inhibition live in R5/R8, not here —
//    this module is the contract only.

import type { IsoTimestamp } from './dashboard-sessions.js';
import type { SourceStatus } from './snapshot/types.js';

/** The closed set of actionable-signal kinds surfaced on the home view. */
export const ALERT_KINDS = [
  'pending-decision',
  'run-needs-operator',
  'run-thrashing',
  'operator-mail',
] as const;
export type AlertKind = (typeof ALERT_KINDS)[number];

/** The data plane an alert was derived from. */
export const ALERT_SOURCES = ['runs', 'mail', 'pending'] as const;
export type AlertSource = (typeof ALERT_SOURCES)[number];

/**
 * Severity tier. Deliberately two-valued — it maps to DESIGN.md's One Mark
 * (a single maroon for the top item), NOT a 0-100 score. `failing` outranks
 * `attention`; see ALERT_SEVERITY_RANK.
 */
export const ALERT_SEVERITIES = ['attention', 'failing'] as const;
export type AlertSeverity = (typeof ALERT_SEVERITIES)[number];

/**
 * Centralized severity ordering so R5 ranking never re-encodes magic numbers.
 * Higher rank = more severe = sorts first.
 */
export const ALERT_SEVERITY_RANK: Readonly<Record<AlertSeverity, number>> = {
  attention: 0,
  failing: 1,
};

/**
 * Deep-link identifiers for an alert. At least one must be present (enforced
 * by makeAlertDedupKey). `requestId` is the PendingInteraction idempotency key
 * (R11) and takes precedence as the dedup identity for pending decisions.
 */
export interface AlertRef {
  readonly runId?: string;
  readonly beadId?: string;
  readonly mailId?: string;
  readonly sessionId?: string;
  readonly requestId?: string;
}

/** One ranked actionable-work item on the home view. */
export interface AlertItem {
  readonly kind: AlertKind;
  readonly source: AlertSource;
  /** Identifiers used to build `href` and `dedupKey`. */
  readonly ref: AlertRef;
  /** Authoritative-surface deep link the row navigates to. */
  readonly href: string;
  readonly title: string;
  /** Machine-derived "why this is here" (e.g. sender role, phase). Never a semantic score. */
  readonly reason: string;
  readonly severity: AlertSeverity;
  readonly occurredAt: IsoTimestamp;
  /** Stable identity for dedup; pair with `version` for R17 last-write-wins. */
  readonly dedupKey: string;
  /**
   * Monotonic upstream version/sequence for this dedupKey. A higher value
   * supersedes a lower one with the same key (R17), so a live SSE row discards
   * a stale snapshot-envelope row instead of contradicting it.
   */
  readonly version: number;
  /** Freshness of the source this item was derived from (R6/R15). */
  readonly provenance: SourceStatus;
  /**
   * Count of lower-severity signals inhibited (folded) under this item (R8).
   * Present and > 0 only on a fold parent; absent means this row folds nothing.
   * A fold must never be silent, so the parent always carries the count.
   */
  readonly foldedCount?: number;
}

/**
 * Build the stable dedup identity for an alert. Mechanical and deterministic
 * (no semantic judgment): `<kind>:<id>` where the id is the most specific
 * available ref. `requestId` wins so a pending decision keeps one identity
 * across snapshot/SSE layers. Throws if no identifying ref is present — an
 * unidentifiable alert is a producer bug, not a silent empty key.
 */
export function makeAlertDedupKey(kind: AlertKind, ref: AlertRef): string {
  const id = ref.requestId ?? ref.runId ?? ref.beadId ?? ref.mailId ?? ref.sessionId;
  if (id === undefined || id.length === 0) {
    throw new Error(`makeAlertDedupKey: AlertRef for kind "${kind}" has no identifying id`);
  }
  return `${kind}:${id}`;
}
