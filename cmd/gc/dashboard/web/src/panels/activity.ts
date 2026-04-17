// Activity panel: live stream of supervisor events. Replaces the
// Go `/api/events` SSE proxy with a direct `EventSource` against
// `/v0/events/stream`. Events are appended to a capped ring so the
// panel stays readable under high event rates.

import { byId, clear, el } from "../util/dom";
import { connectEvents, type SSEHandle } from "../sse";
import { relativeTime } from "../util/time";

interface ActivityEntry {
  id: string;
  type: string;
  actor: string;
  city: string;
  ts: string;
  payload: unknown;
}

const MAX_ENTRIES = 100;
const entries: ActivityEntry[] = [];
let handle: SSEHandle | null = null;

export function startActivityStream(): void {
  if (handle) return;
  handle = connectEvents((msg) => {
    const d = msg.data as Record<string, unknown> | undefined;
    entries.unshift({
      id: msg.id ?? String(Date.now()),
      type: msg.type,
      actor: (d?.actor as string) ?? "",
      city: (d?.city as string) ?? "",
      ts: (d?.ts as string) ?? new Date().toISOString(),
      payload: d,
    });
    if (entries.length > MAX_ENTRIES) entries.length = MAX_ENTRIES;
    renderActivity();
  });
}

export function stopActivityStream(): void {
  handle?.close();
  handle = null;
}

export function renderActivity(): void {
  const container = byId("activity-panel");
  if (!container) return;
  clear(container);
  const list = el("div", { class: "activity-list" });
  for (const e of entries) {
    list.append(el(
      "div",
      { class: `activity-row activity-${e.type.replace(/\./g, "-")}` },
      [
        el("span", { class: "activity-type" }, [e.type]),
        el("span", { class: "activity-actor" }, [e.actor || "—"]),
        el("span", { class: "activity-time" }, [relativeTime(e.ts)]),
      ],
    ));
  }
  if (entries.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["Waiting for events…"]));
  }
  container.append(list);
}
