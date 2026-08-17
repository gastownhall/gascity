import { act, render, renderHook, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RunSummary, SourceState } from 'gas-city-dashboard-shared';
import type { Bead } from 'gas-city-dashboard-shared/gc-supervisor';
import { invalidate } from '../api/cache';
import { setActiveCity } from '../api/cityBase';
import { BeadAttentionPanel } from '../components/beads/BeadAttentionPanel';
import type { OperatorConfig } from '../contexts/OperatorConfigContext';
import type * as SupervisorClient from '../supervisor/client';
import { SupervisorApiError } from '../supervisor/client';
import { composeAttention } from './compose';
import {
  fetchBeadsAttention,
  runsFactsFromSource,
  useLiveAttentionContributors,
} from './liveContributors';

// Operator identity the live hook reads from /config (gascity-dashboard-bhvn).
const testOperator: OperatorConfig = {
  operatorAlias: 'stephanie',
  operatorWireAlias: 'human',
  decisionLabel: 'needs/stephanie',
};

function freshRunsSource(): SourceState<RunSummary> {
  return {
    source: 'runs',
    status: 'fresh',
    fetchedAt: '2026-06-01T00:00:00.000Z',
    staleAt: '2026-06-01T00:01:00.000Z',
    error: { kind: 'none' },
    data: {
      totalActive: 0,
      totalHistorical: 0,
      historicalLanes: [],
      blockedLanes: [],
      runCounts: {
        total: 0,
        visible: 0,
        prReview: 0,
        designReview: 0,
        bugfix: 0,
        blocked: 0,
        other: 0,
      },
      lanes: [],
      recentChanges: [],
      census: { status: 'unavailable', error: 'run health has not been derived' },
    },
  };
}

const mockApi = vi.hoisted(() => ({
  doltTrend: vi.fn(),
  listBuilds: vi.fn(),
  systemHealth: vi.fn(),
}));

const mockSupervisorApi = vi.hoisted(() => ({
  cityHealth: vi.fn(),
  formulaFeed: vi.fn(),
  listAgents: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  listMail: vi.fn(),
  listSessions: vi.fn(),
  sessionPending: vi.fn(),
}));

vi.mock('../api/client', () => ({
  api: mockApi,
  formatApiError: (err: unknown, fallback = 'request failed') =>
    err instanceof Error ? err.message : fallback,
}));

const mockSupervisorApiForRequestBudget = vi.hoisted(() => vi.fn());

vi.mock('../supervisor/client', async (importOriginal) => {
  const actual = await importOriginal<typeof SupervisorClient>();
  return {
    ...actual,
    supervisorApi: () => mockSupervisorApi,
    supervisorApiForRequestBudget: mockSupervisorApiForRequestBudget,
  };
});

const CITY_NOT_FOUND_DETAIL = 'not_found: city not found or not running: captured-city';
const CITY_NOT_FOUND_CODE = 'city-not-found';

describe('useLiveAttentionContributors', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    invalidate('attention:');
    mockSupervisorApiForRequestBudget.mockReset();
    mockSupervisorApiForRequestBudget.mockReturnValue(mockSupervisorApi);
    for (const fn of [
      mockApi.doltTrend,
      mockApi.listBuilds,
      mockApi.systemHealth,
      mockSupervisorApi.cityHealth,
      mockSupervisorApi.formulaFeed,
      mockSupervisorApi.listAgents,
      mockSupervisorApi.listBeads,
      mockSupervisorApi.listEvents,
      mockSupervisorApi.listMail,
      mockSupervisorApi.listSessions,
      mockSupervisorApi.sessionPending,
    ]) {
      fn.mockReset();
    }

    mockSupervisorApi.formulaFeed.mockResolvedValue({
      partial: false,
      items: [
        {
          id: 'run-1',
          root_bead_id: 'B-root',
          root_store_ref: 'city:B-root',
          scope_kind: 'city',
          scope_ref: 'test-city',
          started_at: '2026-05-29T20:00:00.000Z',
          status: 'failed',
          target: 'mayor',
          title: 'Failed run',
          type: 'formula',
          updated_at: '2026-05-29T20:05:00.000Z',
        },
      ],
    });
    mockSupervisorApi.listAgents.mockResolvedValue({
      total: 1,
      items: [
        {
          available: true,
          name: 'reviewer',
          running: true,
          state: 'failed',
          suspended: false,
          session: {
            attached: true,
            last_activity: '2026-05-29T20:00:00.000Z',
            name: 'reviewer',
          },
        },
      ],
    });
    mockSupervisorApi.listSessions.mockResolvedValue({
      total: 1,
      items: [
        {
          id: 'gc-2568',
          session_name: 'reviewer',
          state: 'active',
          template: 'reviewer',
          alias: 'reviewer',
          provider: 'codex',
          running: true,
          attached: true,
          created_at: '2026-05-29T20:00:00.000Z',
        },
      ],
    });
    mockSupervisorApi.sessionPending.mockResolvedValue({
      supported: true,
      pending: {
        kind: 'tool_approval',
        prompt: 'Approve deployment?',
        request_id: 'req-1',
      },
    });
    mockSupervisorApi.listBeads.mockImplementation((_city, query) => {
      // Two dedicated label-filtered queues — the escalation queue surfaces the
      // one abnormally-blocked bead; the mayor-decision queue is empty here. The
      // unfiltered calls (general bead list + the runs summary loader) return no
      // engineering beads, so the Beads badge count is the lone escalation.
      if (query?.label === 'gc:escalation') {
        return Promise.resolve({
          total: 1,
          items: [
            {
              created_at: '2026-05-29T20:00:00.000Z',
              id: 'B-1',
              issue_type: 'task',
              priority: null,
              status: 'blocked',
              title: 'Escalated bead',
              labels: ['gc:escalation'],
            },
          ],
        });
      }
      if (query?.label !== undefined) {
        return Promise.resolve({ total: 0, items: [] });
      }
      return Promise.resolve({ total: 0, items: [] });
    });
    mockSupervisorApi.listMail.mockResolvedValue({
      total: 2,
      items: [
        {
          body: '',
          created_at: '2026-05-29T20:00:00.000Z',
          from: 'sam',
          id: 'M-1',
          read: false,
          subject: 'Need approval',
          to: 'human',
        },
        {
          body: '',
          created_at: '2026-05-29T20:01:00.000Z',
          from: 'sam',
          id: 'M-other',
          read: false,
          subject: 'Someone else needs approval',
          to: 'mayor',
        },
      ],
    });
    mockSupervisorApi.listEvents.mockResolvedValue({
      total: 1,
      items: [
        {
          actor: 'supervisor',
          message: 'session crashed while applying patch',
          payload: {
            reason: 'panic',
            session_id: 'gc-session-1',
            template: 'mayor',
          },
          seq: 42,
          subject: 'gc-session-1',
          ts: '2026-06-01T10:10:00.000Z',
          type: 'session.crashed',
        },
      ],
    });
    mockSupervisorApi.cityHealth.mockResolvedValue({
      city: 'test-city',
      status: 'ok',
      uptime_sec: 300,
      version: '1.0.0',
    });
    mockApi.systemHealth.mockResolvedValue({
      admin: {
        pid: 123,
        uptime_sec: 600,
        rss: { status: 'available', value: 128_000_000 },
        heap_used_bytes: 64_000_000,
        node_version: 'v22.0.0',
      },
      host: {
        load: {
          status: 'available',
          value: { load_avg_1: 0.5, load_avg_5: 0.4, load_avg_15: 0.3 },
        },
        memory: {
          status: 'available',
          value: { total_mem_bytes: 100, free_mem_bytes: 4 },
        },
        cpu_count: 8,
        uptime: { status: 'available', value: 86_400 },
      },
    });
    mockApi.doltTrend.mockResolvedValue({
      available: true,
      samples: [],
      source: 'supervisor',
    });
    mockApi.listBuilds.mockResolvedValue({
      failed_marker: true,
      items: [],
      source: null,
    });
  });

  afterEach(() => {
    vi.useRealTimers();
    invalidate('attention:');
  });

  it('composes Home/nav attention from direct supervisor facts and dashboard-local facts', async () => {
    const { result } = renderHook(() => useLiveAttentionContributors(testOperator, undefined));

    await waitFor(() => {
      const model = composeAttention(result.current);
      // gascity-dashboard-2j8e.7: the Runs badge reads the shared run-summary
      // subscription (passed in as the runsSource arg), not its own fan-out.
      // With no source here it contributes nothing; the blocked-counting logic
      // is covered in registry.test.ts and the source projection in
      // runsFactsFromSource below.
      expect(model.byDomain.runs.attention).toBe(0);
      // gascity-dashboard-2j8e.4: the one agent ('reviewer') is both in a
      // failure state AND awaiting an input decision; selectAgentsNeedingYou
      // counts it once with its highest-priority reason (awaiting-input), so
      // the badge is 1, not the old double-count of 2.
      expect(model.byDomain.agents.attention).toBe(1);
      expect(model.byDomain.beads.attention).toBe(1);
      expect(model.byDomain.mail.attention).toBe(1);
      expect(model.byDomain.mail.watch).toBe(0);
      expect(model.byDomain.mail.items.map((item) => item.id)).toEqual(['mail:M-1:unread-stale']);
      expect(model.byDomain.activity.attention).toBe(2);
      expect(model.byDomain.health.attention).toBe(1);
    });

    expect(mockSupervisorApi.listAgents).toHaveBeenCalledWith('test-city');
    expect(mockSupervisorApi.listSessions).toHaveBeenCalledWith('test-city');
    expect(mockSupervisorApi.sessionPending).toHaveBeenCalledWith('test-city', 'gc-2568');
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith(
      'test-city',
      { limit: 1000 },
      expect.any(AbortSignal),
    );
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith(
      'test-city',
      {
        label: 'needs/stephanie',
        status: 'open',
      },
      expect.any(AbortSignal),
    );
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith(
      'test-city',
      {
        label: 'gc:escalation',
        status: 'open',
      },
      expect.any(AbortSignal),
    );
    expect(mockSupervisorApi.listEvents).toHaveBeenCalledWith('test-city', {
      limit: 100,
      since: '24h',
    });
    expect(mockSupervisorApi.listMail).toHaveBeenCalledWith('test-city', { limit: 100 });
    expect(mockSupervisorApi.cityHealth).toHaveBeenCalledWith('test-city');
    expect(mockApi.listBuilds).toHaveBeenCalledTimes(1);
    expect(mockApi.systemHealth).toHaveBeenCalledTimes(1);
    expect(mockApi.doltTrend).toHaveBeenCalledTimes(1);
  });

  it('uses one captured city for every bead-attention read', async () => {
    mockSupervisorApi.listBeads.mockResolvedValue({ total: 0, items: [] });
    setActiveCity('later-city');

    const facts = await fetchBeadsAttention('captured-city', testOperator.decisionLabel);

    expect(facts).toMatchObject({ items: [], decisions: [], escalations: [] });
    expect(mockSupervisorApi.listBeads.mock.calls.map(([city]) => city)).toEqual([
      'captured-city',
      'captured-city',
      'captured-city',
    ]);
  });

  it('recovers after a city registration gap longer than 250ms', async () => {
    vi.useFakeTimers();
    let calls = 0;
    mockSupervisorApi.listBeads.mockImplementation(() => {
      calls += 1;
      if (calls <= 6) {
        return Promise.reject(
          new SupervisorApiError(
            404,
            'localized city availability detail',
            undefined,
            CITY_NOT_FOUND_CODE,
          ),
        );
      }
      return Promise.resolve({ total: 0, items: [] });
    });

    const pending = fetchBeadsAttention('captured-city', testOperator.decisionLabel);
    await vi.advanceTimersByTimeAsync(749);
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(6);
    await vi.advanceTimersByTimeAsync(1);
    const facts = await pending;

    expect(facts).toMatchObject({ items: [], decisions: [], escalations: [] });
    expect(facts.error).toBeUndefined();
    expect(facts.decisionsError).toBeUndefined();
    expect(facts.escalationsError).toBeUndefined();
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(9);
  });

  it('bounds city-unavailable retries and marks the whole cohort for revalidation', async () => {
    vi.useFakeTimers();
    mockSupervisorApi.listBeads.mockRejectedValue(
      new SupervisorApiError(404, CITY_NOT_FOUND_DETAIL, undefined, CITY_NOT_FOUND_CODE),
    );

    const pending = fetchBeadsAttention('captured-city', testOperator.decisionLabel);
    await vi.runAllTimersAsync();
    const facts = await pending;

    expect(facts).toMatchObject({
      cityUnavailable: true,
      error: CITY_NOT_FOUND_DETAIL,
      decisionsError: CITY_NOT_FOUND_DETAIL,
      escalationsError: CITY_NOT_FOUND_DETAIL,
    });
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(15);
  });

  it('uses the typed problem code instead of matching legacy-looking prose', async () => {
    mockSupervisorApi.listBeads.mockRejectedValue(
      new SupervisorApiError(404, CITY_NOT_FOUND_DETAIL, undefined, 'bead-not-found'),
    );

    const facts = await fetchBeadsAttention('captured-city', testOperator.decisionLabel);

    expect(facts.cityUnavailable).toBeUndefined();
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(3);
  });

  it('does not retry a non-city-unavailable error', async () => {
    mockSupervisorApi.listBeads.mockRejectedValue(
      new SupervisorApiError(503, 'supervisor unavailable', undefined),
    );

    const facts = await fetchBeadsAttention('captured-city', testOperator.decisionLabel);

    expect(facts).toMatchObject({
      error: 'supervisor unavailable',
      decisionsError: 'supervisor unavailable',
      escalationsError: 'supervisor unavailable',
    });
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(3);
  });

  it('propagates cancellation to every in-flight bead-attention read', async () => {
    const controller = new AbortController();
    const fallbackResolvers: Array<(value: { total: number; items: never[] }) => void> = [];
    const seenSignals: Array<AbortSignal | undefined> = [];
    mockSupervisorApi.listBeads.mockImplementation(
      (_city: string, _query: unknown, signal?: AbortSignal) =>
        new Promise((resolve, reject) => {
          seenSignals.push(signal);
          fallbackResolvers.push(resolve);
          signal?.addEventListener('abort', () => reject(signal.reason), { once: true });
        }),
    );

    const pending = fetchBeadsAttention(
      'captured-city',
      testOperator.decisionLabel,
      controller.signal,
    );
    await waitFor(() => expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(3));

    controller.abort(new DOMException('obsolete attention read', 'AbortError'));
    const everyReadWasAborted =
      seenSignals.length === 3 && seenSignals.every((signal) => signal?.aborted === true);
    if (!everyReadWasAborted) {
      for (const resolve of fallbackResolvers) resolve({ total: 0, items: [] });
    }
    await pending.catch(() => undefined);

    expect(everyReadWasAborted).toBe(true);
  });

  it('revalidates an exhausted startup gap until the city recovers', async () => {
    vi.useFakeTimers();
    setActiveCity('captured-city');
    let cityAvailable = false;
    mockSupervisorApi.listBeads.mockImplementation(() =>
      cityAvailable
        ? Promise.resolve({ total: 0, items: [] })
        : Promise.reject(
            new SupervisorApiError(404, CITY_NOT_FOUND_DETAIL, undefined, CITY_NOT_FOUND_CODE),
          ),
    );

    const { result } = renderHook(() => useLiveAttentionContributors(testOperator, undefined));

    await act(async () => {
      await vi.advanceTimersByTimeAsync(3_750);
    });
    expect(composeAttention(result.current).byDomain.beads.items).toHaveLength(3);

    cityAvailable = true;
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000);
    });

    expect(composeAttention(result.current).byDomain.beads.items).toEqual([]);
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledTimes(18);
  });

  it('suppresses an obsolete retry when the active city changes', async () => {
    vi.useFakeTimers();
    setActiveCity('captured-city');
    mockSupervisorApi.listBeads.mockImplementation((city: string) =>
      city === 'captured-city'
        ? Promise.reject(
            new SupervisorApiError(404, CITY_NOT_FOUND_DETAIL, undefined, CITY_NOT_FOUND_CODE),
          )
        : Promise.resolve({ total: 0, items: [] }),
    );

    const { result, rerender } = renderHook(() =>
      useLiveAttentionContributors(testOperator, undefined),
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(callsForCity('captured-city')).toHaveLength(3);

    setActiveCity('later-city');
    rerender();
    await act(async () => {
      await vi.runAllTimersAsync();
    });

    expect(callsForCity('captured-city')).toHaveLength(3);
    expect(callsForCity('later-city')).toHaveLength(3);
    expect(composeAttention(result.current).byDomain.beads.items).toEqual([]);
  });

  it('keeps a remounted attention panel on one city when old queue reads resolve late', async () => {
    setActiveCity('old-city');
    const oldAll = deferred<{ total: number; items: Bead[] }>();
    const oldDecisions = deferred<{ total: number; items: Bead[] }>();
    const oldEscalations = deferred<{ total: number; items: Bead[] }>();
    const requests: Array<{ path: string; queue: string }> = [];

    mockSupervisorApi.listBeads.mockImplementation(
      (city: string, query: Record<string, unknown>) => {
        const queue =
          query.label === testOperator.decisionLabel
            ? 'decisions'
            : query.label === 'gc:escalation'
              ? 'escalations'
              : 'all';
        requests.push({ path: `/v0/city/${city}/beads`, queue });

        if (city === 'old-city') {
          if (queue === 'decisions') return oldDecisions.promise;
          if (queue === 'escalations') return oldEscalations.promise;
          return oldAll.promise;
        }
        if (city !== 'new-city') throw new Error(`unexpected city ${city}`);
        if (queue !== 'decisions') return Promise.resolve({ total: 0, items: [] });
        return Promise.resolve({
          total: 1,
          items: [
            {
              id: 'new-decision',
              title: 'New city decision',
              status: 'open',
              issue_type: 'task',
              created_at: '2026-07-21T00:00:00.000Z',
              labels: [testOperator.decisionLabel],
            },
          ],
        });
      },
    );

    const view = render(<LiveBeadAttentionPanel key="old-city" />);
    await waitFor(() => expect(requests).toHaveLength(3));

    setActiveCity('new-city');
    view.rerender(<LiveBeadAttentionPanel key="new-city" />);

    expect(await screen.findByText('New city decision')).toBeTruthy();
    expect(screen.getByText('Needs you').textContent).toContain('(1)');

    await act(async () => {
      oldAll.resolve({
        total: 1,
        items: [
          {
            id: 'old-ready',
            title: 'Old city ready work',
            status: 'open',
            issue_type: 'task',
            created_at: '2026-01-01T00:00:00.000Z',
          },
        ],
      });
      oldDecisions.resolve({
        total: 1,
        items: [
          {
            id: 'old-decision',
            title: 'Old city decision',
            status: 'open',
            issue_type: 'task',
            created_at: '2026-07-20T00:00:00.000Z',
            labels: [testOperator.decisionLabel],
          },
        ],
      });
      oldEscalations.resolve({
        total: 1,
        items: [
          {
            id: 'old-escalation',
            title: 'Old city escalation',
            status: 'blocked',
            issue_type: 'bug',
            created_at: '2026-07-20T00:00:00.000Z',
            labels: ['gc:escalation'],
          },
        ],
      });
      await Promise.all([oldAll.promise, oldDecisions.promise, oldEscalations.promise]);
    });

    expect(requests).toEqual([
      { path: '/v0/city/old-city/beads', queue: 'all' },
      { path: '/v0/city/old-city/beads', queue: 'decisions' },
      { path: '/v0/city/old-city/beads', queue: 'escalations' },
      { path: '/v0/city/new-city/beads', queue: 'all' },
      { path: '/v0/city/new-city/beads', queue: 'decisions' },
      { path: '/v0/city/new-city/beads', queue: 'escalations' },
    ]);
    expect(screen.queryByText('Old city decision')).toBeNull();
    expect(screen.queryByText(/Old city ready work/)).toBeNull();
    expect(screen.queryByText(/Old city escalation/)).toBeNull();
    expect(screen.getByText('New city decision')).toBeTruthy();
    expect(screen.getByText('Needs you').textContent).toContain('(1)');
  });

  it('suppresses an obsolete retry after unmount', async () => {
    vi.useFakeTimers();
    setActiveCity('captured-city');
    mockSupervisorApi.listBeads.mockRejectedValue(
      new SupervisorApiError(404, CITY_NOT_FOUND_DETAIL, undefined, CITY_NOT_FOUND_CODE),
    );

    const { unmount } = renderHook(() => useLiveAttentionContributors(testOperator, undefined));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(0);
    });
    expect(callsForCity('captured-city')).toHaveLength(3);

    unmount();
    await vi.runAllTimersAsync();

    expect(callsForCity('captured-city')).toHaveLength(3);
  });

  it('projects the shared run-summary source onto the Runs badge facts (gascity-dashboard-2j8e.7)', () => {
    // The badge reads the SAME source object the /runs page renders, so a fresh
    // source carries its summary + status through, an error source carries the
    // message, and an absent source contributes nothing — by-construction parity
    // with no second fan-out.
    expect(runsFactsFromSource(undefined)).toBeUndefined();

    const errorFacts = runsFactsFromSource({
      source: 'runs',
      status: 'error',
      error: 'supervisor warming up',
    });
    expect(errorFacts).toEqual({ error: 'supervisor warming up', provenance: 'error' });

    const fresh = freshRunsSource();
    const freshFacts = runsFactsFromSource(fresh);
    expect(freshFacts?.summary).toBe(fresh.status === 'error' ? undefined : fresh.data);
    expect(freshFacts?.provenance).toBe('fresh');
    expect(freshFacts?.fetchedAt).toBe('2026-06-01T00:00:00.000Z');
  });

  function callsForCity(cityName: string) {
    return mockSupervisorApi.listBeads.mock.calls.filter(([city]) => city === cityName);
  }

  function LiveBeadAttentionPanel() {
    const contributors = useLiveAttentionContributors(testOperator, undefined);
    const items = composeAttention(contributors).byDomain.beads.items;
    return <BeadAttentionPanel items={items} onOpen={() => undefined} />;
  }
});

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}
