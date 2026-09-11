// Active-city request-plane base (gascity-dashboard-ucc).
//
// The dashboard addresses one city at a time via the `/city/:cityName/*`
// browser route, which maps to the backend's `/api/city/:cityName/*` request
// plane. Every city-scoped `api.*` call and EventSource URL is built off the
// active city's base.
//
// The active city is sourced from the URL segment and pushed here by the
// router (CityProvider) on every navigation, BEFORE any city-scoped fetch
// fires. Keeping it module-level (rather than threading a cityName argument
// through ~30 `api.*` call sites) keeps the cutover mechanical: the call sites
// are unchanged; only the URL they resolve to gains the city prefix.
//
// Non-city dashboard-service endpoints (health, csrf, client-errors, git,
// builds) do NOT go through this base — they address `/api/...` directly.
// GC-owned non-city supervisor resources, such as /v0/cities, use the
// generated supervisor client instead.

import { CITY_NAME_RE } from 'gas-city-dashboard-shared';

let activeCity: string | null = null;

/**
 * Set the active city. Called by the router on navigation. Throws on a value
 * that fails the city-name grammar so a malformed segment can never be
 * spliced into a request path (defence in depth — the backend re-validates).
 */
export function setActiveCity(cityName: string): void {
  if (!CITY_NAME_RE.test(cityName)) {
    throw new Error(`invalid city name: ${cityName}`);
  }
  activeCity = cityName;
}

/** The active city name, or null before the router has resolved one. */
export function getActiveCity(): string | null {
  return activeCity;
}

/**
 * The active city name, or throw if the router has not resolved one yet. Used
 * by the generated-supervisor call sites, where a city-scoped request with no
 * active city is a bug, not a default-to-first fallback. `operation` names the
 * caller so the failure is self-describing.
 */
export function activeCityOrThrow(operation: string): string {
  const cityName = activeCity;
  if (cityName === null) {
    throw new Error(`${operation} called before an active city was resolved`);
  }
  return cityName;
}

/**
 * Build a city-scoped request path. `suffix` is the city-relative path
 * (e.g. '/sessions', '/beads/td-1'). Fails loud if called before a city is
 * active — a city-scoped fetch with no city is a bug, not a default-to-first
 * fallback (the decision forbids silent fallback to another city).
 */
export function cityPath(suffix: string): string {
  if (activeCity === null) {
    throw new Error(`cityPath("${suffix}") called before an active city was resolved`);
  }
  return `/api/city/${encodeURIComponent(activeCity)}${suffix}`;
}
