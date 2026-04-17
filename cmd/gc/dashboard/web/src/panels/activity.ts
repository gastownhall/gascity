import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { connectCityEvents, connectEvents, type EventMessage, type SSEHandle } from "../sse";
import { eventCategory, eventIcon, eventSummary, extractRig, formatAgentAddress } from "../util/legacy";
import { relativeTime } from "../util/time";

interface ActivityEntry {
  actor?: string;
  category: string;
  id: string;
  message?: string;
  rig: string;
  subject?: string;
  ts: string;
  type: string;
}

const MAX_ENTRIES = 150;
const entries: ActivityEntry[] = [];
let handle: SSEHandle | null = null;
let categoryFilter = "all";
let rigFilter = "all";
let agentFilter = "all";

export async function seedActivity(entriesFromAPI: ActivityEntry[]): Promise<void> {
  entries.splice(0, entries.length, ...entriesFromAPI.slice(0, MAX_ENTRIES));
  renderActivity();
}

export async function loadActivityHistory(): Promise<void> {
  const city = cityScope();
  const res = city
    ? await api.GET("/v0/city/{cityName}/events", {
        params: { path: { cityName: city }, query: { since: "1h", limit: 100 } },
      })
    : await api.GET("/v0/events", {
        params: { query: { since: "1h" } },
      });
  const normalized = (res.data?.items ?? [])
    .map((item) => toEntry({ type: String((item as { type?: string }).type ?? "event"), data: item }))
    .filter((item): item is ActivityEntry => item !== null);
  await seedActivity(normalized);
}

export function startActivityStream(): void {
  const city = cityScope();
  handle?.close();
  const connect = city ? (onEvent: (msg: EventMessage) => void) => connectCityEvents(city, onEvent) : connectEvents;
  handle = connect((msg) => {
    const entry = toEntry(msg);
    if (!entry) return;
    entries.unshift(entry);
    if (entries.length > MAX_ENTRIES) entries.length = MAX_ENTRIES;
    renderActivity();
  });
}

export function stopActivityStream(): void {
  handle?.close();
  handle = null;
}

export function renderActivity(): void {
  renderFilters();
  const feed = byId("activity-feed");
  if (!feed) return;
  clear(feed);

  const filtered = entries.filter((entry) => {
    if (categoryFilter !== "all" && entry.category !== categoryFilter) return false;
    if (rigFilter !== "all" && entry.rig !== rigFilter) return false;
    if (agentFilter !== "all" && entry.actor !== agentFilter) return false;
    return true;
  });
  byId("activity-count")!.textContent = String(entries.length);

  if (filtered.length === 0) {
    feed.append(el("div", { class: "empty-state" }, [el("p", {}, ["No recent activity"])]));
    return;
  }

  const timeline = el("div", { class: "tl-timeline", id: "activity-timeline" });
  filtered.forEach((entry) => {
    timeline.append(el("div", {
      class: `tl-entry ${activityTypeClass(entry.category)}`,
      "data-category": entry.category,
      "data-rig": entry.rig,
      "data-agent": entry.actor ?? "",
      "data-type": entry.type,
      "data-ts": entry.ts,
    }, [
      el("div", { class: "tl-rail" }, [
        el("span", { class: "tl-time" }, [relativeTime(entry.ts)]),
        el("span", { class: "tl-node" }),
      ]),
      el("div", { class: "tl-content" }, [
        el("div", { class: "tl-header" }, [
          el("span", { class: "tl-icon" }, [eventIcon(entry.type)]),
          el("span", { class: "tl-summary" }, [eventSummary(entry.type, entry.actor, entry.subject, entry.message)]),
        ]),
        el("div", { class: "tl-meta" }, [
          entry.actor ? el("span", { class: "tl-badge tl-badge-agent" }, [formatAgentAddress(entry.actor)]) : null,
          entry.rig ? el("span", { class: "tl-badge tl-badge-rig" }, [entry.rig]) : null,
          el("span", { class: "tl-badge tl-badge-type" }, [entry.type]),
        ]),
      ]),
    ]));
  });
  feed.append(timeline);
}

export function installActivityInteractions(): void {
  document.addEventListener("click", (event) => {
    const target = (event.target as HTMLElement | null)?.closest(".tl-filter-btn") as HTMLElement | null;
    if (!target) return;
    categoryFilter = target.dataset.value ?? "all";
    document.querySelectorAll(".tl-filter-btn").forEach((button) => button.classList.remove("active"));
    target.classList.add("active");
    renderActivity();
  });

  byId<HTMLSelectElement>("tl-rig-filter")?.addEventListener("change", (event) => {
    rigFilter = (event.currentTarget as HTMLSelectElement).value;
    renderActivity();
  });
  byId<HTMLSelectElement>("tl-agent-filter")?.addEventListener("change", (event) => {
    agentFilter = (event.currentTarget as HTMLSelectElement).value;
    renderActivity();
  });
}

function renderFilters(): void {
  const container = byId("activity-filters");
  if (!container) return;
  clear(container);
  if (entries.length === 0) return;
  const rigs = [...new Set(entries.map((entry) => entry.rig).filter(Boolean))].sort();
  const agents = [...new Set(entries.map((entry) => entry.actor).filter(Boolean))].sort() as string[];

  const rigSelect = el("select", { class: "tl-filter-select", id: "tl-rig-filter" }) as HTMLSelectElement;
  rigSelect.append(el("option", { value: "all" }, ["All rigs"]));
  rigs.forEach((rig) => rigSelect.append(el("option", { value: rig, selected: rig === rigFilter }, [rig])));
  rigSelect.addEventListener("change", () => {
    rigFilter = rigSelect.value;
    renderActivity();
  });

  const agentSelect = el("select", { class: "tl-filter-select", id: "tl-agent-filter" }) as HTMLSelectElement;
  agentSelect.append(el("option", { value: "all" }, ["All agents"]));
  agents.forEach((agent) => agentSelect.append(el("option", { value: agent, selected: agent === agentFilter }, [formatAgentAddress(agent)])));
  agentSelect.addEventListener("change", () => {
    agentFilter = agentSelect.value;
    renderActivity();
  });

  container.append(el("div", { class: "tl-filters" }, [
    el("div", { class: "tl-filter-group" }, [
      el("label", {}, ["Category:"]),
      filterButton("all", "All"),
      filterButton("agent", "Agent"),
      filterButton("work", "Work"),
      filterButton("comms", "Comms"),
      filterButton("system", "System"),
    ]),
    el("div", { class: "tl-filter-group" }, [el("label", {}, ["Rig:"]), rigSelect]),
    el("div", { class: "tl-filter-group" }, [el("label", {}, ["Agent:"]), agentSelect]),
  ]));
}

function filterButton(value: string, label: string): HTMLElement {
  const btn = el("button", {
    class: `tl-filter-btn${categoryFilter === value ? " active" : ""}`,
    "data-filter": "category",
    "data-value": value,
    type: "button",
  }, [label]);
  btn.addEventListener("click", () => {
    categoryFilter = value;
    renderActivity();
  });
  return btn;
}

function toEntry(msg: EventMessage): ActivityEntry | null {
  const raw = msg.data as Record<string, unknown> | undefined;
  if (!raw) return null;
  const envelope = raw.data && typeof raw.data === "object" ? raw.data as Record<string, unknown> : raw;
  const type = (raw.type as string) || (msg.type === "event" ? String(envelope.type ?? "") : msg.type);
  if (!type || type === "heartbeat") return null;
  const actor = String(envelope.actor ?? raw.actor ?? "");
  const subject = String(envelope.subject ?? raw.subject ?? "");
  const message = String(envelope.message ?? raw.message ?? "");
  const ts = String(envelope.ts ?? raw.ts ?? new Date().toISOString());
  const city = String(envelope.city ?? raw.city ?? "");
  return {
    id: msg.id ?? `${type}:${ts}`,
    type,
    category: eventCategory(type),
    actor,
    subject,
    message,
    ts,
    rig: extractRig(actor) || city,
  };
}
function activityTypeClass(category: string): string {
  switch (category) {
    case "agent":
      return "activity-agent";
    case "work":
      return "activity-work";
    case "comms":
      return "activity-comms";
    default:
      return "activity-system";
  }
}
