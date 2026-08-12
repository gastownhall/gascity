import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  loadSupervisorRunSummaryActiveSource,
  loadSupervisorRunSummaryMountSource,
  loadSupervisorRunSummaryPreviewSource,
  loadSupervisorRunSummarySource,
} from './runSummary';

// The four source loaders now all delegate to one warm GET against the BFF
// run-projection endpoint, which folds the city event log and layers
// session health/census + thrash marks server-side. Run semantics — grouping,
// phase/stage classification, health/census derivation, stale-latch demotion,
// the cheap/wide read split — moved to Go (internal/runproj) and are covered
// byte-for-byte by the goldens there. What remains in TS is purely the
// SourceState wrapping (fresh on success, error on failure), tested here.

const sampleSummary = {
  totalActive: 1,
  totalHistorical: 0,
  runCounts: { active: 1, blocked: 0, complete: 0 },
  lanes: [],
  historicalLanes: [],
  blockedLanes: [],
  recentChanges: [],
  census: { status: 'unavailable' },
};

const loaders = {
  source: loadSupervisorRunSummarySource,
  mount: loadSupervisorRunSummaryMountSource,
  active: loadSupervisorRunSummaryActiveSource,
  preview: loadSupervisorRunSummaryPreviewSource,
};

describe('run summary source loaders', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  for (const [name, load] of Object.entries(loaders)) {
    it(`${name}: wraps the warm GET as a fresh runs source`, async () => {
      const fetchMock = vi.fn(
        async () =>
          new Response(JSON.stringify(sampleSummary), {
            status: 200,
            headers: { 'content-type': 'application/json' },
          }),
      );
      vi.stubGlobal('fetch', fetchMock);

      const state = await load();

      expect(state).toMatchObject({
        source: 'runs',
        status: 'fresh',
        error: { kind: 'none' },
        data: { totalActive: 1 },
      });
      if (state.status !== 'error') {
        expect(typeof state.fetchedAt).toBe('string');
        expect(Date.parse(state.staleAt)).toBeGreaterThan(Date.parse(state.fetchedAt));
      }
      expect(fetchMock).toHaveBeenCalledWith(
        '/api/city/test-city/runs/summary',
        expect.objectContaining({ method: 'GET' }),
      );
    });

    it(`${name}: returns an error source when the GET fails`, async () => {
      vi.stubGlobal(
        'fetch',
        vi.fn(
          async () =>
            new Response(JSON.stringify({ error: 'run view is warming' }), {
              status: 503,
              headers: { 'content-type': 'application/json' },
            }),
        ),
      );

      const state = await load();

      expect(state).toMatchObject({ source: 'runs', status: 'error' });
      if (state.status === 'error') expect(state.error).toContain('warming');
    });
  }
});
