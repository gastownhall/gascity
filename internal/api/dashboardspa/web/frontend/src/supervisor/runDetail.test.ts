import { afterEach, describe, expect, it, vi } from 'vitest';
import { ApiClientError } from '../api/client';
import { loadSupervisorFormulaRunDetail } from './runDetail';

// The detail pipeline (snapshot synthesis, grouping, phase/stage, edges, lanes,
// formula identity, completeness) moved to Go (internal/runproj.BuildRunDetail)
// and is golden-gated byte-for-byte. The TS loader is now one GET to the BFF
// run-projection endpoint that treats a warming 503 as a poll signal: fast
// initial delays, then a capped cadence, with a total budget sized to the
// server's 180s unknown-run grace window (a just-slung run's bead events land
// 30-120s after sling). This file covers that thin read: the warm path, the
// warming poll (cadence, budget, cancellation, the onWarming signal), and the
// error surface the hook maps.

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

  it('keeps polling a warming 503 at the capped cadence until the run appears (deep-link grace)', async () => {
    // A dashboard deep link printed right after `gc sling` lands before the
    // run's bead events exist (30-120s later). The loader must NOT surface
    // failure after the fast delays (~4s) — it polls at the capped cadence
    // (the server's pinned Retry-After: 5) until the run appears.
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () =>
      jsonResponse({ error: 'run view is warming', reason: 'unknown_run' }, 503),
    );
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    // The fast delays plus eleven capped 5s polls (~60s in, the earliest the
    // controller's cache-reconcile usually surfaces a just-slung run)...
    await vi.advanceTimersByTimeAsync(600 + 1_200 + 2_400 + 11 * 5_000);
    expect(fetchMock).toHaveBeenCalledTimes(15);

    // ...then the run appears and the SAME load resolves.
    fetchMock.mockResolvedValueOnce(jsonResponse(detailBody, 200));
    await vi.advanceTimersByTimeAsync(5_000);
    await expect(pending).resolves.toMatchObject({ runId: 'mol-adopt-1' });
  });

  it('gives up only after the ~180s warming budget is spent and surfaces the 503', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => jsonResponse({ error: 'run view is warming' }, 503));
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    const assertion = expect(pending).rejects.toMatchObject({ status: 503 });
    await vi.advanceTimersByTimeAsync(180_000);
    await assertion;

    // The initial attempt, the three fast retries (600+1200+2400 = 4.2s), then
    // 5s-capped polls until the next delay would overrun the 180s budget:
    // 1 + 3 + 35.
    expect(fetchMock).toHaveBeenCalledTimes(39);
  });

  it('reports each warming 503 (with the graced unknown_run reason) to onWarming', async () => {
    vi.useFakeTimers();
    const fetchMock = vi
      .fn()
      // A cold-replay warming 503 carries no reason; the graced unknown-run
      // 503 carries reason 'unknown_run' (the pinned wire contract).
      .mockResolvedValueOnce(jsonResponse({ error: 'run view is warming' }, 503))
      .mockResolvedValueOnce(
        jsonResponse({ error: 'run view is warming', reason: 'unknown_run' }, 503),
      )
      .mockResolvedValueOnce(jsonResponse(detailBody, 200));
    vi.stubGlobal('fetch', fetchMock);
    const onWarming = vi.fn();

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1', { onWarming });
    await vi.advanceTimersByTimeAsync(600 + 1_200);

    await expect(pending).resolves.toMatchObject({ runId: 'mol-adopt-1' });
    expect(onWarming.mock.calls.map(([warming]) => warming)).toEqual([
      { reason: undefined },
      { reason: 'unknown_run' },
    ]);
  });

  it('stops polling when keepPolling turns false and surfaces the pending 503', async () => {
    // The caller (the hook) supersedes a poll on unmount/navigation/refresh; a
    // superseded poll must stop issuing GETs instead of running out its 180s
    // budget in the background.
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => jsonResponse({ error: 'run view is warming' }, 503));
    vi.stubGlobal('fetch', fetchMock);
    let polling = true;

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1', {
      keepPolling: () => polling,
    });
    const assertion = expect(pending).rejects.toMatchObject({ status: 503 });
    await vi.advanceTimersByTimeAsync(600);
    polling = false;
    await vi.advanceTimersByTimeAsync(1_200);
    await assertion;

    // The initial attempt and the one retry that was already scheduled — the
    // post-delay keepPolling check stops the third GET from ever firing.
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('keeps the short retry budget for a non-warming 5xx', async () => {
    // Only the warming 503 gets the long poll; a persistent upstream 5xx is
    // not a "run still being recorded" signal and surfaces after the fast
    // delays as before.
    vi.useFakeTimers();
    const fetchMock = vi.fn(async () => jsonResponse({ error: 'bad gateway' }, 502));
    vi.stubGlobal('fetch', fetchMock);

    const pending = loadSupervisorFormulaRunDetail('mol-adopt-1');
    const assertion = expect(pending).rejects.toMatchObject({ status: 502 });
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
