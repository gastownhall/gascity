import { useEffect, useRef } from 'react';

export interface VisibleRefreshOptions {
  enabled?: boolean;
  initialBackoffMs?: number;
  maxBackoffMs?: number;
  onError?: (err: unknown) => void;
}

const DEFAULT_INITIAL_BACKOFF_MS = 2_000;
const DEFAULT_MAX_BACKOFF_MS = 60_000;

/**
 * Promise-aware visible polling for refresh callbacks whose data/error state
 * lives elsewhere. This hook deliberately waits for the first interval tick;
 * callers that need an initial load should do that explicitly or use a loader
 * hook such as `useAbortableVisibleRefresh()`.
 */
export function useVisibleRefresh(
  refresh: () => Promise<void>,
  intervalMs: number,
  options: VisibleRefreshOptions = {},
): void {
  const refreshRef = useRef(refresh);
  refreshRef.current = refresh;
  const optionsRef = useRef(resolveOptions(options));
  optionsRef.current = resolveOptions(options);
  const failureCountRef = useRef(0);
  const nextAllowedAtRef = useRef(0);
  const inFlightRef = useRef(false);

  const { enabled, initialBackoffMs, maxBackoffMs } = optionsRef.current;

  useEffect(() => {
    if (!enabled) return;

    const resetBackoff = () => {
      failureCountRef.current = 0;
      nextAllowedAtRef.current = 0;
    };
    const recordFailure = (err: unknown) => {
      const currentOptions = optionsRef.current;
      currentOptions.onError?.(err);
      const delay = Math.min(
        currentOptions.initialBackoffMs * 2 ** failureCountRef.current,
        currentOptions.maxBackoffMs,
      );
      failureCountRef.current += 1;
      nextAllowedAtRef.current = Date.now() + delay;
    };
    const tick = () => {
      if (document.hidden) return;
      if (inFlightRef.current) return;
      if (Date.now() < nextAllowedAtRef.current) return;
      inFlightRef.current = true;
      void refreshRef
        .current()
        .then(resetBackoff, recordFailure)
        .finally(() => {
          inFlightRef.current = false;
        });
    };

    const interval = window.setInterval(tick, intervalMs);
    return () => window.clearInterval(interval);
  }, [enabled, intervalMs, initialBackoffMs, maxBackoffMs]);
}

function resolveOptions(options: VisibleRefreshOptions): Required<VisibleRefreshOptions> {
  return {
    enabled: options.enabled ?? true,
    initialBackoffMs: options.initialBackoffMs ?? DEFAULT_INITIAL_BACKOFF_MS,
    maxBackoffMs: options.maxBackoffMs ?? DEFAULT_MAX_BACKOFF_MS,
    onError: options.onError ?? noopOnError,
  };
}

function noopOnError(): void {}
