import { renderHook } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { useVisibleInterval } from './useVisibleInterval';

describe('useVisibleInterval', () => {
  afterEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
  });

  it('runs the latest callback only while the document is visible', () => {
    vi.useFakeTimers();
    const first = vi.fn();
    const second = vi.fn();
    const hiddenSpy = vi.spyOn(document, 'hidden', 'get');
    hiddenSpy.mockReturnValue(false);

    const { rerender, unmount } = renderHook(
      ({ callback }) => useVisibleInterval(callback, 1_000),
      { initialProps: { callback: first } },
    );

    vi.advanceTimersByTime(1_000);
    expect(first).toHaveBeenCalledTimes(1);

    rerender({ callback: second });
    vi.advanceTimersByTime(1_000);
    expect(first).toHaveBeenCalledTimes(1);
    expect(second).toHaveBeenCalledTimes(1);

    hiddenSpy.mockReturnValue(true);
    vi.advanceTimersByTime(1_000);
    expect(second).toHaveBeenCalledTimes(1);

    unmount();
    hiddenSpy.mockReturnValue(false);
    vi.advanceTimersByTime(1_000);
    expect(second).toHaveBeenCalledTimes(1);
  });

  it('does not schedule an interval while disabled', () => {
    vi.useFakeTimers();
    const callback = vi.fn();
    const { rerender } = renderHook(({ enabled }) => useVisibleInterval(callback, 1_000, enabled), {
      initialProps: { enabled: false },
    });

    vi.advanceTimersByTime(1_000);
    expect(callback).not.toHaveBeenCalled();

    rerender({ enabled: true });
    vi.advanceTimersByTime(1_000);
    expect(callback).toHaveBeenCalledTimes(1);
  });

  it('keeps interval callbacks simple and does not apply async-refresh backoff', () => {
    vi.useFakeTimers();
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false);
    const callback = vi.fn();

    renderHook(() => useVisibleInterval(callback, 1_000));

    vi.advanceTimersByTime(1_000);
    expect(callback).toHaveBeenCalledTimes(1);
    vi.advanceTimersByTime(1_000);
    expect(callback).toHaveBeenCalledTimes(2);
    vi.advanceTimersByTime(1_000);
    expect(callback).toHaveBeenCalledTimes(3);
  });
});
