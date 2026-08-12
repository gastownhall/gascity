import { useCallback, useEffect, useRef, useState } from 'react';
import type { FormulaRunDetail, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';
import { reportClientError } from '../lib/clientErrorReporting';
import {
  loadSupervisorFormulaRunDetail,
  type LoadRunDetailOptions,
  type RunDetailWarming,
} from '../supervisor/runDetail';
import { ApiClientError } from '../api/client';
import { useCachedData } from './useCachedData';
import { useFormulaRunDetailStream } from './useFormulaRunDetailStream';

interface FormulaRunDetailState {
  kind: 'idle' | 'loading' | 'ready' | 'failed' | 'unsupported' | 'not_found';
  refresh: () => Promise<void>;
  /**
   * Whether the per-run SSE stream can still carry detail updates, so the caller
   * can leave detail to the stream and nudge only the diff. False when the runtime
   * has no EventSource (SSR/tests, or a browser without it) OR when the stream has
   * terminally closed (a fatal precheck: 422/404/503, which EventSource does not
   * retry) — both mean detail would freeze unless the nudge keeps refreshing it. A
   * transient reconnect ('connecting') stays true because the browser self-heals
   * it. See the P4 stream-vs-nudge division in FormulaRunDetail.
   */
  streamActive: boolean;
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
  // F4: while the initial load polls the BFF's warming 503s (the just-slung
  // deep-link case, up to ~180s), `warming` carries the loader's signal — with
  // reason 'unknown_run' when the projection is warm but has not seen the run
  // yet — so the route can render honest "may still be being recorded" copy
  // instead of an anonymous spinner. Null while no warming 503 has been seen.
  | (FormulaRunDetailState & { kind: 'loading'; warming: RunDetailWarming | null })
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
  // F4: the loader polls warming 503s for up to ~180s (the just-slung
  // deep-link grace window). Each fetcher invocation gets a generation; a
  // newer invocation (key change, manual refresh, nudge) or unmount
  // supersedes older polls via keepPolling, so a superseded poll stops
  // issuing GETs and its warming signal can never overwrite the current
  // load's state. The settled poll clears its own warming signal so the
  // failed/ready states never carry stale interim copy.
  const [warming, setWarming] = useState<RunDetailWarming | null>(null);
  const pollGenRef = useRef(0);
  useEffect(
    () => () => {
      pollGenRef.current += 1;
    },
    [],
  );
  const {
    data,
    loading,
    error,
    refresh: cachedRefresh,
  } = useCachedData(
    key,
    () => {
      const gen = ++pollGenRef.current;
      const isCurrent = () => pollGenRef.current === gen;
      const load = loadFormulaRunDetail(runId, {
        onWarming: (next) => {
          if (isCurrent()) setWarming(next);
        },
        keepPolling: isCurrent,
      });
      return load.finally(() => {
        if (isCurrent()) setWarming(null);
      });
    },
    {
      onError: (err) => {
        if (runId !== undefined) reportRunDetailError('load detail', runId, err);
      },
    },
  );

  // P4: the per-run SSE stream pushes the whole DTO, so a pushed frame becomes
  // the rendered detail with ZERO refetch. The stream hook warms the SWR cache
  // on every frame; we mirror the freshest frame into local state so the render
  // updates immediately without waiting for a cache re-read. The initial GET
  // (useCachedData) is still first paint and the fallback when the stream is
  // unavailable or errors. The frame is tagged with the cache key the STREAM was
  // opened for (passed in by the stream hook), not this render's key, so a frame
  // that arrives mid-navigation A→B — while the callback ref already points here
  // but A's EventSource has not yet been closed — is stored under A's key and
  // dropped by the `streamed.key === key` guard below rather than flashing A's
  // detail under run B.
  const [streamed, setStreamed] = useState<{ key: string; detail: FormulaRunDetail } | null>(null);
  const onStreamDetail = useCallback(
    (detail: FormulaRunDetail, frameKey: string) => setStreamed({ key: frameKey, detail }),
    [],
  );
  // F4: don't open the stream for a run the GET has definitively resolved as
  // non-streamable — an unsupported (v1/wisp, 422) or not_found (404) run has no
  // graph.v2 detail to push, and connecting would just spend one wasted
  // EventSource that the browser reaps on the fatal 4xx precheck. Any other state
  // (loading, ready, transient failed) keeps the stream enabled.
  const streamEnabled =
    runId !== undefined && data?.kind !== 'unsupported' && data?.kind !== 'not_found';
  const streamState = useFormulaRunDetailStream(
    runId,
    streamEnabled,
    onStreamDetail,
    scopeKind,
    scopeRef,
  );
  const streamedDetail = streamed?.key === key ? streamed.detail : null;

  // A live stream carries detail, so the nudge lane refreshes only the diff. Only
  // 'open' and 'connecting' count as active: 'connecting' is a transient reconnect
  // the browser self-heals (readyState CONNECTING), so it briefly keeps the nudge
  // on diff-only, which is fine. 'closed' is TERMINAL — EventSource sets it after a
  // non-200/wrong-content-type precheck (422/404/503) and never reconnects, so the
  // stream will push no further detail; it must release the nudge back to detail,
  // exactly like 'unavailable' (no EventSource), or the view freezes at the last
  // GET/frame. See the P4 stream-vs-nudge division in FormulaRunDetail.
  const streamActive = streamState === 'open' || streamState === 'connecting';

  // A manual refresh must clear the pinned streamed frame so the fresh GET
  // renders (otherwise streamedDetail permanently shadows the refetch — the
  // Refresh button would appear to no-op for the detail). The next stream frame
  // re-populates it. Streaming keeps the render live between refreshes.
  const refresh = useCallback(async () => {
    setStreamed(null);
    await cachedRefresh();
  }, [cachedRefresh]);

  if (runId === undefined) return { kind: 'idle', refresh: noopRefresh, streamActive };
  // A pushed frame wins over the GET-backed cache value: it is the freshest
  // snapshot of this run's detail. It stays subordinate to a definitive
  // unsupported/not_found answer below (those come only from the GET precheck;
  // the stream never delivers a frame for them).
  const detail = streamedDetail ?? (data?.kind === 'loaded' ? data.detail : null);
  if (detail !== null) {
    return {
      kind: 'ready',
      detail,
      refresh,
      refreshState: refreshState(loading, error),
      streamActive,
    };
  }
  if (data?.kind === 'unsupported') return { kind: 'unsupported', refresh, streamActive };
  if (data?.kind === 'not_found') return { kind: 'not_found', refresh, streamActive };
  if (error !== null) return { kind: 'failed', error, refresh, streamActive };
  return { kind: 'loading', warming, refresh, streamActive };
}

async function loadFormulaRunDetail(
  runId: string | undefined,
  options?: LoadRunDetailOptions,
): Promise<FormulaRunDetailPayload> {
  if (!runId) return { kind: 'unrequested' };
  try {
    const detail = await loadSupervisorFormulaRunDetail(runId, options);
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
