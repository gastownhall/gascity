import { describe, expect, it } from 'vitest';
import { formatDate, formatDateTime, formatHumanSize } from './format';

describe('formatDate', () => {
  it('formats local dates as YYYY-MM-DD', () => {
    expect(formatDate(new Date(2026, 4, 6, 7, 8))).toBe('2026-05-06');
  });

  it('uses the missing-data mark for invalid dates', () => {
    expect(formatDate('not a date')).toBe('·');
  });
});

describe('formatDateTime', () => {
  it('formats local datetimes as YYYY-MM-DD HH:MM', () => {
    expect(formatDateTime(new Date(2026, 4, 6, 7, 8))).toBe('2026-05-06 07:08');
  });
});

describe('formatHumanSize', () => {
  it('formats bytes through GB with named thresholds', () => {
    expect(formatHumanSize(1023)).toBe('1023 B');
    expect(formatHumanSize(1024)).toBe('1.0 KB');
    expect(formatHumanSize(1024 * 1024)).toBe('1.0 MB');
    expect(formatHumanSize(1024 * 1024 * 1024)).toBe('1.00 GB');
  });

  it('formats character counts with the same thresholds', () => {
    expect(formatHumanSize(32, 'chars')).toBe('32 chars');
    expect(formatHumanSize(1024, 'chars')).toBe('1.0 KB');
  });
});
