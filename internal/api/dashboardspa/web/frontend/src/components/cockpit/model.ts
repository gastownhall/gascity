import type { RunLane } from 'gas-city-dashboard-shared';
import type { RunStatusCounts, UsageTotals } from 'gas-city-dashboard-shared/gc-supervisor';
import { runDetailHref } from '../../supervisor/runHref';

const SEGMENT_FLOOR = 2;

export interface PipelineSegment {
  key: 'pending' | 'active' | 'waiting' | 'canceling';
  label: string;
  count: number;
  href: string;
}

export interface RunRingModel {
  id: string;
  label: string;
  stage: number;
  totalStages: number;
  stageWord: string;
  attempt?: number;
  href: string;
}

export function finiteNonNegative(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : 0;
}

export function pipelineWidths(values: readonly number[]): number[] {
  if (values.length === 0) return [];
  const counts = values.map(finiteNonNegative);
  const total = counts.reduce((sum, count) => sum + count, 0);
  if (total === 0 || SEGMENT_FLOOR * counts.length >= 100) {
    return counts.map(() => 100 / counts.length);
  }
  const remaining = 100 - SEGMENT_FLOOR * counts.length;
  return counts.map((count) => SEGMENT_FLOOR + (count / total) * remaining);
}

export function pipelineSegments(counts: RunStatusCounts | null): PipelineSegment[] {
  const safe = (value: unknown) => Math.floor(finiteNonNegative(value));
  return [
    { key: 'pending', label: 'queued', count: safe(counts?.pending), href: '/runs' },
    { key: 'active', label: 'running', count: safe(counts?.active), href: '/runs' },
    { key: 'waiting', label: 'waiting', count: safe(counts?.waiting), href: '/runs' },
    { key: 'canceling', label: 'stopping', count: safe(counts?.canceling), href: '/runs' },
  ];
}

function tokenTotal(totals: UsageTotals): number | null {
  const values = [
    totals.input_tokens,
    totals.output_tokens,
    totals.cache_read_tokens,
    totals.cache_creation_tokens,
  ];
  if (values.some((value) => !Number.isFinite(value) || value < 0)) return null;
  const total = values.reduce((sum, value) => sum + value, 0);
  return Number.isFinite(total) ? total : null;
}

export function tokensPerMinute(totals: UsageTotals, windowSeconds: number): number | null {
  const tokens = tokenTotal(totals);
  if (tokens === null || !Number.isFinite(windowSeconds) || windowSeconds <= 0) return null;
  const rate = (tokens / windowSeconds) * 60;
  return Number.isFinite(rate) ? rate : null;
}

export function burnPerHour(totals: UsageTotals, windowSeconds: number): number | null {
  if (
    !Number.isFinite(totals.cost_usd_estimate) ||
    totals.cost_usd_estimate < 0 ||
    !Number.isFinite(windowSeconds) ||
    windowSeconds <= 0
  ) {
    return null;
  }
  const rate = totals.cost_usd_estimate * (3600 / windowSeconds);
  return Number.isFinite(rate) ? rate : null;
}

const PHASE_STAGE: Record<string, number> = {
  intake: 1,
  implementation: 2,
  review: 3,
  approval: 4,
  finalization: 5,
  complete: 5,
  blocked: 1,
  active: 1,
};

export function laneToRing(lane: RunLane): RunRingModel {
  const progress = lane.progress;
  const stagePosition =
    (progress.status === 'active_step' || progress.status === 'stage_only') &&
    progress.stage.status === 'available'
      ? progress.stage
      : null;
  const stage = Math.max(
    1,
    stagePosition?.index === undefined ? (PHASE_STAGE[lane.phase] ?? 1) : stagePosition.index + 1,
  );
  const totalStages = Math.max(1, lane.stages.length, stage);
  const attempt =
    progress.status === 'active_step' && progress.attempt.status === 'available'
      ? Math.max(1, progress.attempt.value)
      : undefined;
  const formula = lane.formula.status === 'known' ? lane.formula.name : null;
  return {
    id: lane.id,
    label: formula ?? lane.title,
    stage,
    totalStages,
    stageWord: stagePosition?.label ?? lane.phaseLabel,
    ...(attempt === undefined ? {} : { attempt }),
    href: runDetailHref(lane.id, lane.scope),
  };
}
