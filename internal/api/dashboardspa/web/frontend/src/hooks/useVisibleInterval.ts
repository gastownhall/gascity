import { useEffect, useRef } from 'react';

/**
 * Synchronous visible timer for local UI ticks such as relative-time clocks.
 * Async refresh work should use `useVisibleRefresh()` or
 * `useAbortableVisibleRefresh()` instead.
 */
export function useVisibleInterval(callback: () => void, intervalMs: number, enabled = true): void {
  const callbackRef = useRef(callback);
  callbackRef.current = callback;

  useEffect(() => {
    if (!enabled) return;
    const tick = () => {
      if (!document.hidden) callbackRef.current();
    };
    const interval = window.setInterval(tick, intervalMs);
    return () => window.clearInterval(interval);
  }, [enabled, intervalMs]);
}
