import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import type { UsageBody } from 'gas-city-dashboard-shared/gc-supervisor';
import { MemoryRouter } from 'react-router-dom';
import type { RunSummarySubscription } from '../runs/runSummarySubscription';
import { invalidate } from '../api/cache';
import { CockpitHomePage } from './CockpitHome';

const mocks = vi.hoisted(() => ({
  cityUsage: vi.fn(),
  cityStatus: vi.fn(),
  listRuns: vi.fn(),
  listSessions: vi.fn(),
  runSummary: vi.fn(),
}));

vi.mock('../supervisor/client', () => ({
  SUPERVISOR_REQUEST_TIMEOUT_MS: 60_000,
  supervisorApi: () => ({
    cityUsage: mocks.cityUsage,
    cityStatus: mocks.cityStatus,
    listRuns: mocks.listRuns,
    listSessions: mocks.listSessions,
  }),
}));

vi.mock('../runs/runSummarySubscription', () => ({
  useRunSummary: () => mocks.runSummary() as RunSummarySubscription,
}));

const router = (children: React.ReactNode) => (
  <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
    {children}
  </MemoryRouter>
);

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  const promise = new Promise<T>((settle) => {
    resolve = settle;
  });
  return { promise, resolve };
}

function availableRunSummary(): RunSummarySubscription {
  return {
    loading: false,
    error: null,
    refresh: vi.fn(),
    sseState: 'open',
    source: {
      source: 'runs',
      status: 'available',
      fetchedAt: '2026-07-14T12:00:00Z',
      data: {
        totalActive: 1,
        totalHistorical: 0,
        runCounts: {
          total: 1,
          visible: 1,
          prReview: 0,
          designReview: 0,
          bugfix: 0,
          blocked: 0,
          other: 1,
        },
        lanes: [
          {
            id: 'run-1',
            title: 'Deploy',
            formula: { status: 'known', name: 'deploy' },
            scope: { status: 'unavailable', error: 'scope missing' },
            external: { status: 'unavailable', error: 'external missing' },
            phase: 'active',
            phaseLabel: 'publish',
            statusCounts: {},
            activeAssignees: [],
            updatedAt: { status: 'unavailable', error: 'unknown' },
            stages: Array.from({ length: 7 }, (_, index) => ({
              key: `s${index}`,
              label: `S${index}`,
            })),
            progress: {
              status: 'active_step',
              stage: { status: 'available', index: 6, key: 's6', label: 'publish' },
              attempt: { status: 'available', value: 2 },
            },
            formulaStageResolved: true,
            health: { status: 'unavailable', error: 'not enriched' },
          },
        ],
        historicalLanes: [],
        blockedLanes: [],
        recentChanges: [],
        census: { status: 'unavailable', error: 'not needed' },
      },
    },
  } as unknown as RunSummarySubscription;
}

describe('<CockpitHomePage>', () => {
  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  beforeEach(() => {
    invalidate('cockpit:');
    mocks.cityUsage.mockReset().mockResolvedValue({
      available: true,
      recording: true,
      source: 'local_estimate',
      today: {
        invocations: 42,
        compute_facts: 0,
        input_tokens: 1000,
        output_tokens: 200,
        cache_read_tokens: 300,
        cache_creation_tokens: 0,
        wall_seconds: 0,
        cost_usd_estimate: 1.25,
        unpriced: 0,
      },
      recent: {
        invocations: 3,
        compute_facts: 0,
        input_tokens: 500,
        output_tokens: 100,
        cache_read_tokens: 0,
        cache_creation_tokens: 0,
        wall_seconds: 0,
        cost_usd_estimate: 0.5,
        unpriced: 0,
      },
      recent_by_session: [],
      recent_window_secs: 300,
      updated_at: '2026-07-14T12:00:00Z',
    });
    mocks.cityStatus.mockReset().mockResolvedValue({
      name: 'test-city',
      path: '/tmp/test-city',
      agent_count: 2,
      rig_count: 1,
      running: 2,
      suspended: false,
      uptime_sec: 10,
      agents: { total: 2, running: 2, suspended: 0, quarantined: 0 },
      rigs: { total: 1, suspended: 0 },
      work: { open: 3, ready: 1, in_progress: 1 },
      mail: { total: 2, unread: 0 },
      session_counts_detail: { active: 2, suspended: 0 },
      store_health: {
        live_rows: 10,
        path: '/tmp/store',
        ratio_mb_per_row: 0.1,
        size_bytes: 10,
        threshold_mb_per_row: 1,
        warning: false,
      },
    });
    mocks.listRuns.mockReset().mockResolvedValue({
      runs: [],
      status_counts: {
        pending: 2,
        active: 1,
        waiting: 1,
        canceling: 0,
        completed: 4,
        failed: 0,
        canceled: 0,
        skipped: 0,
      },
    });
    mocks.listSessions.mockReset().mockResolvedValue({
      items: [
        {
          id: 's1',
          session_name: 'worker',
          title: 'Worker',
          provider: 'claude',
          template: 'worker',
          state: 'active',
          attached: false,
          running: true,
          created_at: '2026-07-14T00:00:00Z',
          context_pct: 65,
        },
      ],
      total: 1,
    });
    mocks.runSummary.mockReturnValue(availableRunSummary());
  });

  it('renders real cockpit readings, canonical run states, and variable run progress', async () => {
    render(router(<CockpitHomePage />));
    expect(await screen.findByRole('status', { name: 'model calls today: 42' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'queued: 2' })).toBeTruthy();
    expect(
      screen.getByRole('link', { name: 'deploy: stage 7 of 7, retry attempt 2' }),
    ).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Worker: 65% context used' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'live feed: healthy, connected' })).toBeTruthy();
  });

  it('publishes a response slower than the poll cadence without starting an overlapping read', async () => {
    vi.useFakeTimers();
    const initialUsage = (await mocks.cityUsage()) as UsageBody;
    const slowUsage = deferred<UsageBody>();
    mocks.cityUsage
      .mockReset()
      .mockReturnValueOnce(slowUsage.promise)
      .mockResolvedValue(initialUsage);

    render(router(<CockpitHomePage />));
    await act(async () => undefined);
    expect(mocks.cityUsage).toHaveBeenCalledTimes(1);

    await act(() => vi.advanceTimersByTimeAsync(15_001));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(1);

    const publishedUsage = {
      ...initialUsage,
      today: { ...initialUsage.today, invocations: 84 },
      updated_at: '2026-07-14T12:00:16Z',
    };
    await act(async () => {
      slowUsage.resolve(publishedUsage);
      await slowUsage.promise;
    });
    expect(screen.getByRole('status', { name: 'model calls today: 84' })).toBeTruthy();

    await act(() => vi.advanceTimersByTimeAsync(14_999));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(1);
    await act(() => vi.advanceTimersByTimeAsync(1));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(2);
  });

  it('retries after a bounded deadline when a refresh never settles', async () => {
    vi.useFakeTimers();
    const initialUsage = (await mocks.cityUsage()) as UsageBody;
    const recoveredUsage = {
      ...initialUsage,
      today: { ...initialUsage.today, invocations: 126 },
      updated_at: '2026-07-14T12:01:15Z',
    };
    mocks.cityUsage
      .mockReset()
      .mockResolvedValueOnce(initialUsage)
      .mockImplementationOnce(() => new Promise<UsageBody>(() => undefined))
      .mockResolvedValue(recoveredUsage);

    render(router(<CockpitHomePage />));
    await act(async () => undefined);
    expect(screen.getByRole('status', { name: 'model calls today: 42' })).toBeTruthy();

    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(2);

    await act(() => vi.advanceTimersByTimeAsync(59_999));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(2);

    await act(() => vi.advanceTimersByTimeAsync(1));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(3);
    expect(screen.getByRole('status', { name: 'model calls today: 126' })).toBeTruthy();
  });

  it('keeps instruments mounted and labels unavailable sources honestly', async () => {
    mocks.cityUsage.mockRejectedValue(new Error('usage down'));
    mocks.cityStatus.mockRejectedValue(new Error('status down'));
    mocks.listRuns.mockRejectedValue(new Error('runs down'));
    mocks.listSessions.mockRejectedValue(new Error('sessions down'));
    mocks.runSummary.mockReturnValue({
      ...availableRunSummary(),
      source: undefined,
      sseState: 'closed',
    });
    render(router(<CockpitHomePage />));
    await waitFor(() =>
      expect(screen.getByRole('status', { name: 'model calls today: unavailable' })).toBeTruthy(),
    );
    expect(screen.getByTestId('pipeline')).toBeTruthy();
    expect(screen.getByTestId('context-meters')).toBeTruthy();
    expect(screen.getByTestId('run-rings')).toBeTruthy();
    expect(screen.getByRole('link', { name: 'live feed: unknown, disconnected' })).toBeTruthy();
    expect(screen.getAllByText(/unavailable/i).length).toBeGreaterThan(0);
  });

  it('keeps partial usage provenance beside the available odometer reading', async () => {
    const usage = await mocks.cityUsage();
    mocks.cityUsage.mockResolvedValue({
      ...usage,
      partial: true,
      partial_reasons: ['usage history exceeded the dashboard read limit'],
    });

    render(router(<CockpitHomePage />));

    const odometer = await screen.findByRole('status', { name: 'model calls today: 42' });
    expect(odometer.textContent).toContain('usage history exceeded the dashboard read limit');
  });

  it('distinguishes an available historical reading from active recording', async () => {
    const usage = await mocks.cityUsage();
    mocks.cityUsage.mockResolvedValue({ ...usage, recording: false });

    render(router(<CockpitHomePage />));

    const odometer = await screen.findByRole('status', { name: 'model calls today: 42' });
    expect(odometer.textContent).toContain('usage recording is off');
  });

  it('does not present stale status-derived system readings as healthy', async () => {
    const first = render(router(<CockpitHomePage />));
    expect(await screen.findByRole('link', { name: 'dolt store: healthy, healthy' })).toBeTruthy();
    first.unmount();

    mocks.cityStatus.mockRejectedValue(new Error('status refresh failed'));
    render(router(<CockpitHomePage />));

    expect(await screen.findByText('city status is stale · refresh failed')).toBeTruthy();
    expect(
      screen.getByRole('link', {
        name: 'dolt store: unknown, stale · last reported healthy',
      }),
    ).toBeTruthy();
  });

  it('keeps stale status provenance latched while a retry is in flight', async () => {
    const first = render(router(<CockpitHomePage />));
    expect(await screen.findByRole('link', { name: 'dolt store: healthy, healthy' })).toBeTruthy();
    first.unmount();

    vi.useFakeTimers();
    mocks.cityStatus.mockRejectedValueOnce(new Error('status refresh failed'));
    render(router(<CockpitHomePage />));
    await act(async () => undefined);
    expect(screen.getByText('city status is stale · refresh failed')).toBeTruthy();

    mocks.cityStatus.mockImplementationOnce(() => new Promise(() => undefined));
    await act(() => vi.advanceTimersByTimeAsync(15_000));

    expect(screen.getByText('city status is stale · refresh failed')).toBeTruthy();
    expect(
      screen.getByRole('link', {
        name: 'dolt store: unknown, stale · last reported healthy',
      }),
    ).toBeTruthy();
  });

  it('uses the session list when detailed active-session counts are absent', async () => {
    const current = await mocks.cityStatus();
    const { session_counts_detail: _detail, ...withoutSessionCounts } = current;
    mocks.cityStatus.mockResolvedValue({ ...withoutSessionCounts, running: 99 });

    render(router(<CockpitHomePage />));

    expect(await screen.findByRole('link', { name: 'active sessions: 1' })).toBeTruthy();
    expect(screen.queryByRole('link', { name: 'active sessions: 99' })).toBeNull();
  });

  it('reports failed store maintenance when the size ratio is within threshold', async () => {
    const current = await mocks.cityStatus();
    mocks.cityStatus.mockResolvedValue({
      ...current,
      store_health: { ...current.store_health, last_gc_status: 'failed', warning: false },
    });

    render(router(<CockpitHomePage />));

    expect(
      await screen.findByRole('link', {
        name: 'dolt store: warning, maintenance failed',
      }),
    ).toBeTruthy();
  });

  it('marks partial status lamps unknown and reports failed store maintenance', async () => {
    const current = await mocks.cityStatus();
    mocks.cityStatus.mockResolvedValue({
      ...current,
      partial: true,
      store_health: { ...current.store_health, last_gc_status: 'failed' },
    });

    render(router(<CockpitHomePage />));

    expect(
      await screen.findByRole('link', {
        name: 'dolt store: unknown, partial · last reported maintenance failed',
      }),
    ).toBeTruthy();
  });

  it('freezes live instrument projections while paused', async () => {
    vi.useFakeTimers();
    const view = render(router(<CockpitHomePage />));
    await act(async () => undefined);
    expect(screen.getByRole('link', { name: 'live feed: healthy, connected' })).toBeTruthy();
    expect(mocks.cityUsage).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'pause instruments' }));
    mocks.runSummary.mockReturnValue({ ...availableRunSummary(), sseState: 'closed' });
    view.rerender(router(<CockpitHomePage />));

    await act(() => vi.advanceTimersByTimeAsync(120_000));

    expect(screen.getByRole('button', { name: 'resume instruments' })).toBeTruthy();
    expect(screen.getByRole('link', { name: 'live feed: healthy, connected' })).toBeTruthy();
    expect(mocks.cityUsage).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole('button', { name: 'resume instruments' }));
    await act(() => vi.advanceTimersByTimeAsync(15_000));
    expect(mocks.cityUsage).toHaveBeenCalledTimes(2);
  });
});
