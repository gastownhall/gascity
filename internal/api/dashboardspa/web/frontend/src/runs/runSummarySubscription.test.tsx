import { act, cleanup, render, renderHook, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import {
  type RunLane,
  type RunSummary,
  type SourceState,
  type SourceStatus,
} from 'gas-city-dashboard-shared';
import { invalidateKey } from '../api/cache';
import { setActiveCity } from '../api/cityBase';
import {
  loadSupervisorRunSummaryActiveSource,
  loadSupervisorRunSummaryPreviewSource,
  loadSupervisorRunSummarySource,
} from '../supervisor/runSummary';
import { RunSummaryProvider, useRunSummary } from './runSummarySubscription';

// gascity-dashboard-2j8e.7: the nav badge and the /runs page used to run two
// separate run-summary fan-outs (separate cache keys) and the badge never
// SSE-refreshed, so it double-fetched on /runs and drifted after mount. This
// pins the unification: ONE provider-owned subscription serves every consumer
// (one fan-out) and an SSE event refreshes the source every consumer reads
// (no post-mount drift).

vi.mock('../supervisor/runSummary', () => ({
  loadSupervisorRunSummaryPreviewSource: vi.fn(),
  loadSupervisorRunSummarySource: vi.fn(),
  loadSupervisorRunSummaryActiveSource: vi.fn(),
}));

const lastHookCall: { prefixes: ReadonlyArray<string> | null; onMatch: (() => void) | null } = {
  prefixes: null,
  onMatch: null,
};
vi.mock('../hooks/useGcEvents', () => ({
  useGcEventRefresh: vi.fn((prefixes: ReadonlyArray<string>, onMatch: () => void) => {
    lastHookCall.prefixes = prefixes;
    lastHookCall.onMatch = onMatch;
    return 'open' as const;
  }),
}));

const mockPreview = loadSupervisorRunSummaryPreviewSource as Mock;
const mockFull = loadSupervisorRunSummarySource as Mock;
// gascity-dashboard: SSE-driven refreshes route through the CHEAP active source,
// not the wide one — only the manual button + one-time first upgrade are wide.
const mockActive = loadSupervisorRunSummaryActiveSource as Mock;

function buildRunSource(
  status: Exclude<SourceStatus, 'error'>,
  blocked = 0,
): SourceState<RunSummary> {
  return {
    source: 'runs',
    status,
    fetchedAt: '2026-06-01T00:00:00.000Z',
    staleAt: '2026-06-01T00:01:00.000Z',
    error: { kind: 'none' },
    data: {
      totalActive: 0,
      totalHistorical: 0,
      historicalLanes: [],
      blockedLanes: [],
      runCounts: {
        total: 0,
        visible: 0,
        prReview: 0,
        designReview: 0,
        bugfix: 0,
        blocked,
        other: 0,
      },
      lanes: [],
      recentChanges: [],
      census: { status: 'unavailable', error: 'run health has not been derived' },
    },
  };
}

function lane(id: string): RunLane {
  return {
    id,
    title: `Run ${id}`,
    formula: { status: 'known', name: 'mol-focus-review' },
    scope: {
      status: 'available',
      kind: 'city',
      ref: 'racoon-city',
      rootStoreRef: 'city:racoon-city',
    },
    external: { status: 'unavailable', error: 'external unavailable in test' },
    phase: 'implementation',
    phaseLabel: 'implementation',
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-06-01T00:00:00.000Z' },
    stages: [],
    progress: { status: 'unavailable', error: 'run progress unavailable in test' },
    formulaStageResolved: false,
    health: { status: 'unavailable', error: 'run health has not been derived' },
  };
}

// Build a source carrying explicit active / blocked / historical lane sets, so a
// test can assert WHICH run ids land in WHICH section after a cheap merge.
function buildLaneSource(opts: {
  status?: Exclude<SourceStatus, 'error'>;
  active?: string[];
  blocked?: string[];
  historical?: string[];
  totalHistorical?: number;
}): SourceState<RunSummary> {
  const base = buildRunSource(opts.status ?? 'fresh');
  if (base.status === 'error') throw new Error('unreachable');
  const historical = (opts.historical ?? []).map(lane);
  return {
    ...base,
    data: {
      ...base.data,
      lanes: (opts.active ?? []).map(lane),
      blockedLanes: (opts.blocked ?? []).map(lane),
      historicalLanes: historical,
      totalHistorical: opts.totalHistorical ?? historical.length,
    },
  };
}

// A stand-in for the two real consumers — the nav badge and the /runs page —
// rendering the status + blocked count of the source it reads.
function Consumer({ label }: { label: string }) {
  const { source } = useRunSummary();
  const blocked = source && source.status !== 'error' ? source.data.runCounts.blocked : -1;
  return <div data-testid={label}>{`${source?.status ?? 'pending'}:${blocked}`}</div>;
}

// A consumer that renders the active+blocked lane ids, the historical lane ids,
// and totalHistorical — for asserting cheap-refresh history reconciliation.
function LaneConsumer({ label }: { label: string }) {
  const { source } = useRunSummary();
  if (!source || source.status === 'error') return <div data-testid={label}>none</div>;
  const live = [...source.data.lanes, ...source.data.blockedLanes].map((l) => l.id).join(',');
  const hist = source.data.historicalLanes.map((l) => l.id).join(',');
  return (
    <div
      data-testid={label}
    >{`live=[${live}] hist=[${hist}] totalHist=${source.data.totalHistorical}`}</div>
  );
}

beforeEach(() => {
  setActiveCity('racoon-city');
  mockPreview.mockReset();
  mockFull.mockReset();
  mockActive.mockReset();
  lastHookCall.prefixes = null;
  lastHookCall.onMatch = null;
  invalidateKey('runs:summary:racoon-city');
  mockPreview.mockResolvedValue(buildRunSource('fresh'));
  mockFull.mockResolvedValue(buildRunSource('fresh'));
  mockActive.mockResolvedValue(buildRunSource('fresh'));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

describe('useRunSummarySubscription / RunSummaryProvider (gascity-dashboard-2j8e.7)', () => {
  it('serves many consumers from a single fan-out (no double fetch)', async () => {
    render(
      <RunSummaryProvider>
        <Consumer label="badge" />
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // One preview paint + one full upgrade for the whole tree, regardless of
    // how many consumers read it — the badge no longer runs its own fan-out.
    await waitFor(() => expect(mockFull).toHaveBeenCalledTimes(1));
    expect(mockPreview).toHaveBeenCalledTimes(1);

    // Both consumers read the same source object → identical render.
    expect(screen.getByTestId('badge').textContent).toBe(screen.getByTestId('page').textContent);
  });

  it('subscribes to bead events once for the whole tree', async () => {
    render(
      <RunSummaryProvider>
        <Consumer label="badge" />
        <Consumer label="page" />
      </RunSummaryProvider>,
    );
    await waitFor(() => expect(mockFull).toHaveBeenCalledTimes(1));
    expect(lastHookCall.prefixes).toEqual(['bead.']);
    expect(typeof lastHookCall.onMatch).toBe('function');
  });

  it('refreshes every consumer on an SSE event (no post-mount drift)', async () => {
    render(
      <RunSummaryProvider>
        <Consumer label="badge" />
        <Consumer label="page" />
      </RunSummaryProvider>,
    );
    await waitFor(() => expect(mockFull).toHaveBeenCalledTimes(1));
    expect(screen.getByTestId('badge').textContent).toBe('fresh:0');

    // A bead event lands and the next CHEAP load carries a newly-blocked run.
    mockActive.mockResolvedValue(buildRunSource('fresh', 1));
    await act(async () => {
      lastHookCall.onMatch?.();
    });

    await waitFor(() => expect(screen.getByTestId('badge').textContent).toBe('fresh:1'));
    // The page sees the same update in the same pass — they cannot drift apart.
    expect(screen.getByTestId('page').textContent).toBe('fresh:1');
  });

  it('queues a trailing refresh when an SSE event lands mid-load (no stale latch)', async () => {
    // The exact race the CI flake exposed: an SSE event arrives WHILE a load is
    // already in flight. That load started before the event, so it can return
    // the pre-event snapshot — if the event is dropped, the snapshot latches
    // stale and every consumer drifts. Park the full upgrade in flight to drive
    // the race deterministically instead of relying on machine speed.
    let resolveFull: (source: SourceState<RunSummary>) => void = () => {};
    mockFull.mockImplementationOnce(
      () =>
        new Promise<SourceState<RunSummary>>((resolve) => {
          resolveFull = resolve;
        }),
    );

    render(
      <RunSummaryProvider>
        <Consumer label="badge" />
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // The full upgrade has been kicked off and is parked, mid-flight.
    await waitFor(() => expect(mockFull).toHaveBeenCalledTimes(1));

    // A bead event lands while the load is in flight; the queued trailing CHEAP
    // refresh must carry the newly-blocked run.
    mockActive.mockResolvedValue(buildRunSource('fresh', 1));
    await act(async () => {
      lastHookCall.onMatch?.();
    });

    // Let the in-flight load settle with the pre-event snapshot...
    await act(async () => {
      resolveFull(buildRunSource('fresh', 0));
    });

    // ...and the queued trailing refresh reconciles every consumer to the
    // post-event snapshot — no stale latch, no drift.
    await waitFor(() => expect(screen.getByTestId('badge').textContent).toBe('fresh:1'));
    expect(screen.getByTestId('page').textContent).toBe('fresh:1');
  });

  it('keeps the last-good summary as stale when a refresh errors (no blank on transient timeout)', async () => {
    // The /runs UX bug: first paint renders lanes, then a background refresh
    // times out under city load and used to OVERWRITE the good render with the
    // full "Run data unavailable" error state. A transient refresh failure must
    // retain the last-good snapshot, re-published as 'stale' (which RunMap and
    // Runs.tsx render as data), not transition the view to 'error'.
    mockFull.mockResolvedValue(buildRunSource('fresh', 2));

    render(
      <RunSummaryProvider>
        <Consumer label="badge" />
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // First good load lands.
    await waitFor(() => expect(screen.getByTestId('badge').textContent).toBe('fresh:2'));

    // A bead event triggers a CHEAP refresh that resolves to an error source
    // (the supervisor list timed out). Prior good data exists, so the published
    // state must keep that data as 'stale', not flip to 'error'.
    mockActive.mockResolvedValue({
      source: 'runs',
      status: 'error',
      error: 'gc supervisor request timed out after 5000ms',
    } satisfies SourceState<RunSummary>);
    await act(async () => {
      lastHookCall.onMatch?.();
    });

    await waitFor(() => expect(screen.getByTestId('badge').textContent).toBe('stale:2'));
    expect(screen.getByTestId('page').textContent).toBe('stale:2');
  });

  it('keeps the last-good summary as stale when a refresh throws', async () => {
    mockFull.mockResolvedValue(buildRunSource('fresh', 3));

    render(
      <RunSummaryProvider>
        <Consumer label="page" />
      </RunSummaryProvider>,
    );
    await waitFor(() => expect(screen.getByTestId('page').textContent).toBe('fresh:3'));

    // A CHEAP refresh that rejects (rather than resolving to an error source)
    // must also retain the last-good snapshot rather than latching the view dead.
    mockActive.mockRejectedValue(new Error('gc supervisor request timed out after 5000ms'));
    await act(async () => {
      lastHookCall.onMatch?.();
    });

    await waitFor(() => expect(screen.getByTestId('page').textContent).toBe('stale:3'));
  });

  it('self-recovers from a stale-retention: schedules a re-refresh that lands fresh with no SSE event', async () => {
    // The latch bug: a good preview paints, then the one-time full refresh times
    // out ONCE. Last-good retention keeps the view as 'stale' (good UX, never
    // blanks) — but because the masked failure looked like a healthy snapshot to
    // the retry path, nothing re-attempted, and the view stayed stuck on the
    // preview-grade snapshot until some unrelated SSE event happened to fire.
    // The fix decouples what we DISPLAY (stale, not blank) from whether we keep
    // RETRYING (yes, with backoff). Here the full refresh fails once, then the
    // backed-off re-refresh succeeds with full data — all driven by timers, with
    // no SSE event in the test.
    vi.useFakeTimers();
    // Preview paints good data (blocked=4 stands in for preview-grade).
    mockPreview.mockResolvedValue(buildRunSource('fresh', 4));
    // The one-time full upgrade times out the FIRST time it is called...
    mockFull.mockRejectedValueOnce(new Error('gc supervisor request timed out after 5000ms'));
    // ...and every later attempt lands the full session-enriched snapshot.
    mockFull.mockResolvedValue(buildRunSource('fresh', 7));

    render(
      <RunSummaryProvider>
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // Preview lands, then the full refresh fails and is retained as 'stale' over
    // the preview-grade data — the view shows data, never blanks.
    await vi.waitFor(() => expect(screen.getByTestId('page').textContent).toBe('stale:4'));
    // No SSE event has fired; the only thing that can recover us is the scheduled
    // backoff re-refresh (first delay = 2_000ms).
    expect(lastHookCall.onMatch).toBeTypeOf('function');

    // Advance past the first backoff delay to fire the scheduled re-refresh,
    // which now succeeds with the full snapshot.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_000);
    });

    // The view self-recovered to fresh, full data — no external SSE event.
    await vi.waitFor(() => expect(screen.getByTestId('page').textContent).toBe('fresh:7'));
  });

  it('does not leak the recovery timer on unmount', async () => {
    vi.useFakeTimers();
    mockPreview.mockResolvedValue(buildRunSource('fresh', 4));
    mockFull.mockRejectedValue(new Error('gc supervisor request timed out after 5000ms'));

    const { unmount } = render(
      <RunSummaryProvider>
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // A stale-retention is published and a recovery timer is armed.
    await vi.waitFor(() => expect(screen.getByTestId('page').textContent).toBe('stale:4'));
    const fullCallsBeforeUnmount = mockFull.mock.calls.length;

    unmount();

    // After unmount, advancing well past the whole backoff budget must not fire
    // any further refresh — the timer was cleaned up.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(mockFull.mock.calls.length).toBe(fullCallsBeforeUnmount);
  });

  it('surfaces error when the FIRST load fails with no prior good data', async () => {
    // The genuine first-load failure is unchanged: with no prior snapshot to
    // retain, the error state still surfaces so the operator is not shown an
    // empty store as if it were healthy.
    mockPreview.mockResolvedValue({
      source: 'runs',
      status: 'error',
      error: 'formula runs unavailable',
    } satisfies SourceState<RunSummary>);
    mockFull.mockResolvedValue({
      source: 'runs',
      status: 'error',
      error: 'formula runs unavailable',
    } satisfies SourceState<RunSummary>);

    render(
      <RunSummaryProvider>
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    await waitFor(() => expect(screen.getByTestId('page').textContent).toBe('error:-1'));
  });

  it('publishes the active source snapshot verbatim on a cheap refresh (server owns the active/historical split)', async () => {
    // The cheap SSE refresh now hits the same complete BFF read as the wide one,
    // so the active source returns the authoritative active+historical split and
    // the subscription publishes it directly — NO borrowing history from the last
    // wide snapshot, no client-side reconciliation. The discriminating assertion:
    // an active read with empty history publishes empty history (a borrow would
    // have re-surfaced gc-2 from the seeded wide snapshot).
    mockFull.mockResolvedValue(buildLaneSource({ active: [], historical: ['gc-1', 'gc-2'] }));

    render(
      <RunSummaryProvider>
        <LaneConsumer label="page" />
      </RunSummaryProvider>,
    );

    // Wide upgrade landed: gc-1 + gc-2 are historical, nothing live.
    await waitFor(() =>
      expect(screen.getByTestId('page').textContent).toBe('live=[] hist=[gc-1,gc-2] totalHist=2'),
    );

    // A bead event lands; the cheap active read returns its OWN complete snapshot
    // (gc-1 active, no historical). Published verbatim — gc-2 is NOT borrowed back.
    mockActive.mockResolvedValue(buildLaneSource({ active: ['gc-1'], historical: [] }));
    await act(async () => {
      lastHookCall.onMatch?.();
    });

    await waitFor(() =>
      expect(screen.getByTestId('page').textContent).toBe('live=[gc-1] hist=[] totalHist=0'),
    );
  });

  it('does not let a cheap SSE success cancel the bounded wide-failure retry', async () => {
    // MAJOR 2: a failed WIDE upgrade arms the bounded wide retry (off
    // staleDueToFailureRef). A cheap SSE success must NOT clear that flag, so the
    // wide retry still fires; only a successful WIDE refresh marks recovery.
    vi.useFakeTimers();
    mockPreview.mockResolvedValue(buildRunSource('fresh', 4));
    // The one-time wide upgrade fails the first time, arming the retry...
    mockFull.mockRejectedValueOnce(new Error('gc supervisor request timed out after 5000ms'));
    // ...and the retried wide refresh succeeds.
    mockFull.mockResolvedValue(buildRunSource('fresh', 7));
    // A cheap SSE refresh in between succeeds — it must not be treated as recovery.
    mockActive.mockResolvedValue(buildRunSource('fresh', 4));

    render(
      <RunSummaryProvider>
        <Consumer label="page" />
      </RunSummaryProvider>,
    );

    // Preview lands, wide upgrade fails and is retained as stale.
    await vi.waitFor(() => expect(screen.getByTestId('page').textContent).toBe('stale:4'));
    const wideCallsAfterFirstFail = mockFull.mock.calls.length;

    // A cheap SSE refresh succeeds before the wide retry timer fires.
    await act(async () => {
      lastHookCall.onMatch?.();
    });
    // The cheap success did NOT add a wide call and (critically) did not cancel
    // the armed wide retry — the published view is still stale-due-to-failure.
    expect(mockFull.mock.calls.length).toBe(wideCallsAfterFirstFail);

    // Advance through the backoff budget: the WIDE retry must still fire (the
    // cheap success did not cancel it) and recover the view to the full snapshot.
    // The intervening cheap-success render re-arms the retry timer at the next
    // backoff slot, so drain the whole budget rather than just the first delay.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(mockFull.mock.calls.length).toBeGreaterThan(wideCallsAfterFirstFail);
    await vi.waitFor(() => expect(screen.getByTestId('page').textContent).toBe('fresh:7'));
  });

  it('throws when used outside a RunSummaryProvider', () => {
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {});
    expect(() => renderHook(() => useRunSummary())).toThrow(/RunSummaryProvider/);
    spy.mockRestore();
  });
});
