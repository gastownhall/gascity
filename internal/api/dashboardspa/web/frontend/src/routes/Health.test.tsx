import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import { HealthPage } from './Health';
import { invalidate } from '../api/cache';
import { AttentionProvider } from '../attention/context';
import { createAttentionContributors, type HealthAttentionFacts } from '../attention/registry';
import type { HealthOutputBody, StatusBody } from 'gas-city-dashboard-shared/gc-supervisor';
import type {
  DoltNomsTrend,
  LocalToolVersions,
  RigStoreHealthReport,
  SystemHealth,
} from 'gas-city-dashboard-shared';
import { supervisorApiForRequestBudget } from '../supervisor/client';

const mockCityHealth = vi.fn<(cityName: string) => Promise<HealthOutputBody>>();
const mockSupervisorApiForRequestBudget = supervisorApiForRequestBudget as unknown as Mock;

vi.mock('../supervisor/client', () => ({
  supervisorApiForRequestBudget: vi.fn(() => ({
    cityHealth: mockCityHealth,
  })),
}));

// gascity-dashboard-e0hh: coverage for the absent supervisor.city /
// supervisor.version paths in Health.tsx —
// (a) the warn-toned <Kv> blocks render "not reported by supervisor",
// (b) buildSynopsis omits the "on <city>" locator clause, asserted
//     via rendered DOM rather than by exporting the module-private
//     helper. Mirrors the WorkflowRunDetail.test.tsx fetch-stub pattern.

let currentHealth: SystemHealth = baseHealth();
let currentLocalTools: LocalToolVersions = baseLocalTools();
let currentTrend: DoltNomsTrend = baseTrend();
let currentRigStores: RigStoreHealthReport = baseRigStores();
let currentStatus: StatusBody = baseStatus();
let systemHealthMode: 'ok' | 'fail' = 'ok';
let trendMode: 'ok' | 'fail' = 'ok';
let rigStoreMode: 'ok' | 'fail' = 'ok';
// gascity-dashboard-4bol: the Health status widgets read the dashboard
// backend's cached /supervisor-status snapshot, not the supervisor directly.
// 'available' = fresh sample, 'degraded' = last-good served while the latest
// read failed (must still render data), 'warming' = never sampled yet,
// 'blank' = a read failed with no prior sample. A null status under 'pending'
// is modelled via pendingStatusResponse (the local fetch has not resolved yet).
let statusMode: 'available' | 'degraded' | 'warming' | 'blank' = 'available';
let pendingStatusResponse: Promise<Response> | null = null;

beforeEach(() => {
  invalidate('health');
  mockSupervisorApiForRequestBudget.mockClear();
  mockCityHealth.mockReset();
  mockCityHealth.mockResolvedValue(presentLocator());
  currentHealth = baseHealth();
  currentLocalTools = baseLocalTools();
  currentTrend = baseTrend();
  currentRigStores = baseRigStores();
  currentStatus = baseStatus();
  systemHealthMode = 'ok';
  trendMode = 'ok';
  rigStoreMode = 'ok';
  statusMode = 'available';
  pendingStatusResponse = null;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url === '/api/health/system') {
        if (systemHealthMode === 'fail') {
          return errorResponse('system health offline');
        }
        return jsonResponse(currentHealth);
      }
      if (url === '/api/health/local-tools') {
        return jsonResponse(currentLocalTools);
      }
      if (url === '/api/city/test-city/dolt-noms/trend') {
        if (trendMode === 'fail') {
          return errorResponse('dolt trend offline');
        }
        return jsonResponse(currentTrend);
      }
      if (url === '/api/city/test-city/rig-store-health') {
        if (rigStoreMode === 'fail') {
          return errorResponse('rig store health offline');
        }
        return jsonResponse(currentRigStores);
      }
      if (url === '/api/city/test-city/supervisor-status') {
        if (pendingStatusResponse !== null) {
          return pendingStatusResponse;
        }
        if (statusMode === 'warming') {
          return jsonResponse({ available: false, reason: 'not_sampled_yet', status: null });
        }
        if (statusMode === 'blank') {
          return jsonResponse({ available: false, reason: 'status_read_failed', status: null });
        }
        if (statusMode === 'degraded') {
          return jsonResponse({
            available: false,
            reason: 'status_read_failed',
            status: currentStatus,
          });
        }
        return jsonResponse({
          available: true,
          sampledAt: '2026-06-07T00:00:00.000Z',
          status: currentStatus,
        });
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe('HealthPage', () => {
  it('renders warn-toned "not reported by supervisor" for absent city and version', async () => {
    mockCityHealth.mockResolvedValue(absentLocator());

    const { container } = renderPage();
    await screen.findByRole('heading', { name: /^health$/i });
    await screen.findByRole('heading', { name: /supervisor/i });

    const cityValue = valueFor(container, 'City');
    const versionValue = valueFor(container, 'Version');

    expect(cityValue?.textContent).toBe('not reported by supervisor');
    expect(versionValue?.textContent).toBe('not reported by supervisor');
    expect(cityValue?.className).toMatch(/text-warn/);
    expect(versionValue?.className).toMatch(/text-warn/);
  });

  it('omits the "on <city>" locator clause from the synopsis when city is absent', async () => {
    mockCityHealth.mockResolvedValue(absentLocator());

    renderPage();
    // Wait for the data-dependent Supervisor section heading before
    // reading the synopsis — the page-title 'Health' heading renders
    // even during the initial loading state, so anchoring on it could
    // race the fetch resolution on slow CI workers.
    await screen.findByRole('heading', { name: /supervisor/i });
    const heading = await screen.findByRole('heading', { name: /^health$/i });
    const synopsis = synopsisFor(heading);

    expect(synopsis).not.toBeNull();
    // Structural assertion: between "Supervisor" and "uptime" there is
    // no " on " locator clause. Coupled to the synopsis shape, not the
    // exact copy.
    expect(synopsis?.textContent ?? '').toMatch(/Supervisor healthy, uptime /);
    expect(synopsis?.textContent ?? '').not.toMatch(/Supervisor healthy on /);
  });

  it('renders city/version without warn tone and includes the locator clause when present', async () => {
    // Positive contrast for the absent-path tests — guards against a
    // false-positive where the warn tone or the dropped clause was
    // applied to every supervisor render path.
    mockCityHealth.mockResolvedValue(presentLocator());

    const { container } = renderPage();
    // Same as the test above: wait for the Supervisor section heading
    // so the data has actually loaded before we query for the City /
    // Version Kvs the assertion reads.
    await screen.findByRole('heading', { name: /supervisor/i });
    const heading = await screen.findByRole('heading', { name: /^health$/i });

    const cityValue = valueFor(container, 'City');
    const versionValue = valueFor(container, 'Version');

    expect(cityValue?.textContent).toBe('demo-city');
    expect(versionValue?.textContent).toBe('1.4.2');
    expect(cityValue?.className).not.toMatch(/text-warn/);
    expect(versionValue?.className).not.toMatch(/text-warn/);

    const synopsis = synopsisFor(heading);
    // Guard against a vacuous pass: if a future PageHeader refactor
    // restructures the synopsis element, synopsisFor would return null
    // and the positive match below would correctly fail — but assert
    // explicitly so the failure mode is "missing synopsis node" rather
    // than "positive match against an empty string".
    expect(synopsis).not.toBeNull();
    expect(synopsis?.textContent ?? '').toMatch(/Supervisor healthy on demo-city, uptime /);
  });

  it('reads city health via the generated supervisor client and status via the dashboard cached-status route', async () => {
    // gascity-dashboard-4bol: health stays on the direct (fast) supervisor
    // client, but the slow /status read moves behind the dashboard backend's
    // cached sampler at /supervisor-status. The page must NOT hit the supervisor
    // /status directly, and must NOT reintroduce a dashboard city-health mirror.
    renderPage();

    await screen.findByRole('heading', { name: /supervisor/i });

    expect(mockCityHealth).toHaveBeenCalledWith('test-city');
    expect(mockSupervisorApiForRequestBudget).toHaveBeenCalledWith(2500);
    expect(fetch).toHaveBeenCalledWith('/api/city/test-city/supervisor-status', expect.any(Object));
    expect(fetch).toHaveBeenCalledWith('/api/health/system', expect.any(Object));
    expect(fetch).toHaveBeenCalledWith('/api/health/local-tools', expect.any(Object));
    expect(fetch).not.toHaveBeenCalledWith('/api/city/test-city/health/system', expect.any(Object));
    expect(fetch).not.toHaveBeenCalledWith('/api/city/test-city/status', expect.any(Object));
  });

  it('renders fast sections while supervisor status is still pending', async () => {
    const pendingStatus = deferred<Response>();
    pendingStatusResponse = pendingStatus.promise;

    renderPage();

    await screen.findByRole('heading', { name: /supervisor/i });
    await screen.findByRole('heading', { name: /host/i });
    await screen.findByRole('heading', { name: /diagnostics/i });

    expect(screen.getByText('demo-city')).toBeTruthy();
    expect(screen.getByText('8')).toBeTruthy();
    expect(
      screen.getAllByText(/Unavailable: supervisor status still loading\./i).length,
    ).toBeGreaterThan(0);
  });

  it('keeps dashboard-local host health visible when direct supervisor health fails', async () => {
    mockCityHealth.mockRejectedValue(new Error('supervisor unavailable'));

    renderPage();

    await screen.findByRole('heading', { name: /host/i });
    expect(
      screen.getByText(
        'Supervisor not reachable. The dashboard shell stays up; live data is stale.',
      ),
    ).toBeTruthy();
    expect(screen.getByText('8')).toBeTruthy();
  });

  it('keeps host and supervisor sections visible when dolt-noms trend fails', async () => {
    trendMode = 'fail';

    renderPage();

    await screen.findByRole('heading', { name: /host/i });
    expect(screen.getByRole('heading', { name: /supervisor/i })).toBeTruthy();
    expect(screen.getByText(/dolt-noms metric unavailable/i)).toBeTruthy();
  });

  it('keeps supervisor diagnostics visible when dashboard host health fails', async () => {
    systemHealthMode = 'fail';

    renderPage();

    await screen.findByRole('heading', { name: /supervisor/i });
    expect(screen.getByText(/dashboard host health unavailable/i)).toBeTruthy();
    expect(screen.getByRole('heading', { name: /diagnostics/i })).toBeTruthy();
  });

  it('restores diagnostics from local probes plus the cached supervisor status', async () => {
    renderPage();

    await screen.findByRole('heading', { name: /diagnostics/i });

    expect(screen.getByText('Dolt usage')).toBeTruthy();
    expect(screen.getByText('Beads usage')).toBeTruthy();
    expect(screen.getByText('5')).toBeTruthy();
    expect(screen.getByText('Dolt MB-per-row ratio')).toBeTruthy();
    expect(screen.getByText('<= 1')).toBeTruthy();
    // Fresh data carries no stale marker — guards against the marker leaking
    // onto a healthy read (the negative contrast for the degraded test below).
    expect(screen.queryAllByText(/showing the last sample/i)).toHaveLength(0);
  });

  it('shows cached status data plus a stale marker when the latest sample failed but a prior one exists', async () => {
    // gascity-dashboard-4bol: a degraded report (latest /status read failed on a
    // slow supervisor) still carries the last good snapshot, so the widgets must
    // render real data with a "showing last sample" marker, NOT
    // "supervisor status unavailable".
    statusMode = 'degraded';

    renderPage();

    await screen.findByRole('heading', { name: /diagnostics/i });

    expect(screen.getByText('Dolt usage')).toBeTruthy();
    expect(screen.getByText('Beads usage')).toBeTruthy();
    expect(screen.getByText('Dolt MB-per-row ratio')).toBeTruthy();
    expect(screen.queryAllByText(/supervisor status unavailable/i)).toHaveLength(0);
    // The stale marker renders on each cached widget (Dolt usage, Beads usage,
    // Store thresholds), mirroring the rig-store-health "showing last sample".
    expect(
      screen.getAllByText(/showing the last sample; refresh failed/i).length,
    ).toBeGreaterThanOrEqual(3);
  });

  it('surfaces the warming-up copy when the backend has not sampled status yet', async () => {
    statusMode = 'warming';

    renderPage();

    await screen.findByRole('heading', { name: /diagnostics/i });

    expect(screen.getAllByText(/supervisor status sample is warming up/i).length).toBeGreaterThan(
      0,
    );
  });

  it('surfaces the read-failed copy when a sample failed with no prior snapshot', async () => {
    statusMode = 'blank';

    renderPage();

    await screen.findByRole('heading', { name: /diagnostics/i });

    expect(screen.getAllByText(/latest supervisor status read failed/i).length).toBeGreaterThan(0);
  });

  it('surfaces per-rig bead-store health with a down rig flagged', async () => {
    const { container } = renderPage();

    await screen.findByRole('heading', { name: /bead stores · per rig/i });

    // Both rigs render; the down rig surfaces its dolt-down state.
    expect(screen.getByText('codeprobe')).toBeTruthy();
    expect(screen.getByText('geo')).toBeTruthy();
    expect(screen.getByText('dolt down')).toBeTruthy();
    expect(screen.getByText(/DOWN · 127\.0\.0\.1:29620/)).toBeTruthy();
    // The ok rig surfaces an up endpoint.
    expect(screen.getByText(/up · 127\.0\.0\.1:29620/)).toBeTruthy();

    // Worst-first ordering: the down rig (geo) renders before the ok rig.
    const names = Array.from(container.querySelectorAll('span'))
      .map((s) => s.textContent)
      .filter((t) => t === 'codeprobe' || t === 'geo');
    expect(names[0]).toBe('geo');
  });

  it('keeps other Health sections visible when rig-store health fails', async () => {
    rigStoreMode = 'fail';

    renderPage();

    await screen.findByRole('heading', { name: /supervisor/i });
    await screen.findByRole('heading', { name: /bead stores · per rig/i });
    expect(screen.getByText(/per-rig store health unavailable/i)).toBeTruthy();
  });

  it('shows installed tool versions as a plain passthrough', async () => {
    const { container } = renderPage();

    await screen.findByRole('heading', { name: /tool versions/i });

    // The panel reports each installed version with no floor, drift verdict, or
    // warn tone — gc owns any version policy, the dashboard only surfaces it.
    const dolt = toolRow(container, 'dolt');
    expect(dolt?.textContent).toContain('2.0.7');
    expect(dolt?.textContent).not.toMatch(/below floor/i);
    expect(dolt?.querySelector('.text-warn')).toBeNull();

    const beads = toolRow(container, 'bd');
    expect(beads?.textContent).toContain('1.0.4');

    const gc = toolRow(container, 'gc');
    expect(gc?.textContent).toContain('dev');
    expect(gc?.textContent).not.toMatch(/recommended|not pinned|below floor/i);
  });

  it('surfaces a probe failure reason in the tool versions table', async () => {
    currentLocalTools = {
      ...baseLocalTools(),
      gc: { status: 'unavailable', reason: 'gc version probe failed: ENOENT' },
    };

    const { container } = renderPage();

    await screen.findByRole('heading', { name: /tool versions/i });

    const gc = toolRow(container, 'gc');
    expect(gc?.textContent).toContain('unavailable');
    expect(gc?.textContent).toContain('gc version probe failed: ENOENT');
  });

  it('highlights Health sections that match composed attention facts', async () => {
    const supervisor = absentLocator();
    mockCityHealth.mockResolvedValue(supervisor);
    currentHealth = {
      ...baseHealth(),
      host: {
        ...baseHealth().host,
        free_mem_bytes: 400_000_000,
      },
    };
    currentTrend = {
      available: false,
      reason: 'sample_failed',
      samples: [],
    };

    renderPage({
      attention: {
        system: currentHealth,
        supervisor: { status: 'available', data: supervisor },
        trend: currentTrend,
      },
    });

    await screen.findByRole('heading', { name: /dolt-noms/i });

    expect(sectionFor('Supervisor')?.getAttribute('data-attention-severity')).toBe('watch');
    expect(sectionFor('Host')?.getAttribute('data-attention-severity')).toBe('attention');
    expect(sectionFor('Dolt-noms · 24 h')?.getAttribute('data-attention-severity')).toBe('watch');
  });
});

function renderPage({ attention }: { attention?: HealthAttentionFacts } = {}) {
  const contributors = createAttentionContributors(
    attention === undefined ? {} : { health: attention },
  );
  return render(
    <MemoryRouter
      initialEntries={['/health']}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <AttentionProvider contributors={contributors}>
        <HealthPage />
      </AttentionProvider>
    </MemoryRouter>,
  );
}

function jsonResponse(payload: unknown): Response {
  return new Response(JSON.stringify(payload), {
    status: 200,
    headers: { 'content-type': 'application/json' },
  });
}

function errorResponse(message: string): Response {
  return new Response(JSON.stringify({ error: message }), {
    status: 503,
    headers: { 'content-type': 'application/json' },
  });
}

function valueFor(container: HTMLElement, label: string): HTMLElement | null {
  const terms = Array.from(container.querySelectorAll('dt')).filter(
    (dt) => dt.textContent?.trim() === label,
  );
  if (terms.length !== 1) return null;
  return terms[0]?.nextElementSibling as HTMLElement | null;
}

function toolRow(container: HTMLElement, label: string): HTMLElement | null {
  return container.querySelector(`[data-tool-version-row="${label}"]`);
}

function synopsisFor(heading: HTMLElement): HTMLElement | null {
  // PageHeader renders the synopsis as a sibling of the heading inside
  // a shared header element. Walk up to the nearest <header>, then look
  // for a paragraph descendant. Specific to PageHeader's structure but
  // stable — the alternative (text search) would over-couple to copy.
  const header = heading.closest('header');
  return header?.querySelector('p') ?? null;
}

function sectionFor(heading: string): HTMLElement | null {
  return screen.getByRole('heading', { name: heading }).closest('section');
}

function presentLocator(): HealthOutputBody {
  return {
    status: 'ok',
    city: 'demo-city',
    version: '1.4.2',
    uptime_sec: 4200,
  };
}

function absentLocator(): HealthOutputBody {
  // The two fields under test are deliberately omitted, not set to
  // undefined or null — that mirrors what a wire-drifted supervisor
  // payload actually looks like over JSON.
  return {
    status: 'ok',
    uptime_sec: 4200,
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function baseHealth(): SystemHealth {
  return {
    admin: {
      pid: 4242,
      uptime_sec: 600,
      rss_bytes: 50_000_000,
      heap_used_bytes: 30_000_000,
      node_version: 'v20.10.0',
    },
    host: {
      load_avg_1: 0.42,
      load_avg_5: 0.55,
      load_avg_15: 0.61,
      total_mem_bytes: 16_000_000_000,
      free_mem_bytes: 8_000_000_000,
      cpu_count: 8,
      uptime_sec: 86_400,
    },
  };
}

function baseLocalTools(): LocalToolVersions {
  return {
    // Plain installed-version passthrough — no floor or drift verdict.
    dolt: { status: 'available', version: '2.0.7', source: 'local probe: dolt version' },
    beads: { status: 'available', version: '1.0.4', source: 'local probe: bd version' },
    gc: { status: 'available', version: 'dev', source: 'local probe: gc version' },
  };
}

function baseStatus(): StatusBody {
  return {
    agent_count: 1,
    agents: {
      quarantined: 0,
      running: 1,
      suspended: 0,
      total: 1,
    },
    mail: {
      total: 2,
      unread: 1,
    },
    name: 'test-city',
    path: '/srv/gc/test-city',
    rig_count: 1,
    rigs: {
      suspended: 0,
      total: 1,
    },
    running: 1,
    store_health: {
      live_rows: 2000,
      path: '/srv/gc/test-city/.db',
      ratio_mb_per_row: 0.5,
      size_bytes: 1_000_000,
      threshold_mb_per_row: 1,
      warning: false,
      last_gc_status: 'success',
    },
    suspended: false,
    uptime_sec: 4200,
    version: '1.4.2',
    work: {
      open: 5,
      ready: 3,
      in_progress: 1,
    },
  };
}

function baseTrend(): DoltNomsTrend {
  return {
    available: true,
    samples: [],
    source: '/var/gc/.dolt/noms',
  };
}

function baseRigStores(): RigStoreHealthReport {
  return {
    available: true,
    sampledAt: '2026-06-06T00:00:00.000Z',
    rigs: [
      {
        rig: 'codeprobe',
        beadsPath: '/home/ds/projects/codeprobe/.beads',
        rollup: 'ok',
        reachable: true,
        doltEndpoint: '127.0.0.1:29620',
        doltConnected: true,
        issueCount: 129,
        problems: [],
      },
      {
        rig: 'geo',
        beadsPath: '/home/ds/projects/GEO/.beads',
        rollup: 'down',
        reachable: true,
        doltEndpoint: '127.0.0.1:29620',
        doltConnected: false,
        issueCount: null,
        problems: [],
      },
    ],
  };
}
