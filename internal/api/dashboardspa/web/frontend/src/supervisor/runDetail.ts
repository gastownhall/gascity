import type { FormulaRunDetail } from 'gas-city-dashboard-shared';
import { api, ApiClientError } from '../api/client';

// The run-detail view reads from the BFF run-projection endpoint
// (GET /api/city/{city}/runs/{runId}/detail): one warm read of the same fold
// the summary uses, so detail stages == summary stages by construction. The
// whole client-side detail pipeline (the workflowRun snapshot + formulaDetail
// fetch + enrichFormulaRun) moved to Go (internal/runproj.BuildRunDetail) and
// is golden-gated byte-for-byte. Scope is no longer threaded into the read —
// the projection derives a run's scope from its own root bead — though the
// route still parses scope for the separate run-diff endpoint.

// A warming 503 is a poll signal, not a failure. The BFF answers 503 both
// while a city's projection cold-replays (usually clears within ~5s) and — the
// deep-link case — while a truly-unknown runId sits inside its post-sling
// grace window (rundetail_grace.go, 180s): a run slung from the CLI stays
// invisible to the projection until the controller's cache-reconcile emits its
// bead events, 30-120s later. So the loader polls: the fast delays first, then
// a capped cadence, for a total budget sized to the server's grace window.
// A non-503 transient failure (a 5xx upstream-proxy blip, a network-level
// fetch reject) gets ONLY the fast delays, restoring the pre-cutover
// single-transient-retry resilience. A 4xx (404 unknown run, 422 unsupported)
// is definitive and surfaces immediately; SSE refresh and the manual Refresh
// button recover anything past the budget.
const WARMING_RETRY_DELAYS_MS = [600, 1_200, 2_400];
// The capped poll cadence once the fast delays are spent. Matches the
// Retry-After: 5 the BFF pins on the graced unknown-run 503; ApiClientError
// does not carry response headers, so the pinned value is encoded here rather
// than read per-response.
const WARMING_POLL_CAP_MS = 5_000;
// Total warming-poll budget, sized to the BFF's unknown-run grace window
// (unknownRunWarmingGrace = 180s). When the budget is spent the last 503
// surfaces and the caller falls through to its failed state.
const WARMING_POLL_BUDGET_MS = 180_000;

/**
 * A warming 503 observed while the loader polls. `reason` is the BFF's
 * discriminator: 'unknown_run' when the projection is warm but has not seen
 * this run yet (the just-slung grace window); undefined while the projection
 * itself is still cold-replaying.
 */
export interface RunDetailWarming {
  reason: string | undefined;
}

/** Optional hooks into {@link loadSupervisorFormulaRunDetail}'s warming poll. */
export interface LoadRunDetailOptions {
  /**
   * Called on each warming 503 before the next poll, so the caller can render
   * honest interim copy (e.g. "this run may still be being recorded") instead
   * of an anonymous spinner or a premature failure.
   */
  onWarming?: (warming: RunDetailWarming) => void;
  /**
   * Polled between attempts; return false to stop (the caller navigated away
   * or superseded this load with a fresh one). The pending error surfaces
   * immediately and no further GET is issued.
   */
  keepPolling?: () => boolean;
}

/**
 * Load a run's detail DTO from the BFF run-projection endpoint, polling
 * through warming 503s (see the retry policy above) and retrying other
 * transient failures a few times before surfacing one.
 */
export async function loadSupervisorFormulaRunDetail(
  runId: string,
  options?: LoadRunDetailOptions,
): Promise<FormulaRunDetail> {
  let elapsedMs = 0;
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await api.runDetail(runId);
    } catch (err) {
      const delayMs = retryDelayMs(err, attempt, elapsedMs);
      if (delayMs === undefined || options?.keepPolling?.() === false) throw err;
      if (isWarmingError(err)) options?.onWarming?.({ reason: err.reason });
      elapsedMs += delayMs;
      await delay(delayMs);
      // Re-check after the delay so a superseded poll stops BEFORE issuing
      // another GET (the failure-time check above already let this attempt's
      // delay be scheduled).
      if (options?.keepPolling?.() === false) throw err;
    }
  }
}

// A warming 503 polls on the extended schedule: the fast delays, then the
// capped cadence, until the next delay would overrun the grace-window budget.
// Any other transient failure gets only the fast delays. Undefined = give up.
function retryDelayMs(err: unknown, attempt: number, elapsedMs: number): number | undefined {
  if (isWarmingError(err)) {
    const delayMs = WARMING_RETRY_DELAYS_MS[attempt] ?? WARMING_POLL_CAP_MS;
    return elapsedMs + delayMs <= WARMING_POLL_BUDGET_MS ? delayMs : undefined;
  }
  return isTransientDetailError(err) ? WARMING_RETRY_DELAYS_MS[attempt] : undefined;
}

// The BFF's warming signal: 503 while the projection cold-replays (no reason)
// or while an unknown run is inside its grace window (reason 'unknown_run').
function isWarmingError(err: unknown): err is ApiClientError {
  return err instanceof ApiClientError && err.status === 503;
}

// A 4xx (404/422) is a definitive answer about the run — never retry it. A
// non-warming 5xx is transient, as is a network-level fetch reject (a
// TypeError, e.g. "Failed to fetch"); a malformed-body decode error
// (ApiResponseDecodeError) is NOT transient and surfaces immediately.
function isTransientDetailError(err: unknown): boolean {
  if (err instanceof ApiClientError) return err.status >= 500;
  return err instanceof TypeError;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
