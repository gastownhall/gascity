import { cityScope } from "./api";
import { renderCityTabs } from "./panels/cities";
import { renderStatus } from "./panels/status";
import { renderCrew, installCrewInteractions } from "./panels/crew";
import { renderIssues, installIssueInteractions } from "./panels/issues";
import { renderMail, installMailInteractions } from "./panels/mail";
import { renderConvoys, installConvoyInteractions } from "./panels/convoys";
import { loadActivityHistory, startActivityStream, renderActivity, installActivityInteractions } from "./panels/activity";
import { renderAdminPanels, installAdminInteractions } from "./panels/admin";
import { invalidateOptions } from "./panels/options";
import { connectEvents } from "./sse";
import { installPanelAffordances, popPause, refreshPaused } from "./ui";
import { installCommandPalette } from "./palette";

const REFRESH_MS = 30_000;

type RefreshFn = () => Promise<void> | void;

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

const dataPanels: RefreshFn[] = [
  renderStatus,
  renderCrew,
  renderIssues,
  renderMail,
  renderConvoys,
  renderAdminPanels,
  loadActivityHistory,
];

async function refreshAll(): Promise<void> {
  if (refreshPaused()) return;
  await refreshAllForced();
}

async function refreshAllForced(): Promise<void> {
  syncCityScopedControls();
  invalidateOptions();
  await Promise.allSettled([
    renderCityTabs(),
    ...dataPanels.map((fn) => Promise.resolve(fn())),
  ]);
  renderActivity();
}

function wireSSE(): void {
  startActivityStream();
  byId("connection-status")?.replaceChildren(document.createTextNode("Live"));
  byId("connection-status")?.classList.add("connection-live");

  connectEvents((msg) => {
    if (refreshPaused()) return;
    if (msg.type === "message" || msg.type === "heartbeat") return;
    const stateChanging = [
      "session.started", "session.ended", "session.crashed", "session.woke", "session.suspended",
      "bead.created", "bead.updated", "bead.closed",
      "mail.delivered", "mail.read",
      "convoy.created", "convoy.closed",
    ];
    if (stateChanging.includes(msg.type)) {
      void Promise.all(dataPanels.map((fn) => Promise.resolve(fn())));
    }
  });
}

function installInteractions(): void {
  installPanelAffordances();
  installCrewInteractions();
  installIssueInteractions();
  installMailInteractions();
  installConvoyInteractions();
  installActivityInteractions();
  installAdminInteractions();
  installCommandPalette({ refreshAll });
}

async function boot(): Promise<void> {
  installInteractions();
  installCityScopeNavigation();
  await refreshAllForced();
  wireSSE();
  window.setInterval(() => {
    void refreshAll();
  }, REFRESH_MS);
}

function byId(id: string): HTMLElement | null {
  return document.getElementById(id);
}

void boot();

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
    void refreshAllForced();
    startActivityStream();
  });
}

async function navigateCityScope(nextURL: string): Promise<void> {
  window.history.pushState({}, "", nextURL);
  await refreshAllForced();
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
