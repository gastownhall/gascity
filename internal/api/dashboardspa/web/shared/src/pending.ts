// PendingInteraction — the "an agent is blocked waiting on a human decision"
// signal (gascity-dashboard-8167, PRD R3). This is the highest-value home-view
// signal and the supervisor emits it ONLY on the per-session SSE stream
// (event: "pending"); the city event stream does not carry it (spike wf4c).
//
// Dashboard-owned wire shape, translated at the edge: it mirrors the gc
// supervisor `PendingInteraction` schema exactly (request_id + kind required;
// prompt/options/metadata optional). `parsePendingInteraction` is the boundary
// validator so untyped SSE JSON never flows in as a known shape.
//
// `kind` is a free-form string upstream (no enum, no "needs-human-judgment"
// marker — spike wf4c), so it is carried verbatim as the `reason` and never
// used to classify urgency (ZFC). `options` is the authoritative set of
// response verbs for R11's accept/decline; do not hardcode allow/deny.

import type { AlertItem } from './alert.js';
import { makeAlertDedupKey } from './alert.js';
import type { IsoTimestamp } from './dashboard-sessions.js';
import type { SourceStatus } from './snapshot/types.js';

export interface PendingInteraction {
  readonly request_id: string;
  readonly kind: string;
  readonly prompt?: string;
  /** Accepted response verbs for this interaction (R11). Null/absent upstream when unconstrained. */
  readonly options?: readonly string[];
  readonly metadata?: Readonly<Record<string, string>>;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Boundary parse for an SSE `pending` frame. Returns null on any shape that is
 * not a valid PendingInteraction — the caller surfaces that as a degraded
 * stream, never as a silently-accepted partial.
 */
export function parsePendingInteraction(data: unknown): PendingInteraction | null {
  if (!isRecord(data)) return null;
  if (typeof data.request_id !== 'string' || data.request_id.length === 0) return null;
  if (typeof data.kind !== 'string' || data.kind.length === 0) return null;

  const result: {
    request_id: string;
    kind: string;
    prompt?: string;
    options?: readonly string[];
    metadata?: Readonly<Record<string, string>>;
  } = { request_id: data.request_id, kind: data.kind };

  if (typeof data.prompt === 'string') result.prompt = data.prompt;
  if (Array.isArray(data.options) && data.options.every((o) => typeof o === 'string')) {
    result.options = data.options as readonly string[];
  }
  if (isRecord(data.metadata)) {
    const entries = Object.entries(data.metadata).filter(
      (e): e is [string, string] => typeof e[1] === 'string',
    );
    if (entries.length > 0) result.metadata = Object.fromEntries(entries);
  }
  return result;
}

/** First non-empty line of the prompt, trimmed for a one-line title. */
function promptTitle(prompt: string | undefined): string {
  if (prompt === undefined) return 'agent awaiting your decision';
  const firstLine = prompt.split('\n', 1)[0]?.trim() ?? '';
  return firstLine.length > 0 ? firstLine : 'agent awaiting your decision';
}

export interface PendingAlertContext {
  readonly sessionId: string;
  readonly occurredAt: IsoTimestamp;
  /** Monotonic per-dedupKey ordinal for R17 last-write-wins (e.g. an event sequence). */
  readonly version: number;
  /** Freshness of the stream this was observed on (R6/R16); 'fresh' for a live SSE frame. */
  readonly provenance: SourceStatus;
}

/**
 * Map a PendingInteraction to a `pending-decision` AlertItem. Pure — the live
 * SSE layer (or the per-session detail view) supplies the context. Severity is
 * 'attention' (a request for input, not a system failure); pending-decision's
 * rank ABOVE run signals is R5's job via kind precedence, not a severity hack.
 */
export function pendingInteractionToAlert(
  pending: PendingInteraction,
  ctx: PendingAlertContext,
): AlertItem {
  return {
    kind: 'pending-decision',
    source: 'pending',
    ref: { requestId: pending.request_id, sessionId: ctx.sessionId },
    href: `/agents/${encodeURIComponent(ctx.sessionId)}`,
    title: promptTitle(pending.prompt),
    reason: pending.kind,
    severity: 'attention',
    occurredAt: ctx.occurredAt,
    dedupKey: makeAlertDedupKey('pending-decision', { requestId: pending.request_id }),
    version: ctx.version,
    provenance: ctx.provenance,
  };
}
