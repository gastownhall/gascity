import { api, cityScope, type DashboardSchema } from "../api";
import { logWarn } from "../logger";
import { currentCityStatus, isKnownUnavailableCity } from "../state";
import { byId, clear, el } from "../util/dom";
import { ACTIVE_WINDOW_MS, beadPriority, formatTimestamp } from "../util/legacy";

type APIResult<T> = {
  data?: T;
  error?: unknown;
};

type StatusBody = DashboardSchema["StatusBody"];
type SessionList = DashboardSchema["ListBodySessionResponse"];
type BeadList = DashboardSchema["ListBodyBead"];
type SessionSummary = DashboardSchema["SessionResponse"];

const STATUS_REQUEST_TIMEOUT_MS = 1_000;

export async function renderStatus(): Promise<void> {
  const city = cityScope();
  const banner = byId("status-banner");
  if (!banner) return;
  if (!city) {
    await renderSupervisorStatus(banner);
    return;
  }

  const status = currentCityStatus();
  if (isKnownUnavailableCity(status)) {
    const reason = status.kind === "not-running"
      ? (status.city.error ?? status.city.status ?? "City not running")
      : "City unavailable";
    renderCityScopeBannerUnavailable(city, "Sessions unavailable");
    renderUnavailableCitySummary(banner, reason);
    return;
  }

  const statusP = requestWithTimeout<StatusBody>(
    "status",
    city,
    (signal) => api.GET("/v0/city/{cityName}/status", { params: { path: { cityName: city } }, signal }) as Promise<APIResult<StatusBody>>,
  );
  const sessionsP = requestWithTimeout<SessionList>(
    "sessions",
    city,
    (signal) => api.GET("/v0/city/{cityName}/sessions", {
      params: { path: { cityName: city }, query: { state: "active", peek: true } },
      signal,
    }) as Promise<APIResult<SessionList>>,
  );
  const beadsP = requestWithTimeout<BeadList>(
    "beads",
    city,
    (signal) => api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "open", limit: 500 } },
      signal,
    }) as Promise<APIResult<BeadList>>,
  );
  const convoysP = requestWithTimeout<BeadList>(
    "convoys",
    city,
    (signal) => api.GET("/v0/city/{cityName}/convoys", {
      params: { path: { cityName: city }, query: { limit: 200 } },
      signal,
    }) as Promise<APIResult<BeadList>>,
  );

  sessionsP.then((sessionsR) => renderCityScopeFromSessions(city, sessionsR));

  const [statusR, sessionsR, beadsR, convoysR] = await Promise.all([statusP, sessionsP, beadsP, convoysP]);

  if (cityScope() !== city) return;

  const sessions = (sessionsR.data?.items ?? []) as SessionSummary[];
  const beads = beadsR.data?.items ?? [];
  const convoys = convoysR.data?.items ?? [];
  renderCityScopeFromSessions(city, sessionsR);

  // Generic "stuck" detection: any running, pooled agent whose last
  // activity is >30 min old. No role name required.
  const stuckAgents = sessions.filter((session) => {
    if (!session.pool || !session.running || !session.last_active) return false;
    return Date.now() - new Date(session.last_active).getTime() >= 30 * 60 * 1000;
  }).length;
  const staleAssigned = beads.filter((bead) => bead.assignee && bead.status !== "closed").length;
  const highPriorityIssues = beads.filter((bead) => beadPriority(bead.priority) <= 2).length;
  const deadSessions = sessions.filter((session) => !session.running).length;
  const statusUnavailable = Boolean(statusR.error || !statusR.data);
  const partialUnavailable = statusUnavailable || Boolean(sessionsR.error || beadsR.error || convoysR.error);
  const runningAgents = statusR.data?.agents.running ?? sessions.filter((session) => session.running).length;
  const assignedWork = statusR.data?.work.in_progress ?? staleAssigned;
  const openWork = statusR.data?.work.open ?? beads.length;
  const unreadMail = statusR.data?.mail.unread ?? "n/a";

  const statsKey = `${city}|${runningAgents}|${assignedWork}|${openWork}|${convoys.length}|${unreadMail}|${stuckAgents}|${staleAssigned}|${highPriorityIssues}|${deadSessions}|${partialUnavailable}|${statusUnavailable}`;
  if (statsKey !== lastStatusBannerKey) {
    lastStatusBannerKey = statsKey;
    const stats = el("div", { class: "summary-stats" }, [
      statChip(runningAgents, "Agents"),
      statChip(assignedWork, "Assigned"),
      statChip(openWork, "Beads"),
      statChip(convoys.length, "Convoys"),
      statChip(unreadMail, "Unread"),
    ]);

    const alerts = el("div", { class: "summary-alerts" });
    appendAlert(alerts, statusUnavailable, "alert-yellow", "Status API slow");
    appendAlert(alerts, partialUnavailable && !statusUnavailable, "alert-yellow", "Partial data");
    appendAlert(alerts, stuckAgents > 0, "alert-red", `${stuckAgents} stuck`);
    appendAlert(alerts, staleAssigned > 0, "alert-yellow", `${staleAssigned} assigned`);
    appendAlert(alerts, highPriorityIssues > 0, "alert-red", `${highPriorityIssues} P1/P2`);
    appendAlert(alerts, deadSessions > 0, "alert-red", `${deadSessions} dead`);
    if (!alerts.childNodes.length) {
      alerts.append(el("span", { class: "alert-item alert-green" }, ["All clear"]));
    }

    clear(banner);
    banner.append(stats, alerts);
  }
}

function renderUnavailableCitySummary(banner: HTMLElement, reason: string): void {
  lastStatusBannerKey = "";
  clear(banner);
  const stats = el("div", { class: "summary-stats" }, [
    statChip(0, "Agents"),
    statChip(0, "Assigned"),
    statChip(0, "Beads"),
    statChip(0, "Convoys"),
    statChip("n/a", "Unread"),
  ]);
  const alerts = el("div", { class: "summary-alerts" }, [
    el("span", { class: "alert-item alert-yellow" }, [reason]),
  ]);
  banner.append(stats, alerts);
}

async function requestWithTimeout<T>(
  label: string,
  city: string,
  start: (signal: AbortSignal) => Promise<APIResult<T>>,
): Promise<APIResult<T>> {
  const controller = new AbortController();
  let completed = false;
  let timer: ReturnType<typeof setTimeout>;
  return new Promise((resolve) => {
    timer = setTimeout(() => {
      if (completed) return;
      completed = true;
      const error = new Error(`${label} request timed out after ${STATUS_REQUEST_TIMEOUT_MS}ms`);
      controller.abort();
      logWarn("status", "City status dependency timed out", { city, label });
      resolve({ error });
    }, STATUS_REQUEST_TIMEOUT_MS);
    start(controller.signal).then(
      (value) => {
        if (completed) return;
        completed = true;
        clearTimeout(timer);
        resolve(value);
      },
      (error: unknown) => {
        if (completed) return;
        completed = true;
        clearTimeout(timer);
        logWarn("status", "City status dependency failed", { city, error, label });
        resolve({ error });
      },
    );
  });
}

async function renderSupervisorStatus(banner: HTMLElement): Promise<void> {
  renderCityScopeBannerFleet();
  lastStatusBannerKey = "";

  const [healthR, citiesR] = await Promise.all([
    api.GET("/health"),
    api.GET("/v0/cities"),
  ]);
  if (cityScope() !== "") return;

  const health = healthR.data;
  const cities = citiesR.data?.items ?? [];
  const total = health?.cities_total ?? cities.length;
  const running = health?.cities_running ?? cities.filter((city) => city.running === true).length;
  const stopped = Math.max(total - running, 0);
  const errored = cities.filter((city) => Boolean(city.error)).length;

  clear(banner);
  if (healthR.error && citiesR.error) {
    banner.append(el("div", { class: "banner-error" }, ["Supervisor status unavailable"]));
    return;
  }

  const stats = el("div", { class: "summary-stats" }, [
    statChip(total, "🏙️ Cities"),
    statChip(running, "🟢 Running"),
    statChip(stopped, "⏸ Stopped"),
    statChip(formatUptime(health?.uptime_sec), "⏱ Uptime"),
  ]);

  const alerts = el("div", { class: "summary-alerts" });
  appendAlert(alerts, total === 0, "alert-yellow", "No registered cities");
  appendAlert(alerts, stopped > 0, "alert-yellow", `${stopped} ${stopped === 1 ? "city" : "cities"} not running`);
  appendAlert(alerts, errored > 0, "alert-red", `${errored} ${errored === 1 ? "city" : "cities"} reporting errors`);
  appendAlert(
    alerts,
    Boolean(health?.startup && !health.startup.ready),
    "alert-yellow",
    `⏳ Startup: ${health?.startup?.phase || "starting"}`,
  );
  if (!alerts.childNodes.length) {
    alerts.append(el("span", { class: "alert-item alert-green" }, ["✓ Supervisor ready"]));
  }

  banner.append(stats, alerts);
}

function statChip(value: number | string | undefined, label: string): HTMLElement {
  return el("div", { class: "stat" }, [
    el("span", { class: "stat-value" }, [String(value ?? 0)]),
    el("span", { class: "stat-label" }, [label]),
  ]);
}

function appendAlert(container: HTMLElement, show: boolean, klass: string, text: string): void {
  if (!show) return;
  container.append(el("span", { class: `alert-item ${klass}` }, [text]));
}

let lastStatusBannerKey = "";

function renderCityScopeFromSessions(city: string, sessionsR: APIResult<SessionList>): void {
  if (cityScope() !== city) return;
  if (sessionsR.error || !sessionsR.data) {
    renderCityScopeBannerUnavailable(city, "Sessions unavailable");
    return;
  }
  renderCityScopeBanner(city, (sessionsR.data.items ?? []) as SessionSummary[]);
}

function renderCityScopeBanner(city: string, sessions: SessionSummary[]): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;

  const overseer =
    sessions.find((s) => s.configured_named_session && !s.rig) ??
    sessions.find((s) => !s.rig && !s.pool);

  if (!overseer) {
    banner.classList.remove("attached", "detached");
    badge.className = "badge badge-cyan";
    badge.textContent = "City";
    clear(status);
    status.append(
      scopeStat("City", city),
      scopeStat("Session", "none"),
    );
    return;
  }

  banner.classList.remove("attached", "detached");
  badge.className = "badge badge-cyan";
  badge.textContent = "City";
  clear(status);

  const active = overseer.last_active
    ? Date.now() - new Date(overseer.last_active).getTime() < ACTIVE_WINDOW_MS
    : false;
  status.append(
    scopeStat("City", city),
    scopeStat("Session", overseer.template),
    scopeStat("Activity", overseer.last_active ? formatTimestamp(overseer.last_active) : "Unknown", active ? "active" : "idle"),
    scopeStat("Terminal", overseer.attached ? "Attached" : "Detached"),
    scopeStat("State", overseer.running ? "Running" : "Stopped"),
  );
}

function renderCityScopeBannerUnavailable(city: string, reason: string): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached", "detached");
  badge.className = "badge badge-muted";
  badge.textContent = "Unknown";
  clear(status);
  status.append(
    scopeStat("Scope", city),
    scopeStat("Sessions", reason),
  );
}

function renderCityScopeBannerFleet(): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached", "detached");
  badge.className = "badge badge-muted";
  badge.textContent = "Supervisor";
  clear(status);
  status.append(
    scopeStat("Scope", "Fleet"),
    scopeStat("City", "Select one"),
  );
}

function scopeStat(label: string, value: string, variant = ""): HTMLElement {
  return el("div", { class: "scope-stat" }, [
    el("span", { class: "scope-stat-label" }, [label]),
    el("span", { class: `scope-stat-value${variant ? ` ${variant}` : ""}` }, [value]),
  ]);
}

function formatUptime(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return "0m";
  if (seconds < 3600) return `${Math.max(1, Math.floor(seconds / 60))}m`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86_400)}d`;
}
