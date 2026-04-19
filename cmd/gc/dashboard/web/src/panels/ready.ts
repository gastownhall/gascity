// Ready panel: beads that are open + unassigned, prioritized by
// bead priority. The Go `/api/ready` endpoint queried beads with a
// filter and grouped by priority; the SPA does the same against
// the supervisor directly.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";

export async function renderReady(): Promise<void> {
  const container = byId("ready-panel");
  if (!container) return;
  const city = cityScope();
  if (!city) {
    clear(container);
    return;
  }

  const { data, error } = await api.GET("/v0/city/{cityName}/beads", {
    params: { path: { cityName: city }, query: { status: "open" } },
  });
  if (error || !data?.items) {
    clear(container);
    container.append(el("div", { class: "panel-error" }, ["Could not load ready queue."]));
    return;
  }

  const unassigned = data.items.filter((b) => !b.assignee || b.assignee === "");
  // Sort by priority asc (P1 > P2 > P3 > unspecified), then recency desc.
  unassigned.sort((a, b) => {
    const pa = a.priority ?? 99;
    const pb = b.priority ?? 99;
    if (pa !== pb) return pa - pb;
    return (b.created_at ?? "").localeCompare(a.created_at ?? "");
  });

  clear(container);
  const list = el("div", { class: "ready-list" });
  for (const bead of unassigned) {
    const priority = bead.priority ?? 0;
    list.append(el(
      "div",
      { class: `ready-row ready-p${priority}` },
      [
        el("span", { class: `badge badge-p${priority}` }, [`P${priority || "?"}`]),
        el("span", { class: "ready-title" }, [bead.title ?? bead.id ?? ""]),
        bead.issue_type ? el("span", { class: "badge badge-muted" }, [bead.issue_type]) : null,
      ],
    ));
  }
  if (unassigned.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["Queue empty."]));
  }
  container.append(list);
}
