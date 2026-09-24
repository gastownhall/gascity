import type { DashboardSession } from './dashboard-sessions.js';

/**
 * 1M-token context markers, mirrored from the Go source of truth
 * `internal/modelwindow.millionMarkers`. A model ID resolves to the 1M window
 * when any marker appears in it as a case-insensitive substring (see
 * {@link trueContextWindowForModel}), exactly as Go does with
 * `strings.Contains(strings.ToLower(model), marker)`.
 *
 * Using Go's short markers — `opus-5`, `sonnet-5`, `fable`, the explicit
 * `[1m]` launch suffix — rather than full model IDs is what keeps the two
 * resolvers in agreement. A dated, upper-cased, or vendor-prefixed variant of
 * a listed generation (`claude-opus-5-20260724`, `CLAUDE-OPUS-5`,
 * `us.anthropic.claude-opus-5`) resolves to the same window as the bare marker
 * instead of silently failing open to gc's unscaled 200k denominator, while a
 * Go-Default family such as Sonnet 4.5 (`claude-sonnet-4-5-20250929`) or Opus
 * 4.5 (`claude-opus-4-5`) carries no marker and is left to fail open exactly as
 * Go leaves it at 200k. The parity test in `index.test.ts` reads
 * `modelwindow.millionMarkers` and fails if these keys drift from it.
 *
 * Every marker maps to the same 1M window, so the first-substring-hit order in
 * {@link trueContextWindowForModel} is unambiguous; if a non-1M window is ever
 * added here, revisit that ordering (Go resolves markers before families and
 * longest-match-first for exactly this reason).
 */
export const TRUE_CONTEXT_WINDOWS: Readonly<Record<string, number>> = {
  '[1m]': 1_000_000,
  fable: 1_000_000,
  mythos: 1_000_000,
  'opus-4-6': 1_000_000,
  'opus-4-7': 1_000_000,
  'opus-4-8': 1_000_000,
  'opus-5': 1_000_000,
  'sonnet-4-6': 1_000_000,
  'sonnet-5': 1_000_000,
};

/**
 * Resolves a model ID to its true context window in tokens, or `undefined`
 * when the model is not a known extended-context model.
 *
 * Matching mirrors the Go resolver `internal/modelwindow.Window`: the model ID
 * is lower-cased and the first {@link TRUE_CONTEXT_WINDOWS} marker that appears
 * as a substring wins. This covers dated variants (`claude-opus-5-20260724`),
 * vendor-prefixed IDs (`us.anthropic.claude-opus-5`), and case variants
 * (`CLAUDE-OPUS-5`) that an exact full-ID lookup would miss, while leaving
 * Go-Default families (`claude-sonnet-4-5`, `claude-opus-4-5`) unmatched exactly
 * as Go does. Every marker maps to the same 1M window today, so the first
 * substring hit is unambiguous.
 */
export function trueContextWindowForModel(model: string | undefined): number | undefined {
  if (model === undefined) return undefined;
  const lower = model.toLowerCase();
  for (const [marker, window] of Object.entries(TRUE_CONTEXT_WINDOWS)) {
    // Markers are lower-case by construction, matching modelwindow's lower-case
    // markers; compare against the lower-cased model ID.
    if (lower.includes(marker)) return window;
  }
  return undefined;
}

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
  const trueWindow = trueContextWindowForModel(session.model);

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
