import { act, renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  useAbortableVisibleRefresh,
  type AbortableVisibleRefreshState,
} from './useAbortableVisibleRefresh';

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (error: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

async function flush(): Promise<void> {
  await act(async () => {
    await Promise.resolve();
  });
}

describe('useAbortableVisibleRefresh', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('ignores stale responses after a newer visible tick starts', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const first = deferred<string>();
    const second = deferred<string>();
    const calls: AbortSignal[] = [];
    const load = vi.fn((signal: AbortSignal) => {
      calls.push(signal);
      return calls.length === 1 ? first.promise : second.promise;
    });

    const { result } = renderHook(() =>
      useAbortableVisibleRefresh({ enabled: true, intervalMs: 1_000, load }),
    );

    expect(result.current.status).toBe('loading');
    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(load).toHaveBeenCalledTimes(2);
    expect(calls[0]?.aborted).toBe(true);

    await act(async () => {
      first.resolve('stale');
      await first.promise;
    });
    expect(result.current.status).toBe('loading');

    await act(async () => {
      second.resolve('fresh');
      await second.promise;
    });
    expect(result.current).toEqual({
      status: 'ready',
      data: 'fresh',
      refreshing: false,
      error: '',
    } satisfies AbortableVisibleRefreshState<string>);
  });

  it('keeps existing data while a refresh failure is reported', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const first = deferred<string>();
    const second = deferred<string>();
    const load = vi.fn(() => (load.mock.calls.length === 1 ? first.promise : second.promise));

    const { result } = renderHook(() =>
      useAbortableVisibleRefresh({ enabled: true, intervalMs: 1_000, load }),
    );

    await act(async () => {
      first.resolve('ready');
      await first.promise;
    });
    expect(result.current.status).toBe('ready');

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    await act(async () => {
      second.reject(new Error('network down'));
      try {
        await second.promise;
      } catch {
        // Promise rejection is the hook input under test.
      }
    });
    await flush();

    expect(result.current).toEqual({
      status: 'ready',
      data: 'ready',
      refreshing: false,
      error: 'network down',
    } satisfies AbortableVisibleRefreshState<string>);
  });

  it('backs off after load failures and resets after a successful retry', async () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const first = deferred<string>();
    const second = deferred<string>();
    const load = vi.fn(() => (load.mock.calls.length === 1 ? first.promise : second.promise));

    const { result } = renderHook(() =>
      useAbortableVisibleRefresh({
        enabled: true,
        intervalMs: 1_000,
        initialBackoffMs: 2_000,
        maxBackoffMs: 2_000,
        load,
      }),
    );

    await act(async () => {
      first.reject(new Error('network down'));
      try {
        await first.promise;
      } catch {
        // Promise rejection is the hook input under test.
      }
    });
    await flush();
    expect(result.current).toEqual({
      status: 'failed',
      error: 'network down',
    } satisfies AbortableVisibleRefreshState<string>);

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(load).toHaveBeenCalledTimes(1);

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(load).toHaveBeenCalledTimes(2);

    await act(async () => {
      second.resolve('ready');
      await second.promise;
    });
    expect(result.current).toEqual({
      status: 'ready',
      data: 'ready',
      refreshing: false,
      error: '',
    } satisfies AbortableVisibleRefreshState<string>);
  });

  it('does not tick while hidden and aborts on unmount', async () => {
    vi.useFakeTimers();
    const hiddenSpy = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true);
    const first = deferred<string>();
    const calls: AbortSignal[] = [];
    const load = vi.fn((signal: AbortSignal) => {
      calls.push(signal);
      return first.promise;
    });

    const { unmount } = renderHook(() =>
      useAbortableVisibleRefresh({ enabled: true, intervalMs: 1_000, load }),
    );

    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(load).toHaveBeenCalledTimes(1);

    hiddenSpy.mockReturnValue(false);
    await act(async () => {
      vi.advanceTimersByTime(1_000);
    });
    expect(load).toHaveBeenCalledTimes(2);

    unmount();
    expect(calls[1]?.aborted).toBe(true);
  });
});
