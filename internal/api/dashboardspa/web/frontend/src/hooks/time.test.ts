import { describe, expect, it } from 'vitest';
import { formatClockTime, formatRelative, formatShortDate } from './time';

const NOW = Date.parse('2026-05-20T12:00:00.000Z');

function isoAgo(ms: number): string {
  return new Date(NOW - ms).toISOString();
}

describe('formatRelative', () => {
  it('returns the interpunct sentinel for undefined input', () => {
    expect(formatRelative(undefined, NOW)).toBe('·');
  });

  it('returns the interpunct sentinel for null input', () => {
    // The signature accepts null and the guard handles it; this test
    // pins the behavior so a future tightening of the guard can't
    // silently start returning 'now' (Date.parse(null) NaN path) or
    // throwing.
    expect(formatRelative(null, NOW)).toBe('·');
  });

  it('returns the interpunct sentinel for unparseable input', () => {
    expect(formatRelative('not-a-date', NOW)).toBe('·');
    expect(formatRelative('', NOW)).toBe('·');
  });

  it('returns "now" for timestamps less than 5 seconds old', () => {
    expect(formatRelative(isoAgo(0), NOW)).toBe('now');
    expect(formatRelative(isoAgo(2_000), NOW)).toBe('now');
    expect(formatRelative(isoAgo(4_400), NOW)).toBe('now');
  });

  it('formats seconds for diffs in the [5s, 60s) range', () => {
    expect(formatRelative(isoAgo(5_000), NOW)).toBe('5s');
    expect(formatRelative(isoAgo(30_000), NOW)).toBe('30s');
    expect(formatRelative(isoAgo(59_000), NOW)).toBe('59s');
  });

  it('formats minutes for diffs in the [1m, 1h) range', () => {
    expect(formatRelative(isoAgo(60_000), NOW)).toBe('1m');
    expect(formatRelative(isoAgo(5 * 60_000), NOW)).toBe('5m');
    expect(formatRelative(isoAgo(59 * 60_000), NOW)).toBe('59m');
  });

  it('formats hours for diffs in the [1h, 24h) range', () => {
    expect(formatRelative(isoAgo(60 * 60_000), NOW)).toBe('1h');
    expect(formatRelative(isoAgo(6 * 60 * 60_000), NOW)).toBe('6h');
    expect(formatRelative(isoAgo(23 * 60 * 60_000), NOW)).toBe('23h');
  });

  it('formats days for diffs of 1 day or more', () => {
    expect(formatRelative(isoAgo(24 * 60 * 60_000), NOW)).toBe('1d');
    expect(formatRelative(isoAgo(7 * 24 * 60 * 60_000), NOW)).toBe('7d');
    expect(formatRelative(isoAgo(365 * 24 * 60 * 60_000), NOW)).toBe('365d');
  });

  it('clamps future timestamps to "now" (negative diff floored to 0)', () => {
    expect(formatRelative(new Date(NOW + 60_000).toISOString(), NOW)).toBe('now');
  });

  it('accepts a Date instance', () => {
    expect(formatRelative(new Date(NOW - 10_000), NOW)).toBe('10s');
  });

  it('accepts a number (epoch ms)', () => {
    expect(formatRelative(NOW - 10_000, NOW)).toBe('10s');
  });
});

describe('formatClockTime', () => {
  it('returns the interpunct sentinel for undefined input', () => {
    expect(formatClockTime(undefined)).toBe('·');
  });

  it('returns the interpunct sentinel for null input', () => {
    expect(formatClockTime(null)).toBe('·');
  });

  it('returns the interpunct sentinel for an unparseable string', () => {
    expect(formatClockTime('not-a-date')).toBe('·');
    expect(formatClockTime('')).toBe('·');
  });

  it('returns a zero-padded HH:MM string for a parseable ISO', () => {
    // The render uses the host's local TZ. Pin the assertion by computing
    // the expected output from the same Date so the test is portable.
    const iso = '2026-05-20T12:34:56Z';
    const d = new Date(iso);
    const expected = [d.getHours(), d.getMinutes()]
      .map((n) => String(n).padStart(2, '0'))
      .join(':');
    expect(formatClockTime(iso)).toBe(expected);
  });

  it('truncates rather than rounds the seconds component', () => {
    // A turn at HH:MM:59 should still render as HH:MM, not HH:MM+1.
    // Truncation keeps minute boundaries stable across the rendering tick.
    const iso = '2026-05-20T01:02:59Z';
    const d = new Date(iso);
    const expected = [d.getHours(), d.getMinutes()]
      .map((n) => String(n).padStart(2, '0'))
      .join(':');
    expect(formatClockTime(iso)).toBe(expected);
  });

  it('accepts a Date instance', () => {
    const d = new Date('2026-05-20T05:06:07Z');
    const expected = [d.getHours(), d.getMinutes()]
      .map((n) => String(n).padStart(2, '0'))
      .join(':');
    expect(formatClockTime(d)).toBe(expected);
  });

  it('accepts an epoch ms number', () => {
    const ms = Date.parse('2026-05-20T05:06:07Z');
    const d = new Date(ms);
    const expected = [d.getHours(), d.getMinutes()]
      .map((n) => String(n).padStart(2, '0'))
      .join(':');
    expect(formatClockTime(ms)).toBe(expected);
  });

  it('always emits a fixed HH:MM shape regardless of host TZ', () => {
    // Separate shape contract from the local-time pin: if the function
    // ever started emitting `H:MM`, `HH:MM:SS`, or a locale-formatted
    // string ('5:06 AM'), the parity tests above would still pass
    // (assertions derive from the same Date). This shape guard catches
    // any drift away from the documented contract.
    expect(formatClockTime('2026-05-20T05:06:07Z')).toMatch(/^\d{2}:\d{2}$/);
  });
});

describe('formatShortDate', () => {
  it('returns the interpunct sentinel for undefined input', () => {
    expect(formatShortDate(undefined)).toBe('·');
  });

  it('returns the interpunct sentinel for null input', () => {
    expect(formatShortDate(null)).toBe('·');
  });

  it('returns the interpunct sentinel for an unparseable string', () => {
    expect(formatShortDate('not-a-date')).toBe('·');
    expect(formatShortDate('')).toBe('·');
  });

  it('renders as `Mon DD, YYYY` in en-US form for a parseable ISO', () => {
    // The implementation pins the locale to en-US, so the output shape
    // is deterministic regardless of host system locale: a 3-letter
    // month, space, day, comma, year.
    expect(formatShortDate('2026-05-20T12:34:56Z')).toMatch(/^[A-Z][a-z]{2} \d{1,2}, \d{4}$/);
  });

  it('uses the local-time date (matches the host TZ)', () => {
    const iso = '2026-05-20T12:34:56Z';
    const d = new Date(iso);
    const result = formatShortDate(iso);
    // The day-of-month + year in the rendered string must match the
    // local day-of-month of the same instant.
    expect(result).toContain(`${d.getDate()}, ${d.getFullYear()}`);
  });

  it('accepts a Date instance', () => {
    expect(formatShortDate(new Date('2026-05-20T05:06:07Z'))).toMatch(
      /^[A-Z][a-z]{2} \d{1,2}, \d{4}$/,
    );
  });

  it('accepts an epoch ms number', () => {
    expect(formatShortDate(Date.parse('2026-05-20T05:06:07Z'))).toMatch(
      /^[A-Z][a-z]{2} \d{1,2}, \d{4}$/,
    );
  });
});
