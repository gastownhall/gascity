// Top-of-page health + summary banner. The Go `/api/status` endpoint
// aggregated city status + mayor + summary counts; the SPA issues
// the same underlying `/v0/city/{name}/status` call and renders
// inline.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";

export async function renderStatus(): Promise<void> {
  const container = byId("status-banner");
  if (!container) return;

  const city = cityScope();
  if (!city) {
    clear(container);
    container.append(el("div", { class: "banner-muted" }, ["No city selected."]));
    return;
  }

  const { data, error } = await api.GET("/v0/city/{cityName}/status", {
    params: { path: { cityName: city } },
  });
  if (error || !data) {
    clear(container);
    container.append(el("div", { class: "banner-error" }, [`Status unavailable for ${city}`]));
    return;
  }

  clear(container);
  const state = data.suspended ? "suspended" : "running";
  const banner = el("div", { class: "summary-banner" }, [
    el("div", { class: "summary-city" }, [`City: ${data.name ?? city}`]),
    el("div", { class: "summary-state" }, [`State: ${state}`]),
    el("div", { class: "summary-counts" }, [
      `${data.agents?.running ?? 0}/${data.agents?.total ?? 0} agents running`,
      " · ",
      `${data.rigs?.total ?? 0} rig${data.rigs?.total === 1 ? "" : "s"}`,
      " · ",
      `${data.mail?.unread ?? 0} unread mail`,
    ]),
  ]);
  container.append(banner);
}
