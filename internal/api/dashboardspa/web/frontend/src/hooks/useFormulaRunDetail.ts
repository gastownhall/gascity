import type { FormulaRunDetail, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';
import { reportClientError } from '../lib/clientErrorReporting';
import { loadSupervisorFormulaRunDetail } from '../supervisor/runDetail';
import { ApiClientError } from '../api/client';
import { useCachedData } from './useCachedData';

interface FormulaRunDetailState {
  kind: 'idle' | 'loading' | 'ready' | 'failed' | 'unsupported' | 'not_found';
  refresh: () => Promise<void>;
}

type FormulaRunRefreshState =
  | { kind: 'idle' }
  | { kind: 'refreshing' }
  | { kind: 'failed'; error: string };

// gascity-dashboard-9w3k: a v1 / wisp run (not graph.v2) is surfaced in the run
// list but has no graph.v2 step-detail view. The BFF detail endpoint rejects it
// with 422 + reason 'not_run_view' — the RELIABLE v1 signal. We carry that as a
// DISTINCT 'unsupported' payload (not a thrown error → not the generic failed
// state) so the page can render an honest "list-only" message instead of the
// opaque "Formula run unavailable." dead-end.
//
// gascity-dashboard (Major 2): a 404 (no run root in the projection) is
// AMBIGUOUS — it can be a v1/wisp id, a completed run whose events rotated out,
// a pruned/deleted run, or a stale/wrong derived scope. We must NOT assert it is
// definitively v1. It maps to a distinct 'not_found' payload with honest copy
// that lists the possibilities, kept separate from both 'unsupported' (which
// over-claims v1) and the generic transport 'failed' state. A malformed graph.v2
// snapshot (422 + 'invalid_snapshot') stays in that generic 'failed' state.
type FormulaRunDetailPayload =
  | { kind: 'unrequested' }
  | { kind: 'unsupported' }
  | { kind: 'not_found' }
  | {
      kind: 'loaded';
      detail: FormulaRunDetail;
    };

export type FormulaRunDetailLoadState =
  | (FormulaRunDetailState & { kind: 'idle' })
  | (FormulaRunDetailState & { kind: 'loading' })
  | (FormulaRunDetailState & {
      kind: 'ready';
      detail: FormulaRunDetail;
      refreshState: FormulaRunRefreshState;
    })
  | (FormulaRunDetailState & { kind: 'unsupported' })
  | (FormulaRunDetailState & { kind: 'not_found' })
  | (FormulaRunDetailState & { kind: 'failed'; error: string });

export function useFormulaRunDetail(
  runId: string | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): FormulaRunDetailLoadState {
  const key = formulaRunDetailCacheKey(runId, scopeKind, scopeRef);
  const { data, loading, error, refresh } = useCachedData(
    key,
    () => loadFormulaRunDetail(runId),
    {
      onError: (err) => {
        if (runId !== undefined) reportRunDetailError('load detail', runId, err);
      },
    },
  );

  if (runId === undefined) return { kind: 'idle', refresh: noopRefresh };
  if (data?.kind === 'loaded') {
    return {
      kind: 'ready',
      detail: data.detail,
      refresh,
      refreshState: refreshState(loading, error),
    };
  }
  if (data?.kind === 'unsupported') return { kind: 'unsupported', refresh };
  if (data?.kind === 'not_found') return { kind: 'not_found', refresh };
  if (error !== null) return { kind: 'failed', error, refresh };
  return { kind: 'loading', refresh };
}

async function loadFormulaRunDetail(runId: string | undefined): Promise<FormulaRunDetailPayload> {
  if (!runId) return { kind: 'unrequested' };
  try {
    const detail = await loadSupervisorFormulaRunDetail(runId);
    return { kind: 'loaded', detail };
  } catch (err) {
    // gascity-dashboard-9w3k: a v1 / wisp run (not graph.v2) loads but has no
    // graph.v2 step-detail view. The BFF rejects it with 422 + reason
    // 'not_run_view' — the RELIABLE list-only signal — which maps to the
    // distinct 'unsupported' payload so the page renders an honest list-only
    // message instead of a raw error. A malformed graph.v2 snapshot
    // (422 + 'invalid_snapshot') and any other failure propagate as a generic
    // load error.
    if (err instanceof ApiClientError && err.status === 422 && err.reason === 'not_run_view') {
      return { kind: 'unsupported' };
    }
    // gascity-dashboard (Major 2): a 404 (no run root in the projection) is
    // AMBIGUOUS — a v1/wisp id the projection never saw, a completed run whose
    // events rotated out, a pruned/deleted run, or a stale/wrong derived scope.
    // We do NOT claim it is definitively v1; it maps to the distinct 'not_found'
    // payload whose copy lists the possibilities without over-claiming, kept
    // separate from 'unsupported' (which over-claims v1) and the generic
    // transport 'failed' state.
    if (err instanceof ApiClientError && err.status === 404) {
      return { kind: 'not_found' };
    }
    throw err;
  }
}

async function noopRefresh(): Promise<void> {}

function refreshState(loading: boolean, error: string | null): FormulaRunRefreshState {
  if (error !== null) return { kind: 'failed', error };
  return loading ? { kind: 'refreshing' } : { kind: 'idle' };
}

function reportRunDetailError(operation: string, runId: string, err: unknown): void {
  void reportClientError({
    component: 'formula-run-detail',
    operation,
    message: `${runId}: ${errorMessage(err)}`,
  });
}

export function formulaRunDetailCacheKey(
  runId: string | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): string {
  // gascity-dashboard (bvu4): runId and scopeRef can both contain ':'
  // (SCOPE_REF_RE permits it, e.g. 'rig:foo'), so a bare ':'-join lets two
  // distinct (runId, scopeKind, scopeRef) tuples collapse to one key — a refresh
  // for run B then serves or overwrites run A's cached detail. Percent-encode
  // each part so the delimiter can never shift a boundary.
  const parts = ['formula-run', runId ?? 'missing', scopeKind ?? 'default', scopeRef ?? 'default'];
  return parts.map(encodeURIComponent).join(':');
}
