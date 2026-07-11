import type { RunLane } from 'gas-city-dashboard-shared';
import type {
  StatusBody,
  StatusWorkCounts,
  UsageBody,
} from 'gas-city-dashboard-shared/gc-supervisor';
import type { AttentionItem } from '../../attention/compose';
import { formatBytesSI } from '../../lib/format';
import { runDetailHref } from '../../supervisor/runHref';
import type { PipelineSegment } from './PipelineBar';
import type { RunRing } from './RunRings';
import type { StatusLamp } from './StatusLamps';
import type { FeedStats } from './useCockpitTelemetry';

// Pure derivations behind the cockpit home: bead-work → pipeline segments, run
// lanes → progress rings, city status + feed → system lamps, and usage totals →
// burn/throughput metrics. All side-effect-free and unit-covered next to here.

// Map a run lane's phase word to a stage index when the lane carries no resolved
// stage position — a fallback so the ring still sweeps to a sensible fraction.
const PHASE_STAGE_INDEX: Record<string, number> = {
  intake: 1,
  implementation: 2,
  review: 3,
  approval: 4,
  finalization: 5,
  complete: 5,
  blocked: 2,
  active: 1,
};

// The pipeline reads bead work statuses left-to-right, deepening in alpha.
const PIPELINE_STAGES: ReadonlyArray<{
  key: string;
  label: string;
  pick: (work: StatusWorkCounts) => number;
}> = [
  { key: 'ready', label: 'ready', pick: (w) => w.ready },
  { key: 'hooked', label: 'hooked', pick: (w) => w.hooked },
  { key: 'in_progress', label: 'in progress', pick: (w) => w.in_progress },
  { key: 'review', label: 'review', pick: (w) => w.review },
];

export interface UsageMetrics {
  burnPerHr: number;
  tokensPerSec: number;
  tokensToday: number;
  costToday: number;
}

// The four token dimensions shared by UsageTotals and UsageSessionRecent.
export interface TokenCounts {
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_creation_tokens: number;
}

/** sumTokens totals the four token dimensions of a usage record. */
export function sumTokens(tokens: TokenCounts): number {
  return (
    tokens.input_tokens +
    tokens.output_tokens +
    tokens.cache_read_tokens +
    tokens.cache_creation_tokens
  );
}

/**
 * computeUsageMetrics projects a usage read into the cockpit's short-poll
 * fallback rates (burn $/hr, tokens/s) and today's running totals. The recent
 * window scales to an hourly/per-second rate; a zero window is floored to 1s so
 * a cold read never divides by zero.
 */
export function computeUsageMetrics(usage: UsageBody): UsageMetrics {
  const windowSecs = Math.max(usage.recent_window_secs, 1);
  return {
    burnPerHr: usage.recent.cost_usd_estimate * (3600 / windowSecs),
    tokensPerSec: sumTokens(usage.recent) / windowSecs,
    tokensToday: sumTokens(usage.today),
    costToday: usage.today.cost_usd_estimate,
  };
}

/**
 * pipelineSegments reads the four bead work statuses (ready → hooked →
 * in-progress → review) into pipeline segments, each deep-linking to /beads. A
 * null work read renders every segment at zero rather than collapsing the bar.
 */
export function pipelineSegments(work: StatusWorkCounts | null): PipelineSegment[] {
  return PIPELINE_STAGES.map((stage) => ({
    key: stage.key,
    label: stage.label,
    count: work !== null ? stage.pick(work) : 0,
    href: '/beads',
  }));
}

/**
 * laneToRing projects a run lane into a progress ring: the resolved stage
 * position when available, else a phase-word fallback via PHASE_STAGE_INDEX, and
 * the current attempt when the lane exposes one.
 */
export function laneToRing(lane: RunLane): RunRing {
  const progress = lane.progress;
  const stagePos =
    (progress.status === 'active_step' || progress.status === 'stage_only') &&
    progress.stage.status === 'available'
      ? progress.stage
      : null;
  const stage = stagePos !== null ? stagePos.index + 1 : (PHASE_STAGE_INDEX[lane.phase] ?? 1);
  const stageWord = stagePos !== null ? stagePos.label : lane.phaseLabel;
  const attempt =
    progress.status === 'active_step' && progress.attempt.status === 'available'
      ? progress.attempt.value
      : undefined;
  return {
    id: lane.id,
    label: laneLabel(lane),
    stage,
    stageWord,
    ...(attempt === undefined ? {} : { attempt }),
    href: runDetailHref(lane.id, lane.scope),
  };
}

/** laneLabel prefers the external label, then the formula name, then the title. */
export function laneLabel(lane: RunLane): string {
  if (lane.external.status === 'available' || lane.external.status === 'label_only') {
    return lane.external.label;
  }
  if (lane.formula.status === 'known') return lane.formula.name;
  return lane.title;
}

/**
 * statusLamps builds the systems-column lamp set (orders, patrol, dolt store,
 * mail traffic) from city status, the feed-derived stats, and the attention
 * items. Tone is carried by shape/position only (ok/warn), never accent; each
 * lamp deep-links to its spec surface.
 */
export function statusLamps(
  status: StatusBody | null,
  feed: FeedStats,
  attentionItems: readonly AttentionItem[],
): StatusLamp[] {
  const incidents = attentionItems.filter((item) => item.severity === 'attention').length;
  const store = status?.store_health;
  return [
    {
      id: 'orders',
      label: 'orders',
      value: feed.orderFailedRecent ? 'recent failure' : 'firing on time',
      tone: feed.orderFailedRecent ? 'warn' : 'ok',
      href: '/activity?mode=events&type=order.fired',
    },
    {
      id: 'patrol',
      label: 'patrol',
      value: incidents > 0 ? `${incidents} incident${incidents > 1 ? 's' : ''}` : 'quiet',
      tone: incidents > 0 ? 'warn' : 'ok',
      href: '/agents',
    },
    store !== undefined
      ? {
          id: 'store',
          label: 'dolt store',
          value: formatBytesSI(store.size_bytes),
          tone: store.warning ? 'warn' : 'ok',
          href: '/health',
        }
      : { id: 'store', label: 'dolt store', value: '—', tone: 'ok', dim: true, href: '/health' },
    {
      id: 'mail',
      label: 'mail traffic',
      value: `${feed.mailPerMin}/min`,
      tone: 'ok',
      dim: feed.mailPerMin === 0,
      href: '/mail',
    },
  ];
}
