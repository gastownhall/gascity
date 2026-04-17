// Convoys panel: list of open convoys with progress metadata.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";

export async function renderConvoys(): Promise<void> {
  const container = byId("convoys-panel");
  if (!container) return;
  const city = cityScope();
  if (!city) {
    clear(container);
    return;
  }

  const { data, error } = await api.GET("/v0/city/{cityName}/convoys", {
    params: { path: { cityName: city } },
  });
  if (error || !data?.items) {
    clear(container);
    container.append(el("div", { class: "panel-error" }, ["Could not load convoys."]));
    return;
  }

  clear(container);
  const list = el("div", { class: "convoys-list" });
  for (const convoy of data.items) {
    list.append(el(
      "div",
      { class: "convoy-row" },
      [
        el("div", { class: "convoy-title" }, [convoy.title ?? convoy.id ?? ""]),
        el("div", { class: "convoy-meta" }, [
          el("span", { class: "badge badge-muted" }, [convoy.status ?? "open"]),
          convoy.issue_type ? el("span", { class: "badge badge-muted" }, [convoy.issue_type]) : null,
        ]),
      ],
    ));
  }
  if (data.items.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["No open convoys."]));
  }
  container.append(list);
}
