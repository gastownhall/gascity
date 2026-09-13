import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi, type Mock } from 'vitest';
import type {
  RunCensus,
  RunLane,
  RunLaneHealth,
  RunSummary,
  SourceState,
} from 'gas-city-dashboard-shared';
import { MemoryRouter } from 'react-router-dom';
import { setActiveCity } from '../api/cityBase';
import { invalidateKey } from '../api/cache';
import { NowProvider } from '../contexts/NowContext';
import { assertAtMostOneMark } from '../test/assertions/oneMarkRule';
import { supervisorApi } from '../supervisor/client';
import { loadSupervisorRunSummaryMountSource } from '../supervisor/runSummary';
import { AmbientHomePage } from './AmbientHome';

// gascity-dashboard-kb3 — AmbientHome integration coverage.
//
// The unit-level tests on useStaleness, useFaviconSignal, and the three
// component primitives cover their respective derivations. These
// integration tests pin the contract surfaces the bead's acceptance
// criteria are written against:
//   • One Mark Rule       — at most 1 .text-accent in the rendered DOM
//                           (delegated to assertAtMostOneMark — the
//                           shared mechanical gate per gascity-dashboard-mz8).
//   • R10 withholding     — a fully-calm city renders NO concern rows.
//   • R2 maroon-on-inferred — inferred lanes never produce a maroon token.
//   • Non-fresh paths     — runs.status='error' and census.unavailable.
//   • Deep-link encoding  — encodeURIComponent applied to stuckNode.id.
//   • R6 no-negative-reassurance — calm sentence is absent, not "all clear".

vi.mock('../supervisor/runSummary', () => ({
  loadSupervisorRunSummaryMountSource: vi.fn(),
}));

vi.mock('../supervisor/client', () => ({
  supervisorApi: vi.fn(),
}));

// useFaviconSignal mutates the DOM favicon link; setup/teardown the
// element so it never throws and tests can assert on it.
beforeEach(() => {
  const link = document.createElement('link');
  link.id = 'favicon';
  link.rel = 'icon';
  link.href = '/favicon-calm.svg';
  document.head.appendChild(link);
});

afterEach(() => {
  document.head.querySelectorAll('#favicon').forEach((n) => n.remove());
  cleanup();
  invalidateKey('runs:summary:racoon-city');
  invalidateKey('home:work:racoon-city');
  // Keep the first-run note's dismissal from leaking across tests.
  window.localStorage.removeItem('gascity:home-intro-dismissed');
  vi.useRealTimers();
});

const mockLoadRunSummary = loadSupervisorRunSummaryMountSource as Mock;
const mockSupervisorApi = supervisorApi as Mock;

function knownHealth(overrides: Partial<RunLaneHealth> = {}): RunLaneHealth {
  return {
    phaseConfidence: 'known',
    needsOperator: false,
    stuckNode: { status: 'unavailable', error: 'no active step' },
    thrashingDetected: false,
    session: {
      status: 'resolved',
      lastActive: { status: 'available', at: '2026-05-29T20:00:00.000Z' },
      running: { status: 'unavailable', error: 'unused' },
      activity: { status: 'unavailable', error: 'unused' },
    },
    ...overrides,
  };
}

function inferredHealth(overrides: Partial<RunLaneHealth> = {}): RunLaneHealth {
  return { ...knownHealth(overrides), phaseConfidence: 'inferred' };
}

interface LaneFixture {
  id: string;
  title?: string;
  externalLabel?: string;
  scopeKind?: 'city' | 'rig';
  scopeRef?: string;
  updatedAt?: string;
  phase?: RunLane['phase'];
  /** When set, the lane's health is the genuine 'unavailable' shell (no
   *  session list), exercising the structural-needsOperator path. */
  healthUnavailable?: boolean;
  health?: RunLaneHealth;
}

function lane(f: LaneFixture): RunLane {
  const phase = f.phase ?? 'implementation';
  const health: RunLane['health'] = f.healthUnavailable
    ? { status: 'unavailable', error: 'run session list unavailable' }
    : { status: 'available', data: f.health ?? knownHealth() };
  return {
    id: f.id,
    title: f.title ?? f.id,
    formula: { status: 'unavailable', error: 'unused' },
    scope:
      f.scopeKind !== undefined && f.scopeRef !== undefined
        ? {
            status: 'available',
            kind: f.scopeKind,
            ref: f.scopeRef,
            rootStoreRef: `${f.scopeKind}:${f.scopeRef}`,
          }
        : { status: 'unavailable', error: 'unused' },
    external:
      f.externalLabel !== undefined
        ? {
            status: 'available',
            label: f.externalLabel,
            url: `https://example.com/${f.externalLabel}`,
          }
        : { status: 'unavailable', error: 'unused' },
    phase,
    phaseLabel: phase,
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt:
      f.updatedAt !== undefined
        ? { status: 'available', at: f.updatedAt }
        : { status: 'unavailable', error: 'no fact' },
    stages: [],
    progress: { status: 'unavailable', error: 'unused' },
    formulaStageResolved: false,
    health,
  };
}

const DEFAULT_CENSUS: RunCensus = {
  byPhase: {
    intake: 0,
    implementation: 0,
    review: 0,
    approval: 0,
    finalization: 0,
    blocked: 0,
    complete: 0,
    active: 0,
  },
  totalInFlight: 0,
  unverifiable: 0,
  knownDenominator: 0,
  thrashing: 0,
};

function runSource({
  lanes = [],
  census = DEFAULT_CENSUS,
  runsStatus = 'fresh' as 'fresh' | 'fixture' | 'stale',
}: {
  lanes?: RunLane[];
  census?: RunCensus;
  runsStatus?: 'fresh' | 'fixture' | 'stale';
} = {}): SourceState<RunSummary> {
  return {
    source: 'runs',
    status: runsStatus,
    fetchedAt: '2026-05-29T20:00:00.000Z',
    staleAt: '2026-05-29T20:01:00.000Z',
    error: { kind: 'none' },
    data: {
      totalActive: lanes.length,
      totalHistorical: 0,
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
      census: { status: 'available', data: census },
    },
  };
}

function runsErrorSource(): SourceState<RunSummary> {
  return {
    source: 'runs',
    status: 'error',
    error: 'runs source upstream timeout',
  };
}

function censusUnavailableSource(lanes: RunLane[]): SourceState<RunSummary> {
  const source = runSource({ lanes });
  if (source.status === 'error') throw new Error('test source precondition');
  return {
    ...source,
    data: {
      ...source.data,
      census: { status: 'unavailable', error: 'run health has not been derived' },
    },
  };
}

function mount() {
  return render(
    <MemoryRouter
      initialEntries={['/']}
      future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
    >
      <NowProvider intervalMs={1_000_000}>
        <AmbientHomePage />
      </NowProvider>
    </MemoryRouter>,
  );
}

const NOW_AT = Date.parse('2026-05-29T20:00:00.000Z');

beforeEach(() => {
  setActiveCity('racoon-city');
  mockLoadRunSummary.mockReset();
  mockSupervisorApi.mockReset();
  invalidateKey('runs:summary:racoon-city');
  invalidateKey('home:work:racoon-city');
  mockLoadRunSummary.mockResolvedValue(runSource());
  mockSupervisorApi.mockReturnValue({
    cityStatus: vi.fn().mockResolvedValue({ work: { open: 0, ready: 0, in_progress: 0 } }),
  });
  // Deterministic clock for the staleness derivation; useNow() seeds
  // its state from Date.now() at first render. We don't need fake
  // timers because the test snapshots only need ONE point in time and
  // the NowProvider's interval is set to 1_000_000ms in mount().
  vi.spyOn(Date, 'now').mockReturnValue(NOW_AT);
});

describe('AmbientHomePage', () => {
  it('renders the first-run orientation note until dismissed (gascity-dashboard-q89b)', async () => {
    mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());
    expect(screen.getByTestId('first-run-note')).toBeTruthy();
  });

  it('renders the calm-city case with no concern rows and NO maroon (R10 + One Mark)', async () => {
    // Three known lanes, all fresh sessions, no thrashing, no
    // needsOperator. R10: no rows. R6: no fallback prose.
    const lanes = [
      lane({ id: 'a', updatedAt: '2026-05-29T19:59:30.000Z', health: knownHealth() }),
      lane({ id: 'b', updatedAt: '2026-05-29T19:59:30.000Z', health: knownHealth() }),
      lane({ id: 'c', updatedAt: '2026-05-29T19:59:30.000Z', health: knownHealth() }),
    ];
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 3,
      knownDenominator: 3,
      byPhase: { ...DEFAULT_CENSUS.byPhase, implementation: 3 },
    };
    mockLoadRunSummary.mockResolvedValue(runSource({ lanes, census }));

    const { container } = mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    // R6 floor: the status sentence is absent (collapses to null).
    expect(screen.queryByTestId('status-sentence')).toBeNull();
    // R10: the concern region is rendered with opacity 0 and no rows.
    const region = screen.getByTestId('concern-region');
    expect(region.children.length).toBe(0);
    expect((region as HTMLElement).style.opacity).toBe('0');
    // One Mark Rule (DESIGN.md): at most one maroon per viewport — here
    // R10 makes it zero since the calm city has no concern row to mark.
    assertAtMostOneMark(container);
    // Failing clause reads 'nothing failing'; denominator omitted when unverifiable=0.
    expect(screen.getByTestId('phase-census-failing').textContent).toContain('nothing failing');
    expect(screen.getByTestId('phase-census-failing').textContent).not.toContain('of');
  });

  it('surfaces the in-progress work count in the synopsis (gascity-dashboard-aw75)', async () => {
    // The bug: a claimed (in_progress) bead never surfaced because the
    // run-lane census only counts formula-run lanes. The work headline metric
    // closes that gap — its value must appear in the Home synopsis.
    mockSupervisorApi.mockReturnValue({
      cityStatus: vi.fn().mockResolvedValue({ work: { open: 0, ready: 0, in_progress: 3 } }),
    });

    mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    expect(screen.getByText(/racoon-city, 0 active, 3 in progress/)).toBeTruthy();
  });

  it('omits the in-progress clause when the work source is unavailable', async () => {
    mockSupervisorApi.mockReturnValue({
      cityStatus: vi.fn().mockRejectedValue(new Error('down')),
    });

    mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    expect(screen.getByText(/racoon-city, 0 active$/)).toBeTruthy();
    expect(screen.queryByText(/in progress/)).toBeNull();
  });

  it('appends "(of N known)" denominator when unverifiable > 0 (R5)', async () => {
    const lanes = [
      lane({ id: 'a', updatedAt: '2026-05-29T19:59:30.000Z', health: knownHealth() }),
      lane({ id: 'b', updatedAt: '2026-05-29T19:59:30.000Z', health: knownHealth() }),
      lane({ id: 'c-inferred', updatedAt: '2026-05-29T19:59:30.000Z', health: inferredHealth() }),
    ];
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 3,
      knownDenominator: 2,
      unverifiable: 1,
      byPhase: { ...DEFAULT_CENSUS.byPhase, implementation: 3 },
    };
    mockLoadRunSummary.mockResolvedValue(runSource({ lanes, census }));

    mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());
    expect(screen.getByTestId('phase-census-failing').textContent).toContain(
      'nothing failing (of 2 known)',
    );
  });

  it('renders ONE maroon run-id token on the most-severe known-stalled lane and encodes the deep-link', async () => {
    // Inferred lane is older but must never get the maroon (R2). The
    // known lane wins the One Mark slot and the href has encoded
    // stuckNode + scope params.
    const stalledLane = lane({
      id: 'stalled/with chars',
      externalLabel: 'adopt-pr-271',
      scopeKind: 'rig',
      scopeRef: 'gascity',
      updatedAt: '2026-05-29T18:00:00.000Z',
      health: knownHealth({
        thrashingDetected: true,
        stuckNode: { status: 'available', id: 'review:check/2' },
      }),
    });
    const inferredOlderLane = lane({
      id: 'inferred-older',
      updatedAt: '2026-05-29T10:00:00.000Z',
      health: inferredHealth({ thrashingDetected: true }),
    });
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 2,
      knownDenominator: 1,
      unverifiable: 1,
      thrashing: 1, // already gated server-side to known
      byPhase: { ...DEFAULT_CENSUS.byPhase, implementation: 2 },
    };
    mockLoadRunSummary.mockResolvedValue(
      runSource({ lanes: [stalledLane, inferredOlderLane], census }),
    );

    const { container } = mount();
    await waitFor(() => expect(screen.getByTestId('status-sentence')).toBeTruthy());

    const token = screen.getByTestId('status-sentence-token');
    expect(token.tagName.toLowerCase()).toBe('a');
    expect(token.textContent).toBe('adopt-pr-271');
    const href = (token as HTMLAnchorElement).getAttribute('href');
    // Path segment encoding survives the URL pipeline as the consumer's
    // useParams() will decode it once. Query params are encoded only once
    // by URLSearchParams so the consumer's search.get('node') yields the
    // raw 'review:check/2' — Phase 4 caught a pre-fix double-encode.
    expect(href).toMatch(/^\/runs\/stalled%2Fwith%20chars\?/);
    expect(href).toContain('node=review%3Acheck%2F2');
    expect(href).toContain('scope_kind=rig');
    expect(href).toContain('scope_ref=gascity');

    // One Mark Rule (DESIGN.md): the single maroon fires here — the
    // shared helper pins the <=1 invariant; the surrounding assertions
    // (status-sentence-token presence, the maroon anchor's text/href)
    // already pin that exactly one is present, on the right element.
    assertAtMostOneMark(container);
  });

  it('NEVER paints the maroon token on an inferred lane (R2)', async () => {
    // The only candidate is an inferred lane with thrashingDetected=true.
    // Even though it looks structurally "stalled", R2 demands no maroon.
    const inferredOnlyLane = lane({
      id: 'inferred-only',
      externalLabel: 'never-maroon',
      updatedAt: '2026-05-29T10:00:00.000Z',
      health: inferredHealth({
        thrashingDetected: true,
        stuckNode: { status: 'available', id: 'whatever' },
      }),
    });
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 1,
      unverifiable: 1,
      // Server-side gate excludes inferred, so even if backend miscounts
      // here, the client sentence must not surface the maroon.
      thrashing: 0,
      byPhase: { ...DEFAULT_CENSUS.byPhase, implementation: 1 },
    };
    mockLoadRunSummary.mockResolvedValue(runSource({ lanes: [inferredOnlyLane], census }));

    const { container } = mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    expect(screen.queryByTestId('status-sentence')).toBeNull();
    // R2 floor: the One Mark Rule MUST hold even though an inferred
    // lane is structurally "stalled" — the helper enforces <=1; the
    // status-sentence absence above pins that it's zero here.
    assertAtMostOneMark(container);
  });

  it('shows a needsOperator concern row even when no lane is failing', async () => {
    // R10 boundary: healthy-in-flight stays withheld; needsOperator
    // surfaces. needsOperator is STRUCTURAL — derived from the lane's phase
    // (approval/blocked), not from health.data — so the approval-phase lane
    // surfaces while the implementation-phase calm lane is withheld. No
    // maroon on either.
    const decisionLane = lane({
      id: 'decide-me',
      externalLabel: 'issue-99',
      updatedAt: '2026-05-29T19:59:00.000Z',
      phase: 'approval',
    });
    const calmLane = lane({
      id: 'calm',
      updatedAt: '2026-05-29T19:59:30.000Z',
    });
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 2,
      knownDenominator: 2,
      byPhase: { ...DEFAULT_CENSUS.byPhase, approval: 1, implementation: 1 },
    };
    mockLoadRunSummary.mockResolvedValue(runSource({ lanes: [decisionLane, calmLane], census }));

    const { container } = mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    // Failing reads 'nothing failing'; the calm lane never appears.
    expect(screen.getByTestId('phase-census-failing').textContent).toContain('nothing failing');
    expect(screen.queryByTestId('concern-row-calm')).toBeNull();
    expect(screen.getByTestId('concern-row-decide-me')).toBeTruthy();
    // One Mark Rule: a needsOperator row alone never paints maroon
    // (the mark fires only on the maroon run-id token); the helper
    // pins <=1, the status-sentence absence pins that it's zero.
    assertAtMostOneMark(container);
  });

  it('surfaces a needsOperator lane (and counts it as waiting) even when its health is unavailable (0gww)', async () => {
    // Regression (Codex Fix #3): needsOperator is a STRUCTURAL signal
    // (lane.phase ∈ {approval, blocked}), not session-derived. During a
    // session-list outage deriveRunHealth returns health.status:'unavailable'
    // for every lane; a blocked/approval lane that needs an operator decision
    // must STILL appear in the concern region AND be counted in the waiting
    // census, instead of vanishing behind a health.status==='available' gate.
    const blockedUnavailable = lane({
      id: 'blocked-no-sessions',
      externalLabel: 'pr-77',
      updatedAt: '2026-05-29T19:59:00.000Z',
      phase: 'blocked',
      healthUnavailable: true,
    });
    const census: RunCensus = {
      ...DEFAULT_CENSUS,
      totalInFlight: 1,
      // sessions unavailable ⇒ health is not 'known' ⇒ unverifiable, not in
      // the known denominator. The structural waiting count is independent.
      unverifiable: 1,
      knownDenominator: 0,
      byPhase: { ...DEFAULT_CENSUS.byPhase, blocked: 1 },
    };
    mockLoadRunSummary.mockResolvedValue(runSource({ lanes: [blockedUnavailable], census }));

    mount();
    await waitFor(() => expect(screen.getByTestId('phase-census')).toBeTruthy());

    // The row survives the health-unavailable state.
    expect(screen.getByTestId('concern-row-blocked-no-sessions')).toBeTruthy();
    // And it is counted in the waiting census (synopsis "1 waiting").
    expect(screen.getByTestId('phase-census').textContent).toContain('1 waiting');
  });

  it('renders a clear error when the runs source is in error state', async () => {
    mockLoadRunSummary.mockResolvedValue(runsErrorSource());
    mount();
    await waitFor(() => expect(screen.getByTestId('runs-source-error')).toBeTruthy());
    expect(screen.queryByTestId('phase-census')).toBeNull();
  });

  it('renders the census-unavailable affordance (and only that) when the engine has not derived', async () => {
    mockLoadRunSummary.mockResolvedValue(censusUnavailableSource([]));
    mount();
    await waitFor(() => expect(screen.getByTestId('census-unavailable')).toBeTruthy());
    expect(screen.queryByTestId('phase-census')).toBeNull();
    expect(screen.queryByTestId('status-sentence')).toBeNull();
  });
});
