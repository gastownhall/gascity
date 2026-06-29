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

// The endpoint returns 503 while a city's projection is still cold-replaying
// (bounded server-side to ~5s). Retry a few times so the first navigation to a
// large city resolves instead of surfacing a transient "unavailable"; SSE
// refresh and the manual Refresh button recover anything past the budget.
const WARMING_RETRY_DELAYS_MS = [600, 1_200, 2_400];

export async function loadSupervisorFormulaRunDetail(runId: string): Promise<FormulaRunDetail> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      return await api.runDetail(runId);
    } catch (err) {
      const delayMs = WARMING_RETRY_DELAYS_MS[attempt];
      if (err instanceof ApiClientError && err.status === 503 && delayMs !== undefined) {
        await delay(delayMs);
        continue;
      }
      throw err;
    }
  }
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
