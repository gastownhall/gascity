import type { RunLaneScope } from 'gas-city-dashboard-shared';

/**
 * The run-detail route href for a run id + its scope. Centralises the
 * `/runs/:id?scope_kind=&scope_ref=` construction — path segment encoded, the
 * scope query appended only when the scope resolved — so the nav-badge link and
 * the lane-card link to the same run cannot drift (gascity-dashboard-2j8e.2).
 */
export function runDetailHref(runId: string, scope: RunLaneScope): string {
  const path = `/runs/${encodeURIComponent(runId)}`;
  if (scope.status !== 'available') return path;
  const search = new URLSearchParams();
  search.set('scope_kind', scope.kind);
  search.set('scope_ref', scope.ref);
  return `${path}?${search.toString()}`;
}
