// Shared numeric formatters for the cockpit instruments. Instruments show
// compact magnitudes (389.1K, 46.6M) with tabular figures; precision follows
// the mockup: one decimal above the unit boundary, none below.

/** fmtCompact renders 1234 → "1.2K", 46_640_000 → "46.6M", 137 → "137". */
export function fmtCompact(n: number): string {
  const abs = Math.abs(n);
  if (abs >= 1_000_000_000) return `${(n / 1_000_000_000).toFixed(1)}B`;
  if (abs >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (abs >= 1_000) return `${(n / 1_000).toFixed(1)}K`;
  return `${Math.round(n)}`;
}

/** fmtCost renders a USD estimate: "$497.61", "$0.25", "$12.40/hr" is the caller's suffix. */
export function fmtCost(usd: number): string {
  return `$${usd.toFixed(2)}`;
}

/** fmtRate renders a per-second token rate: 33_200 → "33.2K/s". */
export function fmtRate(perSec: number): string {
  return `${fmtCompact(perSec)}/s`;
}

/**
 * agoStr renders a compact age for the live chip: <2s → "now", then "5s",
 * "3m", "2h"; capped at days.
 */
export function agoStr(ms: number): string {
  const s = Math.floor(ms / 1000);
  if (s < 2) return 'now';
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.floor(h / 24)}d`;
}

/** fmtCount renders a nullable count as its number, or an em-dash when null. */
export function fmtCount(value: number | null): string {
  return value === null ? '—' : String(value);
}

/** connLabel maps a gc event-stream connection state to its live-chip word. */
export function connLabel(state: string): string {
  switch (state) {
    case 'open':
      return 'live';
    case 'connecting':
      return 'connecting';
    case 'degraded':
      return 'degraded';
    default:
      return 'offline';
  }
}
