// Issues panel: all open beads (assigned + unassigned), grouped by
// priority. Mirrors the Go `/api/issues/show` + `/api/issues/create`
// flow via typed supervisor endpoints.

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";

export async function renderIssues(): Promise<void> {
  const container = byId("issues-panel");
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
    container.append(el("div", { class: "panel-error" }, ["Could not load issues."]));
    return;
  }

  const items = [...data.items];
  items.sort((a, b) => (a.priority ?? 99) - (b.priority ?? 99));

  clear(container);
  const list = el("div", { class: "issues-list" });
  for (const bead of items) {
    const highPriority = (bead.priority ?? 99) <= 2;
    list.append(el(
      "div",
      { class: `issue-row${highPriority ? " high-priority" : ""}` },
      [
        el("span", { class: `badge badge-p${bead.priority ?? 0}` }, [`P${bead.priority ?? "?"}`]),
        el("span", { class: "issue-title" }, [bead.title ?? bead.id ?? ""]),
        bead.assignee ? el("span", { class: "badge badge-assignee" }, [bead.assignee]) : null,
      ],
    ));
  }
  if (items.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["No open issues."]));
  }
  container.append(list);
}

// Create a new bead. Called by the issue-create form's submit handler.
export async function createIssue(input: {
  title: string;
  description?: string;
  rig?: string;
  priority?: number;
  assignee?: string;
}): Promise<{ ok: boolean; error?: string }> {
  const city = cityScope();
  if (!city) return { ok: false, error: "no city selected" };
  const { error } = await api.POST("/v0/city/{cityName}/beads", {
    params: { path: { cityName: city } },
    body: {
      title: input.title,
      description: input.description,
      rig: input.rig,
      priority: input.priority,
      assignee: input.assignee,
    },
  });
  if (error) {
    return { ok: false, error: error.detail ?? error.title ?? "create failed" };
  }
  return { ok: true };
}
