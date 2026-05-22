export type DashboardResource =
  | "cities"
  | "status"
  | "supervisor"
  | "crew"
  | "issues"
  | "mail"
  | "convoys"
  | "activity"
  | "admin"
  | "options";

export interface CityInfoSummary {
  error?: string;
  name: string;
  path?: string;
  phasesCompleted: string[];
  running: boolean;
  status?: string;
}

const ALL_RESOURCES: DashboardResource[] = [
  "cities",
  "status",
  "supervisor",
  "crew",
  "issues",
  "mail",
  "convoys",
  "activity",
  "admin",
  "options",
];

const CITY_SCOPED_RESOURCES: DashboardResource[] = [
  "status",
  "crew",
  "issues",
  "mail",
  "convoys",
  "activity",
  "admin",
  "options",
];

let currentCity = readCityScope(window.location.search);
let cachedCities: CityInfoSummary[] = [];
let cachedCitiesKnown = false;
const invalidated = new Set<DashboardResource>(ALL_RESOURCES);

export function cityScope(): string {
  return currentCity;
}

export function syncCityScopeFromLocation(): string {
  currentCity = readCityScope(window.location.search);
  return currentCity;
}

export function navigateToScope(nextURL: string): void {
  window.history.pushState({}, "", nextURL);
  syncCityScopeFromLocation();
  invalidateAll();
}

export function invalidate(...resources: DashboardResource[]): void {
  resources.forEach((resource) => invalidated.add(resource));
}

export function invalidateAll(): void {
  invalidate(...ALL_RESOURCES);
}

export function invalidateCityScope(): void {
  invalidate(...CITY_SCOPED_RESOURCES);
}

export function consumeInvalidated(force = false): Set<DashboardResource> {
  if (force) {
    invalidated.clear();
    return new Set(ALL_RESOURCES);
  }
  const next = new Set(invalidated);
  invalidated.clear();
  return next;
}

export function setCachedCities(cities: CityInfoSummary[]): void {
  cachedCitiesKnown = true;
  cachedCities = cities.map((city) => ({
    error: city.error,
    name: city.name,
    path: city.path,
    phasesCompleted: [...(city.phasesCompleted ?? [])],
    running: city.running,
    status: city.status,
  }));
}

export function markCachedCitiesUnknown(): void {
  cachedCitiesKnown = false;
}

export function getCachedCities(): CityInfoSummary[] {
  return cachedCities.map((city) => ({
    error: city.error,
    name: city.name,
    path: city.path,
    phasesCompleted: [...city.phasesCompleted],
    running: city.running,
    status: city.status,
  }));
}

// currentCityStatus classifies the selected city against the cached
// cities list. Used by the boot sequence to decide whether to fire
// every per-city panel fetch (which would 404 on an init_failed
// city and produce a cascade of console errors) or render a single
// "city is not running" banner and skip the fetches.
export type CurrentCityStatus =
  | { kind: "supervisor" } // no city selected; supervisor-scope view
  | { kind: "running"; city: CityInfoSummary }
  | { kind: "not-running"; city: CityInfoSummary }
  | { kind: "unknown"; name: string }; // selected name not in cities list (stale link, etc.)

export function currentCityStatus(): CurrentCityStatus {
  const name = currentCity;
  if (name === "") return { kind: "supervisor" };
  if (!cachedCitiesKnown) return { kind: "unknown", name };
  const city = cachedCities.find((c) => c.name === name);
  if (!city) return { kind: "unknown", name };
  return city.running ? { kind: "running", city } : { kind: "not-running", city };
}

export function canFetchCityScopedResources(status: CurrentCityStatus = currentCityStatus()): boolean {
  if (status.kind === "running") return true;
  if (status.kind === "unknown") return !cachedCitiesKnown;
  return false;
}

export function isKnownUnavailableCity(status: CurrentCityStatus = currentCityStatus()): boolean {
  return status.kind === "not-running" || (status.kind === "unknown" && cachedCitiesKnown);
}

export function invalidateForEventType(type: string): boolean {
  if (!type) return false;
  const hasCityScope = currentCity !== "";
  if (type.startsWith("session.") || type.startsWith("agent.")) {
    if (!hasCityScope) return false;
    invalidate("status", "crew", "options");
    return true;
  }
  if (type.startsWith("bead.")) {
    if (!hasCityScope) return false;
    invalidate("status", "issues");
    return true;
  }
  if (type.startsWith("mail.")) {
    if (!hasCityScope) return false;
    invalidate("status", "mail");
    return true;
  }
  if (type.startsWith("convoy.")) {
    if (!hasCityScope) return false;
    invalidate("status", "convoys");
    return true;
  }
  if (type.startsWith("city.") || type.startsWith("request.result.") || type === "request.failed") {
    invalidate("cities", "status", "supervisor");
    return true;
  }
  if (type.startsWith("service.") || type.startsWith("provider.") || type.startsWith("rig.")) {
    if (!hasCityScope) return false;
    invalidate("admin");
    return true;
  }
  return false;
}

function readCityScope(search: string): string {
  const params = new URLSearchParams(search);
  return (params.get("city") ?? "").trim();
}
