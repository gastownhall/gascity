import { useEffect, useRef, useState } from 'react';
import { reportClientError } from '../lib/clientErrorReporting';
import { getActiveCity } from '../api/cityBase';
import { supervisorApi } from '../supervisor/client';

// gascity-dashboard-iew: EventSource against the supervisor's `/v0/...` event
// stream on the current origin. The gc supervisor serves the SPA and the typed
// API same-origin, so the EventSource URL resolves against `location.origin`
// with no transport proxy. The dashboard no longer owns or parses city event
// DTOs.

export type GcEventConnState = 'connecting' | 'open' | 'degraded' | 'closed';
export type GcEventEnvelope = {
  type: string;
  run_id?: string;
  root_bead_id?: string;
  run?: Record<string, unknown>;
  payload?: Record<string, unknown>;
  bead?: Record<string, unknown>;
  root?: Record<string, unknown>;
  metadata?: Record<string, unknown>;
  [key: string]: unknown;
};

export interface GcEventRefreshOptions {
  matches?: (event: GcEventEnvelope) => boolean;
  /**
   * Trailing-throttle window for event-driven refreshes. A burst of matching
   * events yields at most one onMatch per window (leading + trailing). Defaults
   * to {@link DEFAULT_COALESCE_MS}; widen it for consumers whose refresh is an
   * expensive full refetch (e.g. the beads board's ~1.3MB list) so normal city
   * churn and supervisor latency spikes don't trigger a refetch per event.
   */
  coalesceMs?: number;
}

const CONNECTING_GRACE_MS = 2_000;
const DEFAULT_COALESCE_MS = 2_500;

/**
 * Subscribe to gc events. When an event whose type starts with any of
 * `prefixes` arrives, `onMatch` is invoked. Designed for "refresh this
 * panel when its underlying data changed" — pass refresh().
 */
export function useGcEventRefresh(
  prefixes: ReadonlyArray<string>,
  onMatch: () => void,
  options: GcEventRefreshOptions = {},
): GcEventConnState {
  const [state, setState] = useState<GcEventConnState>('connecting');
  const onMatchRef = useRef(onMatch);
  onMatchRef.current = onMatch;
  const matchesRef = useRef(options.matches);
  matchesRef.current = options.matches;
  const coalesceMsRef = useRef(options.coalesceMs);
  coalesceMsRef.current = options.coalesceMs;
  // Stable hash of prefixes for the effect dep array.
  const prefixKey = prefixes.join(',');

  // gascity-dashboard-0sh (ported from upstream cd-tle7m): coalesce
  // event-driven refetches. A busy city emits many bead.*/session.*
  // events per second; firing onMatch per-event made consumers (e.g. the
  // Kanban) refetch /beads ungated (~1/sec), which both hammered the
  // supervisor's city-store read AND amplified its partial-read flicker
  // (td- beads vanish/reappear). Throttle to at most one onMatch per
  // coalesce window (leading + trailing): a burst yields one refetch now
  // and one after it settles, never a per-event storm. Consumers with an
  // expensive refresh widen the window via options.coalesceMs.
  const lastFireRef = useRef(0);
  const coalesceTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  useEffect(() => {
    if (prefixes.length === 0) {
      setState('closed');
      return;
    }

    let es: EventSource | null = null;
    let cancelled = false;
    let retryTimer: ReturnType<typeof setTimeout> | null = null;
    let connectGraceTimer: ReturnType<typeof setTimeout> | null = null;
    let retryDelayMs = 1_000;
    let malformedEventReported = false;

    const clearConnectGraceTimer = () => {
      if (connectGraceTimer === null) return;
      clearTimeout(connectGraceTimer);
      connectGraceTimer = null;
    };
    const reportMalformedEventOnce = (reason: string) => {
      if (malformedEventReported) return;
      malformedEventReported = true;
      reportMalformedEvent(reason);
    };
    const fireMatch = () => {
      lastFireRef.current = Date.now();
      onMatchRef.current();
    };
    // Leading + trailing throttle: fire immediately when outside the
    // window, otherwise schedule a single trailing fire at the window
    // edge. Coalesces a burst of matching events into <=1 onMatch per
    // coalesce window.
    const scheduleMatch = () => {
      const coalesceMs = coalesceMsRef.current ?? DEFAULT_COALESCE_MS;
      const elapsed = Date.now() - lastFireRef.current;
      if (elapsed >= coalesceMs) {
        if (coalesceTimerRef.current) {
          clearTimeout(coalesceTimerRef.current);
          coalesceTimerRef.current = null;
        }
        fireMatch();
      } else if (coalesceTimerRef.current === null) {
        coalesceTimerRef.current = setTimeout(() => {
          coalesceTimerRef.current = null;
          if (!cancelled) fireMatch();
        }, coalesceMs - elapsed);
      }
    };

    const connect = () => {
      const EventSourceCtor = globalThis.EventSource;
      if (typeof EventSourceCtor !== 'function') {
        setState('closed');
        return;
      }
      const cityName = getActiveCity();
      if (cityName === null) {
        setState('closed');
        return;
      }
      // The browser sends Last-Event-ID automatically on reconnect; the
      // supervisor event stream accepts that header directly.
      const source = new EventSourceCtor(supervisorApi().cityEventStreamUrl(cityName));
      es = source;
      setState('connecting');
      connectGraceTimer = setTimeout(() => {
        if (cancelled || es !== source || source.readyState === EventSourceCtor.CLOSED) return;
        setState('open');
      }, CONNECTING_GRACE_MS);
      es.onopen = () => {
        if (cancelled) return;
        clearConnectGraceTimer();
        setState('open');
        retryDelayMs = 1_000;
      };
      // gc supervisor sends events with `event: event` (the event NAME
      // is literally "event"), not the default "message". EventSource
      // routes named events to addEventListener('<name>', ...) — only
      // unnamed events reach .onmessage. Both handlers point to the same
      // dispatch so the path is identical regardless of how the server
      // names them.
      const handleData = (msg: MessageEvent<string>) => {
        if (cancelled) return;
        let parsed: unknown = null;
        try {
          parsed = JSON.parse(msg.data);
        } catch {
          setState('degraded');
          reportMalformedEventOnce('invalid JSON');
          return;
        }
        if (!isRecord(parsed)) {
          setState('degraded');
          reportMalformedEventOnce('missing string event type');
          return;
        }
        const t = parsed.type;
        if (typeof t !== 'string') {
          setState('degraded');
          reportMalformedEventOnce('missing string event type');
          return;
        }
        setState('open');
        for (const prefix of prefixes) {
          if (t.startsWith(prefix)) {
            const event = parsed as GcEventEnvelope;
            if (matchesRef.current?.(event) ?? true) scheduleMatch();
            break;
          }
        }
      };
      es.onmessage = handleData;
      es.addEventListener('event', handleData as EventListener);
      es.onerror = () => {
        if (cancelled) return;
        clearConnectGraceTimer();
        setState('closed');
        es?.close();
        es = null;
        // Exponential backoff capped at 30s.
        retryTimer = setTimeout(() => {
          retryDelayMs = Math.min(retryDelayMs * 2, 30_000);
          connect();
        }, retryDelayMs);
      };
    };

    connect();

    return () => {
      cancelled = true;
      if (retryTimer) clearTimeout(retryTimer);
      clearConnectGraceTimer();
      if (coalesceTimerRef.current) {
        clearTimeout(coalesceTimerRef.current);
        coalesceTimerRef.current = null;
      }
      es?.close();
    };
    // We re-bind only when the prefix set changes — onMatch is captured in a ref.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [prefixKey]);

  return state;
}

function reportMalformedEvent(reason: string): void {
  void reportClientError({
    component: 'gc-events',
    operation: 'parse event',
    message: `Malformed gc event payload: ${reason}.`,
  });
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}
