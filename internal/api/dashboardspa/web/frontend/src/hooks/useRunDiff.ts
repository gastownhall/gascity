import type { RunDiffResponse, RunExecutionPath, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';
import { api } from '../api/client';
import { reportClientError } from '../lib/clientErrorReporting';
import { useCachedData } from './useCachedData';

interface RunDiffState {
  kind: 'idle' | 'loading' | 'ready' | 'failed';
  refresh: () => Promise<void>;
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
  const { data, loading, error, refresh } = useCachedData(
    key,
    () => loadRunDiff(runId, executionPath, scopeKind, scopeRef),
    {
      // Explicit refresh re-reads local git state for the same supervisor-
      // resolved execution path, in lockstep with detail refreshes.
      refreshFetcher: () => loadRunDiff(runId, executionPath, scopeKind, scopeRef, true),
      onError: (err) => {
        if (runId !== undefined) reportRunDiffError('load diff', runId, err);
      },
    },
  );

  if (runId === undefined || executionPath === undefined) {
    return { kind: 'idle', refresh: noopRefresh };
  }
  if (data?.kind === 'loaded') {
    return {
      kind: 'ready',
      diff: data.diff,
      refresh,
      refreshState: refreshState(loading, error),
    };
  }
  if (error !== null) return { kind: 'failed', error, refresh };
  return { kind: 'loading', refresh };
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
