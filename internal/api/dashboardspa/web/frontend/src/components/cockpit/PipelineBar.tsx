import { Link } from 'react-router-dom';

// Empty segments floor at this width so a stage with no work still reads as a
// visible slot in the pipeline rather than vanishing.
const WIDTH_FLOOR_PCT = 2;

// Fill alpha climbs by segment index so the pipeline reads left-to-right as a
// deepening ramp (matches the cockpit mockup). Indices past the ladder hold at
// the darkest step.
const ALPHA_LADDER = [0.18, 0.36, 0.58, 0.8];

function segmentAlpha(index: number): number {
  const clamped = Math.min(Math.max(index, 0), ALPHA_LADDER.length - 1);
  return ALPHA_LADDER[clamped]!;
}

function segmentColor(index: number): string {
  return `oklch(var(--fg) / ${segmentAlpha(index)})`;
}

/**
 * pipelineWidths maps segment counts to percentage widths. Widths are
 * proportional to count/total, floored at WIDTH_FLOOR_PCT so empty segments
 * stay visible. A zero total (nothing anywhere yet) renders an even split.
 */
export function pipelineWidths(counts: number[]): number[] {
  const total = counts.reduce((sum, c) => sum + c, 0);
  if (total === 0) return counts.map(() => 100 / counts.length);
  return counts.map((c) => Math.max((c / total) * 100, WIDTH_FLOOR_PCT));
}

export interface PipelineSegment {
  /** Stable React key / identity for the segment. */
  key: string;
  /** Human label shown in the legend. */
  label: string;
  /** Work units currently in this stage. */
  count: number;
  /** Deep link to the filtered view for this stage. */
  href: string;
}

export interface PipelineBarProps {
  segments: PipelineSegment[];
}

/**
 * PipelineBar is a segmented horizontal meter: one flex track whose segment
 * widths track each stage's share of the total, over a legend that names and
 * counts each stage. Both the track segment and the legend row deep-link to the
 * stage. It shows proportion, not status, so it never uses accent.
 */
export function PipelineBar({ segments }: PipelineBarProps) {
  const widths = pipelineWidths(segments.map((s) => s.count));

  return (
    <div data-testid="pipeline">
      <div className="flex h-[14px] gap-0.5" data-testid="pipeline-track">
        {segments.map((segment, i) => (
          <Link
            key={segment.key}
            to={segment.href}
            data-testid="pipeline-segment"
            aria-label={`${segment.label}: ${segment.count}`}
            className="focus-mark block rounded-sm no-underline transition-[width] duration-[450ms] ease-out-quart motion-reduce:transition-none"
            style={{ width: `${widths[i]}%`, backgroundColor: segmentColor(i) }}
          />
        ))}
      </div>
      <div className="mt-2 flex flex-wrap gap-x-4 gap-y-1" data-testid="pipeline-legend">
        {segments.map((segment, i) => (
          <Link
            key={segment.key}
            to={segment.href}
            data-testid="pipeline-legend-entry"
            className="focus-mark inline-flex items-baseline gap-1.5 no-underline"
          >
            <span
              aria-hidden
              className="inline-block h-2 w-2 translate-y-[1px] rounded-sm"
              style={{ backgroundColor: segmentColor(i) }}
            />
            <span className="text-label uppercase tracking-wider text-fg-faint">
              {segment.label}
            </span>
            <span className="text-label tnum text-fg-muted">{segment.count}</span>
          </Link>
        ))}
      </div>
    </div>
  );
}
