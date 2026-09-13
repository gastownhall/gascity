import { describe, expect, it } from 'vitest';
import { extractTurnTimestamp } from './SessionPeek';

describe('extractTurnTimestamp', () => {
  it('extracts a bare ISO datetime at the start of the text', () => {
    expect(extractTurnTimestamp('2026-05-20T10:53:10\nrest of turn')).toBe('2026-05-20T10:53:10');
  });

  it('extracts an ISO datetime after a prompt prefix separator', () => {
    const text = '[ds-research] /home/ds/gascity/polecat-4 • 2026-05-20T10:53:10\n\nRun `gc prime`';
    expect(extractTurnTimestamp(text)).toBe('2026-05-20T10:53:10');
  });

  it('preserves the Z suffix when present', () => {
    expect(extractTurnTimestamp('captured 2026-05-20T10:53:10Z by gc')).toBe(
      '2026-05-20T10:53:10Z',
    );
  });

  it('preserves a positive timezone offset', () => {
    expect(extractTurnTimestamp('at 2026-05-20T10:53:10+09:00 in Tokyo')).toBe(
      '2026-05-20T10:53:10+09:00',
    );
  });

  it('preserves a negative timezone offset', () => {
    expect(extractTurnTimestamp('at 2026-05-20T10:53:10-04:00 EDT')).toBe(
      '2026-05-20T10:53:10-04:00',
    );
  });

  it('preserves fractional seconds', () => {
    expect(extractTurnTimestamp('logged 2026-05-20T10:53:10.123456 then')).toBe(
      '2026-05-20T10:53:10.123456',
    );
  });

  it('preserves fractional seconds + Z', () => {
    expect(extractTurnTimestamp('logged 2026-05-20T10:53:10.123Z then')).toBe(
      '2026-05-20T10:53:10.123Z',
    );
  });

  it('returns null when no ISO datetime is present', () => {
    expect(extractTurnTimestamp('hello world, no timestamp here')).toBeNull();
  });

  it('returns null for date-only without time component', () => {
    // Anchoring requires a `T` + time, so a bare `2026-05-20` is ambiguous
    // and not considered a per-message timestamp.
    expect(extractTurnTimestamp('on 2026-05-20 we shipped')).toBeNull();
  });

  it('returns null for time-only without date', () => {
    expect(extractTurnTimestamp('at 10:53:10 in the morning')).toBeNull();
  });

  it('returns the FIRST ISO when multiple are present', () => {
    const text = 'first 2026-05-20T10:53:10 then 2026-05-21T08:00:00 later';
    expect(extractTurnTimestamp(text)).toBe('2026-05-20T10:53:10');
  });

  it('returns null for ISO appearing past the leading search window', () => {
    // Search is bounded to keep large turn bodies cheap. Anything past the
    // first 512 chars is treated as text content, not a header timestamp.
    // Pad with 511 word chars + a space so the timestamp at position 512
    // has a legal word boundary in front of it — the ONLY reason for the
    // null result is the search window cutoff, not the `\b` requirement.
    const padding = 'x'.repeat(511) + ' ';
    const text = `${padding}2026-05-20T10:53:10 here`;
    expect(extractTurnTimestamp(text)).toBeNull();
  });

  it('preserves the offset when the timestamp is followed by a word character', () => {
    // Regression: a previous version used a trailing `\b` which would
    // backtrack and silently drop the offset, returning the timestamp as
    // a naive local-time string. Date.parse would then mis-localise.
    expect(extractTurnTimestamp('boundary 2026-05-20T10:53:10+09:00X tail')).toBe(
      '2026-05-20T10:53:10+09:00',
    );
  });

  it('returns null for empty string', () => {
    expect(extractTurnTimestamp('')).toBeNull();
  });

  it('returns null when only HH:MM is present (seconds component required)', () => {
    // Defends against the regex accidentally matching `2026-05-20T10:53` (no
    // seconds). We require HH:MM:SS — partial times are not enough.
    expect(extractTurnTimestamp('at 2026-05-20T10:53\nthen')).toBeNull();
  });
});
