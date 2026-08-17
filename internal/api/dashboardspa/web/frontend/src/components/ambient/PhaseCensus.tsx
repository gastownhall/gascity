import type { RunCensus } from 'gas-city-dashboard-shared';

// gascity-dashboard-kb3 PRD §4 line 1 — "the <1s pattern-match target".
// Headline scale, tabular figures, interpunct separators. Deterministic
// counts, NO model call. Its *shape* changes when state changes
// ("nothing failing" → "1 failing" in maroon), which is what peripheral
// vision actually catches.
//
// Vocab pinned (Phase 1 architect M4):
//   "in flight" = census.totalInFlight
//   "waiting"   = waitingCount (lanes where health.needsOperator === true)
//   "failing"   = failingCount = census.thrashing + clientStalledCount
//                (both already gated to phaseConfidence='known' upstream)
//
// R5 confidence-scoped denominator: when ANY in-flight lane is inferred
// (census.unverifiable > 0), append "(of N known)" so calm is never
// silently scoped (PRD §12 R5).

export interface PhaseCensusProps {
  census: RunCensus;
  waitingCount: number;
  failingCount: number;
}

export function PhaseCensus({ census, waitingCount, failingCount }: PhaseCensusProps) {
  // R5 trigger: any in-flight lane the engine could not classify with
  // confidence. The denominator clause is suffixed to the failing clause
  // (where the trust anchor lives), not floated as a standalone clause.
  const showDenominator = census.unverifiable > 0;
  const denominatorSuffix = showDenominator ? ` (of ${census.knownDenominator} known)` : '';

  // The only .text-accent (maroon) on the page lives on the
  // StatusSentence's run-id token. The census's failing count uses
  // weight-600 instead so the One Mark Rule (DESIGN.md) holds even
  // when the sentence's maroon token is present.
  const failingClause =
    failingCount === 0
      ? `nothing failing${denominatorSuffix}`
      : `${failingCount} failing${denominatorSuffix}`;

  return (
    <p className="text-title tnum text-fg" data-testid="phase-census">
      <span>{census.totalInFlight} in flight</span>
      <span aria-hidden="true" className="mx-2 text-fg-faint">
        ·
      </span>
      <span>{waitingCount} waiting</span>
      <span aria-hidden="true" className="mx-2 text-fg-faint">
        ·
      </span>
      <span
        className={failingCount > 0 ? 'font-semibold text-fg' : ''}
        aria-live="polite"
        data-testid="phase-census-failing"
      >
        {failingClause}
      </span>
    </p>
  );
}
