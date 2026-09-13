// Same-origin transport (gascity-dashboard-soo). The gc supervisor serves the
// SPA, the typed `/v0/*` API, and `/health` on a single listener, so the
// browser addresses the supervisor at `location.origin` with no proxy prefix.
// Kept as an absolute origin (not a bare `/`) so BOTH the generated @hey-api
// client base AND `supervisorUrl()`/EventSource URLs resolve cleanly — an
// empty or `/`-only base makes `new URL(suffix, base)` throw in the SSE path.
export const SUPERVISOR_PROXY_BASE_URL = '';

export function resolveSupervisorBaseUrl(): string {
  const configured = import.meta.env.VITE_GC_SUPERVISOR_URL;
  if (typeof configured === 'string' && configured.trim().length > 0) {
    return configured.trim();
  }
  const origin = globalThis.location?.origin;
  if (typeof origin === 'string' && origin.length > 0 && origin !== 'null') {
    return origin;
  }
  // No DOM origin (e.g. SSR/tests): fall back to a root-relative base so the
  // generated client still issues `/v0/...` requests. SSE URL construction is
  // browser-only, where the origin branch above always applies.
  return SUPERVISOR_PROXY_BASE_URL;
}

export function resolveClientBaseUrl(baseUrl: string): string {
  if (!baseUrl.startsWith('/')) return baseUrl;
  const origin = globalThis.location?.origin;
  if (typeof origin !== 'string' || origin.length === 0 || origin === 'null') {
    return baseUrl;
  }
  return new URL(baseUrl, origin).toString().replace(/\/$/, '');
}

export function supervisorUrl(
  baseUrl: string,
  path: string,
  query?: Record<string, string>,
): string {
  const normalizedBase = baseUrl.replace(/\/$/, '');
  const search = new URLSearchParams(query).toString();
  const suffix = search.length > 0 ? `${path}?${search}` : path;
  // Empty or root-relative base → same-origin: return the path unchanged so the
  // browser resolves it against the current origin (an empty base would make
  // `new URL(suffix, "/")` throw).
  if (normalizedBase === '') return suffix;
  if (normalizedBase.startsWith('/')) return `${normalizedBase}${suffix}`;
  return new URL(suffix, `${normalizedBase}/`).toString();
}
