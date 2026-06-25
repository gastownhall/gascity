import type { DashboardSession } from './dashboard-sessions.js';

/**
 * Models known to run with the 1M-token extended-context beta header
 * in this deployment. Add new generations as they land.
 */
export const TRUE_CONTEXT_WINDOWS: Readonly<Record<string, number>> = {
  'claude-opus-4-7': 1_000_000,
  'claude-opus-4-8': 1_000_000,
  'claude-sonnet-4-5': 1_000_000,
  'claude-sonnet-4-6': 1_000_000,
};

/**
 * Returns the session's context usage as a percentage of its TRUE
 * context window (not gc's hardcoded denominator). Returns `undefined`
 * when no usable signal is available; returns the raw gc value
 * unchanged when the model is unknown or `context_window` is missing
 * (fail-open so we don't guess).
 *
 * Always returns an integer in [0, 100].
 */
export function effectiveContextPct(
  session: Pick<DashboardSession, 'context_pct' | 'context_window' | 'model'>,
): number | undefined {
  const pct = session.context_pct;
  if (typeof pct !== 'number' || !Number.isFinite(pct)) return undefined;

  const gcWindow = session.context_window;
  const trueWindow = session.model !== undefined ? TRUE_CONTEXT_WINDOWS[session.model] : undefined;

  if (
    typeof gcWindow !== 'number' ||
    typeof trueWindow !== 'number' ||
    gcWindow <= 0 ||
    trueWindow <= 0
  ) {
    // No scale factor available. Fail open to gc's value rather than
    // invent one. Still clamp to [0, 100] for display sanity.
    return clampPct(pct);
  }

  return clampPct(Math.round((pct * gcWindow) / trueWindow));
}

function clampPct(n: number): number {
  if (n < 0) return 0;
  if (n > 100) return 100;
  return n;
}
