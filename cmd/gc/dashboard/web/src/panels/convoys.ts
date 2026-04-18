import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { calculateActivity, statusBadgeClass } from "../util/legacy";
import { popPause, pushPause, showToast } from "../ui";

interface ConvoyRow {
  id: string;
  title?: string;
  status?: string;
  progressPct: number;
  total: number;
  closed: number;
  ready: number;
  inProgress: number;
  assignees: string[];
  lastActivity: ReturnType<typeof calculateActivity>;
}

let currentConvoyID = "";

export async function renderConvoys(): Promise<void> {
  const city = cityScope();
  const container = byId("convoy-list");
  if (!container) return;
  if (!city) {
    resetConvoysNoCity();
    return;
  }

  const listR = await api.GET("/v0/city/{cityName}/convoys", {
    params: { path: { cityName: city }, query: { limit: 200 } },
  });
  if (listR.error || !listR.data?.items) {
    clear(container);
    container.append(el("div", { class: "panel-error" }, ["Could not load convoys."]));
    return;
  }

  const rows = await Promise.all(
    listR.data.items.map(async (convoy) => buildConvoyRow(city, convoy.id ?? "")),
  );
  const filtered = rows.filter((row): row is ConvoyRow => row !== null);
  byId("convoy-count")!.textContent = String(filtered.length);

  clear(container);
  if (filtered.length === 0) {
    container.append(el("div", { class: "empty-state" }, [el("p", {}, ["No active convoys"])]));
    return;
  }

  const tbody = el("tbody");
  filtered.forEach((row) => {
    const tr = el("tr", { class: "convoy-row", "data-convoy-id": row.id }, [
      el("td", {}, [
        el("span", { class: `badge ${statusBadgeClass(convoyState(row))}` }, [convoyLabel(row)]),
      ]),
      el("td", {}, [
        el("span", { class: "convoy-id" }, [row.id]),
        row.title ? el("div", { class: "convoy-title" }, [row.title]) : null,
        row.assignees.length
          ? el("div", { class: "convoy-assignees" }, row.assignees.map((assignee) => el("span", { class: "assignee-chip" }, [assignee])))
          : null,
      ]),
      el("td", { class: "convoy-progress-cell" }, [
        el("div", { class: "convoy-progress-header" }, [
          el("span", { class: "convoy-progress-fraction" }, [`${row.closed}/${row.total}`]),
          row.total > 0 ? el("span", { class: "convoy-progress-pct" }, [`${row.progressPct}%`]) : null,
        ]),
        row.total > 0
          ? el("div", { class: "progress-bar" }, [el("div", { class: "progress-fill", style: `width: ${row.progressPct}%;` })])
          : null,
      ]),
      el("td", { class: "convoy-work-cell" }, [
        el("div", { class: "convoy-work-breakdown" }, [
          row.ready > 0 ? el("span", { class: "work-chip work-ready" }, [`${row.ready} ready`]) : null,
          row.inProgress > 0 ? el("span", { class: "work-chip work-inprogress" }, [`${row.inProgress} active`]) : null,
          row.closed === row.total && row.total > 0 ? el("span", { class: "work-chip work-done" }, ["all done"]) : null,
        ]),
      ]),
      el("td", { class: `activity-${row.lastActivity.colorClass}` }, [
        el("span", { class: "activity-dot" }),
        ` ${row.lastActivity.display}`,
      ]),
    ]);
    tr.addEventListener("click", () => {
      void openConvoyDetail(row.id);
    });
    tbody.append(tr);
  });

  container.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Status"]),
      el("th", {}, ["Convoy"]),
      el("th", {}, ["Progress"]),
      el("th", {}, ["Work"]),
      el("th", {}, ["Activity"]),
    ])]),
    tbody,
  ]));
}

function resetConvoysNoCity(): void {
  const container = byId("convoy-list");
  const detail = byId("convoy-detail");
  const create = byId("convoy-create-form");
  if (!container || !detail || !create) return;

  const hadSubview = detail.style.display === "block" || create.style.display === "block";
  currentConvoyID = "";
  byId("convoy-count")!.textContent = "0";
  detail.style.display = "none";
  create.style.display = "none";
  byId("convoy-add-issue-form")!.style.display = "none";
  container.style.display = "block";
  clear(container);
  container.append(el("div", { class: "empty-state" }, [el("p", {}, ["Select a city to view convoys"])]));
  if (hadSubview) popPause();
}

async function buildConvoyRow(city: string, convoyID: string): Promise<ConvoyRow | null> {
  if (!convoyID) return null;
  const detail = await api.GET("/v0/city/{cityName}/convoy/{id}", {
    params: { path: { cityName: city, id: convoyID } },
  });
  if (detail.error || !detail.data) return null;

  const children = detail.data.children ?? [];
  const assignees = new Set<string>();
  let ready = 0;
  let inProgress = 0;
  let latest = "";
  children.forEach((child) => {
    if ((child.status ?? "").toLowerCase() !== "closed") {
      if (child.assignee) {
        inProgress += 1;
        assignees.add(child.assignee);
      } else {
        ready += 1;
      }
    }
    latest = [latest, child.created_at ?? ""].sort().slice(-1)[0] ?? latest;
  });
  const total = detail.data.progress?.total ?? children.length;
  const closed = detail.data.progress?.closed ?? children.filter((child) => child.status === "closed").length;
  return {
    id: convoyID,
    title: detail.data.convoy?.title ?? convoyID,
    status: detail.data.convoy?.status,
    progressPct: total > 0 ? Math.round((closed / total) * 100) : 0,
    total,
    closed,
    ready,
    inProgress,
    assignees: [...assignees].sort(),
    lastActivity: calculateActivity(latest),
  };
}

function convoyState(row: ConvoyRow): string {
  if (row.total > 0 && row.closed === row.total) return "done";
  if (row.inProgress > 0) return "active";
  if (row.ready > 0) return "waiting";
  return row.status ?? "open";
}

function convoyLabel(row: ConvoyRow): string {
  switch (convoyState(row)) {
    case "done":
      return "✓ Done";
    case "active":
      return "Active";
    case "waiting":
      return "Waiting";
    default:
      return row.status ?? "Open";
  }
}

export function installConvoyInteractions(): void {
  byId("new-convoy-btn")?.addEventListener("click", () => {
    openConvoyCreate();
  });
  byId("convoy-back-btn")?.addEventListener("click", () => closeConvoyDetail());
  byId("convoy-create-back-btn")?.addEventListener("click", () => closeConvoyCreate());
  byId("convoy-create-cancel-btn")?.addEventListener("click", () => closeConvoyCreate());
  byId("convoy-create-submit-btn")?.addEventListener("click", () => {
    void createConvoy();
  });
  byId("convoy-add-issue-btn")?.addEventListener("click", () => {
    byId("convoy-add-issue-form")!.style.display = "flex";
  });
  byId("convoy-add-issue-cancel")?.addEventListener("click", () => {
    byId("convoy-add-issue-form")!.style.display = "none";
  });
  byId("convoy-add-issue-submit")?.addEventListener("click", () => {
    void addIssueToConvoy();
  });
}

export function openConvoyCreate(): void {
  if (!cityScope()) {
    showToast("info", "No city selected", "Select a city to create a convoy");
    return;
  }
  const create = byId("convoy-create-form");
  const wasOpen = create?.style.display === "block";
  currentConvoyID = "";
  byId("convoy-list")!.style.display = "none";
  byId("convoy-detail")!.style.display = "none";
  create!.style.display = "block";
  byId<HTMLInputElement>("convoy-create-name")!.value = "";
  byId<HTMLInputElement>("convoy-create-issues")!.value = "";
  if (!wasOpen) pushPause();
  revealConvoyPanel("convoy-create-name");
  byId<HTMLInputElement>("convoy-create-name")?.focus();
}

async function openConvoyDetail(convoyID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  currentConvoyID = convoyID;
  if (byId("convoy-detail")?.style.display !== "block") pushPause();
  byId("convoy-list")!.style.display = "none";
  byId("convoy-create-form")!.style.display = "none";
  byId("convoy-detail")!.style.display = "block";
  revealConvoyPanel("convoy-detail");
  byId("convoy-detail-id")!.textContent = convoyID;
  byId("convoy-detail-title")!.textContent = `Convoy: ${convoyID}`;
  byId("convoy-issues-loading")!.style.display = "block";
  byId("convoy-issues-table")!.style.display = "none";
  byId("convoy-issues-empty")!.style.display = "none";
  byId("convoy-add-issue-form")!.style.display = "none";

  const detail = await api.GET("/v0/city/{cityName}/convoy/{id}", {
    params: { path: { cityName: city, id: convoyID } },
  });
  byId("convoy-issues-loading")!.style.display = "none";
  if (detail.error || !detail.data) {
    byId("convoy-issues-empty")!.style.display = "block";
    byId("convoy-issues-empty")!.querySelector("p")!.textContent = detail.error?.detail ?? "Failed to load convoy";
    return;
  }

  const total = detail.data.progress?.total ?? detail.data.children?.length ?? 0;
  const closed = detail.data.progress?.closed ?? detail.data.children?.filter((child) => child.status === "closed").length ?? 0;
  byId("convoy-detail-status")!.className = `badge ${statusBadgeClass(detail.data.convoy?.status ?? "open")}`;
  byId("convoy-detail-status")!.textContent = detail.data.convoy?.status ?? "open";
  byId("convoy-detail-progress")!.textContent = `${closed}/${total}`;

  const tbody = byId("convoy-issues-tbody");
  if (!tbody) return;
  clear(tbody);
  const children = detail.data.children ?? [];
  if (children.length === 0) {
    byId("convoy-issues-empty")!.style.display = "block";
    return;
  }
  children.forEach((child) => {
    const progress = child.assignee ? child.assignee : child.status === "closed" ? "done" : "ready";
    tbody.append(el("tr", {}, [
      el("td", { class: "convoy-issue-status" }, [el("span", { class: `badge ${statusBadgeClass(child.status)}` }, [child.status ?? "unknown"])]),
      el("td", {}, [el("span", { class: "issue-id" }, [child.id ?? ""])]),
      el("td", { class: "issue-title" }, [child.title ?? child.id ?? ""]),
      el("td", {}, [child.assignee ? el("span", { class: "badge badge-blue" }, [child.assignee]) : el("span", { class: "badge badge-muted" }, ["Unassigned"])]),
      el("td", {}, [progress]),
    ]));
  });
  byId("convoy-issues-table")!.style.display = "table";
}

function closeConvoyDetail(): void {
  const detail = byId("convoy-detail");
  const wasOpen = detail?.style.display === "block";
  detail!.style.display = "none";
  byId("convoy-list")!.style.display = "block";
  if (wasOpen) popPause();
}

function closeConvoyCreate(): void {
  const create = byId("convoy-create-form");
  const wasOpen = create?.style.display === "block";
  create!.style.display = "none";
  byId("convoy-list")!.style.display = "block";
  if (wasOpen) popPause();
}

async function createConvoy(): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const title = byId<HTMLInputElement>("convoy-create-name")?.value.trim() ?? "";
  const items = (byId<HTMLInputElement>("convoy-create-issues")?.value ?? "")
    .split(/\s+/)
    .map((item) => item.trim())
    .filter(Boolean);
  if (!title) {
    showToast("error", "Missing name", "Convoy name is required");
    return;
  }
  const res = await api.POST("/v0/city/{cityName}/convoys", {
    params: { path: { cityName: city } },
    body: { title, items },
  });
  if (res.error) {
    showToast("error", "Create failed", res.error.detail ?? "Could not create convoy");
    return;
  }
  showToast("success", "Convoy created", title);
  closeConvoyCreate();
  await renderConvoys();
}

async function addIssueToConvoy(): Promise<void> {
  const city = cityScope();
  if (!city || !currentConvoyID) return;
  const input = byId<HTMLInputElement>("convoy-add-issue-input");
  const item = input?.value.trim() ?? "";
  if (!item) return;
  const res = await api.POST("/v0/city/{cityName}/convoy/{id}/add", {
    params: { path: { cityName: city, id: currentConvoyID } },
    body: { items: [item] },
  });
  if (res.error) {
    showToast("error", "Add failed", res.error.detail ?? "Could not add issue");
    return;
  }
  if (input) input.value = "";
  byId("convoy-add-issue-form")!.style.display = "none";
  showToast("success", "Issue added", item);
  await openConvoyDetail(currentConvoyID);
  await renderConvoys();
}

function revealConvoyPanel(focusID: string): void {
  byId("convoy-panel")?.scrollIntoView?.({ behavior: "smooth", block: "center" });
  window.setTimeout(() => {
    byId<HTMLElement>(focusID)?.focus();
  }, 0);
}
