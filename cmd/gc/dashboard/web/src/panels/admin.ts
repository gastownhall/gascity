import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { formatAgentAddress, formatTimestamp, statusBadgeClass, truncate } from "../util/legacy";
import { getOptions } from "./options";
import { showToast } from "../ui";

interface ServiceStatus {
  kind?: string;
  local_state: string;
  publication_state: string;
  service_name: string;
  state?: string;
}

interface RigRecord {
  agent_count: number;
  git?: { branch?: string; dirty?: boolean };
  last_activity?: string;
  name: string;
  running_count: number;
  suspended: boolean;
}

interface BeadRecord {
  assignee?: string;
  created_at?: string;
  description?: string;
  id?: string;
  issue_type?: string;
  labels?: string[] | null;
  priority?: number;
  status?: string;
  title?: string;
}

export async function renderAdminPanels(): Promise<void> {
  const city = cityScope();
  if (!city) {
    renderAdminEmptyStates();
    return;
  }

  const [servicesR, rigsR, escalationsR, assignedR, queuesR] = await Promise.all([
    api.GET("/v0/city/{cityName}/services", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/rigs", { params: { path: { cityName: city }, query: { git: "true" } } }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { label: "gc:escalation", status: "open", limit: 200 } },
    }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "in_progress", limit: 500 } },
    }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { label: "gc:queue", limit: 200 } },
    }),
  ]);

  renderServices(servicesR.data?.items as ServiceStatus[] | null, servicesR.error?.detail);
  renderRigs(rigsR.data?.items as RigRecord[] | null);
  renderEscalations(escalationsR.data?.items as BeadRecord[] | null);
  renderAssigned(assignedR.data?.items as BeadRecord[] | null);
  renderQueues(queuesR.data?.items as BeadRecord[] | null);
}

function renderAdminEmptyStates(): void {
  renderEmptyBody("services-body", "services-count", "Select a city to view services");
  renderEmptyBody("rigs-body", "rigs-count", "Select a city to view rigs");
  renderEmptyBody("escalations-body", "escalations-count", "Select a city to view escalations");
  renderEmptyBody("assigned-body", "assigned-count", "Select a city to view assigned work");
  renderEmptyBody("queues-body", "queues-count", "Select a city to view queues");
  byId("assign-form")!.style.display = "none";
  byId("clear-assigned-btn")!.style.display = "none";
}

export function installAdminInteractions(): void {
  byId("open-assign-btn")?.addEventListener("click", () => {
    byId("assign-form")!.style.display = "block";
    byId<HTMLInputElement>("assign-bead")?.focus();
  });
  byId("assign-cancel-btn")?.addEventListener("click", () => {
    byId("assign-form")!.style.display = "none";
  });
  byId("assign-submit-btn")?.addEventListener("click", () => {
    void assignFromPanel();
  });
  byId("clear-assigned-btn")?.addEventListener("click", () => {
    void clearAllAssigned();
  });
  byId<HTMLInputElement>("assign-bead")?.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      void assignFromPanel();
      return;
    }
    if (event.key === "Escape") {
      event.preventDefault();
      byId("assign-form")!.style.display = "none";
    }
  });
}

function renderServices(items: ServiceStatus[] | null, error?: string): void {
  const body = byId("services-body");
  const count = byId("services-count");
  if (!body || !count) return;
  clear(body);

  if (error) {
    count.textContent = "n/a";
    body.append(el("div", { class: "empty-state" }, [el("p", {}, [error])]));
    return;
  }
  const services = items ?? [];
  count.textContent = String(services.length);
  if (services.length === 0) {
    body.append(el("div", { class: "empty-state" }, [el("p", {}, ["No workspace services"])]));
    return;
  }

  const tbody = el("tbody");
  services.forEach((svc) => {
    const restart = el("button", { class: "esc-btn", type: "button" }, ["Restart"]);
    restart.addEventListener("click", () => {
      void restartService(svc.service_name);
    });
    tbody.append(el("tr", {}, [
      el("td", {}, [el("strong", {}, [svc.service_name])]),
      el("td", {}, [svc.kind ?? "—"]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(svc.state ?? svc.publication_state)}` }, [svc.state ?? svc.publication_state ?? "unknown"])]),
      el("td", {}, [svc.local_state]),
      el("td", {}, [restart]),
    ]));
  });
  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Name"]),
      el("th", {}, ["Kind"]),
      el("th", {}, ["Service"]),
      el("th", {}, ["Local"]),
      el("th", {}, ["Actions"]),
    ])]),
    tbody,
  ]));
}

function renderRigs(items: RigRecord[] | null): void {
  const body = byId("rigs-body");
  const count = byId("rigs-count");
  if (!body || !count) return;
  clear(body);
  const rigs = items ?? [];
  count.textContent = String(rigs.length);
  if (rigs.length === 0) {
    body.append(el("div", { class: "empty-state" }, [el("p", {}, ["No rigs configured"])]));
    return;
  }

  const tbody = el("tbody");
  rigs.forEach((rig) => {
    const suspendResume = el("button", { class: "esc-btn", type: "button" }, [rig.suspended ? "Resume" : "Suspend"]);
    suspendResume.addEventListener("click", () => {
      void rigAction(rig.name, rig.suspended ? "resume" : "suspend");
    });
    const restart = el("button", { class: "esc-btn", type: "button" }, ["Restart"]);
    restart.addEventListener("click", () => {
      void rigAction(rig.name, "restart");
    });
    tbody.append(el("tr", {}, [
      el("td", {}, [el("span", { class: "rig-name" }, [rig.name])]),
      el("td", {}, [String(rig.agent_count - rig.running_count)]),
      el("td", {}, [String(rig.running_count)]),
      el("td", {}, [rig.git?.branch ? `${rig.git.branch}${rig.git.dirty ? "*" : ""}` : "—"]),
      el("td", {}, [formatTimestamp(rig.last_activity)]),
      el("td", {}, [suspendResume, " ", restart]),
    ]));
  });

  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Name"]),
      el("th", {}, ["Idle"]),
      el("th", {}, ["Running"]),
      el("th", {}, ["Git"]),
      el("th", {}, ["Activity"]),
      el("th", {}, ["Actions"]),
    ])]),
    tbody,
  ]));
}

function renderEscalations(items: BeadRecord[] | null): void {
  const body = byId("escalations-body");
  const count = byId("escalations-count");
  if (!body || !count) return;
  clear(body);
  const escalations = (items ?? []).sort((a, b) => (a.created_at ?? "").localeCompare(b.created_at ?? ""));
  count.textContent = String(escalations.length);
  if (escalations.length === 0) {
    body.append(el("div", { class: "empty-state" }, [el("p", {}, ["No escalations"])]));
    return;
  }

  const tbody = el("tbody");
  escalations.forEach((issue) => {
    const severity = extractSeverity(issue.labels ?? []);
    const acked = (issue.labels ?? []).includes("acked");
    const ack = el("button", { class: "esc-btn esc-ack-btn", type: "button" }, ["👍 Ack"]);
    ack.addEventListener("click", () => {
      void ackEscalation(issue);
    });
    const resolve = el("button", { class: "esc-btn esc-resolve-btn", type: "button" }, ["✓ Resolve"]);
    resolve.addEventListener("click", () => {
      if (issue.id) void closeBead(issue.id);
    });
    const reassign = el("button", { class: "esc-btn esc-reassign-btn", type: "button" }, ["↻ Reassign"]);
    reassign.addEventListener("click", () => {
      if (issue.id) void reassignBead(issue.id);
    });

    tbody.append(el("tr", { class: "escalation-row", "data-escalation-id": issue.id ?? "" }, [
      el("td", {}, [el("span", { class: `badge ${severityBadge(severity)}` }, [severity.toUpperCase()])]),
      el("td", {}, [
        issue.title ?? issue.id ?? "",
        acked ? el("span", { class: "badge badge-cyan", style: "margin-left: 4px;" }, ["ACK"]) : null,
      ]),
      el("td", {}, [formatAgentAddress(issue.assignee)]),
      el("td", {}, [formatTimestamp(issue.created_at)]),
      el("td", { class: "escalation-actions" }, [!acked ? ack : null, resolve, reassign]),
    ]));
  });

  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Severity"]),
      el("th", {}, ["Issue"]),
      el("th", {}, ["From"]),
      el("th", {}, ["Age"]),
      el("th", {}, ["Actions"]),
    ])]),
    tbody,
  ]));
}

function renderAssigned(items: BeadRecord[] | null): void {
  const body = byId("assigned-body");
  const count = byId("assigned-count");
  const clearBtn = byId("clear-assigned-btn");
  if (!body || !count || !clearBtn) return;
  clear(body);
  const assigned = (items ?? []).filter((bead) => bead.assignee);
  count.textContent = String(assigned.length);
  clearBtn.style.display = assigned.length > 0 ? "inline-flex" : "none";
  if (assigned.length === 0) {
    body.append(el("div", { class: "empty-state" }, [el("p", {}, ["No assigned work"])]));
    return;
  }

  const tbody = el("tbody");
  assigned.forEach((bead) => {
    const unassign = el("button", { class: "unassign-btn", type: "button" }, ["Unassign"]);
    unassign.addEventListener("click", () => {
      if (bead.id) void unassignBead(bead.id);
    });
    tbody.append(el("tr", {}, [
      el("td", {}, [el("span", { class: "assigned-id" }, [bead.id ?? ""])]),
      el("td", { class: "assigned-title" }, [truncate(bead.title ?? "", 80)]),
      el("td", { class: "assigned-agent" }, [formatAgentAddress(bead.assignee)]),
      el("td", { class: "assigned-age" }, [formatTimestamp(bead.created_at)]),
      el("td", {}, [unassign]),
    ]));
  });

  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Bead"]),
      el("th", {}, ["Title"]),
      el("th", {}, ["Agent"]),
      el("th", {}, ["Since"]),
      el("th", {}, [""]),
    ])]),
    tbody,
  ]));
}

function renderQueues(items: BeadRecord[] | null): void {
  const body = byId("queues-body");
  const count = byId("queues-count");
  if (!body || !count) return;
  clear(body);
  const queues = items ?? [];
  count.textContent = String(queues.length);
  if (queues.length === 0) {
    body.append(el("div", { class: "empty-state" }, [el("p", {}, ["No queues"])]));
    return;
  }

  const tbody = el("tbody");
  queues.forEach((queue) => {
    const counts = parseQueueDescription(queue.description ?? "");
    tbody.append(el("tr", {}, [
      el("td", {}, [queue.title ?? queue.id ?? "queue"]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(queue.status)}` }, [queue.status ?? "open"])]),
      el("td", {}, [String(counts.available)]),
      el("td", {}, [String(counts.processing)]),
      el("td", {}, [String(counts.completed)]),
      el("td", {}, [String(counts.failed)]),
    ]));
  });

  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Queue"]),
      el("th", {}, ["Status"]),
      el("th", {}, ["Avail"]),
      el("th", {}, ["Proc"]),
      el("th", {}, ["Done"]),
      el("th", {}, ["Fail"]),
    ])]),
    tbody,
  ]));
}

function renderEmptyBody(bodyID: string, countID: string, message: string): void {
  const body = byId(bodyID);
  const count = byId(countID);
  if (!body || !count) return;
  clear(body);
  count.textContent = "0";
  body.append(el("div", { class: "empty-state" }, [el("p", {}, [message])]));
}

function extractSeverity(labels: string[]): string {
  for (const label of labels) {
    if (label.startsWith("severity:")) return label.slice("severity:".length);
  }
  return "medium";
}

function severityBadge(severity: string): string {
  switch (severity) {
    case "critical":
      return "badge-red";
    case "high":
      return "badge-orange";
    case "low":
      return "badge-muted";
    default:
      return "badge-yellow";
  }
}

function parseQueueDescription(description: string): Record<string, number> {
  const result = { available: 0, processing: 0, completed: 0, failed: 0 };
  description.split("\n").forEach((line) => {
    const match = line.trim().match(/^([a-z_]+):\s*(\d+)/);
    if (!match) return;
    const key = match[1];
    const value = Number(match[2]);
    if (key === "available_count") result.available = value;
    if (key === "processing_count") result.processing = value;
    if (key === "completed_count") result.completed = value;
    if (key === "failed_count") result.failed = value;
  });
  return result;
}

async function assignFromPanel(): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const beadID = byId<HTMLInputElement>("assign-bead")?.value.trim() ?? "";
  if (!beadID) return;
  const options = await getOptions();
  const target = window.prompt(`Target agent or pool.\nAvailable agents: ${options.agents.join(", ")}`);
  if (!target) return;
  const rig = window.prompt("Rig name (optional)") ?? "";
  const res = await api.POST("/v0/city/{cityName}/sling", {
    params: { path: { cityName: city } },
    body: { bead: beadID, target, rig: rig || undefined },
  });
  if (res.error) {
    showToast("error", "Assign failed", res.error.detail ?? "Could not assign bead");
    return;
  }
  byId("assign-form")!.style.display = "none";
  byId<HTMLInputElement>("assign-bead")!.value = "";
  showToast("success", "Assigned", `${beadID} → ${target}`);
  await renderAdminPanels();
}

async function clearAllAssigned(): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const assigned = await api.GET("/v0/city/{cityName}/beads", {
    params: { path: { cityName: city }, query: { status: "in_progress", limit: 500 } },
  });
  const items = (assigned.data?.items ?? []).filter((bead) => bead.assignee);
  if (items.length === 0) {
    showToast("info", "Nothing to clear", "No assigned work");
    return;
  }
  if (!window.confirm("Unassign all active work?")) return;
  await Promise.all(items.map((bead) =>
    api.POST("/v0/city/{cityName}/bead/{id}/assign", {
      params: { path: { cityName: city, id: bead.id ?? "" } },
      body: { assignee: "" },
    }),
  ));
  showToast("success", "Cleared", `${items.length} assignments removed`);
  await renderAdminPanels();
}

async function unassignBead(beadID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/assign", {
    params: { path: { cityName: city, id: beadID } },
    body: { assignee: "" },
  });
  if (res.error) {
    showToast("error", "Unassign failed", res.error.detail ?? "Could not unassign bead");
    return;
  }
  showToast("success", "Unassigned", beadID);
  await renderAdminPanels();
}

async function restartService(service: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/service/{name}/restart", {
    params: { path: { cityName: city, name: service } },
  });
  if (res.error) {
    showToast("error", "Service failed", res.error.detail ?? "Could not restart service");
    return;
  }
  showToast("success", "Service restarted", service);
  await renderAdminPanels();
}

async function rigAction(rig: string, action: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/rig/{name}/{action}", {
    params: { path: { cityName: city, name: rig, action } },
  });
  if (res.error) {
    showToast("error", "Rig action failed", res.error.detail ?? `Could not ${action} ${rig}`);
    return;
  }
  showToast("success", "Rig updated", `${rig}: ${action}`);
  await renderAdminPanels();
}

async function ackEscalation(issue: BeadRecord): Promise<void> {
  const city = cityScope();
  if (!city || !issue.id) return;
  const labels = Array.from(new Set([...(issue.labels ?? []), "acked"]));
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/update", {
    params: { path: { cityName: city, id: issue.id } },
    body: { labels },
  });
  if (res.error) {
    showToast("error", "Ack failed", res.error.detail ?? "Could not acknowledge escalation");
    return;
  }
  showToast("success", "Acknowledged", issue.id);
  await renderAdminPanels();
}

async function closeBead(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/close", {
    params: { path: { cityName: city, id: issueID } },
  });
  if (res.error) {
    showToast("error", "Resolve failed", res.error.detail ?? "Could not resolve escalation");
    return;
  }
  showToast("success", "Resolved", issueID);
  await renderAdminPanels();
}

async function reassignBead(issueID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const options = await getOptions();
  const assignee = window.prompt(`New assignee.\nAvailable: ${options.agents.join(", ")}`);
  if (assignee == null) return;
  const res = await api.POST("/v0/city/{cityName}/bead/{id}/assign", {
    params: { path: { cityName: city, id: issueID } },
    body: { assignee },
  });
  if (res.error) {
    showToast("error", "Reassign failed", res.error.detail ?? "Could not reassign escalation");
    return;
  }
  showToast("success", "Reassigned", `${issueID} → ${assignee || "unassigned"}`);
  await renderAdminPanels();
}
