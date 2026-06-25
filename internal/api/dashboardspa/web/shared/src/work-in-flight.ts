// Work-in-flight assignee parsing — the primitives the "Workers active" section
// uses to tie an in-progress bead to the live worker session that owns it.
//
// Each in-progress bead carries an `assignee` that embeds the live session id:
//
//   bead gc-5rarj               assignee polecat-gc-335825          → gc-335825
//   bead scix_experiments-4if7h assignee scix-worker-gc-335812      → gc-335812
//   bead EnterpriseBench-mda    assignee enterprisebench-worker-gc-335808 → gc-335808
//
// Pattern: `<worker-role>-<session-id>` where the session id is the trailing
// `gc-…` (or other 2/4-letter-prefixed) handle. The frontend derives the live
// Workers-active list from the SESSIONS (see hooks/activeWorkers), using
// parseAssignee only to best-effort attach a captured bead to a worker row.
//
// Pure over plain strings — no IO, no React, no DOM.

/**
 * The trailing supervisor session-handle embedded in an assignee. Anchored to
 * the END of the string and preceded by a boundary (`-`, `_`, `/`, or start),
 * so `polecat-gc-335825` yields `gc-335825` and the role prefix is whatever
 * comes before.
 *
 * The handle prefix mirrors SESSION_ID_RE (`gc`/`td`/`th` literal or any
 * 4-letter city code). The id BODY is deliberately tightened to `[a-z0-9]`
 * (no internal hyphen) so the match binds to the MINIMAL trailing handle:
 * without that, a role like `scix-worker` (whose `scix` is itself a 4-letter
 * token) would let the greedy body swallow `scix-worker-gc-335812` whole. Live
 * session ids are hyphen-free after the prefix dash (`gc-335825`, `td-9abc`),
 * so this loses nothing real.
 */
const ASSIGNEE_SESSION_ID_RX = /[-_/]((?:gc|td|th|[a-z]{4})-[a-z0-9]{1,32})$/;

// A bare session handle (the assignee IS a session id, no role prefix). Same
// alphabet as ASSIGNEE_SESSION_ID_RX but whole-string. NOT SESSION_ID_RE: that
// validator permits internal hyphens in the body, which would let a composite
// like `scix-worker-gc-335812` masquerade as one bare handle.
//
// The id body MUST contain at least one digit. Live session ids always carry a
// numeric handle (`gc-335825`, `td-9abc`); a plain 4-letter-prefixed *role* like
// `scix-worker` would otherwise match (`scix` prefix + `worker` body) and be
// misparsed as a bare session id. Requiring a digit keeps roles out.
const BARE_SESSION_ID_RX = /^(?:gc|td|th|[a-z]{4})-[a-z0-9]*[0-9][a-z0-9]*$/;

export interface ParsedAssignee {
  /** The extracted live session id (e.g. `gc-335825`), or undefined when the
   *  assignee carries no recognizable session handle. */
  sessionId?: string;
  /** The worker-role prefix with the session-id suffix stripped
   *  (e.g. `polecat`, `scix-worker`, `enterprisebench-worker`). Falls back to
   *  the whole assignee when no session suffix is present. */
  role: string;
}

/**
 * Split a bead assignee into its worker role and embedded session id.
 *
 * `polecat-gc-335825` → `{ role: 'polecat', sessionId: 'gc-335825' }`.
 * An assignee with no session handle (e.g. a bare alias) yields the whole
 * string as the role and no sessionId — the caller degrades gracefully.
 */
export function parseAssignee(assignee: string): ParsedAssignee {
  const trimmed = assignee.trim();
  // Bare handle: the assignee IS a session id with no role prefix. Name the row
  // with the id itself so it still reads.
  if (BARE_SESSION_ID_RX.test(trimmed)) {
    return { role: trimmed, sessionId: trimmed };
  }
  const match = ASSIGNEE_SESSION_ID_RX.exec(trimmed);
  const sessionId = match?.[1];
  if (match === null || sessionId === undefined) {
    return { role: trimmed };
  }
  // Strip the matched `<sep>gc-…` tail to recover the role. The match starts at
  // the separator boundary, so slice up to match.index.
  return { role: trimmed.slice(0, match.index), sessionId };
}

/**
 * The bead status that means "actively being worked on". A single literal so
 * the work-in-flight filter and the status badge stay in lockstep.
 */
export const IN_PROGRESS_STATUS = 'in_progress';
