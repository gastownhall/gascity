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

// Retry transient failures a few times before surfacing one: the BFF's 503
// warming signal while a city's projection cold-replays (bounded server-side to
// ~5s), a 5xx upstream-proxy blip, or a network-level fetch reject. This
// restores the single-transient-retry resilience the pre-cutover supervisor
// read had (fetchCoreRead). A 4xx (404 unknown run, 422 unsupported) is
// definitive and surfaces immediately; SSE refresh and the manual Refresh
// button recover anything past the budget.
const WARMING_RETRY_DELAYS_MS = [600, 1_200, 2_400];

export async function loadSupervisorFormulaRunDetail(runId: string): Promise<FormulaRunDetail> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await api.runDetail(runId);
    } catch (err) {
      const delayMs = WARMING_RETRY_DELAYS_MS[attempt];
      if (delayMs !== undefined && isTransientDetailError(err)) {
        await delay(delayMs);
        continue;
      }
      throw err;
    }
  }
}

// A 4xx (404/422) is a definitive answer about the run — never retry it. The
// BFF's 503 warming signal and any 5xx are transient, as is a network-level
// fetch reject (a TypeError, e.g. "Failed to fetch"); a malformed-body decode
// error (ApiResponseDecodeError) is NOT transient and surfaces immediately.
function isTransientDetailError(err: unknown): boolean {
  if (err instanceof ApiClientError) return err.status >= 500;
  return err instanceof TypeError;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
