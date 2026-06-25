// City-name grammar — single source of truth shared by the backend
// (validates GC_CITY_NAME at boot + the `/api/city/:cityName` dispatch
// segment) and the frontend (validates the `/city/:cityName` route segment
// before splicing it into a request path). gascity-dashboard-ucc.
//
// A city name lands in security-sensitive positions on the backend: a path
// segment under ~/.gascity-dashboard/cities/<cityName>/ and the request-plane
// dispatch key. The grammar is alphanumeric with internal hyphens only — no
// separators, no leading/trailing hyphen — so the segment is inert as a path
// component and as a Map key. Keeping it in `shared` makes the two sides
// provably identical; a drift would otherwise be a runtime 404, not a compile
// error.
export const CITY_NAME_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/i;

/** True when `cityName` is a safe city path segment + dispatch key. */
export function isValidCityName(cityName: string): boolean {
  return CITY_NAME_RE.test(cityName);
}
