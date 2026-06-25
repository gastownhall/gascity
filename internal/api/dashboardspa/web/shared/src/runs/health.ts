import { resolveSessionForTarget } from '../session-resolve.js';
import type { DashboardSession } from '../dashboard-sessions.js';
import type { RunCensus, RunLane, RunLaneHealth, RunPhase } from '../snapshot/types.js';

const DEFAULT_ATTEMPT_CLIMB_MIN = 1;
const DEFAULT_THRASH_DETECTED_STREAK = 2;

export interface HealthThresholds {
  attemptClimbMin: number;
  thrashDetectedStreak: number;
}

const DEFAULT_THRESHOLDS: HealthThresholds = {
  attemptClimbMin: DEFAULT_ATTEMPT_CLIMB_MIN,
  thrashDetectedStreak: DEFAULT_THRASH_DETECTED_STREAK,
};

export interface LaneProgressMark {
  progress: LaneProgressComparison;
  thrashStreak: number;
}

export type LaneProgressComparison =
  | {
      status: 'comparable';
      stepId: string;
      stageIndex: number;
      attempt: number;
    }
  | {
      status: 'not_comparable';
      error: string;
    };

export interface DeriveRunHealthInput {
  lanes: readonly RunLane[];
  sessions: readonly DashboardSession[];
  sessionsAvailable: boolean;
  marks: ReadonlyMap<string, LaneProgressMark>;
  thresholds?: Partial<HealthThresholds>;
}

export interface DeriveRunHealthResult {
  lanes: RunLane[];
  census: RunCensus;
}

/**
 * Structural needs-operator signal for a lane: true when the lane's phase is a
 * human-gate phase ('approval' or 'blocked'). This is derived from lane.phase
 * alone — a structural bead-state fact, NOT a session-derived health
 * conclusion. It therefore stays valid even when the session list is
 * unavailable and per-lane health degrades to status:'unavailable'. Consumers
 * must read needsOperator through this accessor rather than gating it behind
 * health.status === 'available', or a human-gate decision vanishes from the
 * home concern region during a session-list outage.
 */
export function laneNeedsOperator(lane: RunLane): boolean {
  return lane.phase === 'approval' || lane.phase === 'blocked';
}

export function advanceProgressMarks(
  previous: ReadonlyMap<string, LaneProgressMark>,
  lanes: readonly RunLane[],
  thresholds: Partial<HealthThresholds> = {},
): Map<string, LaneProgressMark> {
  const { attemptClimbMin } = { ...DEFAULT_THRESHOLDS, ...thresholds };
  const next = new Map<string, LaneProgressMark>();

  for (const lane of lanes) {
    const progress = comparableProgress(lane);
    const prior = previous.get(lane.id);

    const positionFlat =
      prior !== undefined &&
      prior.progress.status === 'comparable' &&
      progress.status === 'comparable' &&
      prior.progress.stepId === progress.stepId &&
      prior.progress.stageIndex === progress.stageIndex;
    const climbed =
      prior !== undefined &&
      prior.progress.status === 'comparable' &&
      progress.status === 'comparable' &&
      progress.attempt - prior.progress.attempt >= attemptClimbMin;

    const thrashStreak = positionFlat && climbed ? prior.thrashStreak + 1 : 0;

    next.set(lane.id, {
      progress,
      thrashStreak,
    });
  }

  return next;
}

export function deriveRunHealth(input: DeriveRunHealthInput): DeriveRunHealthResult {
  const { thrashDetectedStreak } = { ...DEFAULT_THRESHOLDS, ...input.thresholds };

  const lanes = input.lanes.map((lane): RunLane => {
    // gascity-dashboard (0gww): without the session list, health cannot be
    // derived — phaseConfidence collapses to 'inferred' and the session-trust
    // signals (needsOperator/thrash) lose their grounding. Report the lane's
    // health as genuinely 'unavailable' rather than wrapping a degraded shell in
    // status:'available'. That degraded-but-'available' shell is what made the
    // attention emitter's `health.status === 'available'` guard
    // (attention/registry.ts) skip every lane, so the per-lane
    // health-unavailable signal never fired. Consumers already gate every
    // .health.data read on status === 'available', so they degrade cleanly here.
    if (!input.sessionsAvailable) {
      return { ...lane, health: { status: 'unavailable', error: 'run session list unavailable' } };
    }

    const session = resolveLaneSession(lane, input.sessions);
    const sessionResolved = session.status === 'resolved';

    const phaseConfidence: RunLaneHealth['phaseConfidence'] =
      lane.formulaStageResolved === true && sessionResolved ? 'known' : 'inferred';

    const thrashStreak = input.marks.get(lane.id)?.thrashStreak ?? 0;

    const health: RunLaneHealth = {
      phaseConfidence,
      needsOperator: laneNeedsOperator(lane),
      stuckNode: stuckNode(lane),
      thrashingDetected: thrashStreak >= thrashDetectedStreak,
      session:
        session.status === 'resolved'
          ? sessionFacts(session.session)
          : { status: 'unresolved', error: session.error },
    };

    return { ...lane, health: { status: 'available' as const, data: health } };
  });

  return { lanes, census: buildCensus(lanes) };
}

function resolveLaneSession(
  lane: RunLane,
  sessions: readonly DashboardSession[],
): { status: 'resolved'; session: DashboardSession } | { status: 'unresolved'; error: string } {
  for (const assignee of lane.activeAssignees) {
    const session = resolveSessionForTarget(assignee, sessions);
    if (session !== null) return { status: 'resolved', session };
  }
  return { status: 'unresolved', error: 'run session unresolved' };
}

function comparableProgress(lane: RunLane): LaneProgressComparison {
  if (lane.progress.status !== 'active_step') {
    return { status: 'not_comparable', error: 'run has no active step' };
  }
  if (lane.progress.stage.status !== 'available') {
    return { status: 'not_comparable', error: lane.progress.stage.error };
  }
  if (lane.progress.attempt.status !== 'available') {
    return { status: 'not_comparable', error: lane.progress.attempt.error };
  }
  return {
    status: 'comparable',
    stepId: lane.progress.stepId,
    stageIndex: lane.progress.stage.index,
    attempt: lane.progress.attempt.value,
  };
}

function stuckNode(lane: RunLane): RunLaneHealth['stuckNode'] {
  return lane.progress.status === 'active_step'
    ? { status: 'available', id: lane.progress.stepId }
    : { status: 'unavailable', error: 'active run step unavailable' };
}

function sessionFacts(session: DashboardSession): RunLaneHealth['session'] {
  return {
    status: 'resolved',
    lastActive:
      session.last_active === undefined
        ? { status: 'unavailable', error: 'session last_active unavailable' }
        : { status: 'available', at: session.last_active },
    running: { status: 'available', value: session.running },
    activity:
      session.activity === undefined
        ? { status: 'unavailable', error: 'session activity unavailable' }
        : { status: 'available', value: session.activity },
  };
}

function zeroByPhase(): Record<RunPhase, number> {
  return {
    intake: 0,
    implementation: 0,
    review: 0,
    approval: 0,
    finalization: 0,
    blocked: 0,
    complete: 0,
    active: 0,
  };
}

export function buildCensus(lanes: readonly RunLane[]): RunCensus {
  const byPhase = zeroByPhase();

  let totalInFlight = 0;
  let unverifiable = 0;
  let knownDenominator = 0;
  let thrashing = 0;

  for (const lane of lanes) {
    byPhase[lane.phase] += 1;
    if (lane.phase === 'complete') continue;

    totalInFlight += 1;
    if (lane.health.status === 'available' && lane.health.data.phaseConfidence === 'known') {
      knownDenominator += 1;
      if (lane.health.data.thrashingDetected === true) thrashing += 1;
    } else {
      unverifiable += 1;
    }
  }

  return { byPhase, totalInFlight, unverifiable, knownDenominator, thrashing };
}
