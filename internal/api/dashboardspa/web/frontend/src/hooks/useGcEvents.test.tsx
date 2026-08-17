import { act, cleanup, renderHook } from '@testing-library/react';
import { GC_EVENT_PREFIX } from 'gas-city-dashboard-shared';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { reportClientError } from '../lib/clientErrorReporting';
import { useGcEventRefresh } from './useGcEvents';

const eventSources: FakeEventSource[] = [];
const mockReportClientError = reportClientError as Mock;

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

describe('useGcEventRefresh', () => {
  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', FakeEventSource);
    mockReportClientError.mockClear();
  });

  afterEach(() => {
    cleanup();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('fires once for a matching named gc event', () => {
    const onMatch = vi.fn();
    const { result } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch));

    act(() => eventSources[0]?.open());
    expect(result.current).toBe('open');

    act(() => eventSources[0]?.emitNamed('event', JSON.stringify({ type: 'bead.updated' })));

    expect(onMatch).toHaveBeenCalledTimes(1);
    expect(result.current).toBe('open');
  });

  it('opens the direct supervisor city event stream instead of the dashboard stream mirror', () => {
    renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));

    expect(String(eventSources[0]?.url)).toBe(
      `${window.location.origin}/v0/city/test-city/events/stream`,
    );
  });

  it('lets callers ignore prefix-matching events outside their projection', () => {
    const onMatch = vi.fn();
    const { result } = renderHook(() =>
      useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch, {
        matches: (event) => event.run?.run_id === 'current-run',
      }),
    );

    act(() => eventSources[0]?.open());
    expect(result.current).toBe('open');

    act(() =>
      eventSources[0]?.emitNamed(
        'event',
        JSON.stringify({
          type: 'bead.updated',
          run: { run_id: 'other-run' },
        }),
      ),
    );
    expect(onMatch).not.toHaveBeenCalled();

    act(() =>
      eventSources[0]?.emitNamed(
        'event',
        JSON.stringify({
          type: 'bead.updated',
          run: { run_id: 'current-run' },
        }),
      ),
    );
    expect(onMatch).toHaveBeenCalledTimes(1);
  });

  it('surfaces malformed event payloads as degraded instead of silently swallowing them', () => {
    const onMatch = vi.fn();
    const { result } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch));

    act(() => eventSources[0]?.open());
    act(() => eventSources[0]?.emitNamed('event', 'not json'));

    expect(onMatch).not.toHaveBeenCalled();
    expect(result.current).toBe('degraded');
    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'gc-events',
      operation: 'parse event',
      message: 'Malformed gc event payload: invalid JSON.',
    });
  });

  it('reports closed when EventSource is unavailable', () => {
    vi.unstubAllGlobals();
    vi.stubGlobal('EventSource', undefined);

    const { result } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));

    expect(result.current).toBe('closed');
  });

  it('does not leave a quiet supervisor stream showing connecting indefinitely', () => {
    vi.useFakeTimers();

    const { result } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));

    expect(result.current).toBe('connecting');
    act(() => {
      vi.advanceTimersByTime(1_999);
    });
    expect(result.current).toBe('connecting');
    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(result.current).toBe('open');
  });

  it('does not open a city event stream when no prefixes are requested', () => {
    const { result } = renderHook(() => useGcEventRefresh([], vi.fn()));

    expect(result.current).toBe('closed');
    expect(eventSources).toHaveLength(0);
  });

  describe('reconnect backoff', () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    it('reconnects after an exponentially growing, 30s-capped delay', () => {
      const { result } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));
      expect(eventSources).toHaveLength(1);

      // First error schedules a reconnect using the initial 1s delay.
      act(() => eventSources[0]?.error());
      expect(result.current).toBe('closed');
      expect(eventSources).toHaveLength(1);

      // Just before the 1s delay elapses, no reconnect has happened.
      act(() => {
        vi.advanceTimersByTime(999);
      });
      expect(eventSources).toHaveLength(1);
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(eventSources).toHaveLength(2);

      // The delay doubled to 2s for the next reconnect.
      act(() => eventSources[1]?.error());
      act(() => {
        vi.advanceTimersByTime(1_999);
      });
      expect(eventSources).toHaveLength(2);
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(eventSources).toHaveLength(3);

      // Drive past the cap: each subsequent retry doubles (4s, 8s, 16s) and
      // then clamps at 30s no matter how many failures accrue.
      const driveOneRetry = (delayMs: number) => {
        const idx = eventSources.length - 1;
        act(() => eventSources[idx]?.error());
        act(() => {
          vi.advanceTimersByTime(delayMs - 1);
        });
        expect(eventSources).toHaveLength(idx + 1);
        act(() => {
          vi.advanceTimersByTime(1);
        });
        expect(eventSources).toHaveLength(idx + 2);
      };
      driveOneRetry(4_000);
      driveOneRetry(8_000);
      driveOneRetry(16_000);
      driveOneRetry(30_000);
      // Already at the cap; the next delay stays clamped at 30s, not 60s.
      driveOneRetry(30_000);
    });

    it('resets the backoff delay to 1s once a reconnect opens', () => {
      renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));

      // Two failures grow the delay to 4s for the third attempt.
      act(() => eventSources[0]?.error());
      act(() => {
        vi.advanceTimersByTime(1_000);
      });
      act(() => eventSources[1]?.error());
      act(() => {
        vi.advanceTimersByTime(2_000);
      });
      expect(eventSources).toHaveLength(3);

      // A successful open resets the backoff window.
      act(() => eventSources[2]?.open());
      act(() => eventSources[2]?.error());
      // The delay is back to the initial 1s, not the grown 8s.
      act(() => {
        vi.advanceTimersByTime(999);
      });
      expect(eventSources).toHaveLength(3);
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(eventSources).toHaveLength(4);
    });

    it('cancels a pending reconnect timer on unmount', () => {
      const { unmount } = renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], vi.fn()));
      act(() => eventSources[0]?.error());
      unmount();
      act(() => {
        vi.advanceTimersByTime(60_000);
      });
      // No reconnect after teardown.
      expect(eventSources).toHaveLength(1);
    });
  });

  describe('coalesce throttle window', () => {
    beforeEach(() => {
      vi.useFakeTimers();
    });

    it('fires immediately for the first match, then coalesces a burst into one trailing fire', () => {
      const onMatch = vi.fn();
      renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch));
      act(() => eventSources[0]?.open());

      const emit = () =>
        act(() => eventSources[0]?.emitNamed('event', JSON.stringify({ type: 'bead.updated' })));

      // Leading edge: the first matching event fires onMatch right away.
      emit();
      expect(onMatch).toHaveBeenCalledTimes(1);

      // A burst inside the 2.5s window does not fire again immediately;
      // it schedules a single trailing fire at the window edge.
      emit();
      emit();
      emit();
      expect(onMatch).toHaveBeenCalledTimes(1);

      // Before the window closes, still just the leading fire.
      act(() => {
        vi.advanceTimersByTime(2_499);
      });
      expect(onMatch).toHaveBeenCalledTimes(1);

      // At the window edge the single trailing fire lands: burst -> 2 total.
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(onMatch).toHaveBeenCalledTimes(2);
    });

    it('fires immediately again once the window has elapsed', () => {
      const onMatch = vi.fn();
      renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch));
      act(() => eventSources[0]?.open());

      const emit = () =>
        act(() => eventSources[0]?.emitNamed('event', JSON.stringify({ type: 'bead.updated' })));

      emit();
      expect(onMatch).toHaveBeenCalledTimes(1);

      // After the full window elapses with no events, the next match is
      // outside the window and fires on its own leading edge.
      act(() => {
        vi.advanceTimersByTime(2_500);
      });
      emit();
      expect(onMatch).toHaveBeenCalledTimes(2);
    });

    it('honors a caller-supplied coalesceMs to widen the trailing window', () => {
      const onMatch = vi.fn();
      renderHook(() => useGcEventRefresh([GC_EVENT_PREFIX.bead], onMatch, { coalesceMs: 10_000 }));
      act(() => eventSources[0]?.open());

      const emit = () =>
        act(() => eventSources[0]?.emitNamed('event', JSON.stringify({ type: 'bead.updated' })));

      // Leading edge still fires the first match immediately.
      emit();
      expect(onMatch).toHaveBeenCalledTimes(1);

      // A burst that would have flushed under the default 2.5s window is held
      // back: nothing extra fires until the wider 10s window closes.
      emit();
      emit();
      act(() => {
        vi.advanceTimersByTime(9_999);
      });
      expect(onMatch).toHaveBeenCalledTimes(1);

      // At the widened window edge the single trailing fire lands.
      act(() => {
        vi.advanceTimersByTime(1);
      });
      expect(onMatch).toHaveBeenCalledTimes(2);
    });
  });
});

class FakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = FakeEventSource.CONNECTING;
  private readonly listeners = new Map<string, Set<EventListener>>();

  constructor(readonly url: string | URL) {
    eventSources.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.readyState = FakeEventSource.CLOSED;
  }

  open(): void {
    this.readyState = FakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  error(): void {
    this.onerror?.(new Event('error'));
  }

  emitNamed(type: string, data: string): void {
    const event = new MessageEvent<string>(type, { data });
    this.listeners.get(type)?.forEach((listener) => listener(event));
    if (type === 'message') this.onmessage?.(event);
  }
}
