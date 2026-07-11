import { act, cleanup, render, screen, within } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { MemoryRouter } from 'react-router-dom';
import type { ReactNode } from 'react';
import type { RunLane, RunSummary, SourceState } from 'gas-city-dashboard-shared';
import type {
  SessionResponse,
  StatusBody,
  UsageBody,
  UsageSessionRecent,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { setActiveCity } from '../api/cityBase';
import { invalidateKey } from '../api/cache';
import { composeAttention, type AttentionItem, type AttentionModel } from '../attention/compose';
import { useAttentionModel } from '../attention/context';
import { useRunSummary } from '../runs/runSummarySubscription';
import { useGcEventFeed } from '../hooks/useGcEvents';
import { supervisorApi } from '../supervisor/client';
import { assertAtMostOneMark } from '../test/assertions/oneMarkRule';
import type * as CockpitBarrel from '../components/cockpit';
import { CockpitHomePage } from './CockpitHome';

// CockpitHome integration coverage (specs/plans/0002). The instrument
// primitives and hooks are unit-covered next to their sources; these tests pin
// the page-composition contract: every instrument stays mounted (a degraded
// source rests at zero with microcopy — it never collapses to a sentence), the
// pipeline reads bead work statuses, the odometer counts beads closed today, the
// needs-you strip is the sole maroon mark, the pause toggle freezes the scope,
// and every instrument deep-links to the spec surface.

vi.mock('../runs/runSummarySubscription', () => ({ useRunSummary: vi.fn() }));
vi.mock('../attention/context', () => ({ useAttentionModel: vi.fn() }));
vi.mock('../hooks/useGcEvents', () => ({
  useGcEventFeed: vi.fn(() => 'open'),
  useGcEventRefresh: vi.fn(() => 'open'),
}));
vi.mock('../supervisor/client', () => ({ supervisorApi: vi.fn() }));

// Oscilloscope draws to <canvas> (no jsdom impl) and needs ThemeContext; stub it
// so the page tests observe the `paused` prop without the canvas/theme rig.
vi.mock('../components/cockpit', async (importActual) => {
  const actual = await importActual<typeof CockpitBarrel>();
  return {
    ...actual,
    Oscilloscope: (props: { paused?: boolean }) => (
      <div data-testid="mock-oscilloscope" data-paused={String(props.paused ?? false)} />
    ),
  };
});

const mockUseRunSummary = useRunSummary as Mock;
const mockUseAttentionModel = useAttentionModel as Mock;
const mockUseGcEventFeed = useGcEventFeed as Mock;
const mockSupervisorApi = supervisorApi as Mock;

const CITY = 'test-city';

function usageTotals(overrides: Partial<UsageBody['today']> = {}) {
  return {
    input_tokens: 1000,
    output_tokens: 500,
    cache_read_tokens: 4000,
    cache_creation_tokens: 100,
    cost_usd_estimate: 12.5,
    compute_facts: 3,
    invocations: 9,
    unpriced: 0,
    wall_seconds: 42,
    ...overrides,
  };
}

function sessionRecent(overrides: Partial<UsageSessionRecent> = {}): UsageSessionRecent {
  return {
    session: 'polecat-gc-335825',
    session_id: 'gc-abc',
    input_tokens: 2000,
    output_tokens: 1500,
    cache_read_tokens: 90000,
    cache_creation_tokens: 500,
    cost_usd_estimate: 3.2,
    ...overrides,
  };
}

function usageFixture(overrides: Partial<UsageBody> = {}): UsageBody {
  return {
    totals: usageTotals(),
    today: usageTotals(),
    recent: usageTotals({ cost_usd_estimate: 2.0 }),
    recent_window_secs: 300,
    recent_by_session: [sessionRecent()],
    warnings: [],
    ...overrides,
  };
}

function statusFixture(overrides: Partial<StatusBody> = {}): StatusBody {
  return {
    agent_count: 4,
    agents: { quarantined: 0, running: 3, suspended: 1, total: 4 },
    mail: { total: 20, unread: 2 },
    name: CITY,
    path: '/city',
    rig_count: 2,
    rigs: { suspended: 0, total: 2 },
    running: 3,
    session_counts_detail: { active: 5, suspended: 1 },
    store_health: {
      live_rows: 1200,
      path: '/dolt',
      ratio_mb_per_row: 0.01,
      size_bytes: 48_000_000,
      threshold_mb_per_row: 0.05,
      warning: false,
    },
    suspended: false,
    uptime_sec: 3600,
    work: { in_progress: 8, hooked: 2, review: 1, open: 40, ready: 5 },
    ...overrides,
  };
}

function sessionFixture(overrides: Partial<SessionResponse> = {}): SessionResponse {
  return {
    attached: true,
    created_at: '2026-07-10T20:00:00.000Z',
    id: 'gc-abc',
    provider: 'claude',
    running: true,
    rig: 'gascity',
    session_name: 'polecat-gc-335825',
    state: 'active',
    template: 'polecat',
    title: 'polecat',
    ...overrides,
  };
}

function lane(id: string, phase: RunLane['phase'] = 'implementation'): RunLane {
  return {
    id,
    title: id,
    formula: { status: 'known', name: 'pr-review' },
    scope: { status: 'available', kind: 'rig', ref: 'gascity', rootStoreRef: 'rig:gascity' },
    external: { status: 'available', label: `pr-${id}`, url: `https://example.com/${id}` },
    phase,
    phaseLabel: phase,
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-07-10T20:00:00.000Z' },
    stages: [],
    progress: {
      status: 'active_step',
      stepId: 'implementation:code/1',
      stage: { status: 'available', index: 1, key: 'implementation', label: 'implementation' },
      attempt: { status: 'available', value: 1 },
    },
    formulaStageResolved: true,
    health: { status: 'unavailable', error: 'unused' },
  };
}

function runsSource(
  overrides: {
    lanes?: RunLane[];
    status?: 'fresh' | 'error';
  } = {},
): SourceState<RunSummary> {
  if (overrides.status === 'error') {
    return { source: 'runs', status: 'error', error: 'runs upstream timeout' };
  }
  const lanes = overrides.lanes ?? [lane('r1'), lane('r2', 'approval')];
  return {
    source: 'runs',
    status: 'fresh',
    fetchedAt: '2026-07-10T20:00:00.000Z',
    staleAt: '2026-07-10T20:01:00.000Z',
    error: { kind: 'none' },
    data: {
      totalActive: lanes.length,
      totalHistorical: 37,
      historicalLanes: [],
      blockedLanes: [],
      runCounts: {
        total: lanes.length,
        visible: lanes.length,
        prReview: 0,
        designReview: 0,
        bugfix: 0,
        blocked: 0,
        other: 0,
      },
      lanes,
      recentChanges: [],
      census: { status: 'unavailable', error: 'run health has not been derived' },
    },
  };
}

function attentionWith(items: AttentionItem[]): AttentionModel {
  return composeAttention([{ id: 'test', domain: 'runs', getItems: () => items }]);
}

function closedEventsResult(count: number) {
  return {
    items: Array.from({ length: count }, () => ({ type: 'bead.closed' })),
    total: count,
  };
}

function configure({
  runs = runsSource(),
  attention = attentionWith([]),
  usage = usageFixture(),
  status = statusFixture(),
  sessions = [sessionFixture()],
  closedToday = 0,
  connState = 'open',
}: {
  runs?: SourceState<RunSummary>;
  attention?: AttentionModel;
  usage?: UsageBody | Error;
  status?: StatusBody | Error;
  sessions?: SessionResponse[] | Error;
  closedToday?: number;
  connState?: string;
} = {}) {
  mockUseRunSummary.mockReturnValue({
    source: runs,
    loading: false,
    error: null,
    refresh: vi.fn(),
    sseState: 'open',
  });
  mockUseAttentionModel.mockReturnValue(attention);
  mockUseGcEventFeed.mockReturnValue(connState);
  mockSupervisorApi.mockReturnValue({
    cityUsage:
      usage instanceof Error ? vi.fn().mockRejectedValue(usage) : vi.fn().mockResolvedValue(usage),
    cityStatus:
      status instanceof Error
        ? vi.fn().mockRejectedValue(status)
        : vi.fn().mockResolvedValue(status),
    listSessions:
      sessions instanceof Error
        ? vi.fn().mockRejectedValue(sessions)
        : vi.fn().mockResolvedValue({ items: sessions, total: sessions.length }),
    listEvents: vi.fn().mockResolvedValue(closedEventsResult(closedToday)),
  });
}

function wrap(node: ReactNode) {
  return (
    <MemoryRouter
      initialEntries={['/']}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      {node}
    </MemoryRouter>
  );
}

async function renderPage() {
  const result = render(wrap(<CockpitHomePage />));
  // Flush the mount fetches (usage + status + sessions + closed seed) and fire
  // a couple of ~4Hz ticks so feed-derived readings settle.
  await act(async () => {
    await vi.advanceTimersByTimeAsync(600);
  });
  return result;
}

beforeEach(() => {
  setActiveCity(CITY);
  vi.useFakeTimers();
  mockUseRunSummary.mockReset();
  mockUseAttentionModel.mockReset();
  mockUseGcEventFeed.mockReset();
  mockSupervisorApi.mockReset();
  invalidateKey(`home:usage:${CITY}`);
  invalidateKey(`home:status:${CITY}`);
  invalidateKey(`home:sessions:${CITY}`);
  configure();
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  invalidateKey(`home:usage:${CITY}`);
  invalidateKey(`home:status:${CITY}`);
  invalidateKey(`home:sessions:${CITY}`);
});

describe('CockpitHomePage', () => {
  it('renders the four cockpit bands from live data', async () => {
    await renderPage();

    // Band 1 — oscilloscope (stubbed).
    expect(screen.getByTestId('mock-oscilloscope')).toBeTruthy();
    // Band 2 — odometer + three gauges (events / burn / tokens).
    expect(screen.getByTestId('odometer')).toBeTruthy();
    expect(screen.getAllByTestId('gauge-dial').length).toBe(3);
    // Band 3 — pipeline.
    expect(screen.getByTestId('pipeline')).toBeTruthy();
    // Band 4 — the 5fr/4fr/3fr grid: VU bank, run rings, status lamps.
    expect(screen.getByTestId('vu-bank')).toBeTruthy();
    expect(screen.getByTestId('run-rings')).toBeTruthy();
    expect(screen.getByTestId('status-lamps')).toBeTruthy();
  });

  it('keeps every instrument mounted when the usage read fails', async () => {
    configure({ usage: new Error('usage endpoint down') });
    await renderPage();

    // All three gauges stay on the wall (burn/tokens rest at zero, not collapsed).
    expect(screen.getAllByTestId('gauge-dial').length).toBe(3);
    expect(screen.getByTestId('odometer')).toBeTruthy();
    expect(screen.getByTestId('pipeline')).toBeTruthy();
    expect(screen.getByTestId('vu-bank')).toBeTruthy();
    // Degraded burn/throughput carries a small microcopy line, not a collapse.
    expect(screen.getByTestId('burn-note').textContent).toMatch(/usage unavailable/i);
    expect(screen.getByTestId('tokens-note').textContent).toMatch(/usage unavailable/i);
    // The odometer sub-line always renders, degrading to an em-dash estimate.
    expect(screen.getByText('— est today')).toBeTruthy();
  });

  it('keeps the pipeline, VU bank, and lamps mounted when the run summary errors', async () => {
    configure({ runs: runsSource({ status: 'error' }) });
    await renderPage();

    // The rings band shows its empty-state text; the instrument column stays.
    expect(screen.getByTestId('rings-empty').textContent).toMatch(/run data unavailable/i);
    expect(screen.queryByTestId('run-rings')).toBeNull();
    // Census no longer feeds the pipeline — it reads status.work, unaffected.
    expect(screen.getByTestId('pipeline')).toBeTruthy();
    expect(screen.getByTestId('vu-bank')).toBeTruthy();
    expect(screen.getByTestId('status-lamps')).toBeTruthy();
  });

  it('drives the pipeline from bead work statuses', async () => {
    await renderPage();

    const segments = screen.getAllByTestId('pipeline-segment');
    expect(segments.length).toBe(4);
    for (const segment of segments) {
      expect(segment.getAttribute('href')).toBe('/beads');
    }
    const legend = screen.getByTestId('pipeline-legend');
    expect(legend.textContent).toContain('ready');
    expect(legend.textContent).toContain('hooked');
    expect(legend.textContent).toContain('in progress');
    expect(legend.textContent).toContain('review');
    // Counts come from status.work (ready 5, hooked 2, in_progress 8, review 1).
    const inProgress = within(legend)
      .getAllByTestId('pipeline-legend-entry')
      .find((e) => e.textContent?.includes('in progress'));
    expect(inProgress?.textContent).toContain('8');
  });

  it('seeds the odometer from the beads-closed-today event count', async () => {
    configure({ closedToday: 7 });
    await renderPage();

    // digits=4 → "beads closed today: 7" aria-label on the unlinked block.
    expect(screen.getByLabelText('beads closed today: 7')).toBeTruthy();
  });

  it('renders the spec lamp set with their deep links', async () => {
    await renderPage();

    const lamps = screen.getByTestId('status-lamps');
    const links = within(lamps).getAllByRole('link');
    const byLabel = new Map(
      links.map((link) => [link.getAttribute('aria-label') ?? '', link.getAttribute('href')]),
    );
    expect([...byLabel.keys()].some((k) => /orders/i.test(k))).toBe(true);
    expect([...byLabel.keys()].some((k) => /patrol/i.test(k))).toBe(true);
    expect([...byLabel.keys()].some((k) => /dolt store/i.test(k))).toBe(true);
    expect([...byLabel.keys()].some((k) => /mail traffic/i.test(k))).toBe(true);
    for (const [label, href] of byLabel) {
      if (/orders/i.test(label)) expect(href).toBe('/activity?mode=events&type=order.fired');
      if (/patrol/i.test(label)) expect(href).toBe('/agents');
      if (/dolt store/i.test(label)) expect(href).toBe('/health');
      if (/mail traffic/i.test(label)) expect(href).toBe('/mail');
    }
  });

  it('surfaces the top attention item in the needs-you strip as the sole maroon mark', async () => {
    const attention = attentionWith([
      {
        id: 'mayor-1',
        domain: 'agents',
        severity: 'attention',
        title: 'mayor is blocked on a decision',
        href: '/agents/mayor',
        updatedAt: '2026-07-10T19:59:00.000Z',
      },
      {
        id: 'bead-2',
        domain: 'beads',
        severity: 'watch',
        title: 'a bead needs a look',
        href: '/beads',
      },
    ]);
    configure({ attention });
    const { container } = await renderPage();

    const strip = screen.getByTestId('needs-you');
    const link = within(strip).getByRole('link', { name: /mayor is blocked/i });
    expect(link.getAttribute('href')).toBe('/agents/mayor');
    expect(strip.textContent).toMatch(/needs you/i);
    // A second actionable item surfaces as the overflow count.
    expect(strip.textContent).toMatch(/·\s*1 more/);
    // DESIGN.md One Mark Rule: the glyph + label wrapper is the only maroon mark.
    assertAtMostOneMark(container);
    expect(container.querySelectorAll('.text-accent').length).toBe(1);
  });

  it('reads "nothing needs you" when the city is calm, with zero maroon marks', async () => {
    configure({ attention: attentionWith([]) });
    const { container } = await renderPage();

    expect(screen.getByTestId('needs-you').textContent).toMatch(/nothing needs you/i);
    assertAtMostOneMark(container);
    expect(container.querySelectorAll('.text-accent').length).toBe(0);
  });

  it('freezes the oscilloscope when the pause button is toggled, flipping the chip label', async () => {
    await renderPage();

    expect(screen.getByTestId('mock-oscilloscope').getAttribute('data-paused')).toBe('false');
    expect(screen.getByTestId('live-label').textContent).toBe('live');

    await act(async () => {
      screen.getByTestId('scope-pause').click();
    });

    expect(screen.getByTestId('mock-oscilloscope').getAttribute('data-paused')).toBe('true');
    expect(screen.getByTestId('live-label').textContent).toBe('paused');
  });

  it('does not apply a poll that resolves after pause (frozen synopsis)', async () => {
    // usage A settles on mount; a deferred B stands in for the first post-mount
    // poll so it can be left in flight across the pause boundary.
    let resolveB: (value: UsageBody) => void = () => {};
    const usageA = usageFixture(); // today tokens 5600 → "5.6K"
    const usageB = usageFixture({
      today: usageTotals({
        input_tokens: 50_000,
        output_tokens: 0,
        cache_read_tokens: 0,
        cache_creation_tokens: 0,
      }),
    }); // today tokens 50_000 → "50.0K"
    const cityUsage = vi
      .fn()
      .mockResolvedValueOnce(usageA)
      .mockImplementationOnce(() => new Promise<UsageBody>((res) => (resolveB = res)));
    mockSupervisorApi.mockReturnValue({
      cityUsage,
      cityStatus: vi.fn().mockResolvedValue(statusFixture()),
      listSessions: vi.fn().mockResolvedValue({ items: [sessionFixture()], total: 1 }),
      listEvents: vi.fn().mockResolvedValue(closedEventsResult(0)),
    });

    render(wrap(<CockpitHomePage />));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(600);
    });
    expect(screen.getByTestId('cockpit-synopsis').textContent).toContain('5.6K tokens');

    // Fire the first post-mount usage poll (USAGE_POLL_MS) while still live; its
    // deferred fetch is now in flight.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(20_000);
    });
    expect(cityUsage).toHaveBeenCalledTimes(2);

    // Pause, THEN let the in-flight poll resolve with different data.
    await act(async () => {
      screen.getByTestId('scope-pause').click();
    });
    await act(async () => {
      resolveB(usageB);
      await vi.advanceTimersByTimeAsync(0);
    });

    // The synopsis is frozen at A — the late poll did not move the wall.
    expect(screen.getByTestId('cockpit-synopsis').textContent).toContain('5.6K tokens');
    expect(screen.getByTestId('cockpit-synopsis').textContent).not.toContain('50.0K');
  });

  it('deep-links every instrument to its spec surface', async () => {
    await renderPage();

    const gaugeLinks = screen
      .getAllByTestId('gauge-dial')
      .map((dial) => dial.closest('a')?.getAttribute('href'));
    expect(gaugeLinks).toContain('/activity?mode=events');
    // burn and tokens both point at the worker.operation event view.
    expect(gaugeLinks.filter((h) => h === '/activity?mode=events&type=worker.operation').length).toBe(
      2,
    );

    // The odometer block is NOT a link (spec: not clickable).
    expect(screen.getByTestId('odometer').closest('a')).toBeNull();

    // Pipeline segments → beads.
    for (const segment of screen.getAllByTestId('pipeline-segment')) {
      expect(segment.getAttribute('href')).toBe('/beads');
    }
    // VU meters → agents roster.
    for (const meter of screen.getAllByTestId('vu-meter')) {
      expect(meter.getAttribute('href')).toBe('/agents');
    }
    // Run rings → run-detail.
    const ring = screen.getAllByTestId('run-ring')[0]!;
    expect(ring.getAttribute('href')).toMatch(/^\/runs\/r1/);
  });
});
