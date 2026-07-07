import type { RunDiffResponse, RunExecutionPath, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';
import { api } from '../api/client';
import { reportClientError } from '../lib/clientErrorReporting';
import { useCachedData } from './useCachedData';

interface RunDiffState {
  kind: 'idle' | 'loading' | 'ready' | 'failed';
  /** TTL-bypassing refresh — the manual Refresh lane; re-runs the git diff. */
  refresh: () => Promise<void>;
  /**
   * TTL-absorbed refresh for high-frequency event-driven nudges. Omits the
   * refresh flag (the manual lane sets refresh=true) as a forward-compat client
   * contract for a future server-side diff TTL cache; today the flag is dropped
   * on the wire, so the live burst protection is the tab-gating + event
   * coalescing, not the flag. Use for the bead/session nudge.
   */
  cheapRefresh: () => Promise<void>;
}

type RunDiffRefreshState =
  | { kind: 'idle' }
  | { kind: 'refreshing' }
  | { kind: 'failed'; error: string };

type RunDiffPayload =
  | { kind: 'unrequested' }
  | {
      kind: 'loaded';
      diff: RunDiffResponse;
    };

export type RunDiffLoadState =
  | (RunDiffState & { kind: 'idle' })
  | (RunDiffState & { kind: 'loading' })
  | (RunDiffState & {
      kind: 'ready';
      diff: RunDiffResponse;
      refreshState: RunDiffRefreshState;
    })
  | (RunDiffState & { kind: 'failed'; error: string });

export function useRunDiff(
  runId: string | undefined,
  executionPath: RunExecutionPath | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): RunDiffLoadState {
  const key = runDiffCacheKey(runId, executionPath, scopeKind, scopeRef);
  const { data, loading, error, refresh, cheapRefresh } = useCachedData(
    key,
    () => loadRunDiff(runId, executionPath, scopeKind, scopeRef),
    {
      // refresh=true/false is a forward-compat CLIENT contract for a future
      // server-side diff cache: the manual Refresh button takes the bypass lane
      // (refresh=true), event-driven nudges take the cheap lane (refresh=false).
      // NOTE: today the diff endpoint has NO cache and runQuery drops the flag,
      // so both lanes issue an identical git read on the wire — this phase's live
      // win is the tab-gating in FormulaRunDetail (the diff refetches only while
      // the Diff tab is open), not the flag. When a server diff cache lands, the
      // cheap lane already lets it absorb bursts with no client change.
      refreshFetcher: () => loadRunDiff(runId, executionPath, scopeKind, scopeRef, true),
      sseRefreshFetcher: () => loadRunDiff(runId, executionPath, scopeKind, scopeRef, false),
      onError: (err) => {
        if (runId !== undefined) reportRunDiffError('load diff', runId, err);
      },
    },
  );

  if (runId === undefined || executionPath === undefined) {
    return { kind: 'idle', refresh: noopRefresh, cheapRefresh: noopRefresh };
  }
  if (data?.kind === 'loaded') {
    return {
      kind: 'ready',
      diff: data.diff,
      refresh,
      cheapRefresh,
      refreshState: refreshState(loading, error),
    };
  }
  if (error !== null) return { kind: 'failed', error, refresh, cheapRefresh };
  return { kind: 'loading', refresh, cheapRefresh };
}

async function loadRunDiff(
  runId: string | undefined,
  executionPath: RunExecutionPath | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
  refresh?: boolean,
): Promise<RunDiffPayload> {
  if (!runId || executionPath === undefined) return { kind: 'unrequested' };
  const params: { scopeKind?: RunScopeKind; scopeRef?: string; refresh?: boolean } = {};
  if (scopeKind !== undefined) params.scopeKind = scopeKind;
  if (scopeRef !== undefined) params.scopeRef = scopeRef;
  if (refresh) params.refresh = true;
  const diff = await api.runDiff(runId, { executionPath }, params);
  return { kind: 'loaded', diff };
}

async function noopRefresh(): Promise<void> {}

function refreshState(loading: boolean, error: string | null): RunDiffRefreshState {
  if (error !== null) return { kind: 'failed', error };
  return loading ? { kind: 'refreshing' } : { kind: 'idle' };
}

function reportRunDiffError(operation: string, runId: string, err: unknown): void {
  void reportClientError({
    component: 'formula-run-detail',
    operation,
    message: `${runId}: ${errorMessage(err)}`,
  });
}

function runDiffCacheKey(
  runId: string | undefined,
  executionPath: RunExecutionPath | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): string {
  const parts = [
    'formula-run-diff',
    runId ?? 'missing',
    executionPathCacheKey(executionPath),
    scopeKind ?? 'default',
    scopeRef ?? 'default',
  ];
  return parts.join(':');
}

function executionPathCacheKey(executionPath: RunExecutionPath | undefined): string {
  if (executionPath === undefined) return 'path:missing';
  if (executionPath.kind === 'known') return `path:${executionPath.path}`;
  return `path:${executionPath.reason}`;
}
