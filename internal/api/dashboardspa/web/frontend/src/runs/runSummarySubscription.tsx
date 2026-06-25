import {
  GC_EVENT_PREFIX,
  type RunSummary,
  type SourceAvailableState,
  type SourceState,
  type SourceStatus,
} from 'gas-city-dashboard-shared';
import { createContext, useCallback, useContext, useEffect, useRef, type ReactNode } from 'react';
import { getActiveCity } from '../api/cityBase';
import { useCachedData } from '../hooks/useCachedData';
import { useGcEventRefresh, type GcEventConnState } from '../hooks/useGcEvents';
import {
  loadSupervisorRunSummaryActiveSource,
  loadSupervisorRunSummaryPreviewSource,
  loadSupervisorRunSummarySource,
} from '../supervisor/runSummary';

// gascity-dashboard-2j8e.7: ONE shared, SSE-refreshed run-summary subscription.
//
// Before this, the /runs page and the nav attention badge each ran their OWN
// full run-summary fan-out under separate cache keys (`runs:summary:*` vs
// `attention:runs:*`), so visiting /runs fetched the supervisor twice, and the
// badge — fetched once at mount with no SSE wiring — drifted from the live page
// after any post-mount churn. This subscription is owned once at the App root
// (always mounted, so the always-visible header badge stays live) and exposed
// via context, so the badge and the page read literally the SAME source object:
// one fan-out, by-construction parity, and a single SSE refresh path feeding
// both. The page reads it through useRunSummary(); the badge reads its `source`
// through the attention layer (liveContributors `runsFactsFromSource`).
//
// The fetch contract: the cheap preview (2.5s budget) paints first, a one-time
// full refresh (30s, session-enriched, WIDE) upgrades the snapshot to the
// page-complete one and populates the historical lanes, a bounded backoff
// recovers a degraded first load (WIDE), and SSE-driven refetches take the CHEAP
// active-only path (skips the molecule(all=true) + city feed + per-rig reads,
// merging historical lanes from the last wide snapshot) gated by an in-component
// debounce floor on top of useGcEventRefresh's own coalescing (architect H1/H2
// upstream-load protection during slung-pipeline bursts). The manual Refresh
// button still triggers a WIDE scan on demand.

const REFRESH_DEBOUNCE_MS = 10_000;
// gascity-dashboard-4xcv: bounded retry backoff for a degraded first load (error
// source, or a partial fetch that produced zero lanes). After the budget is
// spent, SSE events and the manual Refresh button take over.
const RETRY_DELAYS_MS = [2_000, 5_000, 10_000];

export interface RunSummarySubscription {
  /** The shared run-summary source state; undefined until the first read lands. */
  source: SourceState<RunSummary> | undefined;
  loading: boolean;
  error: string | null;
  refresh: () => Promise<void>;
  sseState: GcEventConnState;
}

/**
 * Own the single run-summary subscription: cache-warm preview paint, one-time
 * full-source upgrade, bounded degraded-load retry, and SSE-driven refresh. Call
 * once near the App root via {@link RunSummaryProvider}; everything else reads
 * the result through {@link useRunSummary}.
 */
export function useRunSummarySubscription(): RunSummarySubscription {
  const cityName = getActiveCity();

  // Last-good retention (the /runs blank-on-transient-timeout bug): a background
  // refresh that resolves to status:'error' — or throws — under city load used
  // to OVERWRITE the already-rendered lanes with the full "Run data unavailable"
  // page. A transient refresh failure must not blank a good view: if a prior
  // available snapshot exists, keep serving it, re-published as 'stale' (which
  // RunMap and Runs.tsx already render as data, with a subtle stale hint). The
  // error state is only published when there is NO prior good snapshot — a
  // genuine first-load failure, where an empty view would lie about the store.
  const lastGoodRef = useRef<SourceAvailableState<RunSummary> | null>(null);
  // The recovery signal, kept distinct from the PUBLISHED status. When a refresh
  // fails but we serve last-good as 'stale', the view shows data — but a 'stale'
  // status is indistinguishable from a healthy snapshot to the retry effect, so
  // it would treat the failure as resolved and stop trying. That latches the
  // preview-grade snapshot until an unrelated SSE event happens to fire. This
  // flag records "the last refresh failed; the displayed data is stale BECAUSE
  // of a failure" so recovery keeps driving regardless of what we display, and
  // is cleared the moment a genuine fresh/full result lands.
  const staleDueToFailureRef = useRef(false);
  const refreshWithLastGoodRetention = useCallback(async (): Promise<SourceState<RunSummary>> => {
    const result = await loadSupervisorRunSummarySource().catch(
      (err): SourceState<RunSummary> => ({
        source: 'runs',
        status: 'error',
        error: err instanceof Error ? err.message : 'formula runs unavailable',
      }),
    );
    if (result.status !== 'error') {
      staleDueToFailureRef.current = false;
      return result;
    }
    const lastGood = lastGoodRef.current;
    if (lastGood === null) return result;
    staleDueToFailureRef.current = true;
    return { ...lastGood, status: 'stale' };
  }, []);

  // gascity-dashboard: the CHEAP SSE-burst refresh. loadSupervisorRunSummaryActiveSource
  // skips the molecule(all=true) + city feed + per-rig task reads, so a routine
  // bead burst no longer saturates the browser connection pool and queues the
  // run-detail's fast workflowRun read behind it. The active/blocked set is fully
  // correct (scope/health/counts/census); only HISTORICAL lanes come from the
  // recent reads, so we merge those back from the last wide snapshot to keep the
  // History section populated. Failure handling mirrors the wide wrapper.
  const refreshActiveWithMerge = useCallback(async (): Promise<SourceState<RunSummary>> => {
    const result = await loadSupervisorRunSummaryActiveSource().catch(
      (err): SourceState<RunSummary> => ({
        source: 'runs',
        status: 'error',
        error: err instanceof Error ? err.message : 'formula runs unavailable',
      }),
    );
    if (result.status !== 'error') {
      // A cheap (active-only) success does NOT prove the WIDE failure recovered:
      // the wide retry loop must keep driving off staleDueToFailureRef until a
      // wide refresh actually lands (refreshWithLastGoodRetention clears it). So
      // we deliberately do NOT clear the flag here.
      const lastGood = lastGoodRef.current;
      const borrowedHistorical = lastGood?.data.historicalLanes ?? [];
      // Reconcile the borrowed history against the CURRENT active/blocked set: a
      // run that was historical in last-good but is active or blocked again in
      // this fresh snapshot would otherwise appear in BOTH sets (double-display).
      // Drop any historical lane whose run is now live; recompute the count to
      // match. (A run that completes between wide refreshes still lags into
      // History on the next wide scan — accepted; this only kills the overlap.)
      const liveIds = new Set<string>([
        ...result.data.lanes.map((lane) => lane.id),
        ...result.data.blockedLanes.map((lane) => lane.id),
      ]);
      const historicalLanes = borrowedHistorical.filter((lane) => !liveIds.has(lane.id));
      const dropped = borrowedHistorical.length - historicalLanes.length;
      const totalHistorical = Math.max(0, (lastGood?.data.totalHistorical ?? 0) - dropped);
      return {
        ...result,
        data: {
          ...result.data,
          historicalLanes,
          totalHistorical,
        },
      };
    }
    const lastGood = lastGoodRef.current;
    if (lastGood === null) return result;
    staleDueToFailureRef.current = true;
    return { ...lastGood, status: 'stale' };
  }, []);

  const { data, loading, error, refresh, cheapRefresh } = useCachedData(
    `runs:summary:${cityName ?? 'no-city'}`,
    loadSupervisorRunSummaryPreviewSource,
    {
      refreshFetcher: refreshWithLastGoodRetention,
      sseRefreshFetcher: refreshActiveWithMerge,
    },
  );
  // Capture the latest available snapshot so the next failed refresh can fall
  // back to it. A re-published 'stale' snapshot stays good data, so it keeps
  // being retained across a run of consecutive failures.
  if (data !== undefined && data.status !== 'error') {
    lastGoodRef.current = data;
  }
  const runs = data ?? null;
  const runsStatusRef = useRef<SourceStatus | null>(null);
  runsStatusRef.current = runs?.status ?? null;
  const loadingRef = useRef(loading);
  loadingRef.current = loading;
  const lastRefreshAtRef = useRef(0);

  // Upgrade the cheap first-paint preview to the full session-enriched snapshot
  // exactly once per city — the snapshot the page renders and the badge counts.
  const fullRefreshKeyRef = useRef<string | null>(null);
  useEffect(() => {
    if (runs === null || runs.status === 'error') return;
    const refreshKey = cityName ?? 'no-city';
    if (fullRefreshKeyRef.current === refreshKey) return;
    fullRefreshKeyRef.current = refreshKey;
    void refresh().catch(() => {
      fullRefreshKeyRef.current = null;
    });
  }, [cityName, refresh, runs]);

  // gascity-dashboard-4xcv: bounded auto-retry on a degraded load. An error
  // source (or a partial fetch with zero lanes) used to latch the page dead;
  // retry a few times with backoff and reset once a healthy load lands.
  //
  // A stale-due-to-failure snapshot is degraded too, even though it RENDERS as
  // healthy data: the displayed lanes are last-good (possibly preview-grade),
  // and the refresh that should have upgraded them failed. We must keep
  // attempting recovery off `staleDueToFailureRef` — NOT off the published
  // status — so the view self-recovers to fresh/full data without depending on
  // an unrelated SSE event ever firing. The same backoff budget bounds it; once
  // a genuine fresh result lands, the failure flag is cleared (in
  // refreshWithLastGoodRetention) and the next effect run resets the attempt.
  const retryAttemptRef = useRef(0);
  useEffect(() => {
    if (runs === null) return;
    const degraded =
      runs.status === 'error'
        ? true
        : staleDueToFailureRef.current ||
          (runs.data.lanesPartial === true &&
            runs.data.lanes.length === 0 &&
            runs.data.blockedLanes.length === 0);
    if (!degraded) {
      retryAttemptRef.current = 0;
      return;
    }
    const delay = RETRY_DELAYS_MS[retryAttemptRef.current];
    if (delay === undefined) return;
    retryAttemptRef.current += 1;
    const timer = setTimeout(() => void refresh(), delay);
    return () => clearTimeout(timer);
  }, [runs, refresh]);

  // A bead event arriving while a load is already in flight is queued, not
  // dropped: that load started before the event, so it can return the pre-event
  // snapshot, and dropping the event would latch it stale until the next event
  // (post-mount drift — the exact thing 2j8e.7 exists to kill). One flag
  // coalesces a whole in-flight burst into a single trailing refresh.
  const pendingRefreshRef = useRef(false);
  const trailingTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const runRefresh = useCallback(() => {
    // Any refresh — leading or trailing — satisfies a queued trailing refresh,
    // so cancel a pending trailing timer here rather than only in the effect
    // cleanup: that closes the window where a leading refresh and a due-but-not-
    // yet-fired trailing timer both run and double-fetch inside one floor.
    if (trailingTimerRef.current !== null) {
      clearTimeout(trailingTimerRef.current);
      trailingTimerRef.current = null;
    }
    lastRefreshAtRef.current = Date.now();
    // SSE-driven bursts take the CHEAP path (active-only + merged history); the
    // wide scan is reserved for the manual Refresh button and the one-time
    // first-upgrade.
    void cheapRefresh().catch(() => {
      // Reset on error so the next event retries instead of being silently
      // dropped for the rest of the debounce window.
      lastRefreshAtRef.current = 0;
    });
  }, [cheapRefresh]);

  const onSseMatch = useCallback(() => {
    // Skip when fixtures are serving (supervisor down) — every forced refresh
    // under fixture-fallback re-runs loadFixture(), wasted file IO. An 'error'
    // or 'stale' source is the opposite case: a bead event is exactly the cue
    // to try loading live data again (gascity-dashboard-4xcv).
    if (runsStatusRef.current === null || runsStatusRef.current === 'fixture') return;
    // A refresh is already in flight: a fast SSE event can't race it into a
    // last-write-wins overwrite, but it must not be lost either — queue a single
    // trailing refresh to reconcile once the in-flight load settles.
    if (loadingRef.current) {
      pendingRefreshRef.current = true;
      return;
    }
    const elapsed = Date.now() - lastRefreshAtRef.current;
    if (elapsed < REFRESH_DEBOUNCE_MS) return;
    runRefresh();
  }, [runRefresh]);

  // Trailing edge: fire the queued refresh once the in-flight load settles. The
  // debounce floor still applies on this edge — a slung-pipeline burst becomes
  // one follow-up fan-out, not a hammer (architect H1/H2 upstream-load
  // protection) — so a too-recent refresh defers to the remainder of the floor
  // (remaining 0 → next tick). The handle lives in a ref so runRefresh can
  // cancel it from the leading path too (see runRefresh).
  useEffect(() => {
    if (loading || !pendingRefreshRef.current) return;
    pendingRefreshRef.current = false;
    const remaining = Math.max(0, REFRESH_DEBOUNCE_MS - (Date.now() - lastRefreshAtRef.current));
    trailingTimerRef.current = setTimeout(runRefresh, remaining);
    return () => {
      if (trailingTimerRef.current !== null) {
        clearTimeout(trailingTimerRef.current);
        trailingTimerRef.current = null;
      }
    };
  }, [loading, runRefresh]);

  const sseState = useGcEventRefresh([GC_EVENT_PREFIX.bead], onSseMatch);

  return { source: data, loading, error, refresh, sseState };
}

const RunSummaryContext = createContext<RunSummarySubscription | null>(null);

export function RunSummaryProvider({ children }: { children: ReactNode }) {
  const subscription = useRunSummarySubscription();
  return <RunSummaryContext.Provider value={subscription}>{children}</RunSummaryContext.Provider>;
}

export function useRunSummary(): RunSummarySubscription {
  const subscription = useContext(RunSummaryContext);
  if (subscription === null) {
    throw new Error('useRunSummary must be used within a RunSummaryProvider');
  }
  return subscription;
}
