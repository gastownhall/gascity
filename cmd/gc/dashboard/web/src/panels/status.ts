import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { ACTIVE_WINDOW_MS, beadPriority, formatTimestamp } from "../util/legacy";

interface SessionSummary {
  attached: boolean;
  last_active?: string;
  pool?: string;
  rig?: string;
  running: boolean;
  template: string;
}

export async function renderStatus(): Promise<void> {
  const city = cityScope();
  const banner = byId("status-banner");
  if (!banner) return;
  if (!city) {
    await renderSupervisorStatus(banner);
    return;
  }
  renderMayorUnknown();

  const [statusR, sessionsR, beadsR, convoysR] = await Promise.all([
    api.GET("/v0/city/{cityName}/status", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/sessions", {
      params: { path: { cityName: city }, query: { state: "active", peek: "true" } },
    }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "open", limit: 500 } },
    }),
    api.GET("/v0/city/{cityName}/convoys", { params: { path: { cityName: city }, query: { limit: 200 } } }),
  ]);

  if (statusR.error || !statusR.data) {
    clear(banner);
    banner.append(el("div", { class: "banner-error" }, [`Status unavailable for ${city}`]));
    return;
  }

  const sessions = (sessionsR.data?.items ?? []) as SessionSummary[];
  const beads = beadsR.data?.items ?? [];
  const convoys = convoysR.data?.items ?? [];
  renderMayor(sessions);

  const stuckPolecats = sessions.filter((session) => {
    if (!session.rig || !session.pool || !session.running || !session.last_active) return false;
    return Date.now() - new Date(session.last_active).getTime() >= 30 * 60 * 1000;
  }).length;
  const staleAssigned = beads.filter((bead) => bead.assignee && bead.status !== "closed").length;
  const highPriorityIssues = beads.filter((bead) => beadPriority(bead.priority) <= 2).length;
  const deadSessions = sessions.filter((session) => !session.running).length;

  const stats = el("div", { class: "summary-stats" }, [
    statChip(statusR.data.agents.running, "🦨 Polecats"),
    statChip(statusR.data.work.in_progress, "👤 Assigned"),
    statChip(statusR.data.work.open, "📋 Beads"),
    statChip(convoys.length, "🚚 Convoys"),
    statChip(statusR.data.mail.unread, "✉️ Unread"),
  ]);

  const alerts = el("div", { class: "summary-alerts" });
  appendAlert(alerts, stuckPolecats > 0, "alert-red", `💀 ${stuckPolecats} stuck`);
  appendAlert(alerts, staleAssigned > 0, "alert-yellow", `⏰ ${staleAssigned} assigned`);
  appendAlert(alerts, highPriorityIssues > 0, "alert-red", `🔥 ${highPriorityIssues} P1/P2`);
  appendAlert(alerts, deadSessions > 0, "alert-red", `☠️ ${deadSessions} dead`);
  if (!alerts.childNodes.length) {
    alerts.append(el("span", { class: "alert-item alert-green" }, ["✓ All clear"]));
  }

  clear(banner);
  banner.append(stats, alerts);
}

async function renderSupervisorStatus(banner: HTMLElement): Promise<void> {
  renderMayorSupervisor();

  const [healthR, citiesR] = await Promise.all([
    api.GET("/health"),
    api.GET("/v0/cities"),
  ]);
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

function renderMayorUnknown(): void {
  const banner = byId("mayor-banner");
  const badge = byId("mayor-badge");
  const status = byId("mayor-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached");
  banner.classList.add("detached");
  badge.className = "badge badge-muted";
  badge.textContent = "Unknown";
  clear(status);
}

function renderMayorSupervisor(): void {
  const banner = byId("mayor-banner");
  const badge = byId("mayor-badge");
  const status = byId("mayor-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached");
  banner.classList.add("detached");
  badge.className = "badge badge-muted";
  badge.textContent = "Supervisor";
  clear(status);
  status.append(
    mayorStat("Scope", "Fleet"),
    mayorStat("Mayor", "Select a city"),
  );
}

function renderMayor(sessions: SessionSummary[]): void {
  const banner = byId("mayor-banner");
  const badge = byId("mayor-badge");
  const status = byId("mayor-status");
  if (!banner || !badge || !status) return;

  const mayor = sessions.find((session) => !session.rig && !session.pool);
  if (!mayor) {
    renderMayorUnknown();
    return;
  }

  banner.classList.remove("attached", "detached");
  banner.classList.add(mayor.attached ? "attached" : "detached");
  badge.className = `badge ${mayor.attached ? "badge-green" : "badge-muted"}`;
  badge.textContent = mayor.attached ? "Attached" : "Detached";
  clear(status);

  if (!mayor.attached) return;

  const active = mayor.last_active
    ? Date.now() - new Date(mayor.last_active).getTime() < ACTIVE_WINDOW_MS
    : false;

  status.append(
    mayorStat("Activity", mayor.last_active ? formatTimestamp(mayor.last_active) : "Unknown", active ? "active" : "idle"),
    mayorStat("State", mayor.running ? "Running" : "Stopped"),
  );
}

function mayorStat(label: string, value: string, variant = ""): HTMLElement {
  return el("div", { class: "mayor-stat" }, [
    el("span", { class: "mayor-stat-label" }, [label]),
    el("span", { class: `mayor-stat-value${variant ? ` ${variant}` : ""}` }, [value]),
  ]);
}

function formatUptime(seconds: number | undefined): string {
  if (!seconds || seconds <= 0) return "0m";
  if (seconds < 3600) return `${Math.max(1, Math.floor(seconds / 60))}m`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86_400)}d`;
}
