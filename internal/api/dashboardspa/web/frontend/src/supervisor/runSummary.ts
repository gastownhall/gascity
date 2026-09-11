import type { RunSummary, SourceAvailableState, SourceState } from 'gas-city-dashboard-shared';
import { api } from '../api/client';

// The run summary is served by one warm GET to the BFF run-projection endpoint
// (internal/api/dashboardbff/runtailer.go): a sub-second fold of the per-city
// event log that already layers session health/census and the monotonic
// thrash/progress marks server-side. The four exported source loaders —
// historically a tight "mount" read, a wide refresh, a cheap active-only SSE
// read, and a first-paint preview — collapse to that single complete read. The
// old client-side fold (buildRunSummary + enrichRunSummary over molecule/feed/
// per-rig/session fan-out) is gone, and with it the cheap/wide split that only
// existed because those scans were slow.
const RUNS_STALE_AFTER_MS = 60 * 1000;

async function loadRunSummarySource(): Promise<SourceState<RunSummary>> {
  const fetchedAt = new Date().toISOString();
  try {
    const summary = await api.runSummary();
    const source: SourceAvailableState<RunSummary> = {
      source: 'runs',
      status: 'fresh',
      fetchedAt,
      staleAt: new Date(Date.parse(fetchedAt) + RUNS_STALE_AFTER_MS).toISOString(),
      error: { kind: 'none' },
      data: summary,
    };
    return source;
  } catch (err) {
    return {
      source: 'runs',
      status: 'error',
      error: errorMessage(err, 'formula runs unavailable'),
    };
  }
}

/**
 * The authoritative run-summary source for the shared subscription
 * (runs/runSummarySubscription) — the snapshot the /runs page renders and the
 * nav attention badge counts off one fetch.
 */
export function loadSupervisorRunSummarySource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource();
}

/** Mount / first-paint source for Home and Formula Run Detail. */
export function loadSupervisorRunSummaryMountSource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource();
}

/** Cheap SSE-burst refresh source for the shared subscription. */
export function loadSupervisorRunSummaryActiveSource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource();
}

/** First-paint preview source for the shared subscription. */
export function loadSupervisorRunSummaryPreviewSource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource();
}

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error && err.message.trim().length > 0 ? err.message : fallback;
}
