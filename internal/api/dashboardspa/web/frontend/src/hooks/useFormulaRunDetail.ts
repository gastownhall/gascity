import type { FormulaRunDetail, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage, UnsupportedRunError } from 'gas-city-dashboard-shared';
import { reportClientError } from '../lib/clientErrorReporting';
import { loadSupervisorFormulaRunDetail } from '../supervisor/runDetail';
import { SupervisorApiError } from '../supervisor/errors';
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
// list but has no graph.v2 step-detail view. When its snapshot LOADS but isn't a
// run view, enrichFormulaRun throws UnsupportedRunError('not_run_view') — the
// RELIABLE v1 signal. We carry that as a DISTINCT 'unsupported' payload (not a
// thrown error → not the generic failed state) so the page can render an honest
// "list-only" message instead of the opaque "Formula run unavailable." dead-end.
//
// gascity-dashboard (Major 2): a raw SupervisorApiError 404 (no snapshot at all)
// is AMBIGUOUS — it can be a v1/wisp id the workflow endpoint never knew, a
// completed run whose snapshot wasn't retained, a pruned/deleted run, or a
// stale/wrong derived scope. We must NOT assert it is definitively v1. It maps
// to a distinct 'not_found' payload with honest copy that lists the
// possibilities, kept separate from both 'unsupported' (which over-claims v1)
// and the generic transport 'failed' state. No shared wire-shape field is added.
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
    () => loadFormulaRunDetail(runId, scopeKind, scopeRef),
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

async function loadFormulaRunDetail(
  runId: string | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): Promise<FormulaRunDetailPayload> {
  if (!runId) return { kind: 'unrequested' };
  const params: { scopeKind?: RunScopeKind; scopeRef?: string } = {};
  if (scopeKind !== undefined) params.scopeKind = scopeKind;
  if (scopeRef !== undefined) params.scopeRef = scopeRef;
  try {
    const detail = await loadSupervisorFormulaRunDetail(runId, params.scopeKind, params.scopeRef);
    return { kind: 'loaded', detail };
  } catch (err) {
    // gascity-dashboard-9w3k: a snapshot that LOADS but isn't a graph.v2 run
    // view throws UnsupportedRunError('not_run_view'). That is the RELIABLE v1 /
    // wisp signal, so it maps to the 'unsupported' payload and the page renders
    // the honest list-only message instead of a raw error.
    if (err instanceof UnsupportedRunError && err.reason === 'not_run_view') {
      return { kind: 'unsupported' };
    }
    // gascity-dashboard (Major 2): a raw SupervisorApiError 404 (no snapshot at
    // all) is AMBIGUOUS — v1/wisp id the workflow endpoint never knew, a
    // completed run whose snapshot wasn't retained, a pruned/deleted run, or a
    // stale/wrong derived scope. We do NOT claim it is definitively v1; it maps
    // to the distinct 'not_found' payload whose copy lists the possibilities
    // without over-claiming. A malformed graph.v2 snapshot ('invalid_snapshot')
    // and any other transport failure still propagate as a generic load error.
    if (err instanceof SupervisorApiError && err.status === 404) {
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
