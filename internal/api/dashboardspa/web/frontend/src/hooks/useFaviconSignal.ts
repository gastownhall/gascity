import { useEffect, useRef } from 'react';

// gascity-dashboard-kb3 R8: the favicon is the ambient-tab "alarm of last
// resort" — visible in peripheral vision when the dashboard is in a
// non-focused tab. PRD R8: require the failing predicate to hold for two
// consecutive snapshot cycles before flipping (alarm-fatigue gate), and
// symmetric two-cycle hysteresis on the return. A single flaky cycle
// must NEVER flip the favicon.
//
// Multi-signal note (Phase 1 architect H1): the input `failing` is itself
// already gated upstream — it's `census.thrashing + clientStalledLaneIds.length`,
// both of which are filtered to phaseConfidence='known'. The hook itself
// only adds the temporal hysteresis layer; the semantic gates live in
// useStaleness (R2) and the census (server-side R2).

export const FAVICON_CALM_HREF = '/favicon-calm.svg';
export const FAVICON_ALERT_HREF = '/favicon-alert.svg';

type SignalState = 'calm' | 'alert';
const REQUIRED_CYCLES = 2;

function setFaviconHref(href: string): void {
  const link = document.getElementById('favicon');
  if (!(link instanceof HTMLLinkElement)) return;
  // Safari aggressively caches favicons by URL; the ?v=<timestamp>
  // suffix forces a re-fetch on every swap (Phase 1 architect H6).
  link.href = `${href}?v=${Date.now()}`;
}

interface FaviconSignalInput {
  /** Number of failing lanes in the current snapshot (gated upstream). */
  failing: number;
  /**
   * Per-snapshot identity. The hysteresis advances exactly once per
   * distinct cycleKey value, NOT on every consumer re-render. The
   * 1s NowContext tick would otherwise cycle the hysteresis 60x per
   * snapshot and defeat the alarm-fatigue gate. Consumers pass
   * `snapshot.generatedAt` (an ISO string is fine — equality is by
   * value).
   */
  cycleKey: string | number;
}

export function useFaviconSignal({ failing, cycleKey }: FaviconSignalInput): void {
  // The current side-effected state; lives in a ref so the consumer
  // does not re-render when the favicon flips.
  const stateRef = useRef<SignalState>('calm');
  // Count of consecutive cycles the failing predicate has matched the
  // OPPOSITE of the current state. Reset whenever a cycle confirms the
  // current state.
  const streakRef = useRef<number>(0);
  // Track the last cycleKey we processed so we advance hysteresis at
  // most once per snapshot, regardless of how many now-ticks re-render
  // the consumer between cycles.
  const lastCycleKeyRef = useRef<string | number | null>(null);

  useEffect(() => {
    if (lastCycleKeyRef.current === cycleKey) return;
    lastCycleKeyRef.current = cycleKey;

    const current = stateRef.current;
    const observed: SignalState = failing > 0 ? 'alert' : 'calm';

    if (observed === current) {
      streakRef.current = 0;
      return;
    }
    streakRef.current += 1;
    if (streakRef.current < REQUIRED_CYCLES) return;

    // Hysteresis crossed — flip the visible state.
    stateRef.current = observed;
    streakRef.current = 0;
    setFaviconHref(observed === 'alert' ? FAVICON_ALERT_HREF : FAVICON_CALM_HREF);
  }, [failing, cycleKey]);
}
