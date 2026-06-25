import type { ListBodyBead } from 'gas-city-dashboard-shared/gc-supervisor';

// Shared partial/truncation predicates for supervisor bead list reads
// (gascity-dashboard-q89b). Bounded fetches must degrade visibly: a page is
// incomplete when the supervisor flags it partial OR when the upstream total
// exceeds what one bounded page returned.

export function listIsPartial(list: ListBodyBead): boolean {
  // `partial`/`partial_errors` signal a backend-side failure, but the supervisor
  // also truncates at the fetch limit and reports more via `next_cursor` without
  // setting `partial` (gascity-dashboard-4xcv). Treat a present cursor as partial
  // so saturation surfaces through the same partial-notice + retry paths instead
  // of silently dropping lanes.
  return (
    list.partial === true ||
    (list.partial_errors?.length ?? 0) > 0 ||
    (list.next_cursor?.length ?? 0) > 0
  );
}

export function listIsIncomplete(list: ListBodyBead, fetchedCount: number): boolean {
  return listIsPartial(list) || (typeof list.total === 'number' && list.total > fetchedCount);
}
