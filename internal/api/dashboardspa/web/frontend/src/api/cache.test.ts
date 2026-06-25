import { afterEach, describe, expect, it } from 'vitest';
import { getCached, getCachedFetchedAt, invalidate, invalidateKey, setCached } from './cache';

afterEach(() => invalidate(''));

describe('api cache fetch timestamp', () => {
  it('stamps an ISO fetch timestamp on write and exposes it alongside the value', () => {
    expect(getCachedFetchedAt('k')).toBeUndefined();

    setCached('k', { n: 1 });

    expect(getCached('k')).toEqual({ n: 1 });
    const fetchedAt = getCachedFetchedAt('k');
    expect(fetchedAt).toEqual(expect.any(String));
    // A round-trippable ISO instant.
    expect(new Date(fetchedAt!).toISOString()).toBe(fetchedAt);
  });

  it('advances the timestamp on each successive write', () => {
    setCached('k', 'first');
    const firstAt = getCachedFetchedAt('k');
    setCached('k', 'second');
    const secondAt = getCachedFetchedAt('k');

    expect(getCached('k')).toBe('second');
    expect(secondAt).toEqual(expect.any(String));
    expect(Date.parse(secondAt!)).toBeGreaterThanOrEqual(Date.parse(firstAt!));
  });

  it('drops the timestamp when the entry is invalidated', () => {
    setCached('k', 'v');
    invalidateKey('k');

    expect(getCached('k')).toBeUndefined();
    expect(getCachedFetchedAt('k')).toBeUndefined();
  });
});
