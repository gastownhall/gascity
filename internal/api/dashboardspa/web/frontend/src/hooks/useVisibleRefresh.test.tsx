import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useVisibleRefresh } from './useVisibleRefresh';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

describe('useVisibleRefresh', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('backs off after rejected refresh promises', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const first = deferred<void>();
    const refresh = vi.fn(() => first.promise);
    const onError = vi.fn();

    renderHook(() =>
      useVisibleRefresh(refresh, 1_000, {
        initialBackoffMs: 2_000,
        maxBackoffMs: 2_000,
        onError,
      }),
    );

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(refresh).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.reject(new Error('network down'));
      try {
        await first.promise;
      } catch {
        // Rejection drives the hook path under test.
      }
    });
    expect(onError).toHaveBeenCalledOnce();

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(refresh).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('does not overlap refresh calls while one is still in flight', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const first = deferred<void>();
    const refresh = vi.fn(() => first.promise);

    renderHook(() => useVisibleRefresh(refresh, 1_000));

    await act(async () => {
      vi.advanceTimersByTime(1_000);
      vi.advanceTimersByTime(1_000);
    });

    expect(refresh).toHaveBeenCalledTimes(1);

    await act(async () => {
      first.resolve();
      await first.promise;
    });

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });

    expect(refresh).toHaveBeenCalledTimes(2);
  });

  it('does not run an initial refresh before the first visible interval tick', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const refresh = vi.fn(() => Promise.resolve());

    renderHook(() => useVisibleRefresh(refresh, 1_000));

    expect(refresh).not.toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(999);
    });
    expect(refresh).not.toHaveBeenCalled();
    await act(async () => {
      vi.advanceTimersByTime(1);
    });
    expect(refresh).toHaveBeenCalledTimes(1);
  });
});
