import type { BeadRecord } from "../api";
import { api, cityScope, mutationHeaders } from "../api";
import { promptActionDialog } from "../modals";
import { byId, clear, el } from "../util/dom";
import { beadPriority, formatTimestamp, priorityBadgeClass, truncate } from "../util/legacy";
import { getOptions } from "./options";
import { popPause, pushPause, showToast } from "../ui";

let allIssues: BeadRecord[] = [];
let currentTab: "ready" | "progress" | "all" = "ready";
let currentRig = "all";
let currentIssueID = "";

export async function renderIssues(): Promise<void> {
  const city = cityScope();
  const issuesList = byId("issues-list");
  if (!issuesList) return;
  if (!city) {
    resetIssuesNoCity();
    return;
  }

  const [openR, progressR, options] = await Promise.all([
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "open", limit: 500 } },
    }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "in_progress", limit: 500 } },
    }),
    getOptions(),
  ]);
  if ((openR.error && progressR.error) || (!openR.data?.items && !progressR.data?.items)) {
    clear(issuesList);
    issuesList.append(el("div", { class: "panel-error" }, ["Could not load beads."]));
    return;
  }

  allIssues = sortIssues(
    [...(openR.data?.items ?? []), ...(progressR.data?.items ?? [])]
      .filter((bead) => !isInternalBead(bead)),
  );
  byId("issues-count")!.textContent = String(allIssues.length);

  const rigTabs = byId("rig-filter-tabs");
  if (rigTabs) {
    clear(rigTabs);
    rigTabs.append(rigButton("all", currentRig === "all"));
    options.rigs.forEach((rig) => rigTabs.append(rigButton(rig, currentRig === rig)));
  }

  renderIssueTable();
}

function resetIssuesNoCity(): void {
  const issuesList = byId("issues-list");
  const rigTabs = byId("rig-filter-tabs");
  const detail = byId("issue-detail");
  if (!issuesList || !rigTabs || !detail) return;

  closeIssueModal();
  const detailOpen = detail.style.display === "block";
  detail.style.display = "none";
  issuesList.style.display = "block";
  clear(issuesList);
  issuesList.append(el("div", { class: "empty-state" }, [el("p", {}, ["Select a city to view beads"])]));
  clear(rigTabs);
  currentRig = "all";
  currentIssueID = "";
  allIssues = [];
  rigTabs.append(rigButton("all", true));
  byId("issues-count")!.textContent = "0";
  if (detailOpen) popPause();
}

function renderIssueTable(): void {
  const container = byId("issues-list");
  if (!container) return;
  clear(container);

  const filtered = allIssues.filter((issue) => {
    const state = issue.assignee ? "progress" : "ready";
    const matchesTab = currentTab === "all" || currentTab === state;
    const matchesRig = currentRig === "all" || inferRig(issue) === currentRig;
    return matchesTab && matchesRig;
  });

  if (filtered.length === 0) {
    container.append(el("div", { class: "empty-state" }, [el("p", {}, ["No beads"])]));
    return;
  }

  const tbody = el("tbody");
  filtered.forEach((issue) => {
    const row = el("tr", {
      class: `issue-row priority-${beadPriority(issue.priority)}`,
      "data-issue-id": issue.id ?? "",
      "data-status": issue.assignee ? "progress" : "ready",
      "data-rig": inferRig(issue),
    }, [
      el("td", {}, [el("span", { class: `badge ${priorityBadgeClass(issue.priority)}` }, [`P${beadPriority(issue.priority)}`])]),
      el("td", {}, [el("span", { class: "issue-id" }, [issue.id ?? ""])]),
      el("td", { class: "issue-title" }, [truncate(issue.title ?? issue.id ?? "", 80)]),
      el("td", { class: "issue-rig" }, [inferRig(issue)]),
      el("td", { class: "issue-status" }, [
        issue.assignee
          ? el("span", { class: "badge badge-blue", title: issue.assignee }, [issue.assignee])
          : el("span", { class: "badge badge-green" }, ["Ready"]),
      ]),
      el("td", { class: "issue-age" }, [formatTimestamp(issue.created_at)]),
      el("td", {}, [slingButton(issue.id ?? "")]),
    ]);
    row.addEventListener("click", (event) => {
      const target = event.target as HTMLElement;
      if (target.closest(".sling-btn")) return;
      if (issue.id) void openIssueDetail(issue.id);
    });
    tbody.append(row);
  });

  container.append(el("table", { id: "work-table" }, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Pri"]),
      el("th", {}, ["ID"]),
      el("th", {}, ["Title"]),
      el("th", {}, ["Rig"]),
      el("th", {}, ["Status"]),
      el("th", {}, ["Age"]),
      el("th", {}, ["Actions"]),
    ])]),
    tbody,
  ]));
}

function rigButton(rig: string, active: boolean): HTMLElement {
  const btn = el("button", { class: `rig-btn${active ? " active" : ""}`, "data-rig": rig }, [rig === "all" ? "All" : rig]);
  btn.addEventListener("click", () => {
    currentRig = rig;
    document.querySelectorAll(".rig-btn").forEach((node) => node.classList.remove("active"));
    btn.classList.add("active");
    renderIssueTable();
  });
  return btn;
}

function inferRig(issue: BeadRecord): string {
  return issue.id?.split("-")[0] ?? "city";
}

function isInternalBead(issue: BeadRecord): boolean {
  if ((issue.issue_type ?? "").toLowerCase() === "convoy") return true;
  return (issue.labels ?? []).some((label) => label.startsWith("gc:queue") || label.startsWith("gc:message"));
}

function sortIssues(issues: BeadRecord[]): BeadRecord[] {
  return [...issues].sort((a, b) => {
    const pa = beadPriority(a.priority);
    const pb = beadPriority(b.priority);
    if (pa !== pb) return pa - pb;
    return (b.created_at ?? "").localeCompare(a.created_at ?? "");
  });
}

export function installIssueInteractions(): void {
  document.querySelectorAll(".tab-btn").forEach((node) => {
    node.addEventListener("click", (event) => {
      const button = event.currentTarget as HTMLElement;
      currentTab = (button.dataset.tab as typeof currentTab) ?? "ready";
      document.querySelectorAll(".tab-btn").forEach((btn) => btn.classList.remove("active"));
      button.classList.add("active");
      renderIssueTable();
    });
  });

  byId("new-issue-btn")?.addEventListener("click", () => openIssueModal());
  byId("issue-modal-close-btn")?.addEventListener("click", () => closeIssueModal());
  byId("issue-modal-cancel-btn")?.addEventListener("click", () => closeIssueModal());
  byId("issue-modal")?.querySelector(".modal-backdrop")?.addEventListener("click", () => closeIssueModal());
  byId("issue-form")?.addEventListener("submit", (event) => {
    event.preventDefault();
    void createIssueFromModal();
  });
  byId("issue-back-btn")?.addEventListener("click", () => closeIssueDetail());
  document.addEventListener("keydown", (event) => {
    if (event.key !== "Escape") return;
    if (byId("issue-modal")?.style.display === "block") {
      closeIssueModal();
    }
  });
}

export function openIssueModal(): void {
  if (!cityScope()) {
    showToast("info", "No city selected", "Select a city to create a bead");
    return;
  }
  const modal = byId("issue-modal");
  if (!modal) return;
  if (modal.style.display !== "block") pushPause();
  modal.style.display = "block";
  byId("issues-panel")?.scrollIntoView?.({ behavior: "smooth", block: "center" });
  byId<HTMLInputElement>("issue-title")?.focus();
}

export function closeIssueModal(): void {
  const modal = byId("issue-modal");
  if (!modal) return;
  const wasOpen = modal.style.display === "block";
  modal.style.display = "none";
  (byId("issue-form") as HTMLFormElement | null)?.reset();
  if (wasOpen) popPause();
}

async function createIssueFromModal(): Promise<void> {
  const title = byId<HTMLInputElement>("issue-title")?.value.trim() ?? "";
  const description = byId<HTMLTextAreaElement>("issue-description")?.value.trim() ?? "";
  const priority = Number(byId<HTMLSelectElement>("issue-priority")?.value ?? "2");
  if (!title) return;
  const result = await createIssue({ title, description, priority });
  if (!result.ok) {
    showToast("error", "Create failed", result.error ?? "Could not create issue");
    return;
  }
  showToast("success", "Issue created", title);
  closeIssueModal();
  await renderIssues();
}

async function openIssueDetail(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  currentIssueID = issueID;
  if (byId("issue-detail")?.style.display !== "block") pushPause();
  byId("issues-list")!.style.display = "none";
  byId("issue-detail")!.style.display = "block";

  const [issueR, depsR, options] = await Promise.all([
    api.GET("/v0/city/{cityName}/bead/{id}", { params: { path: { cityName: city, id: issueID } } }),
    api.GET("/v0/city/{cityName}/bead/{id}/deps", { params: { path: { cityName: city, id: issueID } } }),
    getOptions(),
  ]);
  if (issueR.error || !issueR.data) {
    showToast("error", "Issue failed", issueR.error?.detail ?? "Could not load bead");
    return;
  }
  const issue = issueR.data;
  byId("issue-detail-id")!.textContent = issue.id ?? issueID;
  byId("issue-detail-title-text")!.textContent = issue.title ?? issueID;
  byId("issue-detail-description")!.textContent = issue.description || "(no description)";
  const pri = byId("issue-detail-priority")!;
  pri.className = `badge ${priorityBadgeClass(issue.priority)}`;
  pri.textContent = `P${beadPriority(issue.priority)}`;
  byId("issue-detail-status")!.textContent = issue.status ?? "open";
  byId("issue-detail-status")!.className = `issue-status ${issue.status ?? "open"}`;
  byId("issue-detail-type")!.textContent = issue.issue_type ? `Type: ${issue.issue_type}` : "";
  byId("issue-detail-owner")!.textContent = issue.assignee ? `Owner: ${issue.assignee}` : "Owner: unassigned";
  byId("issue-detail-created")!.textContent = issue.created_at ? `Created: ${formatTimestamp(issue.created_at)}` : "";

  renderIssueActions(issue, options.agents);
  renderDependencies(depsR.data?.children ?? []);
}

function renderDependencies(children: BeadRecord[]): void {
  const depsSection = byId("issue-detail-deps");
  const dependsOn = byId("issue-detail-depends-on");
  const blocksSection = byId("issue-detail-blocks-section");
  const blocks = byId("issue-detail-blocks");
  if (!depsSection || !dependsOn || !blocksSection || !blocks) return;
  clear(dependsOn);
  clear(blocks);
  if (children.length === 0) {
    depsSection.style.display = "none";
    blocksSection.style.display = "none";
    return;
  }
  depsSection.style.display = "block";
  children.forEach((child) => {
    const pill = el("span", { class: "issue-dep-item", "data-issue-id": child.id ?? "" }, [`→ ${child.id ?? ""}`]);
    pill.addEventListener("click", () => {
      if (child.id) void openIssueDetail(child.id);
    });
    dependsOn.append(pill);
  });
  blocksSection.style.display = "none";
}

function renderIssueActions(issue: BeadRecord, agents: string[]): void {
  const actions = byId("issue-detail-actions");
  if (!actions || !issue.id) return;
  clear(actions);

  const bar = el("div", { class: "issue-actions-bar" });
  const primary = issue.status === "closed"
    ? actionButton("↺ Reopen", "reopen", () => void reopenIssue(issue.id!))
    : actionButton("✓ Close", "close", () => void closeIssue(issue.id!));
  bar.append(primary);
  if (issue.status !== "closed") {
    bar.append(actionButton("🚚 Sling", "sling", () => void slingIssue(issue.id!)));
  }

  const priorityGroup = el("div", { class: "issue-action-group" }, [
    el("label", { class: "issue-action-label" }, ["Priority"]),
    prioritySelect(issue.id, issue.priority),
  ]);
  const assigneeGroup = el("div", { class: "issue-action-group" }, [
    el("label", { class: "issue-action-label" }, ["Assign"]),
    assigneeSelect(issue.id, issue.assignee, agents),
  ]);

  actions.append(bar, priorityGroup, assigneeGroup);
}

function actionButton(label: string, klass: string, onClick: () => void): HTMLElement {
  const button = el("button", { class: `issue-action-btn ${klass}`, type: "button" }, [label]);
  button.addEventListener("click", onClick);
  return button;
}

function prioritySelect(issueID: string, current: number | undefined): HTMLElement {
  const select = el("select", { class: "issue-action-select", id: "issue-action-priority", "aria-label": "Priority" }) as HTMLSelectElement;
  [1, 2, 3, 4].forEach((priority) => {
    const option = el("option", { value: priority, selected: beadPriority(current) === priority }, [`P${priority}`]) as HTMLOptionElement;
    select.append(option);
  });
  select.addEventListener("change", () => {
    void updateIssuePriority(issueID, Number(select.value));
  });
  return select;
}

function assigneeSelect(issueID: string, current: string | undefined, agents: string[]): HTMLElement {
  const select = el("select", { class: "issue-action-select", id: "issue-action-assignee", "aria-label": "Assignee" }) as HTMLSelectElement;
  select.append(el("option", { value: "" }, ["Unassigned"]));
  agents.forEach((agent) => {
    select.append(el("option", { value: agent, selected: current === agent }, [agent]));
  });
  select.addEventListener("change", () => {
    void assignIssue(issueID, select.value);
  });
  return select;
}

function closeIssueDetail(): void {
  const detail = byId("issue-detail");
  const wasOpen = detail?.style.display === "block";
  detail!.style.display = "none";
  byId("issues-list")!.style.display = "block";
  currentIssueID = "";
  if (wasOpen) popPause();
}

async function closeIssue(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/close", {
    params: { path: { cityName: city, id: issueID }, header: mutationHeaders },
  });
  if (res.error) {
    showToast("error", "Close failed", res.error.detail ?? "Could not close issue");
    return;
  }
  showToast("success", "Closed", issueID);
  await renderIssues();
  await openIssueDetail(issueID);
}

async function reopenIssue(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/reopen", {
    params: { path: { cityName: city, id: issueID }, header: mutationHeaders },
  });
  if (res.error) {
    showToast("error", "Reopen failed", res.error.detail ?? "Could not reopen issue");
    return;
  }
  showToast("success", "Reopened", issueID);
  await renderIssues();
  await openIssueDetail(issueID);
}

async function updateIssuePriority(issueID: string, priority: number): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/update", {
    params: { path: { cityName: city, id: issueID }, header: mutationHeaders },
    body: { priority },
  });
  if (res.error) {
    showToast("error", "Priority failed", res.error.detail ?? "Could not update priority");
    return;
  }
  showToast("success", "Priority updated", `${issueID} → P${priority}`);
  await renderIssues();
  await openIssueDetail(issueID);
}

async function assignIssue(issueID: string, assignee: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/assign", {
    params: { path: { cityName: city, id: issueID }, header: mutationHeaders },
    body: { assignee },
  });
  if (res.error) {
    showToast("error", "Assign failed", res.error.detail ?? "Could not update assignee");
    return;
  }
  showToast("success", "Assignment updated", assignee || "Unassigned");
  await renderIssues();
  await openIssueDetail(issueID);
}

async function slingIssue(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const selection = await promptActionDialog({
    beadID: issueID,
    beadLabel: issueID,
    mode: "sling",
    title: "Sling Bead",
  });
  if (!selection) return;
  const res = await api.POST("/v0/city/{cityName}/sling", {
    params: { path: { cityName: city }, header: mutationHeaders },
    body: { bead: issueID, target: selection.target, rig: selection.rig || undefined },
  });
  if (res.error) {
    showToast("error", "Sling failed", res.error.detail ?? "Could not sling issue");
    return;
  }
  showToast("success", "Work assigned", `${issueID} → ${selection.target}`);
  await renderIssues();
  if (currentIssueID === issueID) {
    await openIssueDetail(issueID);
  }
}

function slingButton(issueID: string): HTMLElement {
  const btn = el("button", { class: "sling-btn", type: "button", "data-bead-id": issueID }, ["Sling"]);
  btn.addEventListener("click", (event) => {
    event.stopPropagation();
    void slingIssue(issueID);
  });
  return btn;
}

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
    params: { path: { cityName: city }, header: mutationHeaders },
    body: {
      title: input.title,
      description: input.description,
      rig: input.rig,
      priority: input.priority,
      assignee: input.assignee,
    },
  });
  if (error) return { ok: false, error: error.detail ?? error.title ?? "create failed" };
  return { ok: true };
}
