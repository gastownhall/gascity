import { renderHook, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { RunSummary, SourceState } from 'gas-city-dashboard-shared';
import { invalidate } from '../api/cache';
import { setActiveCity } from '../api/cityBase';
import type { OperatorConfig } from '../contexts/OperatorConfigContext';
import { composeAttention } from './compose';
import { runsFactsFromSource, useLiveAttentionContributors } from './liveContributors';

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

vi.mock('../supervisor/client', () => ({
  supervisorApi: () => mockSupervisorApi,
  supervisorApiForRequestBudget: mockSupervisorApiForRequestBudget,
}));

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
        rss_bytes: 128_000_000,
        heap_used_bytes: 64_000_000,
        node_version: 'v22.0.0',
      },
      host: {
        load_avg_1: 0.5,
        load_avg_5: 0.4,
        load_avg_15: 0.3,
        total_mem_bytes: 100,
        free_mem_bytes: 4,
        cpu_count: 8,
        uptime_sec: 86_400,
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
    invalidate('attention:');
  });

  it('composes Home/nav attention from direct supervisor facts and dashboard-local facts', async () => {
    const { result } = renderHook(() =>
      useLiveAttentionContributors(testOperator, undefined),
    );

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
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith('test-city', { limit: 1000 });
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith('test-city', {
      label: 'needs/stephanie',
      status: 'open',
    });
    expect(mockSupervisorApi.listBeads).toHaveBeenCalledWith('test-city', {
      label: 'gc:escalation',
      status: 'open',
    });
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
});
