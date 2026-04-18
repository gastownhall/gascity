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
  cachedCities = cities.map((city) => ({
    error: city.error,
    name: city.name,
    path: city.path,
    phasesCompleted: [...(city.phasesCompleted ?? [])],
    running: city.running,
    status: city.status,
  }));
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

export function invalidateForEventType(type: string): void {
  if (!type) return;
  if (type.startsWith("session.") || type.startsWith("agent.")) {
    invalidate("status", "crew", "options");
    return;
  }
  if (type.startsWith("bead.")) {
    invalidate("status", "issues", "convoys", "admin", "options");
    return;
  }
  if (type.startsWith("mail.")) {
    invalidate("status", "mail", "options");
    return;
  }
  if (type.startsWith("convoy.")) {
    invalidate("status", "convoys");
    return;
  }
  if (type.startsWith("city.")) {
    invalidate("cities", "status", "supervisor");
    return;
  }
  if (type.startsWith("service.") || type.startsWith("provider.") || type.startsWith("rig.")) {
    invalidate("admin");
    return;
  }
}

function readCityScope(search: string): string {
  const params = new URLSearchParams(search);
  return (params.get("city") ?? "").trim();
}
