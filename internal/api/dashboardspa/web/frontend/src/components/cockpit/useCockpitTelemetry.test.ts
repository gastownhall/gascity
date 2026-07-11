import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';

import { useGcEventFeed, type GcEventEnvelope } from '../../hooks/useGcEvents';
import { supervisorApi } from '../../supervisor/client';
import { useCockpitTelemetry } from './useCockpitTelemetry';

// Unit coverage for the cockpit live-telemetry model. useGcEventFeed is mocked
// so the test drives synthetic envelopes through the captured onEvent callback;
// fake timers make every rolling-window computation deterministic. This is the
// math (events/min, burn/throughput windows, order-fail recency, mail rate,
// per-session tok/min, closed-today seed + reseed, pause freeze) that had no
// coverage while it lived inline in the page.

vi.mock('../../hooks/useGcEvents', () => ({ useGcEventFeed: vi.fn(() => 'open') }));
vi.mock('../../supervisor/client', () => ({ supervisorApi: vi.fn() }));

const mockUseGcEventFeed = useGcEventFeed as Mock;
const mockSupervisorApi = supervisorApi as Mock;

const CITY = 'test-city';

// Mirror of the module constants — kept local so the test pins the values.
const TICK_MS = 250;
const WORKER_WINDOW_SECS = 120;

let capturedOnEvent: ((event: GcEventEnvelope) => void) | null = null;
let listEventsMock: Mock;

function configure(closedItems = 0) {
  listEventsMock = vi
    .fn()
    .mockResolvedValue({ items: Array.from({ length: closedItems }, () => ({})), total: closedItems });
  mockSupervisorApi.mockReturnValue({ listEvents: listEventsMock });
  mockUseGcEventFeed.mockImplementation((_prefixes: unknown, onEvent: typeof capturedOnEvent) => {
    capturedOnEvent = onEvent;
    return 'open';
  });
}

function emit(event: Partial<GcEventEnvelope> & { type: string }) {
  act(() => {
    capturedOnEvent!(event as GcEventEnvelope);
  });
}

async function tick(ms = TICK_MS) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms);
  });
}

/** Flush the mount seed promise (a resolved fetch that only writes a ref). */
async function flushSeed() {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(0);
  });
}

function renderTelemetry(paused = false) {
  return renderHook(({ paused: p }) => useCockpitTelemetry(CITY, p), {
    initialProps: { paused },
  });
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(2026, 6, 11, 14, 23, 30));
  mockUseGcEventFeed.mockReset();
  mockSupervisorApi.mockReset();
  capturedOnEvent = null;
  configure();
});

afterEach(() => {
  vi.useRealTimers();
});

describe('useCockpitTelemetry', () => {
  it('counts events per rolling minute and ages them out after 60s', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({ type: 'session.started' });
    emit({ type: 'bead.updated' });
    emit({ type: 'worker.operation' });
    await tick();
    expect(result.current.feedStats.perMin).toBe(3);

    // Push every event past the 60s window with no fresh traffic.
    await tick(60_000);
    expect(result.current.feedStats.perMin).toBe(0);
  });

  it('leaves the burn fallback in effect when window ops carry tokens but no cost', async () => {
    const { result } = renderTelemetry();
    await flushSeed();
    emit({
      type: 'worker.operation',
      payload: { prompt_tokens: 500, completion_tokens: 100, session_id: 'gc-1' },
    });
    await tick();
    // Tokens are real -> throughput reads from the window; cost was never
    // measured -> burn must stay null so the usage-poll fallback applies
    // (no fabricated $0/hr).
    expect(result.current.feedStats.windowTokensPerSec).not.toBeNull();
    expect(result.current.feedStats.windowCostPerHr).toBeNull();
  });

  it('reports the age of the last event', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({ type: 'session.started' });
    await tick(); // one tick = 250ms after the emit
    expect(result.current.feedStats.lastAgeMs).toBe(250);
  });

  it('trips order-fail recency and expires it after 90s', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({ type: 'order.failed' });
    await tick();
    expect(result.current.feedStats.orderFailedRecent).toBe(true);

    await tick(90_000);
    expect(result.current.feedStats.orderFailedRecent).toBe(false);
  });

  it('reports mail.sent traffic per minute', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({ type: 'mail.sent' });
    emit({ type: 'mail.sent' });
    emit({ type: 'mail.sent' });
    await tick();
    expect(result.current.feedStats.mailPerMin).toBe(3);
  });

  it('derives burn/throughput from worker.operation tokens over the 120s window', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({
      type: 'worker.operation',
      payload: {
        prompt_tokens: 1000,
        completion_tokens: 200,
        cache_read_tokens: 0,
        cache_creation_tokens: 0,
        cost_usd_estimate: 0.5,
        session_id: 'gc-1',
      },
    });
    await tick();
    // tokens = 1200 → /120 = 10 tok/s; cost 0.5 × (3600/120) = $15/hr.
    expect(result.current.feedStats.windowTokensPerSec).toBe(1200 / WORKER_WINDOW_SECS);
    expect(result.current.feedStats.windowCostPerHr).toBe(0.5 * (3600 / WORKER_WINDOW_SECS));
    expect(result.current.feedStats.sessionTokPerMin).toEqual({ 'gc-1': 1200 });

    // Past the 120s window the op is pruned and the rates fall back to null.
    await tick(120_000);
    expect(result.current.feedStats.windowTokensPerSec).toBeNull();
    expect(result.current.feedStats.windowCostPerHr).toBeNull();
    expect(result.current.feedStats.sessionTokPerMin).toEqual({});
  });

  it('drops a worker op from the per-session (60s) window while it still feeds burn (120s)', async () => {
    const { result } = renderTelemetry();
    await flushSeed();

    emit({
      type: 'worker.operation',
      payload: { prompt_tokens: 600, cost_usd_estimate: 0.2, session_id: 'gc-2' },
    });
    // 70s later: aged out of the 60s VU window, still inside the 120s burn window.
    await tick(70_000);
    expect(result.current.feedStats.sessionTokPerMin).toEqual({});
    expect(result.current.feedStats.windowTokensPerSec).toBe(600 / WORKER_WINDOW_SECS);
  });

  it('increments beads-closed-today from the seed on each bead.closed', async () => {
    const { result } = renderTelemetry();
    await flushSeed(); // seed resolves to 0 items

    emit({ type: 'bead.closed' });
    emit({ type: 'bead.closed' });
    await tick();
    expect(result.current.feedStats.beadsClosedToday).toBe(2);
  });

  it('seeds beads-closed-today from the listEvents count', async () => {
    configure(7);
    const { result } = renderTelemetry();
    await flushSeed();
    await tick();
    expect(result.current.feedStats.beadsClosedToday).toBe(7);
  });

  it('seeds closed-today with the bead.closed / since-midnight / limit query', async () => {
    renderTelemetry();
    await flushSeed();
    // 14:23:30 local → 14h23m since local midnight; generous 5000-row cap.
    expect(listEventsMock).toHaveBeenCalledWith(CITY, {
      type: 'bead.closed',
      since: '14h23m',
      limit: 5000,
    });
  });

  it('re-seeds closed-today on the 5-minute interval', async () => {
    renderTelemetry();
    await flushSeed();
    expect(listEventsMock).toHaveBeenCalledTimes(1);

    await tick(300_000);
    expect(listEventsMock.mock.calls.length).toBeGreaterThanOrEqual(2);
  });

  it('freezes feedStats while paused but keeps accumulating, catching up on resume', async () => {
    const { result, rerender } = renderTelemetry(false);
    await flushSeed();

    emit({ type: 'session.started' });
    emit({ type: 'session.started' });
    await tick();
    expect(result.current.feedStats.perMin).toBe(2);

    rerender({ paused: true });
    emit({ type: 'session.started' });
    emit({ type: 'session.started' });
    emit({ type: 'session.started' });
    await tick();
    // Frozen: the derived snapshot does not move while paused.
    expect(result.current.feedStats.perMin).toBe(2);

    rerender({ paused: false });
    await tick();
    // Resume catches up to everything that accumulated underneath.
    expect(result.current.feedStats.perMin).toBe(5);
  });
});
