import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

async function waitFor(assertion: () => void | Promise<void>): Promise<void> {
  const deadline = Date.now() + 2_000;
  let lastError: unknown;
  while (Date.now() < deadline) {
    try {
      await assertion();
      return;
    } catch (error) {
      lastError = error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
  throw lastError;
}

function installDOM(): void {
  document.body.innerHTML = `
    <div id="city-tabs"></div>
    <div id="connection-status"></div>
    <div id="convoy-panel"></div>
    <div id="crew-panel"></div>
    <div id="rigged-panel"></div>
    <div id="mail-panel"></div>
    <div id="escalations-panel"></div>
    <div id="services-panel"></div>
    <div id="rigs-panel"></div>
    <div id="pooled-panel"></div>
    <div id="queues-panel"></div>
    <div id="beads-panel"></div>
    <div id="assigned-panel"></div>
    <div id="agent-log-drawer"></div>
    <button id="new-convoy-btn"></button>
    <button id="new-issue-btn"></button>
    <button id="compose-mail-btn"></button>
    <button id="open-assign-btn"></button>
  `;
}

describe("dashboard city scope navigation", () => {
  beforeEach(() => {
    vi.resetModules();
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard?city=running-city");
    installDOM();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard");
  });

  it("clears city-scoped panels when navigating to a stopped city", async () => {
    vi.doMock("./logger", () => ({
      installDashboardLogging: vi.fn(),
      logInfo: vi.fn(),
    }));
    vi.doMock("./ui", () => ({
      installPanelAffordances: vi.fn(),
      popPause: vi.fn(),
      refreshPaused: vi.fn(() => false),
      reportUIError: vi.fn(),
      setPopPauseListener: vi.fn(),
    }));
    vi.doMock("./refresh_scheduler", () => ({
      createRefreshScheduler: vi.fn(() => ({ schedule: vi.fn() })),
    }));
    vi.doMock("./modals", () => ({
      installSharedModals: vi.fn(),
    }));
    vi.doMock("./palette", () => ({
      installCommandPalette: vi.fn(),
    }));
    vi.doMock("./panels/cities", () => ({
      renderCityTabs: vi.fn(async () => {
        const { setCachedCities } = await import("./state");
        setCachedCities([
          { name: "running-city", phasesCompleted: [], running: true },
          { name: "stopped-city", phasesCompleted: [], running: false, status: "init_failed" },
        ]);
        document.getElementById("city-tabs")!.innerHTML = `
          <a class="city-tab" href="/dashboard?city=running-city">running-city</a>
          <a class="city-tab" href="/dashboard?city=stopped-city">stopped-city</a>
        `;
      }),
    }));
    vi.doMock("./panels/status", () => ({
      renderStatus: vi.fn(async () => {}),
    }));
    vi.doMock("./panels/crew", () => ({
      closeLogDrawerExternal: vi.fn(),
      installCrewInteractions: vi.fn(),
      renderCrew: vi.fn(async () => {}),
      resetCrewNoCity: vi.fn(),
    }));
    vi.doMock("./panels/issues", () => ({
      installIssueInteractions: vi.fn(),
      renderIssues: vi.fn(async () => {
        document.getElementById("beads-panel")!.textContent = "stale bead mlc1-627";
      }),
      resetIssuesNoCity: vi.fn(() => {
        document.getElementById("beads-panel")!.textContent = "cleared beads";
      }),
    }));
    vi.doMock("./panels/mail", () => ({
      installMailInteractions: vi.fn(),
      renderMail: vi.fn(async () => {}),
      resetMailNoCity: vi.fn(),
    }));
    vi.doMock("./panels/convoys", () => ({
      installConvoyInteractions: vi.fn(),
      renderConvoys: vi.fn(async () => {}),
      resetConvoysNoCity: vi.fn(),
    }));
    vi.doMock("./panels/activity", () => ({
      eventTypeFromMessage: vi.fn(() => ""),
      installActivityInteractions: vi.fn(),
      loadActivityHistory: vi.fn(async () => {}),
      resetActivity: vi.fn(),
      startActivityStream: vi.fn(),
      stopActivityStream: vi.fn(),
    }));
    vi.doMock("./panels/admin", () => ({
      installAdminInteractions: vi.fn(),
      renderAdminEmptyStates: vi.fn(),
      renderAdminPanels: vi.fn(async () => {}),
    }));
    vi.doMock("./panels/options", () => ({
      invalidateOptions: vi.fn(),
    }));
    vi.doMock("./panels/supervisor", () => ({
      renderSupervisorOverview: vi.fn(),
    }));

    await import("./main");
    await waitFor(() => {
      expect(document.getElementById("beads-panel")?.textContent).toContain("mlc1-627");
    });

    document.querySelector<HTMLAnchorElement>('a[href="/dashboard?city=stopped-city"]')!.click();

    await waitFor(() => {
      expect(window.location.search).toBe("?city=stopped-city");
      expect(document.getElementById("beads-panel")?.hidden).toBe(true);
      expect(document.getElementById("beads-panel")?.textContent).not.toContain("mlc1-627");
    });
  });

  it("keeps city-scoped panels enabled before the city list is known-good", async () => {
    const renderIssues = vi.fn(async () => {
      document.getElementById("beads-panel")!.textContent = "loaded selected city";
    });
    const resetIssuesNoCity = vi.fn(() => {
      document.getElementById("beads-panel")!.textContent = "cleared beads";
    });

    vi.doMock("./logger", () => ({
      installDashboardLogging: vi.fn(),
      logInfo: vi.fn(),
    }));
    vi.doMock("./ui", () => ({
      installPanelAffordances: vi.fn(),
      popPause: vi.fn(),
      refreshPaused: vi.fn(() => false),
      reportUIError: vi.fn(),
      setPopPauseListener: vi.fn(),
    }));
    vi.doMock("./refresh_scheduler", () => ({
      createRefreshScheduler: vi.fn(() => ({ schedule: vi.fn() })),
    }));
    vi.doMock("./modals", () => ({
      installSharedModals: vi.fn(),
    }));
    vi.doMock("./palette", () => ({
      installCommandPalette: vi.fn(),
    }));
    vi.doMock("./panels/cities", () => ({
      renderCityTabs: vi.fn(async () => {
        throw new Error("temporary city list failure");
      }),
    }));
    vi.doMock("./panels/status", () => ({
      renderStatus: vi.fn(async () => {}),
    }));
    vi.doMock("./panels/crew", () => ({
      closeLogDrawerExternal: vi.fn(),
      installCrewInteractions: vi.fn(),
      renderCrew: vi.fn(async () => {}),
      resetCrewNoCity: vi.fn(),
    }));
    vi.doMock("./panels/issues", () => ({
      installIssueInteractions: vi.fn(),
      renderIssues,
      resetIssuesNoCity,
    }));
    vi.doMock("./panels/mail", () => ({
      installMailInteractions: vi.fn(),
      renderMail: vi.fn(async () => {}),
      resetMailNoCity: vi.fn(),
    }));
    vi.doMock("./panels/convoys", () => ({
      installConvoyInteractions: vi.fn(),
      renderConvoys: vi.fn(async () => {}),
      resetConvoysNoCity: vi.fn(),
    }));
    vi.doMock("./panels/activity", () => ({
      eventTypeFromMessage: vi.fn(() => ""),
      installActivityInteractions: vi.fn(),
      loadActivityHistory: vi.fn(async () => {}),
      resetActivity: vi.fn(),
      startActivityStream: vi.fn(),
      stopActivityStream: vi.fn(),
    }));
    vi.doMock("./panels/admin", () => ({
      installAdminInteractions: vi.fn(),
      renderAdminEmptyStates: vi.fn(),
      renderAdminPanels: vi.fn(async () => {}),
    }));
    vi.doMock("./panels/options", () => ({
      invalidateOptions: vi.fn(),
    }));
    vi.doMock("./panels/supervisor", () => ({
      renderSupervisorOverview: vi.fn(),
    }));

    await import("./main");

    await waitFor(() => {
      expect(renderIssues).toHaveBeenCalled();
      expect(document.getElementById("beads-panel")?.hidden).toBe(false);
      expect(document.getElementById("beads-panel")?.textContent).toBe("loaded selected city");
    });
    expect(resetIssuesNoCity).not.toHaveBeenCalled();
  });
});
