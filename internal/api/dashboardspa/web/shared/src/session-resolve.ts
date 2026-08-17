import type { DashboardSession } from './dashboard-sessions.js';

// The lossy role/assignee → session resolution (gascity-dashboard-3ax).
//
// This is the SINGLE implementation of the 4-step resolution used by run/bead
// session joins. PRD risk R2 calls this join out as load-bearing for the
// run-health engine — role-pool dispatch routinely fails it — so it must not
// drift between consumers.
//
// A bead's `assignee`, a sling `target`, and a role label are the same kind
// of value: a role / pool / alias the supervisor resolves to a concrete
// session at dispatch time. The dashboard re-derives that mapping from the
// session list because gc does not expose the resolved id.

/**
 * Resolves a role / assignee / target label to the concrete DashboardSession that
 * carries it, or null when none match. `active` sessions outrank non-active
 * (the caller wants the live agent); within a tier, first match wins
 * (deterministic given gc's recency-sorted iteration order).
 *
 * Pure: no IO, no mutation.
 */
export function resolveSessionForTarget(
  target: string,
  sessions: readonly DashboardSession[],
): DashboardSession | null {
  if (target.length === 0 || sessions.length === 0) return null;
  const active = sessions.filter((s) => s.state === 'active');
  return matchFirst(target, active) ?? matchFirst(target, sessions);
}

function matchFirst(
  target: string,
  sessions: readonly DashboardSession[],
): DashboardSession | null {
  for (const s of sessions) {
    if (matchesSessionTarget(s, target)) return s;
  }
  return null;
}

/**
 * True when `session` carries `target` in any of the four documented
 * positions: exact alias, pool, last-segment of alias (split on '/' '.'),
 * or last-segment of session_name (split on '__' '--').
 */
export function matchesSessionTarget(session: DashboardSession, target: string): boolean {
  if (session.alias === target) return true;
  if (session.pool === target) return true;
  if (session.alias !== undefined && lastSegment(session.alias, ['/', '.']) === target) {
    return true;
  }
  if (session.session_name !== undefined) {
    if (lastSegment(session.session_name, ['__', '--']) === target) return true;
  }
  return false;
}

/**
 * Substring AFTER the last occurrence of any separator in `seps`
 * (whole-token match for multi-char separators). Returns `value` unchanged
 * when no separator is present.
 */
export function lastSegment(value: string, seps: readonly string[]): string {
  let cut = -1;
  let sepLen = 0;
  for (const sep of seps) {
    const idx = value.lastIndexOf(sep);
    if (idx > cut) {
      cut = idx;
      sepLen = sep.length;
    }
  }
  if (cut < 0) return value;
  return value.slice(cut + sepLen);
}
