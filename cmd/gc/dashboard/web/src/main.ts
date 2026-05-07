import { cityScope } from "./api";
import { renderCityTabs } from "./panels/cities";
import { renderStatus } from "./panels/status";
import { renderCrew, installCrewInteractions, closeLogDrawerExternal, resetCrewNoCity } from "./panels/crew";
import { renderIssues, installIssueInteractions, resetIssuesNoCity } from "./panels/issues";
import { renderMail, installMailInteractions, resetMailNoCity } from "./panels/mail";
import { renderConvoys, installConvoyInteractions, resetConvoysNoCity } from "./panels/convoys";
import { eventTypeFromMessage, loadActivityHistory, resetActivity, startActivityStream, stopActivityStream, installActivityInteractions } from "./panels/activity";
import { renderAdminPanels, installAdminInteractions, renderAdminEmptyStates } from "./panels/admin";
import { invalidateOptions } from "./panels/options";
import { installPanelAffordances, popPause, refreshPaused, reportUIError, setPopPauseListener } from "./ui";
import { installCommandPalette } from "./palette";
import { installDashboardLogging, logInfo } from "./logger";
import {
  consumeInvalidated,
  canFetchCityScopedResources,
  currentCityStatus,
  invalidateAll,
  invalidateForEventType,
  isKnownUnavailableCity,
  markCachedCitiesUnknown,
  syncCityScopeFromLocation,
  type DashboardResource,
} from "./state";
import { renderSupervisorOverview } from "./panels/supervisor";
import { installSharedModals } from "./modals";
import { createRefreshScheduler } from "./refresh_scheduler";

const CITY_SCOPED_PANEL_IDS = [
  "convoy-panel",
  "crew-panel",
  "rigged-panel",
  "mail-panel",
  "escalations-panel",
  "services-panel",
  "rigs-panel",
  "pooled-panel",
  "queues-panel",
  "beads-panel",
  "assigned-panel",
  "agent-log-drawer",
];

async function refreshAll(): Promise<void> {
  if (refreshPaused()) return;
  await refreshVisibleResources();
}

// flushPendingInvalidations renders any resources that were marked dirty
// while the UI was paused (modal open, panel expanded). Called by ui.ts
// from popPause() when the pause counter returns to zero, so the
// dashboard catches up rather than staying stale.
export async function flushPendingInvalidations(): Promise<void> {
  if (refreshPaused()) return;
  await refreshVisibleResources().catch((error) => reportUIError("Catch-up refresh failed", error));
}

async function refreshAllForced(): Promise<void> {
  invalidateAll();
  await refreshVisibleResources(true);
}

function wireSSE(): void {
  // Stopped-city gate: do not open a city-scoped stream when the
  // selected city is not running. connectCityEvents' auto-reconnect
  // loop would otherwise sit in "Reconnecting…" forever, hammering
  // the supervisor with every backoff tick. Supervisor-scope streams
  // (kind === "supervisor") always open.
  const status = currentCityStatus();
  if (isKnownUnavailableCity(status)) {
    stopActivityStream();
    setConnectionBadge("connecting"); // visible "not wired" state; re-wires on next city switch
    return;
  }
  setConnectionBadge("connecting");
  startActivityStream(
    (msg) => {
      const eventType = eventTypeFromMessage(msg);
      if (!eventType || eventType === "heartbeat") return;
      // Always mark the dirty set — the pause guard only defers the
      // render. Without this, events that arrive while a modal is open
      // get dropped and panels stay stale after the modal closes.
      const needsRefresh = invalidateForEventType(eventType);
      if (!needsRefresh) return;
      if (refreshPaused()) return;
      scheduleRefresh();
    },
    setConnectionBadge,
  );
}

function setConnectionBadge(status: "connecting" | "live" | "reconnecting"): void {
  const el = byId("connection-status");
  if (!el) return;
  const labels: Record<typeof status, string> = {
    connecting: "Connecting…",
    live: "Live",
    reconnecting: "Reconnecting…",
  };
  el.replaceChildren(document.createTextNode(labels[status]));
  el.classList.remove("connection-live", "connection-connecting", "connection-reconnecting");
  el.classList.add(`connection-${status}`);
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
  // Catch up when the last pause lifts: renders deferred-while-paused
  // resources so the UI isn't stale after modals close.
  setPopPauseListener(() => {
    void flushPendingInvalidations();
  });
  await refreshAllForced();
  wireSSE();
  logInfo("dashboard", "Boot complete", { city: cityScope(), href: window.location.href });
}

function byId(id: string): HTMLElement | null {
  return document.getElementById(id);
}

void boot().catch((error) => reportUIError("Dashboard boot failed", error));

function syncCityScopedControls(enabled: boolean): void {
  syncCityScopedPanels(enabled);
  setControlState("new-convoy-btn", enabled, "Select a running city to create a convoy");
  setControlState("new-issue-btn", enabled, "Select a running city to create a bead");
  setControlState("compose-mail-btn", enabled, "Select a running city to compose mail");
  setControlState("open-assign-btn", enabled, "Select a running city to assign work");
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
    // Same rationale as navigateCityScope: tear down city-owned
    // subviews before we leave the current scope.
    closeLogDrawerExternal();
    syncCityScopeFromLocation();
    invalidateAll();
    void refreshVisibleResources().catch((error) => reportUIError("Refresh failed", error));
    // Reuse wireSSE (not bare startActivityStream) so live invalidation
    // and the connection badge callback stay wired after back/forward.
    wireSSE();
  });
}

async function navigateCityScope(nextURL: string): Promise<void> {
  logInfo("dashboard", "Navigate city scope", { nextURL });
  // Close any city-owned subviews (log drawer + its session stream +
  // its pushPause token) BEFORE the scope actually changes. Without
  // this a drawer open at click time keeps its stream alive into the
  // next scope and pauseCount stays > 0 → every refresh is skipped.
  closeLogDrawerExternal();
  window.history.pushState({}, "", nextURL);
  syncCityScopeFromLocation();
  invalidateAll();
  await refreshVisibleResources();
  // Reuse wireSSE so live invalidation + badge callback stay wired after
  // the new city scope opens its stream. Calling bare startActivityStream
  // here loses the SSE→panel invalidation bridge.
  wireSSE();
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

const REFRESH_DEBOUNCE_MS = 1_000;
const REFRESH_MIN_INTERVAL_MS = 10_000;

const refreshScheduler = createRefreshScheduler({
  delayMs: REFRESH_DEBOUNCE_MS,
  isPaused: refreshPaused,
  minIntervalMs: REFRESH_MIN_INTERVAL_MS,
  onError: (error) => reportUIError("Refresh failed", error),
  run: () => refreshVisibleResources(),
});

function scheduleRefresh(): void {
  refreshScheduler.schedule();
}

async function refreshVisibleResources(force = false): Promise<void> {
  syncCityScopeFromLocation();

  const dirty = consumeInvalidated(force);
  if (dirty.size === 0) return;
  if (dirty.has("options")) {
    invalidateOptions();
  }

  if (dirty.has("cities")) {
    await renderCityTabs().catch((error) => {
      markCachedCitiesUnknown();
      reportUIError("City tabs failed", error);
    });
  }

  const tasks: Array<Promise<void>> = [];
  const status = currentCityStatus();
  const canFetchCity = canFetchCityScopedResources(status);
  syncCityScopedControls(canFetchCity);
  if (isKnownUnavailableCity(status)) {
    resetCityScopedResourceViews();
  }

  queueRefresh(tasks, dirty, "status", () => renderStatus());
  if (status.kind === "supervisor" || canFetchCity) {
    queueRefresh(tasks, dirty, "activity", () => loadActivityHistory());
  } else {
    resetActivity();
  }
  // Fan out city-scoped fetches for running cities and for selected
  // cities whose availability is not known yet. Reset/hide only once
  // the city list is known-good and proves the city is stopped or absent.
  if (canFetchCity) {
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

function resetCityScopedResourceViews(): void {
  resetConvoysNoCity();
  resetCrewNoCity();
  resetIssuesNoCity();
  resetMailNoCity();
  renderAdminEmptyStates();
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
