import { describe, expect, it } from 'vitest';
import type { RunLane } from 'gas-city-dashboard-shared';
import type { StatusBody, UsageBody } from 'gas-city-dashboard-shared/gc-supervisor';
import type { AttentionItem } from '../../attention/compose';
import type { FeedStats } from './useCockpitTelemetry';
import {
  computeUsageMetrics,
  laneToRing,
  pipelineSegments,
  statusLamps,
  sumTokens,
} from './derive';

// Fixtures ─────────────────────────────────────────────────────────────────

function lane(overrides: Partial<RunLane> = {}): RunLane {
  return {
    id: 'r1',
    title: 'r1-title',
    formula: { status: 'known', name: 'pr-review' },
    scope: { status: 'available', kind: 'rig', ref: 'gascity', rootStoreRef: 'rig:gascity' },
    external: { status: 'available', label: 'pr-42', url: 'https://example.com/42' },
    phase: 'implementation',
    phaseLabel: 'implementation',
    statusCounts: { in_progress: 1 },
    activeAssignees: [],
    updatedAt: { status: 'available', at: '2026-07-10T20:00:00.000Z' },
    stages: [],
    progress: {
      status: 'active_step',
      stepId: 'implementation:code/1',
      stage: { status: 'available', index: 1, key: 'implementation', label: 'implementation' },
      attempt: { status: 'available', value: 3 },
    },
    formulaStageResolved: true,
    health: { status: 'unavailable', error: 'unused' },
    ...overrides,
  };
}

function tokens(over: Partial<UsageBody['recent']> = {}): UsageBody['recent'] {
  return {
    input_tokens: 0,
    output_tokens: 0,
    cache_read_tokens: 0,
    cache_creation_tokens: 0,
    cost_usd_estimate: 0,
    compute_facts: 0,
    invocations: 0,
    unpriced: 0,
    wall_seconds: 0,
    ...over,
  };
}

function usage(over: Partial<UsageBody> = {}): UsageBody {
  return {
    totals: tokens(),
    today: tokens({ input_tokens: 1000, output_tokens: 500, cost_usd_estimate: 12.5 }),
    recent: tokens({ input_tokens: 600, output_tokens: 600, cost_usd_estimate: 2 }),
    recent_window_secs: 120,
    recent_by_session: [],
    warnings: [],
    ...over,
  };
}

function status(over: Partial<StatusBody> = {}): StatusBody {
  return {
    agent_count: 1,
    agents: { quarantined: 0, running: 1, suspended: 0, total: 1 },
    mail: { total: 0, unread: 0 },
    name: 'c',
    path: '/c',
    rig_count: 1,
    rigs: { suspended: 0, total: 1 },
    running: 1,
    session_counts_detail: { active: 1, suspended: 0 },
    store_health: {
      live_rows: 100,
      path: '/dolt',
      ratio_mb_per_row: 0.01,
      size_bytes: 48_000_000,
      threshold_mb_per_row: 0.05,
      warning: false,
    },
    suspended: false,
    uptime_sec: 10,
    work: { in_progress: 8, hooked: 2, review: 1, open: 40, ready: 5 },
    ...over,
  };
}

const EMPTY_FEED: FeedStats = {
  perMin: 0,
  lastAgeMs: null,
  windowCostPerHr: null,
  windowTokensPerSec: null,
  beadsClosedToday: 0,
  orderFailedRecent: false,
  mailPerMin: 0,
  sessionTokPerMin: {},
};

function attentionItem(over: Partial<AttentionItem> = {}): AttentionItem {
  return { id: 'a', domain: 'agents', severity: 'attention', title: 't', ...over };
}

// Tests ──────────────────────────────────────────────────────────────────────

describe('sumTokens', () => {
  it('adds the four token dimensions', () => {
    expect(sumTokens(tokens({ input_tokens: 1, output_tokens: 2, cache_read_tokens: 3, cache_creation_tokens: 4 }))).toBe(10);
  });
});

describe('computeUsageMetrics', () => {
  it('scales the recent window to hourly/per-second rates and reads today totals', () => {
    const m = computeUsageMetrics(usage());
    // recent cost 2 over 120s → $60/hr; recent tokens 1200 over 120s → 10 tok/s.
    expect(m.burnPerHr).toBe(2 * (3600 / 120));
    expect(m.tokensPerSec).toBe(1200 / 120);
    expect(m.tokensToday).toBe(1500);
    expect(m.costToday).toBe(12.5);
  });

  it('floors a zero recent window to one second (no divide-by-zero)', () => {
    const m = computeUsageMetrics(usage({ recent_window_secs: 0, recent: tokens({ cost_usd_estimate: 1 }) }));
    expect(m.burnPerHr).toBe(1 * 3600);
  });
});

describe('pipelineSegments', () => {
  it('reads the four bead work statuses into deep-linked segments', () => {
    const segs = pipelineSegments(status().work);
    expect(segs.map((s) => [s.key, s.count])).toEqual([
      ['ready', 5],
      ['hooked', 2],
      ['in_progress', 8],
      ['review', 1],
    ]);
    expect(segs.every((s) => s.href === '/beads')).toBe(true);
  });

  it('renders every segment at zero for a null work read', () => {
    const segs = pipelineSegments(null);
    expect(segs.map((s) => s.count)).toEqual([0, 0, 0, 0]);
  });
});

describe('laneToRing', () => {
  it('maps a resolved stage position to stage index + 1 and carries the attempt', () => {
    const ring = laneToRing(lane());
    expect(ring.stage).toBe(2); // stage.index 1 → 2
    expect(ring.stageWord).toBe('implementation');
    expect(ring.attempt).toBe(3);
    expect(ring.href).toMatch(/^\/runs\/r1/);
  });

  it('falls back to the phase-word stage index when no stage resolves', () => {
    const ring = laneToRing(
      lane({
        phase: 'approval',
        phaseLabel: 'approval',
        progress: { status: 'unavailable', error: 'no stage' },
      }),
    );
    expect(ring.stage).toBe(4); // PHASE_STAGE_INDEX.approval
    expect(ring.stageWord).toBe('approval');
    expect(ring.attempt).toBeUndefined();
  });

  it('omits the attempt for a stage_only lane', () => {
    const ring = laneToRing(
      lane({
        progress: {
          status: 'stage_only',
          stage: { status: 'available', index: 3, key: 'review', label: 'review' },
          error: 'no step',
        },
      }),
    );
    expect(ring.stage).toBe(4); // index 3 → 4
    expect(ring.stageWord).toBe('review');
    expect(ring.attempt).toBeUndefined();
  });

  it('reads the phase-stage table for the fallback (implementation → 2)', () => {
    const ring = laneToRing(lane({ progress: { status: 'unavailable', error: 'x' } }));
    expect(ring.stage).toBe(2); // PHASE_STAGE_INDEX.implementation
  });
});

describe('statusLamps', () => {
  it('trips the orders lamp to warn on a recent failure', () => {
    const [orders] = statusLamps(status(), { ...EMPTY_FEED, orderFailedRecent: true }, []);
    expect(orders).toMatchObject({ id: 'orders', value: 'recent failure', tone: 'warn' });
    expect(orders!.href).toBe('/activity?mode=events&type=order.fired');
  });

  it('rests the orders lamp at ok when firing on time', () => {
    const [orders] = statusLamps(status(), EMPTY_FEED, []);
    expect(orders).toMatchObject({ value: 'firing on time', tone: 'ok' });
  });

  it('counts attention-severity items as patrol incidents', () => {
    const lamps = statusLamps(status(), EMPTY_FEED, [
      attentionItem({ id: '1' }),
      attentionItem({ id: '2' }),
      attentionItem({ id: '3', severity: 'watch' }), // not counted
    ]);
    const patrol = lamps.find((l) => l.id === 'patrol')!;
    expect(patrol).toMatchObject({ value: '2 incidents', tone: 'warn', href: '/agents' });
  });

  it('rests patrol at quiet/ok with no incidents', () => {
    const patrol = statusLamps(status(), EMPTY_FEED, []).find((l) => l.id === 'patrol')!;
    expect(patrol).toMatchObject({ value: 'quiet', tone: 'ok' });
  });

  it('reads the dolt store size and warning tone from store_health', () => {
    const warn = statusLamps(
      status({ store_health: { ...status().store_health!, warning: true } }),
      EMPTY_FEED,
      [],
    ).find((l) => l.id === 'store')!;
    expect(warn).toMatchObject({ value: '48 MB', tone: 'warn', href: '/health' });
  });

  it('dims the store lamp to an em-dash when store_health is absent', () => {
    const noStore = status();
    delete noStore.store_health;
    const store = statusLamps(noStore, EMPTY_FEED, []).find((l) => l.id === 'store')!;
    expect(store).toMatchObject({ value: '—', tone: 'ok', dim: true });
  });

  it('dims the mail lamp at zero traffic and lights it above zero', () => {
    const dim = statusLamps(status(), EMPTY_FEED, []).find((l) => l.id === 'mail')!;
    expect(dim).toMatchObject({ value: '0/min', dim: true });
    const lit = statusLamps(status(), { ...EMPTY_FEED, mailPerMin: 4 }, []).find(
      (l) => l.id === 'mail',
    )!;
    expect(lit).toMatchObject({ value: '4/min', dim: false });
  });

  it('reads null city status as an absent store with the other lamps intact', () => {
    const lamps = statusLamps(null, EMPTY_FEED, []);
    expect(lamps.map((l) => l.id)).toEqual(['orders', 'patrol', 'store', 'mail']);
    expect(lamps.find((l) => l.id === 'store')).toMatchObject({ value: '—', dim: true });
  });
});
