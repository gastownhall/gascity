import { describe, expect, it } from 'vitest';
import { formatBytesSI, formatDate, formatDateTime, formatHumanSize } from './format';

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

describe('formatBytesSI', () => {
  it('formats bytes through GB with decimal (SI, 1000-based) thresholds', () => {
    expect(formatBytesSI(512)).toBe('512 B');
    expect(formatBytesSI(999)).toBe('999 B');
    expect(formatBytesSI(1_000)).toBe('1 KB');
    expect(formatBytesSI(1_500)).toBe('2 KB');
    expect(formatBytesSI(1_000_000)).toBe('1 MB');
    expect(formatBytesSI(48_000_000)).toBe('48 MB');
    expect(formatBytesSI(1_000_000_000)).toBe('1.0 GB');
    expect(formatBytesSI(2_200_000_000)).toBe('2.2 GB');
  });

  it('uses SI (1000) boundaries, distinct from formatHumanSize binary (1024)', () => {
    // 1_048_576 is exactly 1 binary MiB but 1.05 SI MB.
    expect(formatBytesSI(1_048_576)).toBe('1 MB');
    expect(formatHumanSize(1_048_576)).toBe('1.0 MB');
  });
});
