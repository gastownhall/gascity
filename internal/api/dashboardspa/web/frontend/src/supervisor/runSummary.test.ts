import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import type {
  Bead,
  FormulaFeedBody,
  ListBodyBead,
  ListBodySessionResponse,
  MonitorFeedItemResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { resetSupervisorApiForTests, setSupervisorApiForTests, type SupervisorApi } from './client';
import { SupervisorApiError } from './errors';
import {
  CORE_RUN_SUMMARY_TIMEOUT_MS,
  loadSupervisorRunSummaryActiveSource,
  loadSupervisorRunSummaryMountSource,
  loadSupervisorRunSummaryPreviewSource,
  loadSupervisorRunSummarySource,
  resetSupervisorRunSummaryStateForTests,
} from './runSummary';

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  listCities: vi.fn(),
  listAgents: vi.fn(),
  listRigs: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  getBead: vi.fn(),
  createBead: vi.fn(),
  updateBead: vi.fn(),
  closeBead: vi.fn(),
  nudgeAgent: vi.fn(),
  agentPrime: vi.fn(),
  sling: vi.fn(),
  formulaFeed: vi.fn(),
  listMail: vi.fn(),
  markMailRead: vi.fn(),
  markMailUnread: vi.fn(),
  archiveMail: vi.fn(),
  replyMail: vi.fn(),
  sendMail: vi.fn(),
  mailThread: vi.fn(),
  cityEventStreamUrl: vi.fn(),
  sessionStreamUrl: vi.fn(),
  listSessions: vi.fn(),
  sessionPending: vi.fn(),
  respondSession: vi.fn(),
  sessionTranscript: vi.fn(),
  workflowRun: vi.fn(),
  formulaDetail: vi.fn(),
  mutationHeaders: () => ({ 'X-GC-Request': 'dashboard' }),
};

describe('loadSupervisorRunSummaryPreviewSource', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    resetSupervisorRunSummaryStateForTests();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-06-01T12:00:00.000Z'));
  });

  afterEach(() => {
    resetSupervisorApiForTests();
    resetSupervisorRunSummaryStateForTests();
    vi.unstubAllGlobals();
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('builds a first-paint summary from bounded active and recent run reads', async () => {
    const listBeads = vi.fn(async () => beadList([runRoot()]));
    const formulaFeed = vi.fn(async () => feed([feedRun()]));
    const listSessions = vi.fn(async () => sessionList());
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed,
      listSessions,
    });

    const source = await loadSupervisorRunSummaryPreviewSource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes[0]?.id).toBe('run-1');
    expect(source.data.lanes[0]?.health.status).toBe('unavailable');
    expect(listBeads).toHaveBeenCalledTimes(3);
    expect(listBeads).toHaveBeenCalledWith('test-city', { limit: 500 });
    expect(listBeads).toHaveBeenCalledWith('test-city', {
      limit: 500,
      type: 'molecule',
      all: true,
    });
    expect(listBeads).toHaveBeenCalledWith('test-city', {
      limit: 500,
      type: 'task',
      rig: 'rig-a',
      all: true,
    });
    expect(formulaFeed).toHaveBeenCalledWith('test-city', {
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
    expect(listSessions).not.toHaveBeenCalled();
  });

  it('marks the summary partial when a slow enrichment read exceeds the tight first-paint budget (gascity-dashboard-4bol)', async () => {
    // First paint blocks on the preview load, so it keeps a tight 2.5s budget: a
    // rig read that takes 10s on a slow supervisor degrades to partial rather
    // than holding the tab blank. The wider refresh budget then clears it.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') {
        return new Promise<ListBodyBead>((resolve) => {
          setTimeout(() => resolve(beadList([])), 10_000);
        });
      }
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummaryPreviewSource();
    await vi.advanceTimersByTimeAsync(2_500);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBe(true);
  });
});

describe('loadSupervisorRunSummarySource', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    resetSupervisorRunSummaryStateForTests();
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-06-01T12:00:00.000Z'));
  });

  afterEach(() => {
    resetSupervisorApiForTests();
    resetSupervisorRunSummaryStateForTests();
    vi.unstubAllGlobals();
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('builds the run summary from direct supervisor beads, feed, and sessions', async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') {
        return beadList([
          bead({
            id: 'run-1-step-1',
            title: 'Implementation patch',
            status: 'in_progress',
            metadata: {
              'gc.kind': 'step',
              'gc.root_bead_id': 'run-1',
              'gc.parent_bead_id': 'run-1',
              'gc.step_id': 'implementation.patch',
            },
          }),
        ]);
      }
      return beadList([runRoot()]);
    });
    const formulaFeed = vi.fn(async () => feed([feedRun()]));
    const listSessions = vi.fn(async () => sessionList());
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed,
      listSessions,
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.fetchedAt).toBe('2026-06-01T12:00:00.000Z');
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes).toHaveLength(1);
    expect(source.data.lanes[0]).toMatchObject({
      id: 'run-1',
      title: 'Adopt PR #42',
      phase: 'implementation',
      scope: {
        status: 'available',
        kind: 'rig',
        ref: 'rig-a',
        rootStoreRef: 'rig:rig-a',
      },
      statusCounts: { open: 1, in_progress: 1 },
    });
    expect(source.data.census.status).toBe('available');
    expect(listBeads).toHaveBeenCalledWith('test-city', { limit: 500 });
    expect(listBeads).toHaveBeenCalledWith('test-city', {
      limit: 500,
      type: 'molecule',
      all: true,
    });
    expect(listBeads).toHaveBeenCalledWith('test-city', {
      limit: 500,
      type: 'task',
      rig: 'rig-a',
      all: true,
    });
    expect(formulaFeed).toHaveBeenCalledWith('test-city', {
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
    expect(listSessions).toHaveBeenCalledWith('test-city');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('derives a rig lane scope from the feed root_store_ref even when the feed scope_kind is city (q89b detail scope leak)', async () => {
    // The formula feed's top-level scope_kind is always 'city'; the rig identity
    // lives only in root_store_ref. When the root bead carries no scope metadata
    // (scope must come from the feed map), the lane must still resolve to its rig
    // scope so the detail href carries it and the workflow fetch hits the fast
    // single-store path — not the city-wide full-store scan.
    const rootNoScope = runRoot({
      id: 'run-2',
      metadata: {
        'gc.kind': 'run',
        'gc.formula': 'mol-adopt-pr-v2',
        'gc.formula_contract': 'graph.v2',
      },
    });
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig !== undefined) return beadList([]);
      return beadList([rootNoScope]);
    });
    const cityScopedFeedRun = feedRun({
      id: 'run-2',
      root_bead_id: 'run-2',
      workflow_id: 'run-2',
      root_store_ref: 'rig:rig-b',
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([cityScopedFeedRun])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    const lane = source.data.lanes.find((l) => l.id === 'run-2');
    expect(lane?.scope).toEqual({
      status: 'available',
      kind: 'rig',
      ref: 'rig-b',
      rootStoreRef: 'rig:rig-b',
    });
  });

  it('does not emit a malformed feed root_store_ref as the lane scope (validates against SCOPE_REF_RE)', async () => {
    // The store-ref-first branch in discoverFromFeed must validate the parsed
    // ref against SCOPE_REF_RE before using it — fromStoreRef does not validate,
    // so a malformed root_store_ref like 'rig:bad ref@!' would otherwise become a
    // scope_ref the detail route rejects. It must fall back to the feed scope.
    const rootNoScope = runRoot({
      id: 'run-3',
      metadata: {
        'gc.kind': 'run',
        'gc.formula': 'mol-adopt-pr-v2',
        'gc.formula_contract': 'graph.v2',
      },
    });
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig !== undefined) return beadList([]);
      return beadList([rootNoScope]);
    });
    const malformedFeedRun = feedRun({
      id: 'run-3',
      root_bead_id: 'run-3',
      workflow_id: 'run-3',
      root_store_ref: 'rig:bad ref@!',
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([malformedFeedRun])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    const lane = source.data.lanes.find((l) => l.id === 'run-3');
    expect(lane?.scope).toMatchObject({
      status: 'available',
      kind: 'city',
      ref: 'test-city',
    });
    if (lane?.scope.status === 'available') {
      expect(lane.scope.ref).not.toBe('bad ref@!');
    }
  });

  it('enriches blocked lanes with health and keeps them out of the active set (gascity-dashboard-4xcv)', async () => {
    // gc-1920 repro: a stale blocked formula latch must land in
    // blockedLanes (with derived health, so attention still sees it),
    // never in lanes/totalActive.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') return beadList([]);
      return beadList([
        runRoot(),
        runRoot({
          id: 'gc-1920',
          title: 'mol-focus-review',
          status: 'blocked',
          metadata: {
            'gc.kind': 'workflow',
            'gc.formula_contract': 'graph.v2',
            'gc.scope_kind': 'city',
            'gc.scope_ref': 'test-city',
            'gc.root_store_ref': 'city:test-city',
          },
        }),
      ]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
    expect(source.data.blockedLanes.map((lane) => lane.id)).toEqual(['gc-1920']);
    expect(source.data.blockedLanes[0]?.health.status).toBe('available');
    expect(source.data.runCounts.blocked).toBe(1);
  });

  // gascity-dashboard-s4rp: the gc-1920 phantom now surfaces NOT as blocked but
  // as a stale session-less approval latch — counted Active:1 despite no live
  // session, no in_progress step, and ~4d since its last write. Enrichment must
  // demote it out of the Active set and count once sessions resolve.
  function staleLatch(): Bead {
    return runRoot({
      id: 'gc-1920',
      title: 'mol-focus-review',
      description: 'Focus + in-session review formula. Approval gate, review.',
      status: 'open',
      updated_at: '2026-05-28T00:00:00.000Z',
      metadata: {
        'gc.kind': 'run',
        'gc.formula_contract': 'graph.v2',
        'gc.scope_kind': 'city',
        'gc.scope_ref': 'test-city',
        'gc.root_store_ref': 'city:test-city',
      },
    });
  }

  it('demotes a stale session-less approval latch out of the Active set (gascity-dashboard-s4rp)', async () => {
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') {
        return beadList([
          bead({
            id: 'run-1-step-1',
            title: 'Implementation patch',
            status: 'in_progress',
            updated_at: '2026-06-01T11:55:00.000Z',
            metadata: {
              'gc.kind': 'step',
              'gc.root_bead_id': 'run-1',
              'gc.step_id': 'implementation.patch',
            },
          }),
        ]);
      }
      return beadList([runRoot(), staleLatch()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    // run-1 has an in_progress step and stays Active; gc-1920 is demoted out of
    // the Active set, count, AND the blocked/historical buckets (dropped).
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
    expect(source.data.blockedLanes).toEqual([]);
    expect(source.data.historicalLanes.map((lane) => lane.id)).not.toContain('gc-1920');
    expect(source.data.runCounts.total).toBe(1);
  });

  it('keeps a session-less recent latch in the Active set (gascity-dashboard-s4rp)', async () => {
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') return beadList([]);
      // Same shape as the stale latch but written 30 minutes ago — a recent
      // session-less latch must still appear as Active (not demoted by liveness).
      return beadList([{ ...staleLatch(), updated_at: '2026-06-01T11:30:00.000Z' }]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['gc-1920']);
    // gascity-dashboard-q3p1: this latch is a bare root with NO step beads, so
    // phase comes from the tightened keyword fallback. Its title
    // ('mol-focus-review') carries a 'review' signal → 'review'. The old
    // assertion was 'approval', which was the bug itself: the OLD scan read
    // 'approval'/'gate' out of the run's DESCRIPTION ("Approval gate, review.")
    // even though no approval-gate step exists. The lane staying Active — the
    // actual subject of this test — is unchanged.
    expect(source.data.lanes[0]?.phase).toBe('review');
  });

  it('demotes a phantom exactly even when active runs exceed the visible cap (gascity-dashboard-s4rp)', async () => {
    // Demotion runs on the FULL active set, not the capped window, so totalActive
    // is exact: a phantom is removed from the count even with >8 active runs.
    const liveRuns = Array.from({ length: 9 }, (_, i) =>
      runRoot({
        id: `live-${i}`,
        title: `Live run ${i}`,
        status: 'open',
        updated_at: '2026-06-01T11:55:00.000Z',
        metadata: {
          'gc.kind': 'run',
          'gc.formula_contract': 'graph.v2',
          'gc.scope_kind': 'city',
          'gc.scope_ref': 'test-city',
          'gc.root_store_ref': 'city:test-city',
        },
      }),
    );
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') return beadList([]);
      return beadList([...liveRuns, staleLatch()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(9);
    // gascity-dashboard: `lanes` carries the FULL active set now — the rendered
    // 8-lane collapse is applied by RunMap, not the wire.
    expect(source.data.lanes).toHaveLength(9);
    expect(source.data.lanes.map((lane) => lane.id)).not.toContain('gc-1920');
    // The phantom (the 10th, session-less latch) is demoted; all 9 live runs survive.
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(liveRuns.map((_, i) => `live-${i}`));
    expect(source.data.runCounts.total).toBe(9);
    expect(source.data.runCounts.visible).toBe(9);
  });

  it('keeps available lanes while marking the summary partial when a recent rig read fails', async () => {
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') throw new Error('rig unavailable');
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('marks the summary partial when the active bead list is cursor-truncated', async () => {
    // gascity-dashboard-4xcv: the supervisor truncates at the fetch limit and
    // reports the rest via next_cursor WITHOUT setting partial. Treat a present
    // cursor as partial so saturation surfaces the notice + retry instead of
    // silently dropping lanes at 501+ beads.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      return beadList([runRoot()], false, 'next-page-token');
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('marks the summary partial when the active bead list is truncated at the fetch limit', async () => {
    // gascity-dashboard-q89b: the primary fetch is bounded; when the upstream
    // total exceeds what one page returned, active runs may be missing and the
    // lanes must read as partial rather than complete.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') return beadList([]);
      return { ...beadList([runRoot()]), total: 501 };
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('does not let optional enrichment reads hold the summary refresh indefinitely', async () => {
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return new Promise<ListBodyBead>(() => {});
      if (query?.rig === 'rig-a') return beadList([]);
      return beadList([runRoot()]);
    });
    const formulaFeed = vi.fn(async () => new Promise<FormulaFeedBody>(() => {}));
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed,
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummarySource();
    // The full source is Runs.tsx's background refresh, so its enrichment runs on
    // the wider REFRESH budget (gascity-dashboard-4bol); the cap still fires.
    await vi.advanceTimersByTimeAsync(30_000);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('clears the spurious partial when a slow-but-available enrichment read lands within the wider refresh budget (gascity-dashboard-4bol)', async () => {
    // The background refresh tolerates a slow supervisor: a rig read that takes
    // 10s — past the 2.5s first-paint budget but inside the 30s refresh budget —
    // lands, so the lanes are NOT latched partial (upstream gascity-dashboard#88).
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') {
        return new Promise<ListBodyBead>((resolve) => {
          setTimeout(() => resolve(beadList([])), 10_000);
        });
      }
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummarySource();
    await vi.advanceTimersByTimeAsync(10_000);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBeUndefined();
  });

  it('keeps the tight first-paint budget on the mount source so Home/Formula Run Detail never block on a slow read (gascity-dashboard-4bol)', async () => {
    // Same 10s rig read as the refresh test above, but the mount source (Home,
    // Formula Run Detail first paint) runs on the 2.5s budget — the read does NOT
    // land, so the lanes are latched partial rather than blocking ~30s on a cold
    // navigation. This is the regression guard for the refresh-budget leak.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') {
        return new Promise<ListBodyBead>((resolve) => {
          setTimeout(() => resolve(beadList([])), 10_000);
        });
      }
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummaryMountSource();
    await vi.advanceTimersByTimeAsync(2_500);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('keeps active lanes and marks partial when the molecule-history read rejects (gascity-dashboard-9rk2)', async () => {
    // The molecule(all=true) scan surfaces historical run roots only; it is
    // best-effort. A rejection must fold to `partial` with the active lanes still
    // present — never escalate to a whole-view error/"Run data unavailable".
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') throw new Error('molecule history unavailable');
      if (query?.rig === 'rig-a') return beadList([]);
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('bounds a slow molecule-history read to its own timeout and folds it to partial (gascity-dashboard-9rk2)', async () => {
    // Live the molecule scan runs ~6.8s — past the 5s required-fetch budget. On
    // the wide 30s refresh it would otherwise stall the whole refresh; its own
    // 3s bound caps it so the active set still paints and only the historical
    // lanes degrade to partial.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') {
        return new Promise<ListBodyBead>((resolve) => {
          setTimeout(() => resolve(beadList([])), 6_800);
        });
      }
      if (query?.rig === 'rig-a') return beadList([]);
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummarySource();
    // Advance past the molecule's own 3s bound but well short of its 6.8s
    // completion: the bound fires, the active set still resolves.
    await vi.advanceTimersByTimeAsync(3_000);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
    expect(source.data.lanesPartial).toBe(true);
  });

  it('returns an error source when the active bead list fails', async () => {
    setSupervisorApiForTests({
      ...baseApi,
      listBeads: vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
        if (query?.type === 'molecule') return beadList([]);
        throw new Error('beads unavailable');
      }),
      formulaFeed: vi.fn(async () => feed([])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(source).toEqual({
      source: 'runs',
      status: 'error',
      error: 'beads unavailable',
    });
  });

  // gascity-dashboard fix/runs-fetch-resilience: a transient core-read timeout
  // under a CPU burst must not blank the view on a first load (no last-good
  // snapshot yet). The core active-bead read retries once before giving up.
  it('retries the core active-bead read once on a transient timeout and resolves to data', async () => {
    let coreAttempts = 0;
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      if (query?.rig === 'rig-a') return beadList([]);
      coreAttempts += 1;
      if (coreAttempts === 1) {
        throw new SupervisorApiError(
          undefined,
          'gc supervisor request timed out after 15000ms',
          undefined,
        );
      }
      return beadList([runRoot()]);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([feedRun()])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummarySource();
    // Drain the short retry backoff so the second attempt fires.
    await vi.advanceTimersByTimeAsync(250);
    const source = await pending;

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    expect(coreAttempts).toBe(2);
    expect(source.data.totalActive).toBe(1);
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
  });

  it('surfaces an error when the core active-bead read times out on every attempt', async () => {
    let coreAttempts = 0;
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      coreAttempts += 1;
      throw new SupervisorApiError(
        undefined,
        'gc supervisor request timed out after 15000ms',
        undefined,
      );
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const pending = loadSupervisorRunSummarySource();
    await vi.advanceTimersByTimeAsync(250);
    const source = await pending;

    // A sustained failure is not hidden: after the retries are spent the view
    // still surfaces the error so a real outage isn't masked forever.
    expect(coreAttempts).toBe(2);
    expect(source.status).toBe('error');
    if (source.status !== 'error') throw new Error('expected error source');
    expect(source.error).toContain('timed out after 15000ms');
  });

  it('does not retry the core read on a non-transient (4xx) failure', async () => {
    let coreAttempts = 0;
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query?.type === 'molecule') return beadList([]);
      coreAttempts += 1;
      throw new SupervisorApiError(400, 'bad request', undefined);
    });
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      formulaFeed: vi.fn(async () => feed([])),
      listSessions: vi.fn(async () => sessionList()),
    });

    const source = await loadSupervisorRunSummarySource();

    expect(coreAttempts).toBe(1);
    expect(source.status).toBe('error');
  });

  it('reads the core active-bead fetch on the raised burst-tolerant budget', () => {
    // The path is ~0.02s normally, so the higher ceiling only matters during a
    // spike; it must stay well above the old 5s budget to absorb a burst.
    expect(CORE_RUN_SUMMARY_TIMEOUT_MS).toBe(15_000);
    expect(CORE_RUN_SUMMARY_TIMEOUT_MS).toBeGreaterThan(5_000);
  });
});

describe('loadSupervisorRunSummaryActiveSource — cheap SSE-burst path', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    resetSupervisorRunSummaryStateForTests();
  });
  afterEach(() => {
    resetSupervisorApiForTests();
    vi.restoreAllMocks();
  });

  it('skips the molecule history scan, city feed, and per-rig task reads', async () => {
    // The core active read returns ONLY the no-query (active) listBeads call —
    // any molecule/per-rig listBeads call or formulaFeed call would mean the
    // cheap path is still firing the expensive reads. The run carries an
    // in-progress step in the SAME core read (no per-rig fetch on the cheap
    // path), so the lane stays active without session enrichment.
    const listBeads = vi.fn(async (_cityName: string, query?: Record<string, unknown>) => {
      if (query && (query.type !== undefined || query.rig !== undefined)) {
        throw new Error(`cheap path must not call listBeads with ${JSON.stringify(query)}`);
      }
      return beadList([
        runRoot(),
        bead({
          id: 'run-1-step-1',
          title: 'Implementation patch',
          status: 'in_progress',
          metadata: {
            'gc.kind': 'step',
            'gc.root_bead_id': 'run-1',
            'gc.parent_bead_id': 'run-1',
            'gc.step_id': 'implementation.patch',
          },
        }),
      ]);
    });
    const formulaFeed = vi.fn(async () => feed([]));
    const listSessions = vi.fn(async () => sessionList());
    setSupervisorApiForTests({ ...baseApi, listBeads, formulaFeed, listSessions });

    const source = await loadSupervisorRunSummaryActiveSource();

    expect(source.status).toBe('fresh');
    if (source.status === 'error') throw new Error(source.error);
    // The active lane is present, and its rig scope resolves from the bead's own
    // gc.root_store_ref metadata — no feed needed.
    expect(source.data.lanes.map((lane) => lane.id)).toEqual(['run-1']);
    expect(source.data.lanes[0]?.scope).toMatchObject({ status: 'available', kind: 'rig' });
    // No history reads on the cheap path.
    expect(source.data.historicalLanes).toEqual([]);
    expect(source.data.totalHistorical).toBe(0);
    // The expensive reads were never issued.
    expect(formulaFeed).not.toHaveBeenCalled();
    expect(
      listBeads.mock.calls.every(([, query]) => query === undefined || query.limit !== undefined),
    ).toBe(true);
    expect(listBeads.mock.calls.some(([, query]) => query?.type === 'molecule')).toBe(false);
    expect(listBeads.mock.calls.some(([, query]) => query?.rig !== undefined)).toBe(false);
  });
});

function runRoot(overrides: Partial<Bead> = {}): Bead {
  return bead({
    id: 'run-1',
    title: 'Adopt PR #42',
    issue_type: 'molecule',
    metadata: {
      'gc.kind': 'run',
      'gc.formula': 'mol-adopt-pr-v2',
      'gc.formula_contract': 'graph.v2',
      'gc.scope_kind': 'rig',
      'gc.scope_ref': 'rig-a',
      'gc.root_store_ref': 'rig:rig-a',
    },
    ...overrides,
  });
}

function bead(overrides: Partial<Bead> = {}): Bead {
  return {
    id: 'bead-1',
    title: 'Bead',
    issue_type: 'task',
    status: 'open',
    created_at: '2026-06-01T11:00:00.000Z',
    ...overrides,
  };
}

function beadList(items: Bead[], partial = false, nextCursor?: string): ListBodyBead {
  return {
    items,
    partial,
    total: items.length,
    ...(nextCursor !== undefined ? { next_cursor: nextCursor } : {}),
  };
}

function feed(items: MonitorFeedItemResponse[], partial = false): FormulaFeedBody {
  return {
    items,
    partial,
  };
}

function feedRun(overrides: Partial<MonitorFeedItemResponse> = {}): MonitorFeedItemResponse {
  return {
    id: 'run-1',
    root_bead_id: 'run-1',
    root_store_ref: 'rig:rig-a',
    workflow_id: 'run-1',
    scope_kind: 'rig',
    scope_ref: 'rig-a',
    started_at: '2026-06-01T11:00:00.000Z',
    status: 'running',
    target: 'rig-a/codex',
    title: 'Adopt PR #42',
    type: 'formula',
    updated_at: '2026-06-01T11:05:00.000Z',
    ...overrides,
  };
}

function sessionList(): ListBodySessionResponse {
  return {
    items: [],
    total: 0,
  };
}
