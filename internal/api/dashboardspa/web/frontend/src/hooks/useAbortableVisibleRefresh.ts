import { useEffect, useRef, useState } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';

export type AbortableVisibleRefreshState<T> =
  | { status: 'idle' }
  | { status: 'loading' }
  | { status: 'failed'; error: string }
  | { status: 'ready'; data: T; refreshing: boolean; error: string };

export interface UseAbortableVisibleRefreshOptions<T> {
  enabled: boolean;
  intervalMs: number;
  load: (signal: AbortSignal) => Promise<T>;
  formatError?: (err: unknown) => string;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
}

const DEFAULT_INITIAL_BACKOFF_MS = 2_000;
const DEFAULT_MAX_BACKOFF_MS = 60_000;

export function useAbortableVisibleRefresh<T>({
  enabled,
  intervalMs,
  load,
  formatError,
  initialBackoffMs = DEFAULT_INITIAL_BACKOFF_MS,
  maxBackoffMs = DEFAULT_MAX_BACKOFF_MS,
}: UseAbortableVisibleRefreshOptions<T>): AbortableVisibleRefreshState<T> {
  const [state, setState] = useState<AbortableVisibleRefreshState<T>>({ status: 'idle' });
  const failureCountRef = useRef(0);
  const nextAllowedAtRef = useRef(0);

  useEffect(() => {
    if (!enabled) {
      failureCountRef.current = 0;
      nextAllowedAtRef.current = 0;
      setState({ status: 'idle' });
      return;
    }

    let cancelled = false;
    let controller = new AbortController();
    const resetBackoff = () => {
      failureCountRef.current = 0;
      nextAllowedAtRef.current = 0;
    };
    const recordFailure = () => {
      const delay = Math.min(initialBackoffMs * 2 ** failureCountRef.current, maxBackoffMs);
      failureCountRef.current += 1;
      nextAllowedAtRef.current = Date.now() + delay;
    };

    const tick = async () => {
      if (Date.now() < nextAllowedAtRef.current) return;
      controller.abort();
      controller = new AbortController();
      const localController = controller;
      setState((prev) => {
        if (prev.status === 'ready') {
          return { ...prev, refreshing: true, error: '' };
        }
        return { status: 'loading' };
      });

      try {
        const data = await load(localController.signal);
        if (cancelled || localController.signal.aborted) return;
        resetBackoff();
        setState({ status: 'ready', data, refreshing: false, error: '' });
      } catch (err) {
        if (cancelled || localController.signal.aborted) return;
        recordFailure();
        const error = formatError ? formatError(err) : errorMessage(err);
        setState((prev) => {
          if (prev.status === 'ready') {
            return { ...prev, refreshing: false, error };
          }
          return { status: 'failed', error };
        });
      }
    };

    void tick();
    const interval = window.setInterval(() => {
      if (!document.hidden) void tick();
    }, intervalMs);

    return () => {
      cancelled = true;
      controller.abort();
      window.clearInterval(interval);
    };
  }, [enabled, intervalMs, load, formatError, initialBackoffMs, maxBackoffMs]);

  return state;
}
