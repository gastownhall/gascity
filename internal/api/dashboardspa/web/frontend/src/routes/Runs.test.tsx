import { act, cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import {
  GC_EVENT_PREFIX,
  type RunSummary,
  type SourceStatus,
  type RunLane,
  type SourceState,
} from 'gas-city-dashboard-shared';
import { setActiveCity } from '../api/cityBase';
import { invalidateKey } from '../api/cache';
import { AttentionProvider } from '../attention/context';
import type { AttentionContributor } from '../attention/compose';
import { RunsPage } from './Runs';
import { RunSummaryProvider } from '../runs/runSummarySubscription';
import { MemoryRouter } from 'react-router-dom';
import { NowProvider } from '../contexts/NowContext';
import {
  loadSupervisorRunSummaryActiveSource,
  loadSupervisorRunSummaryPreviewSource,
  loadSupervisorRunSummarySource,
} from '../supervisor/runSummary';

// gascity-dashboard-bqn: regression coverage for the live-updates wiring
// on /runs. The actual SSE / coalesce / reconnect behavior lives in
// useGcEventRefresh (untested today — separate follow-up bead). These
// tests pin Runs.tsx's contract with that hook + with the api
// client's bypass-TTL refresh path.
//
// What's pinned here:
//   - useGcEventRefresh is called with GC_EVENT_PREFIX.bead and a function.
//   - <SseIndicator state={...} /> renders inside PageHeader meta.
//   - The manual Refresh button refetches the direct supervisor run summary,
//     not the dashboard snapshot facade.
//   - A burst of synthetic SSE matches within the in-component debounce
//     window produces AT MOST one direct run-summary refresh (architect H2
//     upstream-load protection).
//   - The SSE callback no-ops when runs.status !== 'fresh' so
//     fixture-fallback mode isn't hammered (architect H1).

vi.mock('../supervisor/runSummary', () => ({
  loadSupervisorRunSummaryPreviewSource: vi.fn(),
  loadSupervisorRunSummarySource: vi.fn(),
  loadSupervisorRunSummaryActiveSource: vi.fn(),
}));

// Capture the prefixes + onMatch passed to useGcEventRefresh so each
// test can fire synthetic events into Runs' callback directly.
// Bypasses real EventSource — the hook's own coalesce / reconnect is
// not under test here.
const lastHookCall: { prefixes: ReadonlyArray<string> | null; onMatch: (() => void) | null } = {
  prefixes: null,
  onMatch: null,
};
vi.mock('../hooks/useGcEvents', () => ({
  useGcEventRefresh: vi.fn((prefixes: ReadonlyArray<string>, onMatch: () => void) => {
    lastHookCall.prefixes = prefixes;
    lastHookCall.onMatch = onMatch;
    return 'open' as const;
  }),
}));

const mockLoadRunSummaryPreview = loadSupervisorRunSummaryPreviewSource as Mock;
const mockLoadRunSummary = loadSupervisorRunSummarySource as Mock;
// gascity-dashboard: SSE-driven refreshes route through the CHEAP active source;
// only the manual Refresh button + one-time first upgrade use the wide source.
const mockLoadRunSummaryActive = loadSupervisorRunSummaryActiveSource as Mock;

function buildRunSource(
  runsStatus: Exclude<SourceStatus, 'error'> = 'fresh',
): SourceState<RunSummary> {
  return {
    source: 'runs',
    status: runsStatus,
    fetchedAt: '2026-05-25T00:00:00.000Z',
    staleAt: '2026-05-25T00:01:00.000Z',
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

function completedLane(): RunLane {
  return {
    id: 'done-root',
    title: 'Completed formula run',
    formula: { status: 'known', name: 'mol-adopt-pr-v2' },
    scope: {
      status: 'available',
      kind: 'city',
      ref: 'racoon-city',
      rootStoreRef: 'city:racoon-city',
    },
    external: { status: 'unavailable', error: 'external unavailable in test' },
    phase: 'complete',
    phaseLabel: 'complete',
    statusCounts: { closed: 2 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-05-27T22:01:00Z' },
    stages: [
      { key: 'intake', label: 'Intake', status: 'complete' },
      { key: 'implementation', label: 'Implementation', status: 'complete' },
      { key: 'review', label: 'Review', status: 'complete' },
      { key: 'approval', label: 'Approval', status: 'complete' },
      { key: 'finalization', label: 'Finalization', status: 'complete' },
    ],
    progress: {
      status: 'stage_only',
      stage: {
        status: 'available',
        index: 4,
        key: 'finalization',
        label: 'Finalization',
      },
      error: 'active run step unavailable',
    },
    formulaStageResolved: false,
    health: {
      status: 'available',
      data: {
        phaseConfidence: 'known',
        needsOperator: false,
        stuckNode: { status: 'unavailable', error: 'run stuck node unavailable' },
        thrashingDetected: false,
        session: { status: 'unresolved', error: 'run session unresolved' },
      },
    },
  };
}

function requireRunData(source: SourceState<RunSummary>) {
  if (source.status === 'error') throw new Error(source.error);
  return source.data;
}

function activeLane(overrides: Partial<RunLane> = {}): RunLane {
  return {
    ...completedLane(),
    id: 'active-root',
    title: 'Active formula run',
    phase: 'implementation',
    phaseLabel: 'implementation',
    statusCounts: { in_progress: 1 },
    health: {
      status: 'available',
      data: {
        phaseConfidence: 'known',
        needsOperator: false,
        stuckNode: { status: 'unavailable', error: 'run stuck node unavailable' },
        thrashingDetected: false,
        session: { status: 'unresolved', error: 'run session unresolved' },
      },
    },
    ...overrides,
  };
}

function contributor(items: ReturnType<AttentionContributor['getItems']>): AttentionContributor {
  return {
    id: 'runs:test',
    domain: 'runs',
    getItems: () => items,
  };
}

beforeEach(() => {
  setActiveCity('racoon-city');
  mockLoadRunSummaryPreview.mockReset();
  mockLoadRunSummary.mockReset();
  mockLoadRunSummaryActive.mockReset();
  lastHookCall.prefixes = null;
  lastHookCall.onMatch = null;
  invalidateKey('runs:summary:racoon-city');
  mockLoadRunSummaryPreview.mockResolvedValue(buildRunSource('fresh'));
  mockLoadRunSummary.mockResolvedValue(buildRunSource('fresh'));
  mockLoadRunSummaryActive.mockResolvedValue(buildRunSource('fresh'));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
});

function mount(initialPath = '/runs', contributors: readonly AttentionContributor[] = []) {
  return render(
    <MemoryRouter
      initialEntries={[initialPath]}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <AttentionProvider contributors={contributors}>
          <RunSummaryProvider>
            <RunsPage />
          </RunSummaryProvider>
        </AttentionProvider>
      </NowProvider>
    </MemoryRouter>,
  );
}

async function waitForMount() {
  // Wait until the Refresh button mounts AND becomes enabled (loading
  // flips back to false after the initial fetcher resolves). Using the
  // disabled-to-enabled transition rather than text presence keeps the
  // wait stable across copy changes and avoids substring collisions
  // (the page renders both "Active" in CountsHeader and "active
  // runs" in the synopsis line).
  //
  // gascity-dashboard-fc3k: this only proves the preview fetch resolved
  // and the button enabled — the run-summary lanes / rig headers / counts
  // / error banner paint on a LATER state commit, and the full refresh is
  // dispatched from a useEffect that runs after that. So any assertion
  // that reads loaded run-summary data must use an async query
  // (findByText / findByRole / waitFor), not a synchronous getBy*, or it
  // races the paint under full-suite parallel load.
  const btn = (await screen.findByRole('button', { name: /refresh/i })) as HTMLButtonElement;
  await waitFor(() => expect(btn.disabled).toBe(false));
}

describe('RunsPage — SSE wiring (gascity-dashboard-bqn)', () => {
  it('paints from the fast preview source before the full run summary resolves', async () => {
    const preview = buildRunSource('fresh');
    const previewRuns = requireRunData(preview);
    previewRuns.totalActive = 1;
    previewRuns.runCounts.total = 1;
    previewRuns.runCounts.visible = 1;
    previewRuns.lanes = [activeLane({ title: 'Preview formula run' })];
    mockLoadRunSummaryPreview.mockResolvedValue(preview);
    const full = deferred<SourceState<RunSummary>>();
    mockLoadRunSummary.mockReturnValue(full.promise);

    mount();

    expect(await screen.findByRole('link', { name: /Preview formula run/i })).toBeTruthy();
    expect(screen.queryByText(/Loading formula runs/i)).toBeNull();
    expect(mockLoadRunSummaryPreview).toHaveBeenCalledTimes(1);
    // waitFor (see waitForMount): the full refresh fires from a useEffect a
    // commit after the preview link paints. Mock is called once, so this
    // still pins "exactly one" without weakening the assertion.
    await waitFor(() => expect(mockLoadRunSummary).toHaveBeenCalledTimes(1));

    await act(async () => {
      full.resolve(buildRunSource('fresh'));
      await full.promise;
    });
    await waitForMount();
  });

  it('subscribes to useGcEventRefresh with [bead.] prefix', async () => {
    mount();
    await waitForMount();
    expect(lastHookCall.prefixes).toEqual([GC_EVENT_PREFIX.bead]);
    expect(typeof lastHookCall.onMatch).toBe('function');
  });

  it('renders the SseIndicator in PageHeader meta', async () => {
    mount();
    await waitForMount();
    // SseIndicator with state='open' renders a StatusBadge with label 'live'.
    expect(await screen.findByText(/^live$/i)).toBeTruthy();
  });

  it('marks run lanes that match composed run attention without hiding other runs', async () => {
    const source = buildRunSource('fresh');
    const runs = requireRunData(source);
    const blocked = activeLane({
      id: 'blocked-root',
      title: 'Blocked formula run',
      phase: 'blocked',
      phaseLabel: 'blocked',
      statusCounts: { blocked: 1 },
      health: {
        status: 'available',
        data: {
          phaseConfidence: 'known',
          needsOperator: true,
          stuckNode: { status: 'unavailable', error: 'run stuck node unavailable' },
          thrashingDetected: false,
          session: { status: 'unresolved', error: 'run session unresolved' },
        },
      },
    });
    const calm = activeLane({
      id: 'calm-root',
      title: 'Calm formula run',
      phase: 'implementation',
      phaseLabel: 'implementation',
      statusCounts: { in_progress: 1 },
    });
    runs.totalActive = 2;
    runs.runCounts.total = 2;
    runs.runCounts.blocked = 1;
    runs.lanes = [blocked, calm];
    mockLoadRunSummary.mockResolvedValue(source);

    mount('/runs', [
      contributor([
        {
          id: 'runs:blocked-root:needs-operator',
          domain: 'runs',
          severity: 'attention',
          title: 'Blocked formula run needs operator',
        },
      ]),
    ]);

    const blockedLink = await screen.findByRole('link', { name: /Blocked formula run/i });
    const calmLink = await screen.findByRole('link', { name: /Calm formula run/i });

    expect(blockedLink.closest('li')?.getAttribute('data-attention-severity')).toBe('attention');
    expect(calmLink.closest('li')?.getAttribute('data-attention-severity')).toBeNull();
  });

  it('does not flatten an unavailable run count into zero (first load, no prior data)', async () => {
    // A GENUINE first-load failure — both the preview paint and the full refresh
    // error with no prior good snapshot to retain — still surfaces the error, so
    // an empty view never lies about the store. (A refresh that errors AFTER a
    // good paint is covered by the last-good-retention test in
    // runSummarySubscription.test.tsx: it keeps the good lanes as stale.)
    mockLoadRunSummaryPreview.mockResolvedValue({
      source: 'runs',
      status: 'error',
      error: 'run collector unavailable in test',
    } satisfies SourceState<RunSummary>);
    mockLoadRunSummary.mockResolvedValue({
      source: 'runs',
      status: 'error',
      error: 'run collector unavailable in test',
    } satisfies SourceState<RunSummary>);

    mount();
    await waitForMount();

    expect(
      await screen.findByText(/Run counts unavailable: run collector unavailable in test/i),
    ).toBeTruthy();
    expect(screen.queryByText(/^0 active runs/i)).toBeNull();
  });

  // yh5i: completed lanes now land in historicalLanes (toggle-visible),
  // not the default-visible `lanes`. The test below pins the new contract;
  // see the toggle tests further down for the ?history=1 reveal path.
  it('yh5i: hides completed formula runs from default view, shows them under ?history=1', async () => {
    const source = buildRunSource('fresh');
    const lane = completedLane();
    const runs = requireRunData(source);
    runs.totalActive = 0;
    runs.totalHistorical = 1;
    runs.lanes = [];
    runs.historicalLanes = [lane];
    runs.census = {
      status: 'available',
      data: {
        byPhase: {
          intake: 0,
          implementation: 0,
          review: 0,
          approval: 0,
          finalization: 0,
          blocked: 0,
          complete: 1,
          active: 0,
        },
        totalInFlight: 0,
        unverifiable: 0,
        knownDenominator: 0,
        thrashing: 0,
      },
    };
    mockLoadRunSummary.mockResolvedValue(source);

    // Default view (/runs): historical lane is hidden, empty-state
    // trailer hints at the count.
    mount();
    await waitForMount();
    expect(screen.queryByText('Completed formula run')).toBeNull();
    expect(await screen.findByText(/No active formula runs\. \(1 completed\.\)/i)).toBeTruthy();
    // The toggle button is enabled (totalHistorical > 0) and labeled
    // with the count.
    const toggleDefault = (await screen.findByRole('button', {
      name: /show 1 completed/i,
    })) as HTMLButtonElement;
    await waitFor(() => expect(toggleDefault.disabled).toBe(false));
    expect(toggleDefault.getAttribute('aria-expanded')).toBe('false');
    cleanup();

    // History view (?history=1): the historical section renders the lane.
    mount('/runs?history=1');
    await waitForMount();
    expect(await screen.findByText('Completed formula run')).toBeTruthy();
    const toggleHistory = (await screen.findByRole('button', {
      name: /hide historical/i,
    })) as HTMLButtonElement;
    expect(toggleHistory.getAttribute('aria-expanded')).toBe('true');
    expect(toggleHistory.getAttribute('aria-controls')).toBeTruthy();
  });

  it('7hek: groups active lanes by rig under section headers and shows each root bead id', async () => {
    const source = buildRunSource('fresh');
    const runs = requireRunData(source);
    const laneA: RunLane = {
      ...completedLane(),
      id: 'gc-aaa',
      phase: 'approval',
      phaseLabel: 'approval',
      scope: { status: 'available', kind: 'rig', ref: 'gascity', rootStoreRef: 'rig:gascity' },
    };
    const laneB: RunLane = {
      ...completedLane(),
      id: 'gc-bbb',
      phase: 'approval',
      phaseLabel: 'approval',
      scope: {
        status: 'available',
        kind: 'rig',
        ref: 'gascity-packs',
        rootStoreRef: 'rig:gascity-packs',
      },
    };
    runs.totalActive = 2;
    runs.lanes = [laneA, laneB];
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();
    // Rig section headers (the `rig:` prefix is stripped for display).
    expect(await screen.findByText('gascity')).toBeTruthy();
    expect(await screen.findByText('gascity-packs')).toBeTruthy();
    // Each run's root bead id is rendered so same-formula runs are distinguishable.
    expect(await screen.findByText('gc-aaa')).toBeTruthy();
    expect(await screen.findByText('gc-bbb')).toBeTruthy();
  });

  it('yh5i: toggle button is disabled when totalHistorical is 0', async () => {
    // Default run source has totalHistorical = 0.
    mount();
    await waitForMount();
    const toggle = (await screen.findByRole('button', {
      name: /no completed formula runs in the current window/i,
    })) as HTMLButtonElement;
    expect(toggle.disabled).toBe(true);
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
    // aria-controls must NOT reference a non-existent DOM id when the
    // historical section is not rendered (WAI-ARIA spec).
    expect(toggle.getAttribute('aria-controls')).toBeNull();
  });

  it('yh5i: toggle stays enabled when showHistory=true even if totalHistorical drops to 0', async () => {
    // Reachable via back-button + SSE refresh: URL has ?history=1 but the
    // last historical lane has since rolled out. The user must still be
    // able to dismiss the historical section.
    mount('/runs?history=1');
    await waitForMount();
    const toggle = (await screen.findByRole('button', {
      name: /hide historical/i,
    })) as HTMLButtonElement;
    expect(toggle.disabled).toBe(false);
    expect(await screen.findByText(/No completed runs in the current window/i)).toBeTruthy();
  });

  it('manual Refresh button refetches the direct supervisor run summary', async () => {
    mount();
    await waitFor(() => expect(mockLoadRunSummary).toHaveBeenCalledTimes(1));
    await waitForMount();
    // Reset to ignore the mount-effect call.
    mockLoadRunSummary.mockClear();

    const btn = screen.getByRole('button', { name: /refresh/i }) as HTMLButtonElement;
    await act(async () => {
      btn.click();
    });

    expect(mockLoadRunSummary).toHaveBeenCalledTimes(1);
  });

  it('coalesces a burst of SSE matches to AT MOST one run-summary refresh within the debounce window', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mount();
    await waitForMount();
    mockLoadRunSummaryActive.mockClear();

    // Simulate a busy slung pipeline: 5 onMatch calls within 1s. The
    // 10s in-component debounce floor must collapse this to a single
    // upstream read. SSE bursts take the CHEAP active source.
    // (useGcEventRefresh's own 2.5s coalesce sits in front of onMatch in
    // production; this test exercises ONLY the Runs-side debounce per
    // architect H2.)
    await act(async () => {
      for (let i = 0; i < 5; i++) {
        lastHookCall.onMatch?.();
        await vi.advanceTimersByTimeAsync(50);
      }
    });

    // Within the 10s window, exactly one (the leading edge) fires.
    // `toBe(1)` rather than `toBeLessThanOrEqual(1)` so a regression
    // that suppresses the leading edge entirely (count would be 0) is
    // caught loudly.
    expect(mockLoadRunSummaryActive.mock.calls.length).toBe(1);
  });

  it('fires a second run-summary refresh once the debounce window elapses', async () => {
    // Pins the trailing edge of the in-component debounce. The burst
    // test above proves we collapse a flurry to one POST; this test
    // proves we DON'T accidentally latch the gate shut forever. If a
    // future refactor drops the `lastRefreshAtRef.current = Date.now()`
    // reset (or fails to clear it on error), the second event would be
    // silently swallowed and the page would stop receiving live updates
    // until full reload. Catch that loudly here.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    mount();
    await waitForMount();
    mockLoadRunSummaryActive.mockClear();

    // Leading edge: one event, one CHEAP read.
    await act(async () => {
      lastHookCall.onMatch?.();
    });
    expect(mockLoadRunSummaryActive.mock.calls.length).toBe(1);

    // Advance past the 10s debounce floor (REFRESH_DEBOUNCE_MS = 10_000
    // in the subscription; +100ms cushion so we're unambiguously past it).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_100);
    });

    // Second event after the window must fire a second CHEAP read.
    await act(async () => {
      lastHookCall.onMatch?.();
    });
    expect(mockLoadRunSummaryActive.mock.calls.length).toBe(2);
  });

  it('SSE callback no-ops when runs source status is not fresh', async () => {
    // First load returns a fixture-status source (gc supervisor is
    // down, committed fixtures are serving). SSE-driven force
    // refresh must NOT fire in this state — otherwise we hammer
    // loadFixture every coalesce-tick during a gc outage.
    mockLoadRunSummary.mockResolvedValue(buildRunSource('fixture'));
    mount();
    await waitForMount();
    mockLoadRunSummary.mockClear();
    mockLoadRunSummaryActive.mockClear();

    await act(async () => {
      lastHookCall.onMatch?.();
    });

    // Fixture status short-circuits before any refresh — neither path fires.
    expect(mockLoadRunSummary).not.toHaveBeenCalled();
    expect(mockLoadRunSummaryActive).not.toHaveBeenCalled();
  });
});

describe('RunsPage — partial lane set (gascity-dashboard-n6f1)', () => {
  it('surfaces a "runs partial" degraded signal when lanesPartial is set', async () => {
    const source = buildRunSource('fresh');
    requireRunData(source).lanesPartial = true;
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    const marker = await screen.findByText(/runs partial/i);
    const live = await screen.findByText(/^live$/i);
    expect(marker).toBeTruthy();
    expect(marker.getAttribute('role')).toBe('status');
    expect(live.compareDocumentPosition(marker) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('omits the partial signal on a clean direct run source', async () => {
    mount();
    await waitForMount();

    expect(screen.queryByRole('status')).toBeNull();
  });
});

describe('RunsPage — blocked lanes are not Active (gascity-dashboard-4xcv)', () => {
  function blockedLane(): RunLane {
    return activeLane({
      id: 'gc-1920',
      title: 'mol-focus-review latch',
      phase: 'blocked',
      phaseLabel: 'blocked',
      statusCounts: { blocked: 1 },
      health: {
        status: 'available',
        data: {
          phaseConfidence: 'inferred',
          needsOperator: true,
          stuckNode: { status: 'unavailable', error: 'run stuck node unavailable' },
          thrashingDetected: false,
          session: { status: 'unresolved', error: 'run session unresolved' },
        },
      },
    });
  }

  it('renders a blocked lane under the Blocked section, not among active rig groups', async () => {
    const source = buildRunSource('fresh');
    const runs = requireRunData(source);
    runs.totalActive = 1;
    runs.lanes = [
      activeLane({
        scope: { status: 'available', kind: 'rig', ref: 'gascity', rootStoreRef: 'rig:gascity' },
      }),
    ];
    runs.blockedLanes = [blockedLane()];
    runs.runCounts = { ...runs.runCounts, total: 1, visible: 1, blocked: 1 };
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    const blockedSection = await screen.findByRole('region', { name: /blocked runs/i });
    expect(blockedSection.textContent).toContain('mol-focus-review latch');
    // The active rig group does not contain the blocked lane.
    const activeGroupHeading = await screen.findByText('gascity');
    expect(activeGroupHeading.parentElement?.textContent).not.toContain('mol-focus-review latch');
  });

  it('omits the Blocked section when nothing is blocked', async () => {
    mount();
    await waitForMount();
    expect(screen.queryByRole('region', { name: /blocked runs/i })).toBeNull();
  });
});

describe('RunsPage — blocked legibility + partial glyph (gascity-dashboard-2j8e.2)', () => {
  it('shows why-blocked and how-to-unblock per blocked run, headed by the count', async () => {
    const source = buildRunSource('fresh');
    const runs = requireRunData(source);
    runs.blockedLanes = [
      activeLane({
        id: 'gc-1920',
        title: 'mol-focus-review latch',
        phase: 'blocked',
        phaseLabel: 'blocked',
        statusCounts: { blocked: 1 },
        activeAssignees: [],
      }),
    ];
    runs.runCounts = { ...runs.runCounts, blocked: 1 };
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    const blockedSection = await screen.findByRole('region', { name: /blocked runs/i });
    // The header count is the same selectBlockedRuns the nav badge counts.
    expect(blockedSection.textContent).toContain('Blocked (1)');
    // Why-blocked (the completedLane base resolves the Finalization stage).
    expect(blockedSection.textContent).toContain('Blocked at Finalization');
    // How-to-unblock.
    expect(blockedSection.textContent).toContain('No worker assigned. Claim or dispatch one.');
  });

  it('pairs the partial indicator with a ◐ glyph (DESIGN.md status = glyph + word)', async () => {
    const source = buildRunSource('fresh');
    requireRunData(source).lanesPartial = true;
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    const marker = await screen.findByRole('status');
    expect(marker.textContent).toContain('◐');
    expect(marker.textContent).toContain('runs partial');
  });
});

describe('RunsPage — run scope labels (gascity-dashboard-4xcv)', () => {
  it('groups city-scoped and scope-unavailable lanes under a single "city" header, never "unknown rig"', async () => {
    const source = buildRunSource('fresh');
    const runs = requireRunData(source);
    runs.totalActive = 2;
    runs.lanes = [
      activeLane({
        id: 'gc-city-run',
        scope: {
          status: 'available',
          kind: 'city',
          ref: 'racoon-city',
          rootStoreRef: 'city:racoon-city',
        },
      }),
      activeLane({
        id: 'gc-scopeless-run',
        scope: { status: 'unavailable', error: 'run scope metadata unavailable' },
      }),
    ];
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    expect(await screen.findByText('gc-city-run')).toBeTruthy();
    expect(await screen.findByText('gc-scopeless-run')).toBeTruthy();
    expect(screen.getAllByText('city')).toHaveLength(1);
    expect(screen.queryByText(/unknown rig/i)).toBeNull();
  });
});

describe('RunsPage — degraded first load recovery (gascity-dashboard-4xcv)', () => {
  it('renders the partial notice, not the empty state, when a partial fetch yields zero lanes', async () => {
    const source = buildRunSource('fresh');
    requireRunData(source).lanesPartial = true;
    mockLoadRunSummary.mockResolvedValue(source);

    mount();
    await waitForMount();

    expect(await screen.findByText(/Run sources were partially unavailable/i)).toBeTruthy();
    expect(screen.queryByText(/No active formula runs/i)).toBeNull();
  });

  const errorSource = {
    source: 'runs',
    status: 'error',
    error: 'supervisor warming up',
  } satisfies SourceState<RunSummary>;

  it('auto-retries a degraded load with backoff, bounded by the retry budget', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    // Both fetchers keep failing so the degraded state persists and the
    // retry chain is observable in isolation (a successful retry would
    // also trigger the one-time full refresh, which is covered elsewhere).
    // Fresh object per call: the real loaders construct a new source every
    // load, and React's setState bails out on identical references.
    mockLoadRunSummaryPreview.mockImplementation(async () => ({ ...errorSource }));
    mockLoadRunSummary.mockImplementation(async () => ({ ...errorSource }));

    mount();
    await waitForMount();
    expect(mockLoadRunSummary).not.toHaveBeenCalled();

    // Retries fire on the 2s / 5s / 10s backoff ladder...
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100);
    });
    expect(mockLoadRunSummary).toHaveBeenCalledTimes(1);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_100);
    });
    expect(mockLoadRunSummary).toHaveBeenCalledTimes(2);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(10_100);
    });
    expect(mockLoadRunSummary).toHaveBeenCalledTimes(3);

    // ...and stop once the budget is spent (SSE / manual take over).
    await act(async () => {
      await vi.advanceTimersByTimeAsync(60_000);
    });
    expect(mockLoadRunSummary).toHaveBeenCalledTimes(3);
  });

  it('SSE callback refreshes when the runs source is in error state', async () => {
    // The old guard skipped every non-fresh status, so an error first
    // load latched the page dead until a manual refresh. A bead event is
    // exactly the cue to try live data again — via the CHEAP active source.
    mockLoadRunSummaryPreview.mockResolvedValue(errorSource);
    mockLoadRunSummary.mockResolvedValue(errorSource);
    mockLoadRunSummaryActive.mockResolvedValue(errorSource);

    mount();
    await waitForMount();
    mockLoadRunSummaryActive.mockClear();

    await act(async () => {
      lastHookCall.onMatch?.();
    });

    expect(mockLoadRunSummaryActive).toHaveBeenCalledTimes(1);
  });
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
