import type { LinkResolutionStat } from '../links.js';

// R11 — resolution instrumentation (RK4).
//
// Every link-view build records per-edge-type resolution outcomes into a
// rollup. The backend can expose the process-level rollup; the browser direct
// loader normally omits a recorder and receives only per-view stats.

export type ResolutionOutcome = 'resolved' | 'unresolved' | 'n-candidates';

export type ResolutionRecorder = (relation: string, outcome: ResolutionOutcome) => void;

interface MutableStat {
  resolved: number;
  unresolved: number;
  nCandidates: number;
}

export class ResolutionRollup {
  private readonly byRelation = new Map<string, MutableStat>();

  record(relation: string, outcome: ResolutionOutcome): void {
    const stat =
      this.byRelation.get(relation) ??
      this.byRelation.set(relation, { resolved: 0, unresolved: 0, nCandidates: 0 }).get(relation)!;
    if (outcome === 'resolved') stat.resolved += 1;
    else if (outcome === 'unresolved') stat.unresolved += 1;
    else stat.nCandidates += 1;
  }

  recorder(): ResolutionRecorder {
    return (relation, outcome) => this.record(relation, outcome);
  }

  snapshot(): LinkResolutionStat[] {
    return [...this.byRelation.entries()]
      .map(([relation, s]) => ({
        relation,
        resolved: s.resolved,
        unresolved: s.unresolved,
        nCandidates: s.nCandidates,
      }))
      .sort((a, b) => a.relation.localeCompare(b.relation));
  }
}

export function recordResolution(
  recorder: ResolutionRecorder | undefined,
  relation: string,
  outcome: ResolutionOutcome,
): void {
  recorder?.(relation, outcome);
}
