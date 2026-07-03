import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiClientError } from '../api/client';
import { loadSupervisorFormulaRunDetail } from './runDetail';

// The detail pipeline (snapshot synthesis, grouping, phase/stage, edges, lanes,
// formula identity, completeness) moved to Go (internal/runproj.BuildRunDetail)
// and is golden-gated byte-for-byte. The TS loader is now one GET to the BFF
// run-projection endpoint with a bounded retry while the projection is still
// cold-replaying (HTTP 503). This file covers that thin read: the warm path, the
// warming retry, and the error surface the hook maps.

const detailBody = {
  runId: 'mol-adopt-1',
  rootBeadId: 'b-1',
  rootStoreRef: 'rig:demo',
  resolvedRootStore: 'rig:demo',
  scopeKind: 'rig',
  scopeRef: 'demo',
  title: 'Adopt PR',
  formula: { kind: 'unavailable', reason: 'missing_formula_metadata' },
  formulaDetail: { kind: 'unavailable', reason: 'missing_formula_metadata' },
  executionPath: { kind: 'unavailable', reason: 'missing_cwd_and_rig_root' },
  snapshotVersion: 1,
  snapshotEventSeq: { kind: 'known', seq: 100 },
  completeness: { kind: 'complete' },
  progress: { statusCounts: {} },
  phase: 'intake',
  stages: [],
  nodes: [],
  edges: [],
  lanes: [],
};

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}

describe('loadSupervisorFormulaRunDetail', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('reads the run detail from the city-scoped BFF projection endpoint', async () => {
    const fetchMock = vi.fn(async () => jsonResponse(detailBody, 200));
    vi.stubGlobal('fetch', fetchMock);

    await expect(loadSupervisorFormulaRunDetail('mol-adopt-1')).resolves.toMatchObject({
      runId: 'mol-adopt-1',
    });
    expect(fetchMock).toHaveBeenCalledWith(
      '/api/city/test-city/runs/mol-adopt-1/detail',
      expect.objectContaining({ method: 'GET' }),
    );
  });

  it('retries while the projection is warming (503) and resolves once it is ready', async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: 'run view is warming' }, 503))
      .mockResolvedValueOnce(jsonResponse(detailBody, 200));
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    await vi.advanceTimersByTimeAsync(600);

    await expect(pending).resolves.toMatchObject({ runId: 'mol-adopt-1' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('gives up after the warming budget is spent and surfaces the 503', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => jsonResponse({ error: 'run view is warming' }, 503));
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    const assertion = expect(pending).rejects.toMatchObject({ status: 503 });
    await vi.advanceTimersByTimeAsync(600 + 1_200 + 2_400);
    await assertion;

    // The initial attempt plus three bounded retries.
    expect(fetchMock).toHaveBeenCalledTimes(4);
  });

  it('propagates a 422 unsupported run with its reason for the hook to map', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        jsonResponse({ error: 'run is not a graph.v2 run', reason: 'not_run_view' }, 422),
      ),
    );

    const err = await loadSupervisorFormulaRunDetail('v1-run').catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiClientError);
    expect(err).toMatchObject({ status: 422, reason: 'not_run_view' });
  });

  it('retries a transient 5xx (not just 503) and resolves', async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(jsonResponse({ error: 'bad gateway' }, 502))
      .mockResolvedValueOnce(jsonResponse(detailBody, 200));
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    await vi.advanceTimersByTimeAsync(600);

    await expect(pending).resolves.toMatchObject({ runId: 'mol-adopt-1' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('does not retry a 404', async () => {
    const fetchMock = vi.fn(async () => jsonResponse({ error: 'unknown run' }, 404));
    vi.stubGlobal('fetch', fetchMock);

    await expect(loadSupervisorFormulaRunDetail('ghost')).rejects.toMatchObject({ status: 404 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });
});
