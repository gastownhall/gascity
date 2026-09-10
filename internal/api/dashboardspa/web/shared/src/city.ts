// City-name grammar — single source of truth shared by the backend
// (validates GC_CITY_NAME at boot + the `/api/city/:cityName` dispatch
// segment) and the frontend (validates the `/city/:cityName` route segment
// before splicing it into a request path). gascity-dashboard-ucc.
//
// A city name lands in security-sensitive positions on the backend: a path
// segment under ~/.gascity-dashboard/cities/<cityName>/ and the request-plane
// dispatch key. The grammar mirrors the supervisor registry's own city-name
// grammar (internal/supervisor/registry.go's validCityName): must start with
// an alphanumeric character, followed by any number of alphanumerics,
// hyphens, underscores, or dots — no path separators, so the segment stays
// inert as a path component and as a Map key, and a leading-alphanumeric
// requirement means it can never resolve to a bare "." or ".." segment.
// Keeping it in `shared` makes the character class provably identical on
// both sides; a drift would otherwise be a runtime 404, not a compile error.
// The 64-character length cap is enforced Go-side only (dashboardbff.
// ValidCityName), so an over-long name passes this gate and 404s at /api.
export const CITY_NAME_RE = /^[a-z0-9][a-z0-9._-]*$/i;

/** True when `cityName` is a safe city path segment + dispatch key. */
export function isValidCityName(cityName: string): boolean {
  return CITY_NAME_RE.test(cityName);
}
