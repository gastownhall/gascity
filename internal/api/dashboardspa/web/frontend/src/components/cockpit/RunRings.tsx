import { Link } from 'react-router-dom';

// Ring geometry from the cockpit mockup: a 76×76 box with a radius-30 dial
// centered at (38, 38). The progress stroke is rotated so it starts at 12
// o'clock and sweeps clockwise.
const RING_BOX = 76;
const RING_CENTER = RING_BOX / 2;

/** Ring radius shared by the run-progress dials. */
export const RING_R = 30;

/** Circumference of the run-progress ring; the stroke dash length. */
export const RING_CIRC = 2 * Math.PI * RING_R;

/**
 * ringDashoffset maps a stage count to the stroke-dashoffset that leaves the
 * completed fraction painted. offset = CIRC × (1 − stage/total), clamped so an
 * overshooting or negative stage still reads on-scale.
 */
export function ringDashoffset(stage: number, totalStages: number): number {
  const frac = totalStages <= 0 ? 0 : Math.min(Math.max(stage / totalStages, 0), 1);
  return RING_CIRC * (1 - frac);
}

export interface RunRing {
  /** Stable identity / React key. */
  id: string;
  /** Run label under the ring. */
  label: string;
  /** Current stage index (0..totalStages). */
  stage: number;
  /** Word for the current stage, shown in the ring center. */
  stageWord: string;
  /** Attempt number; > 1 swaps the stage word for a warn-toned retry marker. */
  attempt?: number;
  /** Deep link to the run. */
  href: string;
}

export interface RunRingsProps {
  runs: RunRing[];
  /** Stages in a full run; defaults to 5. */
  totalStages?: number;
}

/**
 * RunRings is a row of run-progress dials: a track ring plus a progress ring
 * that sweeps to stage/total, with the stage count and word in the center.
 * A retry (attempt > 1) swaps the stage word for a warn-toned "▲ attempt N" —
 * warn, never accent, since accent is reserved for needs-you.
 */
export function RunRings({ runs, totalStages = 5 }: RunRingsProps) {
  return (
    <div className="flex flex-wrap gap-3" data-testid="run-rings">
      {runs.map((run) => {
        const isRetry = run.attempt !== undefined && run.attempt > 1;
        return (
          <Link
            key={run.id}
            to={run.href}
            data-id={run.id}
            data-testid="run-ring"
            aria-label={`${run.label}: stage ${run.stage} of ${totalStages}`}
            className="focus-mark block no-underline"
          >
            <div className="relative h-[76px] w-[76px]">
              <svg viewBox={`0 0 ${RING_BOX} ${RING_BOX}`} width={RING_BOX} height={RING_BOX} aria-hidden>
                <circle
                  cx={RING_CENTER}
                  cy={RING_CENTER}
                  r={RING_R}
                  className="stroke-rule"
                  strokeWidth={3}
                  fill="none"
                />
                <circle
                  data-testid="ring-progress"
                  cx={RING_CENTER}
                  cy={RING_CENTER}
                  r={RING_R}
                  className="stroke-fg transition-[stroke-dashoffset] duration-500 ease-out-quart motion-reduce:transition-none"
                  strokeWidth={3}
                  strokeLinecap="round"
                  fill="none"
                  strokeDasharray={RING_CIRC}
                  strokeDashoffset={ringDashoffset(run.stage, totalStages)}
                  transform={`rotate(-90 ${RING_CENTER} ${RING_CENTER})`}
                />
              </svg>
              <div className="absolute inset-0 flex flex-col items-center justify-center gap-0.5">
                <span className="text-body tnum text-fg" data-testid="ring-count">
                  {run.stage}/{totalStages}
                </span>
                {isRetry ? (
                  <span
                    className="text-label uppercase tracking-wider text-warn"
                    data-testid="ring-attempt"
                  >
                    ▲ attempt {run.attempt}
                  </span>
                ) : (
                  <span
                    className="text-label uppercase tracking-wider text-fg-faint"
                    data-testid="ring-stageword"
                  >
                    {run.stageWord}
                  </span>
                )}
              </div>
            </div>
            <div className="mt-1 text-center text-label uppercase tracking-wider text-fg-faint">
              {run.label}
            </div>
          </Link>
        );
      })}
    </div>
  );
}
