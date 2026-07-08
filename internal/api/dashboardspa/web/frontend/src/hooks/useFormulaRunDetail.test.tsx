import { act, cleanup, renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import type { FormulaRunDetail } from 'gas-city-dashboard-shared';
import { invalidate } from '../api/cache';
import { ApiClientError } from '../api/client';
import { reportClientError } from '../lib/clientErrorReporting';
import { loadSupervisorFormulaRunDetail } from '../supervisor/runDetail';
import { formulaRunDetailCacheKey, useFormulaRunDetail } from './useFormulaRunDetail';

vi.mock('../api/cityBase', () => ({
  getActiveCity: () => 'test-city',
  activeCityOrThrow: () => 'test-city',
  cityPath: (suffix: string) => `/api/city/test-city${suffix}`,
}));

vi.mock('../lib/clientErrorReporting', () => ({
  reportClientError: vi.fn(() => Promise.resolve({ status: 'reported' })),
}));

// The run-detail loader is now a thin BFF GET (covered by runDetail.test.ts);
// the hook's job is purely mapping its result/errors onto the view states, so
// mock the loader directly and drive each state from what it resolves/throws.
vi.mock('../supervisor/runDetail', () => ({
  loadSupervisorFormulaRunDetail: vi.fn(),
}));

const mockReportClientError = reportClientError as Mock;
const mockLoadDetail = loadSupervisorFormulaRunDetail as Mock;

function runDetail(overrides: Partial<FormulaRunDetail> = {}): FormulaRunDetail {
  return {
    runId: 'wf-1',
    rootBeadId: 'wf-1',
    rootStoreRef: 'city:test-city',
    resolvedRootStore: 'city:test-city',
    scopeKind: 'city',
    scopeRef: 'test-city',
    title: 'Direct supervisor run',
    formula: { kind: 'known', name: 'mol-test', source: 'metadata' },
    formulaDetail: { kind: 'available', name: 'mol-test', target: 'test-city/codex' },
    executionPath: { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' },
    snapshotVersion: 1,
    snapshotEventSeq: { kind: 'known', seq: 100 },
    completeness: { kind: 'complete' },
    progress: {
      snapshotVersion: 1,
      snapshotEventSeq: { kind: 'known', seq: 100 },
      snapshotPartial: false,
      terminal: false,
      totalNodeCount: 0,
      visibleNodeCount: 0,
      edgeCount: 0,
      executionInstanceCount: 0,
      sessionLinkCount: 0,
      streamableSessionCount: 0,
      streamableSessionIds: [],
      statusCounts: {},
      allStatusCounts: {},
    },
    phase: 'intake',
    stages: [],
    nodes: [],
    edges: [],
    lanes: [],
    ...overrides,
  };
}

afterEach(() => {
  cleanup();
  invalidate('');
  vi.clearAllMocks();
});

describe('useFormulaRunDetail', () => {
  beforeEach(() => {
    mockLoadDetail.mockResolvedValue(runDetail());
  });

  it('does not fetch or report when no run id is available', async () => {
    const { result } = renderHook(() => useFormulaRunDetail(undefined));

    await waitFor(() => expect(result.current.kind).toBe('idle'));

    expect(mockLoadDetail).not.toHaveBeenCalled();
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('reports run detail load failures to the centralized client log', async () => {
    mockLoadDetail.mockRejectedValue(new Error('detail unavailable'));

    const { result } = renderHook(() => useFormulaRunDetail('wf-1'));

    await waitFor(() =>
      expect(result.current).toMatchObject({ kind: 'failed', error: 'detail unavailable' }),
    );

    expect(mockReportClientError).toHaveBeenCalledWith({
      component: 'formula-run-detail',
      operation: 'load detail',
      message: 'wf-1: detail unavailable',
    });
  });

  it('loads formula run detail from the BFF projection endpoint', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('ready'));

    if (result.current.kind !== 'ready') throw new Error('run detail did not load');
    expect(result.current.detail.runId).toBe('wf-1');
    expect(result.current.detail.title).toBe('Direct supervisor run');
    expect(result.current.refreshState).toEqual({ kind: 'idle' });
    expect('diff' in result.current).toBe(false);
    // The loader is scope-independent now (the projection derives scope from the
    // run's own root bead); the route's scope still drives only the cache key.
    expect(mockLoadDetail).toHaveBeenCalledWith('wf-1');
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('reaches ready for a run that lacks formula metadata (no hang)', async () => {
    mockLoadDetail.mockResolvedValue(
      runDetail({
        formula: { kind: 'unavailable', reason: 'missing_formula_metadata' },
        formulaDetail: { kind: 'unavailable', reason: 'missing_formula_metadata' },
      }),
    );

    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('ready'));

    if (result.current.kind !== 'ready') throw new Error('run detail did not load');
    expect(result.current.detail.formula).toEqual({
      kind: 'unavailable',
      reason: 'missing_formula_metadata',
    });
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('surfaces a 422 not_run_view as unsupported, not a generic failure', async () => {
    // A v1 / wisp run loads server-side but is not a graph.v2 run, so the BFF
    // returns 422 + reason 'not_run_view'. The hook maps ONLY that case to
    // {kind:'unsupported'} (list-only message), never the error path.
    mockLoadDetail.mockRejectedValue(
      new ApiClientError(422, 'run is not a graph.v2 run', undefined, 'not_run_view'),
    );

    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('unsupported'));
    expect(mockReportClientError).not.toHaveBeenCalled();
  });

  it('surfaces a 422 invalid_snapshot as a generic load failure, not unsupported', async () => {
    // A malformed graph.v2 snapshot is a genuine load failure: it must propagate
    // to the generic 'failed' state, distinct from the honest v1 'unsupported'.
    mockLoadDetail.mockRejectedValue(
      new ApiClientError(422, 'run snapshot is invalid', undefined, 'invalid_snapshot'),
    );

    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('failed'));
  });

  it('surfaces an exhausted 503 warming budget as a generic failure, never not_found/unsupported', async () => {
    // The loader retries 503 internally and, once the budget is spent, re-throws
    // the ApiClientError(503). The hook must route that to the generic 'failed'
    // state — a 503 is neither an honest list-only run nor a missing one.
    mockLoadDetail.mockRejectedValue(new ApiClientError(503, 'run view is warming'));

    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('failed'));
    expect(result.current.kind).not.toBe('not_found');
    expect(result.current.kind).not.toBe('unsupported');
  });

  it('surfaces a 404 as not_found, not v1-unsupported', async () => {
    // gascity-dashboard (Major 2): a 404 (no run root in the projection) is
    // AMBIGUOUS — a v1/wisp id, a completed run whose events rotated out, a
    // pruned run, or a wrong derived scope. It gets its own honest 'not_found'
    // state, never mislabeled as the definitive v1 'unsupported'.
    mockLoadDetail.mockRejectedValue(new ApiClientError(404, 'unknown run'));

    const { result } = renderHook(() => useFormulaRunDetail('gc-p7yf1m', 'city', 'test-city'));

    await waitFor(() => expect(result.current.kind).toBe('not_found'));
    expect(result.current.kind).not.toBe('unsupported');
    expect(result.current.kind).not.toBe('failed');
    expect(mockReportClientError).not.toHaveBeenCalled();
  });
});

describe('useFormulaRunDetail SSE stream integration (P4)', () => {
  const eventSources = streamEventSources;

  beforeEach(() => {
    eventSources.length = 0;
    vi.stubGlobal('EventSource', StreamFakeEventSource);
    mockLoadDetail.mockResolvedValue(runDetail({ title: 'first paint (GET)' }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders a pushed frame with ZERO extra GET after first paint', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));

    // First paint comes from the GET (one call).
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    if (result.current.kind !== 'ready') throw new Error('did not reach ready');
    expect(result.current.detail.title).toBe('first paint (GET)');
    expect(mockLoadDetail).toHaveBeenCalledTimes(1);

    // A pushed frame must update the rendered detail — and must NOT trigger any
    // additional GET.
    act(() => eventSources[0]?.open());
    act(() =>
      eventSources[0]?.emit('detail', JSON.stringify(runDetail({ title: 'pushed frame' }))),
    );

    await waitFor(() => {
      if (result.current.kind !== 'ready') throw new Error('lost ready');
      expect(result.current.detail.title).toBe('pushed frame');
    });
    expect(mockLoadDetail).toHaveBeenCalledTimes(1); // still exactly the first-paint GET
  });

  it('falls back to the GET-backed value when the stream errors (no frame)', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));

    // Stream errors before delivering any frame: the GET first-paint value stays
    // rendered (the fallback), never blanked.
    act(() => eventSources[0]?.fail());
    if (result.current.kind !== 'ready') throw new Error('fallback lost ready');
    expect(result.current.detail.title).toBe('first paint (GET)');
  });

  it('closes the stream on unmount', async () => {
    const { result, unmount } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    expect(eventSources[0]?.closed).toBe(false);
    unmount();
    expect(eventSources[0]?.closed).toBe(true);
  });

  it('renders the fresh GET on a manual Refresh even after a stream frame (F3)', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));

    // A stream frame pins the rendered detail.
    act(() => eventSources[0]?.open());
    act(() => eventSources[0]?.emit('detail', JSON.stringify(runDetail({ title: 'streamed' }))));
    await waitFor(() => {
      if (result.current.kind !== 'ready') throw new Error('lost ready');
      expect(result.current.detail.title).toBe('streamed');
    });

    // A manual Refresh returns a NEWER GET result; it must render (the streamed
    // frame must not permanently shadow the refetch — the Refresh no-op bug).
    mockLoadDetail.mockResolvedValueOnce(runDetail({ title: 'manual refresh GET' }));
    if (result.current.kind !== 'ready') throw new Error('lost ready before refresh');
    await act(async () => {
      await result.current.refresh();
    });

    await waitFor(() => {
      if (result.current.kind !== 'ready') throw new Error('lost ready after refresh');
      expect(result.current.detail.title).toBe('manual refresh GET');
    });
  });

  it('reports streamActive true when EventSource is present', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    expect(result.current.streamActive).toBe(true);
  });

  it('releases the nudge fallback (streamActive false) when the stream terminally closes', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));

    // A live stream carries detail, so the nudge lane refreshes only the diff.
    act(() => eventSources[0]?.open());
    await waitFor(() => expect(result.current.streamActive).toBe(true));

    // A fatal precheck (422/404/503) sets EventSource CLOSED with no reconnect,
    // so the stream will push no further detail. streamActive MUST flip false so
    // runDetailNudgeRefresh (tested directly in FormulaRunDetail.test.tsx) resumes
    // refreshing detail as well as the diff — otherwise detail freezes at the last
    // frame forever. A terminal close previously stayed "active" and froze detail.
    act(() => eventSources[0]?.fail());
    await waitFor(() => expect(result.current.streamActive).toBe(false));
  });

  it('keeps a ready run streaming but tears the stream down once the run resolves unsupported (F4)', async () => {
    const { result: readyResult } = renderHook(() =>
      useFormulaRunDetail('wf-ready', 'city', 'test-city'),
    );
    await waitFor(() => expect(readyResult.current.kind).toBe('ready'));
    expect(eventSources).toHaveLength(1);
    expect(eventSources[0]?.closed).toBe(false); // ready run → stream stays open

    eventSources.length = 0;
    mockLoadDetail.mockRejectedValueOnce(
      new ApiClientError(422, 'run is not a graph.v2 run', undefined, 'not_run_view'),
    );
    const { result: unsupportedResult } = renderHook(() =>
      useFormulaRunDetail('wf-v1', 'city', 'test-city'),
    );
    await waitFor(() => expect(unsupportedResult.current.kind).toBe('unsupported'));
    // The stream may open optimistically during loading, but once the GET
    // resolves the run as definitively non-streamable (422 not_run_view) it must
    // be torn down — never left open on a fatal 4xx (F4).
    await waitFor(() => {
      expect(eventSources.every((source) => source.closed)).toBe(true);
    });
  });
});

describe('useFormulaRunDetail without EventSource (F2)', () => {
  beforeEach(() => {
    // No EventSource stub → the stream is permanently unavailable.
    vi.stubGlobal('EventSource', undefined);
    mockLoadDetail.mockResolvedValue(runDetail({ title: 'no-stream GET' }));
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('reports streamActive false so the caller keeps detail on the nudge', async () => {
    const { result } = renderHook(() => useFormulaRunDetail('wf-1', 'city', 'test-city'));
    await waitFor(() => expect(result.current.kind).toBe('ready'));
    expect(result.current.streamActive).toBe(false);
  });
});

describe('formulaRunDetailCacheKey (bvu4)', () => {
  // SCOPE_REF_RE permits ':' in scopeRef (and run ids can carry it), so a bare
  // ':'-join let two distinct (runId, scopeKind, scopeRef) tuples collapse to the
  // same key — a refresh for one run then served/overwrote another run's detail.
  it('does not collide when a colon-bearing part shifts the join boundary', () => {
    // Both tuples produced the SAME key under the old un-escaped ':'-join
    // ('formula-run:a:rig:rig:y'): runId 'a' + scopeRef 'rig:y' vs runId 'a:rig'
    // + scopeRef 'y'. Distinct runs must map to distinct cache slots.
    const a = formulaRunDetailCacheKey('a', 'rig', 'rig:y');
    const b = formulaRunDetailCacheKey('a:rig', 'rig', 'y');
    expect(a).not.toBe(b);
  });

  it('keeps distinct scopes on the same run apart', () => {
    expect(formulaRunDetailCacheKey('run', 'rig', 'app')).not.toBe(
      formulaRunDetailCacheKey('run', 'city', 'app'),
    );
  });
});

const streamEventSources: StreamFakeEventSource[] = [];

class StreamFakeEventSource {
  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSED = 2;

  onopen: ((event: Event) => void) | null = null;
  onmessage: ((event: MessageEvent<string>) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  readyState = StreamFakeEventSource.CONNECTING;
  closed = false;
  private readonly listeners = new Map<string, Set<EventListener>>();

  constructor(readonly url: string | URL) {
    streamEventSources.push(this);
  }

  addEventListener(type: string, listener: EventListener): void {
    const listeners = this.listeners.get(type) ?? new Set<EventListener>();
    listeners.add(listener);
    this.listeners.set(type, listeners);
  }

  removeEventListener(type: string, listener: EventListener): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.readyState = StreamFakeEventSource.CLOSED;
    this.closed = true;
  }

  open(): void {
    this.readyState = StreamFakeEventSource.OPEN;
    this.onopen?.(new Event('open'));
  }

  fail(): void {
    this.readyState = StreamFakeEventSource.CLOSED;
    this.onerror?.(new Event('error'));
  }

  emit(type: string, data: string): void {
    const event = new MessageEvent<string>(type, { data });
    this.listeners.get(type)?.forEach((listener) => listener(event));
    if (type === 'message') this.onmessage?.(event);
  }
}
