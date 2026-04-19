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
  renderCityScopeBannerIdle();

  const [statusR, sessionsR, beadsR, convoysR] = await Promise.all([
    api.GET("/v0/city/{cityName}/status", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/sessions", {
      params: { path: { cityName: city }, query: { state: "active", peek: true } },
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
  renderCityScopeBanner(city, sessions);

  // Generic "stuck" detection: any running, pooled agent whose last
  // activity is >30 min old. No role name required.
  const stuckAgents = sessions.filter((session) => {
    if (!session.pool || !session.running || !session.last_active) return false;
    return Date.now() - new Date(session.last_active).getTime() >= 30 * 60 * 1000;
  }).length;
  const staleAssigned = beads.filter((bead) => bead.assignee && bead.status !== "closed").length;
  const highPriorityIssues = beads.filter((bead) => beadPriority(bead.priority) <= 2).length;
  const deadSessions = sessions.filter((session) => !session.running).length;

  const stats = el("div", { class: "summary-stats" }, [
    statChip(statusR.data.agents.running, "Agents"),
    statChip(statusR.data.work.in_progress, "Assigned"),
    statChip(statusR.data.work.open, "Beads"),
    statChip(convoys.length, "Convoys"),
    statChip(statusR.data.mail.unread, "Unread"),
  ]);

  const alerts = el("div", { class: "summary-alerts" });
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

async function renderSupervisorStatus(banner: HTMLElement): Promise<void> {
  renderCityScopeBannerFleet();

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

// renderCityScopeBanner renders a generic "scope" banner that reports
// whether any un-rigged, un-pooled session (the city-scope overseer, if
// the pack defines one) is currently attached. The dashboard makes no
// assumption about what that session is called — it just surfaces the
// attached/detached state the API provides. Packs that don't define a
// city-scope session show "Detached" and that's fine.
function renderCityScopeBanner(city: string, sessions: SessionSummary[]): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;

  const overseer = sessions.find((session) => !session.rig && !session.pool);
  if (!overseer) {
    banner.classList.remove("attached");
    banner.classList.add("detached");
    badge.className = "badge badge-muted";
    badge.textContent = "Detached";
    clear(status);
    status.append(
      scopeStat("Scope", city),
      scopeStat("Overseer", "none"),
    );
    return;
  }

  banner.classList.remove("attached", "detached");
  banner.classList.add(overseer.attached ? "attached" : "detached");
  badge.className = `badge ${overseer.attached ? "badge-green" : "badge-muted"}`;
  badge.textContent = overseer.attached ? "Attached" : "Detached";
  clear(status);

  const active = overseer.last_active
    ? Date.now() - new Date(overseer.last_active).getTime() < ACTIVE_WINDOW_MS
    : false;
  status.append(
    scopeStat("Scope", city),
    scopeStat("Session", overseer.template),
    scopeStat("Activity", overseer.last_active ? formatTimestamp(overseer.last_active) : "Unknown", active ? "active" : "idle"),
    scopeStat("State", overseer.running ? "Running" : "Stopped"),
  );
}

function renderCityScopeBannerIdle(): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached");
  banner.classList.add("detached");
  badge.className = "badge badge-muted";
  badge.textContent = "Idle";
  clear(status);
}

function renderCityScopeBannerFleet(): void {
  const banner = byId("scope-banner");
  const badge = byId("scope-badge");
  const status = byId("scope-status");
  if (!banner || !badge || !status) return;
  banner.classList.remove("attached");
  banner.classList.add("detached");
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
