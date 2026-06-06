import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { api } from "../api";
import { canFetchCityScopedResources, currentCityStatus, syncCityScopeFromLocation } from "../state";
import { renderCityTabs } from "./cities";

describe("city tabs", () => {
  beforeEach(() => {
    document.body.innerHTML = `<div id="city-tabs"></div>`;
    window.history.pushState({}, "", "/dashboard?city=mc-city");
    syncCityScopeFromLocation();
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.pushState({}, "", "/dashboard");
    syncCityScopeFromLocation();
  });

  it("keeps the previous city tab list visible when refresh fails", async () => {
    const get = vi.spyOn(api, "GET");
    get.mockResolvedValueOnce({
      data: {
        items: [{ error: "", name: "mc-city", path: "/tmp/mc-city", phases_completed: [], running: true, status: "ok" }],
      },
    } as never);
    await renderCityTabs();
    expect(document.getElementById("city-tabs")?.textContent).toContain("mc-city");
    expect(document.getElementById("city-tabs")?.textContent).toContain("Supervisor");

    get.mockResolvedValueOnce({ error: { detail: "boom" } } as never);
    await renderCityTabs();
    expect(document.getElementById("city-tabs")?.textContent).toContain("mc-city");
    expect(document.getElementById("city-tabs")?.textContent).toContain("Supervisor");
  });

  it("treats unknown selected cities as fetchable after a failed city-list refresh", async () => {
    const get = vi.spyOn(api, "GET");
    get.mockResolvedValueOnce({
      data: {
        items: [{ error: "", name: "mc-city", path: "/tmp/mc-city", phases_completed: [], running: true, status: "ok" }],
      },
    } as never);
    await renderCityTabs();

    get.mockResolvedValueOnce({ error: { detail: "boom" } } as never);
    await renderCityTabs();
    window.history.pushState({}, "", "/dashboard?city=not-yet-listed");
    syncCityScopeFromLocation();

    expect(currentCityStatus()).toEqual({ kind: "unknown", name: "not-yet-listed" });
    expect(canFetchCityScopedResources()).toBe(true);
  });

  it("does not trust stale stopped-city status after a failed city-list refresh", async () => {
    const get = vi.spyOn(api, "GET");
    get.mockResolvedValueOnce({
      data: {
        items: [{ error: "", name: "mc-city", path: "/tmp/mc-city", phases_completed: [], running: false, status: "stopped" }],
      },
    } as never);
    await renderCityTabs();
    expect(currentCityStatus().kind).toBe("not-running");
    expect(canFetchCityScopedResources()).toBe(false);

    get.mockResolvedValueOnce({ error: { detail: "boom" } } as never);
    await renderCityTabs();

    expect(currentCityStatus()).toEqual({ kind: "unknown", name: "mc-city" });
    expect(canFetchCityScopedResources()).toBe(true);
  });
});
