import { act, cleanup, render, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { NowProvider, useNow } from './NowContext';

// The 1s tick is the only React-driven re-render path for the ambient home.
// useStaleness and useFaviconSignal both subscribe through it, so:
//   (1) the provider MUST own exactly one setInterval regardless of how many
//       consumers are mounted (CPU budget for an ambient tab);
//   (2) tick frequency MUST be configurable so tests can drive the clock
//       deterministically and the prod default stays at 1s.

describe('useNow / NowProvider', () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
    cleanup();
  });

  it('returns the initial wall clock and ticks at the provider interval', () => {
    vi.setSystemTime(new Date('2026-05-29T20:00:00.000Z'));
    const { result } = renderHook(() => useNow(), {
      wrapper: ({ children }) => <NowProvider intervalMs={1000}>{children}</NowProvider>,
    });

    const t0 = result.current;
    expect(t0).toBe(Date.parse('2026-05-29T20:00:00.000Z'));

    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(result.current).toBe(Date.parse('2026-05-29T20:00:01.000Z'));

    act(() => {
      vi.advanceTimersByTime(5_000);
    });
    expect(result.current).toBe(Date.parse('2026-05-29T20:00:06.000Z'));
  });

  it('throws a helpful error when useNow is called outside a NowProvider', () => {
    // The helpful error catches the most likely setup bug for any future
    // consumer: forgetting to wrap the route tree in NowProvider.
    const consoleErrorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => renderHook(() => useNow())).toThrow(/useNow.*NowProvider/);
    consoleErrorSpy.mockRestore();
  });

  it('runs exactly one setInterval regardless of consumer count', () => {
    // The whole point of the context is to amortize a single tick across
    // every staleness/favicon consumer. If a future refactor accidentally
    // moves the interval into the hook, every consumer pays its own tax —
    // this test catches that regression.
    const setIntervalSpy = vi.spyOn(window, 'setInterval');

    function Consumer() {
      useNow();
      return null;
    }

    render(
      <NowProvider intervalMs={1000}>
        <Consumer />
        <Consumer />
        <Consumer />
        <Consumer />
      </NowProvider>,
    );

    expect(setIntervalSpy).toHaveBeenCalledTimes(1);
    setIntervalSpy.mockRestore();
  });

  it('clears its interval when the provider unmounts', () => {
    const clearIntervalSpy = vi.spyOn(window, 'clearInterval');
    const { unmount } = render(
      <NowProvider intervalMs={1000}>
        <span>x</span>
      </NowProvider>,
    );
    unmount();
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });
});
