import type { SessionRecord } from "../api";
import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { calculateActivity, formatTimestamp, statusBadgeClass, truncate } from "../util/legacy";
import { connectAgentOutput, type AgentOutputMessage, type SSEHandle } from "../sse";
import { popPause, pushPause, showToast } from "../ui";

let logHandle: SSEHandle | null = null;
let logSessionID = "";
let logBeforeCursor = "";
let logCount = 0;

export async function renderCrew(): Promise<void> {
  const city = cityScope();
  if (!city) {
    resetCrewNoCity();
    return;
  }

  const crewLoading = byId("crew-loading");
  const crewTable = byId<HTMLTableElement>("crew-table");
  const crewEmpty = byId("crew-empty");
  const crewBody = byId("crew-tbody");
  const polecatsBody = byId("polecats-body");
  const dogsBody = byId("dogs-body");
  if (!crewLoading || !crewTable || !crewEmpty || !crewBody || !polecatsBody || !dogsBody) return;

  setCrewEmptyMessage("No crew configured");
  crewLoading.style.display = "block";
  crewTable.style.display = "none";
  crewEmpty.style.display = "none";
  clear(crewBody);

  const { data, error } = await api.GET("/v0/city/{cityName}/sessions", {
    params: { path: { cityName: city }, query: { state: "active", peek: "true" } },
  });
  if (error || !data?.items) {
    crewLoading.textContent = "Failed to load crew";
    renderSimpleEmpty(polecatsBody, "No polecats");
    renderSimpleEmpty(dogsBody, "No dogs in kennel");
    return;
  }

  const sessions = data.items;
  const pending = await Promise.all(
    sessions.map(async (session) => {
      const res = await api.GET("/v0/city/{cityName}/session/{id}/pending", {
        params: { path: { cityName: city, id: session.id } },
      });
      return Boolean(res.data?.pending);
    }),
  );

  const beadTitles = new Map<string, string>();
  await Promise.all(
    sessions.map(async (session) => {
      if (!session.active_bead) return;
      if (beadTitles.has(session.active_bead)) return;
      const res = await api.GET("/v0/city/{cityName}/bead/{id}", {
        params: { path: { cityName: city, id: session.active_bead } },
      });
      beadTitles.set(session.active_bead, res.data?.id ? (res.data.title ?? res.data.id) : session.active_bead);
    }),
  );

  const crew = sessions;
  crew.forEach((session, index) => {
    const state = classifyCrewState(session, pending[index] ?? false);
    const beadText = session.active_bead ? truncate(beadTitles.get(session.active_bead) ?? session.active_bead, 24) : "—";
    const row = el("tr", {}, [
      el("td", {}, [session.template]),
      el("td", {}, [session.rig ?? "city"]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(state)}` }, [state])]),
      el("td", {}, [beadText]),
      el("td", { class: calculateActivity(session.last_active).colorClass ? `activity-${calculateActivity(session.last_active).colorClass}` : "" }, [
        el("span", { class: "activity-dot" }),
        ` ${calculateActivity(session.last_active).display}`,
      ]),
      el("td", {}, [
        el("span", { class: `badge ${session.attached ? "badge-green" : "badge-muted"}` }, [
          session.attached ? "Attached" : "Detached",
        ]),
      ]),
      el("td", {}, [
        attachButton(session.template),
        " ",
        logButton(session.id, session.template),
      ]),
    ]);
    crewBody.append(row);
  });

  byId("crew-count")!.textContent = String(crew.length);
  crewLoading.style.display = "none";
  if (crew.length > 0) {
    crewTable.style.display = "table";
  } else {
    setCrewEmptyMessage("No crew configured");
    crewEmpty.style.display = "block";
  }

  renderPolecats(sessions, beadTitles);
  renderDogs(sessions);
}

function resetCrewNoCity(): void {
  const crewLoading = byId("crew-loading");
  const crewTable = byId<HTMLTableElement>("crew-table");
  const crewEmpty = byId("crew-empty");
  const crewBody = byId("crew-tbody");
  const polecatsBody = byId("polecats-body");
  const dogsBody = byId("dogs-body");
  if (!crewLoading || !crewTable || !crewEmpty || !crewBody || !polecatsBody || !dogsBody) return;

  closeLogDrawer();
  byId("crew-count")!.textContent = "0";
  byId("polecats-count")!.textContent = "0";
  byId("dogs-count")!.textContent = "0";
  crewLoading.style.display = "none";
  crewTable.style.display = "none";
  crewEmpty.style.display = "block";
  setCrewEmptyMessage("Select a city to view crew");
  clear(crewBody);
  renderSimpleEmpty(polecatsBody, "Select a city to view polecats");
  renderSimpleEmpty(dogsBody, "Select a city to view dogs");
}

function setCrewEmptyMessage(message: string): void {
  byId("crew-empty")?.querySelector("p")?.replaceChildren(document.createTextNode(message));
}

function classifyCrewState(session: SessionRecord, hasPending: boolean): string {
  if (hasPending) return "questions";
  if (session.active_bead) return "spinning";
  if (!session.running) return "finished";
  return "idle";
}

function attachButton(template: string): HTMLElement {
  const btn = el("button", { class: "attach-btn", type: "button" }, ["📎 Attach"]);
  btn.addEventListener("click", async () => {
    const command = `gc agent attach ${template}`;
    try {
      await navigator.clipboard.writeText(command);
      showToast("success", "Attach command copied", command);
    } catch {
      showToast("error", "Copy failed", command);
    }
  });
  return btn;
}

function logButton(sessionID: string, label: string): HTMLElement {
  const btn = el("button", { class: "agent-log-link", type: "button", "data-session-id": sessionID }, [label]);
  btn.addEventListener("click", () => {
    void openLogDrawer(sessionID, label);
  });
  return btn;
}

function renderPolecats(sessions: SessionRecord[], beadTitles: Map<string, string>): void {
  const body = byId("polecats-body");
  const count = byId("polecats-count");
  if (!body || !count) return;

  const rows = sessions.filter((session) => session.rig && session.pool);
  count.textContent = String(rows.length);
  if (rows.length === 0) {
    renderSimpleEmpty(body, "No polecats");
    return;
  }

  const tbody = el("tbody");
  rows.forEach((session) => {
    const activity = calculateActivity(session.last_active);
    const workStatus = !session.active_bead ? "Idle" : activity.colorClass === "red" ? "Stuck" : activity.colorClass === "yellow" ? "Stale" : "Working";
    tbody.append(el("tr", { class: `polecat-${workStatus.toLowerCase()}` }, [
      el("td", {}, [logButton(session.id, session.template)]),
      el("td", {}, [el("span", { class: `badge ${session.pool === "refinery" ? "badge-blue" : "badge-muted"}` }, [session.pool ?? "polecat"])]),
      el("td", {}, [session.rig ?? "city"]),
      el("td", { class: "polecat-issue" }, [
        session.active_bead
          ? `${session.active_bead} ${beadTitles.get(session.active_bead) ?? ""}`.trim()
          : "—",
      ]),
      el("td", {}, [el("span", { class: `badge ${statusBadgeClass(workStatus)}` }, [workStatus])]),
      el("td", { class: `activity-${activity.colorClass}` }, [el("span", { class: "activity-dot" }), ` ${activity.display}`]),
    ]));
  });

  clear(body);
  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Worker"]),
      el("th", {}, ["Type"]),
      el("th", {}, ["Rig"]),
      el("th", {}, ["Working On"]),
      el("th", {}, ["Status"]),
      el("th", {}, ["Activity"]),
    ])]),
    tbody,
  ]));
}

function renderDogs(sessions: SessionRecord[]): void {
  const body = byId("dogs-body");
  const count = byId("dogs-count");
  if (!body || !count) return;
  const rows = sessions.filter((session) => !session.rig && session.pool);
  count.textContent = String(rows.length);
  if (rows.length === 0) {
    renderSimpleEmpty(body, "No dogs in kennel");
    return;
  }

  const tbody = el("tbody");
  rows.forEach((session) => {
    tbody.append(el("tr", {}, [
      el("td", {}, [session.template]),
      el("td", {}, [el("span", { class: `badge ${session.active_bead ? "badge-yellow" : "badge-green"}` }, [session.active_bead ? "Working" : "Idle"])]),
      el("td", { class: "status-hint" }, [truncate(session.last_output, 80) || "—"]),
      el("td", {}, [formatTimestamp(session.last_active)]),
    ]));
  });

  clear(body);
  body.append(el("table", {}, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["Name"]),
      el("th", {}, ["State"]),
      el("th", {}, ["Work"]),
      el("th", {}, ["Activity"]),
    ])]),
    tbody,
  ]));
}

function renderSimpleEmpty(container: HTMLElement, message: string): void {
  clear(container);
  container.append(el("div", { class: "empty-state" }, [el("p", {}, [message])]));
}

export function installCrewInteractions(): void {
  byId("log-drawer-close-btn")?.addEventListener("click", () => closeLogDrawer());
  byId("log-drawer-older-btn")?.addEventListener("click", () => {
    if (!logSessionID || !logBeforeCursor) return;
    void loadTranscript(logSessionID, true);
  });
}

async function openLogDrawer(sessionID: string, label: string): Promise<void> {
  const drawer = byId("agent-log-drawer");
  const nameEl = byId("log-drawer-agent-name");
  const messagesEl = byId("log-drawer-messages");
  const loadingEl = byId("log-drawer-loading");
  if (!drawer || !nameEl || !messagesEl || !loadingEl) return;

  if (logSessionID === sessionID && drawer.style.display !== "none") {
    closeLogDrawer();
    return;
  }

  closeLogDrawer();
  logSessionID = sessionID;
  logBeforeCursor = "";
  logCount = 0;

  nameEl.textContent = label;
  clear(messagesEl);
  messagesEl.append(loadingEl);
  loadingEl.style.display = "block";
  drawer.style.display = "block";
  pushPause();

  await loadTranscript(sessionID, false);
  const city = cityScope();
  if (!city) return;
  logHandle = connectAgentOutput(city, sessionID, (msg) => appendStreamEvent(msg));
}

function closeLogDrawer(): void {
  logHandle?.close();
  logHandle = null;
  logSessionID = "";
  logBeforeCursor = "";
  const drawer = byId("agent-log-drawer");
  if (drawer && drawer.style.display !== "none") {
    drawer.style.display = "none";
    popPause();
  }
}

async function loadTranscript(sessionID: string, prepend: boolean): Promise<void> {
  const city = cityScope();
  const messagesEl = byId("log-drawer-messages");
  const loadingEl = byId("log-drawer-loading");
  const olderBtn = byId<HTMLButtonElement>("log-drawer-older-btn");
  const countEl = byId("log-drawer-count");
  if (!city || !messagesEl || !loadingEl || !olderBtn || !countEl) return;

  loadingEl.style.display = "block";
  const res = await api.GET("/v0/city/{cityName}/session/{id}/transcript", {
    params: {
      path: { cityName: city, id: sessionID },
      query: { tail: String(prepend ? 50 : 25), before: prepend ? logBeforeCursor : undefined },
    },
  });
  loadingEl.style.display = "none";
  if (res.error || !res.data) {
    showToast("error", "Transcript failed", res.error?.detail ?? "Could not load transcript");
    return;
  }

  const fragment = document.createDocumentFragment();
  for (const turn of res.data.turns ?? []) {
    fragment.append(renderTurn(turn.role, turn.text, turn.timestamp));
    logCount += 1;
  }
  if (prepend) {
    messagesEl.prepend(fragment);
  } else {
    clear(messagesEl);
    messagesEl.append(fragment);
  }
  countEl.textContent = String(logCount);

  logBeforeCursor = res.data.pagination?.truncated_before_message ?? "";
  olderBtn.style.display = res.data.pagination?.has_older_messages ? "inline-flex" : "none";
}

function appendStreamEvent(msg: AgentOutputMessage): void {
  const messagesEl = byId("log-drawer-messages");
  if (!messagesEl) return;
  const payload = msg.data as { data?: { message?: { role?: string; text?: string; timestamp?: string } }; event?: string } | null;
  if (msg.type !== "message" || !payload?.data?.message) return;
  messagesEl.append(renderTurn(payload.data.message.role ?? "agent", payload.data.message.text ?? "", payload.data.message.timestamp));
  logCount += 1;
  byId("log-drawer-count")!.textContent = String(logCount);
  const body = byId("log-drawer-body");
  if (body) body.scrollTop = body.scrollHeight;
}

function renderTurn(role: string, text: string, timestamp: string | undefined): HTMLElement {
  return el("div", { class: "log-msg" }, [
    el("div", { class: "log-msg-header" }, [
      el("span", { class: `log-msg-type log-msg-type-${roleClass(role)}` }, [role]),
      el("span", { class: "log-msg-time" }, [formatTimestamp(timestamp)]),
    ]),
    el("div", { class: "log-msg-body" }, [text]),
  ]);
}

function roleClass(role: string): string {
  switch ((role ?? "").toLowerCase()) {
    case "assistant":
    case "agent":
      return "assistant";
    case "system":
      return "system";
    case "result":
      return "result";
    default:
      return "user";
  }
}
