import { useCallback, useEffect, useRef, useState } from 'react';
import { supervisorApi } from '../../supervisor/client';
import { useGcEventFeed, type GcEventConnState, type GcEventEnvelope } from '../../hooks/useGcEvents';

// The live-telemetry model behind the cockpit home. One gc event subscription
// fans out to the oscilloscope (via the stable `subscribe` handed to it) and to
// a set of rolling-window accumulators; a ~4Hz tick derives the feed-fed
// readings from those windows so an event burst never storms React. Pause
// freezes the derived snapshot on the wall while the windows keep accumulating
// underneath, so a resume catches up instantly rather than starting cold.

const TICK_MS = 250; // ~4Hz refresh of the feed-derived gauges + live-chip age
const EVENTS_WINDOW_MS = 60_000; // rolling window backing the events/min reading
const MAIL_WINDOW_MS = 60_000; // rolling window backing the mail-traffic lamp
const VU_WINDOW_MS = 60_000; // rolling window for per-session tok/min
const WORKER_WINDOW_MS = 120_000; // rolling window backing burn + throughput
const WORKER_WINDOW_SECS = 120;
const ORDER_FAIL_RECENT_MS = 90_000; // orders lamp trips warn within this of a failure
const CLOSED_RESEED_MS = 300_000; // re-seed the beads-closed odometer every 5 min
const CLOSED_SEED_LIMIT = 5_000; // generous cap; the odometer reads 4 digits anyway

// A single catch-all prefix: every typed gc event (bead./session./worker./
// order./mail./health./… ) feeds the oscilloscope, the events/min rate, and the
// feed-derived instruments. `''` prefixes every type; the feed still requires a
// non-empty prefix array to connect.
const ALL_EVENT_PREFIXES = [''] as const;

export interface WorkerOpSample {
  t: number;
  tokens: number;
  cost: number;
  key: string | null;
}

export interface FeedStats {
  perMin: number;
  lastAgeMs: number | null;
  /** Σcost×(3600/window) over the 120s window; null when no token-bearing ops. */
  windowCostPerHr: number | null;
  /** Σtokens/windowSecs over the 120s window; null when no token-bearing ops. */
  windowTokensPerSec: number | null;
  beadsClosedToday: number;
  orderFailedRecent: boolean;
  mailPerMin: number;
  /** Per-session tokens over the 60s window (= tok/min), keyed by id/name. */
  sessionTokPerMin: Record<string, number>;
}

const EMPTY_FEED_STATS: FeedStats = {
  perMin: 0,
  lastAgeMs: null,
  windowCostPerHr: null,
  windowTokensPerSec: null,
  beadsClosedToday: 0,
  orderFailedRecent: false,
  mailPerMin: 0,
  sessionTokPerMin: {},
};

export interface CockpitTelemetry {
  /** The feed-derived readings, refreshed on the ~4Hz tick (frozen while paused). */
  feedStats: FeedStats;
  /** Stable per-event subscription for the oscilloscope; returns an unsubscribe. */
  subscribe: (onEvent: () => void) => () => void;
  /** Connection state of the underlying gc event stream. */
  connState: GcEventConnState;
}

/**
 * useCockpitTelemetry owns the cockpit home's live-activity model: it subscribes
 * to the gc event stream, accumulates rolling windows (events, mail, worker ops)
 * plus the beads-closed-today counter, and derives {@link FeedStats} on a ~4Hz
 * tick. While `paused` is true the derived snapshot is held steady on the wall,
 * but the underlying windows and the closed-today counter keep accumulating so a
 * resume reflects reality immediately. `city` scopes the closed-today seed;
 * pass `null` to leave the counter unseeded until a city is active.
 */
export function useCockpitTelemetry(city: string | null, paused: boolean): CockpitTelemetry {
  // Ref mirror so interval callbacks read the live pause state without
  // re-subscribing. Written in render (idempotent) so it is current before the
  // effects below run.
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  // Event feed fan-out. One subscription drives the oscilloscope (via the stable
  // `subscribe` handed to it) and every feed-derived reading. onEvent is
  // captured in a ref inside useGcEventFeed, so its identity is irrelevant.
  const listenersRef = useRef<Set<() => void>>(new Set());
  const eventTimesRef = useRef<number[]>([]);
  const mailSentTimesRef = useRef<number[]>([]);
  const workerOpsRef = useRef<WorkerOpSample[]>([]);
  const lastEventAtRef = useRef<number | null>(null);
  const orderFailedAtRef = useRef<number | null>(null);
  // Beads closed today: null until first seeded, then live-incremented and
  // periodically re-seeded to self-correct.
  const beadClosedCountRef = useRef<number | null>(null);

  const handleFeedEvent = useCallback((event: GcEventEnvelope) => {
    const now = Date.now();
    eventTimesRef.current.push(now);
    lastEventAtRef.current = now;
    switch (event.type) {
      case 'bead.closed':
        beadClosedCountRef.current = (beadClosedCountRef.current ?? 0) + 1;
        break;
      case 'order.failed':
        orderFailedAtRef.current = now;
        break;
      case 'mail.sent':
        mailSentTimesRef.current.push(now);
        break;
      case 'worker.operation': {
        const payload = event.payload;
        const tokens =
          payloadNum(payload, 'prompt_tokens') +
          payloadNum(payload, 'completion_tokens') +
          payloadNum(payload, 'cache_read_tokens') +
          payloadNum(payload, 'cache_creation_tokens');
        const cost = payloadNum(payload, 'cost_usd_estimate');
        const key =
          payloadStr(payload, 'session_id') ??
          payloadStr(payload, 'session_name') ??
          (typeof event.session_id === 'string' ? event.session_id : null);
        workerOpsRef.current.push({ t: now, tokens, cost, key });
        break;
      }
    }
    for (const listener of listenersRef.current) listener();
  }, []);

  const connState = useGcEventFeed(ALL_EVENT_PREFIXES, handleFeedEvent);

  const subscribe = useCallback((onEvent: () => void) => {
    listenersRef.current.add(onEvent);
    return () => {
      listenersRef.current.delete(onEvent);
    };
  }, []);

  // Seed (and periodically re-seed) beads-closed-today by counting bead.closed
  // events since local midnight. A failed seed leaves the live count untouched —
  // it must never zero a running counter.
  useEffect(() => {
    if (city === null) return;
    let cancelled = false;
    const seed = async () => {
      try {
        const res = await supervisorApi().listEvents(city, {
          type: 'bead.closed',
          since: durationSinceMidnight(),
          limit: CLOSED_SEED_LIMIT,
        });
        if (!cancelled) beadClosedCountRef.current = res.items?.length ?? 0;
      } catch {
        // Leave the current count; a failed seed must not zero a live counter.
      }
    };
    void seed();
    const id = setInterval(() => {
      if (!pausedRef.current) void seed();
    }, CLOSED_RESEED_MS);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [city]);

  // Derive the feed-fed readings on a ~4Hz timer rather than per-event, so a
  // burst never storms React. Windows are pruned every tick (bounding memory
  // even while paused); the state write is gated on pause so the wall freezes.
  const [feedStats, setFeedStats] = useState<FeedStats>(EMPTY_FEED_STATS);
  useEffect(() => {
    const id = setInterval(() => {
      const now = Date.now();
      pruneBefore(eventTimesRef.current, now - EVENTS_WINDOW_MS);
      pruneBefore(mailSentTimesRef.current, now - MAIL_WINDOW_MS);
      pruneOpsBefore(workerOpsRef.current, now - WORKER_WINDOW_MS);
      if (pausedRef.current) return;

      let sumCost = 0;
      let sumTokens = 0;
      let hasTokenBearingOp = false;
      let hasCostBearingOp = false;
      const sessionTokPerMin: Record<string, number> = {};
      for (const op of workerOpsRef.current) {
        sumCost += op.cost;
        sumTokens += op.tokens;
        if (op.tokens > 0) hasTokenBearingOp = true;
        if (op.cost > 0) hasCostBearingOp = true;
        if (op.key !== null && now - op.t <= VU_WINDOW_MS) {
          sessionTokPerMin[op.key] = (sessionTokPerMin[op.key] ?? 0) + op.tokens;
        }
      }

      setFeedStats({
        perMin: eventTimesRef.current.length,
        lastAgeMs: lastEventAtRef.current === null ? null : now - lastEventAtRef.current,
        windowCostPerHr: hasCostBearingOp ? sumCost * (3600 / WORKER_WINDOW_SECS) : null,
        windowTokensPerSec: hasTokenBearingOp ? sumTokens / WORKER_WINDOW_SECS : null,
        beadsClosedToday: beadClosedCountRef.current ?? 0,
        orderFailedRecent:
          orderFailedAtRef.current !== null && now - orderFailedAtRef.current < ORDER_FAIL_RECENT_MS,
        mailPerMin: mailSentTimesRef.current.length,
        sessionTokPerMin,
      });
    }, TICK_MS);
    return () => clearInterval(id);
  }, []);

  return { feedStats, subscribe, connState };
}

function payloadNum(payload: Record<string, unknown> | undefined, key: string): number {
  const value = payload?.[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : 0;
}

function payloadStr(payload: Record<string, unknown> | undefined, key: string): string | null {
  const value = payload?.[key];
  return typeof value === 'string' && value.length > 0 ? value : null;
}

/** Drops leading timestamps older than `cutoff` from an ascending-time array. */
function pruneBefore(times: number[], cutoff: number): void {
  while (times.length > 0 && times[0]! < cutoff) times.shift();
}

function pruneOpsBefore(ops: WorkerOpSample[], cutoff: number): void {
  while (ops.length > 0 && ops[0]!.t < cutoff) ops.shift();
}

/**
 * durationSinceMidnight formats the elapsed time since local midnight as a Go
 * duration string (e.g. "14h23m"), the shape the events `since` filter parses.
 */
function durationSinceMidnight(now: Date = new Date()): string {
  const midnight = new Date(now.getFullYear(), now.getMonth(), now.getDate());
  const totalMinutes = Math.max(0, Math.floor((now.getTime() - midnight.getTime()) / 60_000));
  const hours = Math.floor(totalMinutes / 60);
  const minutes = totalMinutes % 60;
  if (hours > 0) return `${hours}h${minutes}m`;
  if (minutes > 0) return `${minutes}m`;
  // Just past midnight — a small non-zero window keeps the filter well-formed.
  return '1m';
}
