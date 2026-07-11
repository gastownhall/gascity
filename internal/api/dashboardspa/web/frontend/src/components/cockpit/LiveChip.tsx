import type { GcEventConnState } from '../../hooks/useGcEvents';
import { agoStr, connLabel } from './format';

// The header meta chip: a connection dot (tone by stream state, opacity pulsing
// on fresh events), the connection/paused label, the age of the last event, and
// the scope pause toggle. Presentational — the route owns the pause state and
// passes the toggle.

export interface LiveChipProps {
  connState: GcEventConnState;
  paused: boolean;
  /** Age of the last event in ms, or null when none has arrived. */
  lastAgeMs: number | null;
  onToggle: () => void;
}

export function LiveChip({ connState, paused, lastAgeMs, onToggle }: LiveChipProps) {
  const liveTone = connState === 'open' ? 'ok' : 'warn';
  const liveLabel = paused ? 'paused' : connLabel(connState);
  const ageText =
    lastAgeMs === null
      ? 'no events'
      : agoStr(lastAgeMs) === 'now'
        ? 'last event now'
        : `last event ${agoStr(lastAgeMs)} ago`;
  // Pulse the dot bright on a fresh event, dim as it ages; forced full under
  // reduced motion (the transition is disabled globally there anyway).
  const dotOpacity =
    prefersReducedMotion() || (lastAgeMs !== null && lastAgeMs < 1000) ? 1 : 0.35;

  return (
    <div className="flex items-center gap-3" data-testid="live-chip">
      <span className="inline-flex items-baseline gap-1.5">
        <span
          aria-hidden
          data-testid="live-dot"
          className={`text-[0.85em] leading-none transition-opacity duration-700 ease-out-quart ${
            liveTone === 'ok' ? 'text-ok' : 'text-warn'
          }`}
          style={{ opacity: dotOpacity }}
        >
          ●
        </span>
        <span className="text-label uppercase tracking-wider text-fg-muted" data-testid="live-label">
          {liveLabel}
        </span>
      </span>
      <span className="text-label uppercase tracking-wider tnum text-fg-faint">{ageText}</span>
      <button
        type="button"
        data-testid="scope-pause"
        onClick={onToggle}
        aria-pressed={paused}
        className="inline-flex items-center rounded-sm border border-transparent px-2.5 py-1 text-label lowercase tracking-wider text-fg-muted transition-colors duration-150 ease-out-quart hover:text-fg focus-mark"
      >
        {paused ? 'resume' : 'pause'}
      </button>
    </div>
  );
}

function prefersReducedMotion(): boolean {
  return (
    typeof window !== 'undefined' &&
    typeof window.matchMedia === 'function' &&
    window.matchMedia('(prefers-reduced-motion: reduce)').matches
  );
}
