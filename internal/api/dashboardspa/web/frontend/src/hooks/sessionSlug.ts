import type { DashboardSession } from 'gas-city-dashboard-shared';
import type { AgentResponse } from 'gas-city-dashboard-shared/gc-supervisor';

// Resolution order for the drilldown URL segment. session_name is gc's
// URL-safe primary; alias is human-readable; id is the stable fallback.
// Mirrors the resolution order on the receiving page so a slug always
// round-trips for as long as the session exists.
//
// `session_name` is required per OpenAPI (6bv7 F10), so a `??` chain
// would be type-dead. `||` is used so an empty-string session_name
// (which `z.string()` still accepts) falls through to alias/id rather
// than producing an unroutable `/agents/` URL.
export function sessionSlug(s: DashboardSession): string {
  return s.session_name || s.alias || s.id;
}

/**
 * Slug for an `AgentResponse` row in the Agents list (gascity-dashboard-ay6).
 * AgentDetail resolves a slug by matching against session.session_name,
 * session.alias, or session.id (in that order). For agents with an active
 * session we hand back the supervisor session name (highest priority on
 * the receiving end); for orphan agents (configured but not running) we
 * fall back to the agent's own alias, which AgentDetail will still
 * surface even without a backing session.
 */
export function agentSlug(a: AgentResponse): string {
  return a.session?.name ?? a.name;
}
