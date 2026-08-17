/**
 * Time formatting helpers shared across routes.
 *
 * `formatRelative` produces compact human-readable durations relative to a
 * caller-supplied `now`. Passing `now` in (rather than calling `Date.now()`
 * internally) lets the app-level `NowProvider` drive one shared tick so every
 * relative timestamp in a render is consistent with every other.
 *
 * Output grammar (diffs are rounded to the nearest second before bucketing,
 * so e.g. 4.5s renders as '5s', not 'now'):
 *   '·'    — invalid / missing / null input (interpunct sentinel; see DESIGN.md)
 *   'now'  — rounded diff < 5 seconds (future timestamps clamp to 0)
 *   'Ns'   — rounded diff in [5s, 60s)
 *   'Nm'   — rounded diff in [1m, 1h)
 *   'Nh'   — rounded diff in [1h, 24h)
 *   'Nd'   — rounded diff ≥ 24h
 */
/**
 * Resolve a string|number|Date into epoch milliseconds. Callers must
 * pre-filter null/undefined/'' — this helper assumes a real value.
 */
function toEpochMs(ts: string | number | Date): number {
  return ts instanceof Date ? ts.getTime() : typeof ts === 'number' ? ts : Date.parse(ts);
}

export function formatRelative(ts: string | number | Date | undefined | null, now: number): string {
  if (ts === undefined || ts === null || ts === '') return '·';

  const ms = toEpochMs(ts);
  if (!Number.isFinite(ms)) return '·';

  const diffSec = Math.max(0, Math.round((now - ms) / 1_000));
  if (diffSec < 5) return 'now';
  if (diffSec < 60) return `${diffSec}s`;
  if (diffSec < 3_600) return `${Math.round(diffSec / 60)}m`;
  if (diffSec < 86_400) return `${Math.round(diffSec / 3_600)}h`;
  return `${Math.round(diffSec / 86_400)}d`;
}

/**
 * Render a timestamp as a 24-hour `HH:MM` local clock string. Returns
 * the `·` interpunct sentinel for missing or unparseable input (matches
 * the `formatRelative` sentinel convention; see DESIGN.md).
 *
 * Seconds are truncated, not rounded — a turn at `HH:MM:59` renders as
 * `HH:MM`, keeping minute boundaries stable across the rendering tick.
 */
export function formatClockTime(ts: string | number | Date | undefined | null): string {
  if (ts === undefined || ts === null || ts === '') return '·';

  const ms = toEpochMs(ts);
  if (!Number.isFinite(ms)) return '·';

  const d = new Date(ms);
  const hh = String(d.getHours()).padStart(2, '0');
  const mm = String(d.getMinutes()).padStart(2, '0');
  return `${hh}:${mm}`;
}

/**
 * Render a timestamp as a short date string in `Mon DD, YYYY` form
 * (e.g. `May 20, 2026`). Returns the `·` interpunct sentinel for
 * missing or unparseable input.
 *
 * The locale is pinned to `en-US` deliberately: the dashboard's UX copy
 * is English-only, and a deterministic output keeps the rendered date
 * consistent regardless of the host system's locale.
 *
 * Used as a once-per-modal "dateline" in the SessionPeek modal so the
 * operator knows the calendar context without per-turn date duplication.
 */
export function formatShortDate(ts: string | number | Date | undefined | null): string {
  if (ts === undefined || ts === null || ts === '') return '·';

  const ms = toEpochMs(ts);
  if (!Number.isFinite(ms)) return '·';

  return new Date(ms).toLocaleDateString('en-US', {
    month: 'short',
    day: 'numeric',
    year: 'numeric',
  });
}
