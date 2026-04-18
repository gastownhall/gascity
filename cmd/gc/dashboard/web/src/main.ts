import { cityScope } from "./api";
import { renderCityTabs } from "./panels/cities";
import { renderStatus } from "./panels/status";
import { renderCrew, installCrewInteractions } from "./panels/crew";
import { renderIssues, installIssueInteractions } from "./panels/issues";
import { renderMail, installMailInteractions } from "./panels/mail";
import { renderConvoys, installConvoyInteractions } from "./panels/convoys";
import { eventTypeFromMessage, loadActivityHistory, startActivityStream, installActivityInteractions } from "./panels/activity";
import { renderAdminPanels, installAdminInteractions } from "./panels/admin";
import { invalidateOptions } from "./panels/options";
import { installPanelAffordances, popPause, refreshPaused, reportUIError } from "./ui";
import { installCommandPalette } from "./palette";
import { installDashboardLogging, logInfo } from "./logger";
import {
  consumeInvalidated,
  invalidateAll,
  invalidateForEventType,
  syncCityScopeFromLocation,
  type DashboardResource,
} from "./state";
import { renderSupervisorOverview } from "./panels/supervisor";
import { installSharedModals } from "./modals";

const CITY_SCOPED_PANEL_IDS = [
  "convoy-panel",
  "crew-panel",
  "polecats-panel",
  "mail-panel",
  "escalations-panel",
  "services-panel",
  "rigs-panel",
  "dogs-panel",
  "queues-panel",
  "beads-panel",
  "assigned-panel",
];

async function refreshAll(): Promise<void> {
  if (refreshPaused()) return;
  await refreshVisibleResources();
}

async function refreshAllForced(): Promise<void> {
  invalidateAll();
  await refreshVisibleResources(true);
}

function wireSSE(): void {
  startActivityStream((msg) => {
    if (refreshPaused()) return;
    const eventType = eventTypeFromMessage(msg);
    if (!eventType || eventType === "heartbeat") return;
    invalidateForEventType(eventType);
    void refreshVisibleResources().catch((error) => reportUIError("Refresh failed", error));
  });
  byId("connection-status")?.replaceChildren(document.createTextNode("Live"));
  byId("connection-status")?.classList.add("connection-live");
}

function installInteractions(): void {
  installPanelAffordances();
  installSharedModals();
  installCrewInteractions();
  installIssueInteractions();
  installMailInteractions();
  installConvoyInteractions();
  installActivityInteractions();
  installAdminInteractions();
  installCommandPalette({ refreshAll });
}

async function boot(): Promise<void> {
  installDashboardLogging();
  logInfo("dashboard", "Boot start", { city: cityScope(), href: window.location.href });
  installInteractions();
  installCityScopeNavigation();
  await refreshAllForced();
  wireSSE();
  logInfo("dashboard", "Boot complete", { city: cityScope(), href: window.location.href });
}

function byId(id: string): HTMLElement | null {
  return document.getElementById(id);
}

void boot().catch((error) => reportUIError("Dashboard boot failed", error));

function syncCityScopedControls(): void {
  const hasCity = cityScope() !== "";
  syncCityScopedPanels(hasCity);
  setControlState("new-convoy-btn", hasCity, "Select a city to create a convoy");
  setControlState("new-issue-btn", hasCity, "Select a city to create a bead");
  setControlState("compose-mail-btn", hasCity, "Select a city to compose mail");
  setControlState("open-assign-btn", hasCity, "Select a city to assign work");
}

function setControlState(id: string, enabled: boolean, disabledTitle: string): void {
  const button = byId(id) as HTMLButtonElement | null;
  if (!button) return;
  if (button.dataset.defaultTitle === undefined) {
    button.dataset.defaultTitle = button.title || "";
  }
  button.disabled = !enabled;
  button.title = enabled ? button.dataset.defaultTitle : disabledTitle;
}

function installCityScopeNavigation(): void {
  document.addEventListener("click", (event) => {
    const link = (event.target as HTMLElement | null)?.closest("a.city-tab") as HTMLAnchorElement | null;
    if (!link) return;
    const nextURL = link.href;
    if (!nextURL || nextURL === window.location.href) return;
    event.preventDefault();
    void navigateCityScope(nextURL);
  });

  window.addEventListener("popstate", () => {
    logInfo("dashboard", "Popstate navigation", { href: window.location.href });
    syncCityScopeFromLocation();
    invalidateAll();
    void refreshVisibleResources().catch((error) => reportUIError("Refresh failed", error));
    startActivityStream();
  });
}

async function navigateCityScope(nextURL: string): Promise<void> {
  logInfo("dashboard", "Navigate city scope", { nextURL });
  window.history.pushState({}, "", nextURL);
  syncCityScopeFromLocation();
  invalidateAll();
  await refreshVisibleResources();
  startActivityStream();
}

function syncCityScopedPanels(hasCity: boolean): void {
  CITY_SCOPED_PANEL_IDS.forEach((id) => {
    const panel = byId(id);
    if (!panel) return;
    const hidingExpanded = !hasCity && panel.classList.contains("expanded");
    panel.hidden = !hasCity;
    if (hidingExpanded) {
      panel.classList.remove("expanded");
      const expandBtn = panel.querySelector(".expand-btn");
      if (expandBtn) expandBtn.textContent = "Expand";
      popPause();
    }
  });
}

async function refreshVisibleResources(force = false): Promise<void> {
  syncCityScopeFromLocation();
  syncCityScopedControls();

  const dirty = consumeInvalidated(force);
  if (dirty.size === 0) return;
  if (dirty.has("options")) {
    invalidateOptions();
  }

  if (dirty.has("cities")) {
    await renderCityTabs().catch((error) => reportUIError("City tabs failed", error));
  }

  const tasks: Array<Promise<void>> = [];
  const hasCity = cityScope() !== "";

  queueRefresh(tasks, dirty, "status", () => renderStatus());
  queueRefresh(tasks, dirty, "activity", () => loadActivityHistory());
  if (hasCity) {
    queueRefresh(tasks, dirty, "crew", () => renderCrew());
    queueRefresh(tasks, dirty, "issues", () => renderIssues());
    queueRefresh(tasks, dirty, "mail", () => renderMail());
    queueRefresh(tasks, dirty, "convoys", () => renderConvoys());
    queueRefresh(tasks, dirty, "admin", () => renderAdminPanels());
  }

  const results = await Promise.allSettled(tasks);
  const failure = results.find((result): result is PromiseRejectedResult => result.status === "rejected");
  if (failure) {
    reportUIError("Panel refresh failed", failure.reason);
  }

  if (dirty.has("supervisor") || dirty.has("cities")) {
    renderSupervisorOverview();
  }
}

function queueRefresh(
  tasks: Array<Promise<void>>,
  dirty: Set<DashboardResource>,
  resource: DashboardResource,
  run: () => Promise<void>,
): void {
  if (!dirty.has(resource)) return;
  tasks.push(run());
}
