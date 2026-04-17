// Mail panel: inbox view grouped into threads. Port of the Go
// `groupIntoThreads` routine in `api.go` (thread key = reply-to
// parent if set, else message ID).

import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { relativeTime } from "../util/time";

interface MailRow {
  id: string;
  from: string;
  to: string;
  subject: string;
  body: string;
  timestamp: string;
  unread: boolean;
  threadID: string;
  replyTo?: string;
}

interface MailThread {
  id: string;
  subject: string;
  messages: MailRow[];
  lastActivity: string;
  unreadCount: number;
}

export async function renderMail(): Promise<void> {
  const container = byId("mail-panel");
  if (!container) return;
  const city = cityScope();
  if (!city) {
    clear(container);
    return;
  }

  const { data, error } = await api.GET("/v0/city/{cityName}/mail", {
    params: { path: { cityName: city } },
  });
  if (error || !data?.items) {
    clear(container);
    container.append(el("div", { class: "panel-error" }, ["Could not load mail."]));
    return;
  }

  const rows: MailRow[] = data.items.map((m) => ({
    id: m.id ?? "",
    from: m.from ?? "",
    to: m.to ?? "",
    subject: m.subject ?? "",
    body: m.body ?? "",
    timestamp: m.created_at ?? "",
    unread: !m.read,
    threadID: m.thread_id ?? m.id ?? "",
    replyTo: m.reply_to ?? undefined,
  }));

  const threads = groupIntoThreads(rows);

  clear(container);
  const list = el("div", { class: "mail-list" });
  for (const thread of threads) {
    const cls = `mail-thread${thread.unreadCount > 0 ? " has-unread" : ""}`;
    list.append(el(
      "div",
      { class: cls },
      [
        el("div", { class: "mail-subject" }, [thread.subject || "(no subject)"]),
        el("div", { class: "mail-meta" }, [
          `${thread.messages.length} message${thread.messages.length === 1 ? "" : "s"}`,
          thread.unreadCount > 0
            ? el("span", { class: "badge badge-unread" }, [`${thread.unreadCount} unread`])
            : null,
          el("span", { class: "mail-time" }, [relativeTime(thread.lastActivity)]),
        ]),
      ],
    ));
  }
  if (threads.length === 0) {
    list.append(el("div", { class: "panel-muted" }, ["Inbox empty."]));
  }
  container.append(list);
}

// Port of dashboard/api.go `groupIntoThreads`. Resolves each message
// to its root via replyTo chain, then groups. Within a thread,
// messages sort ascending by timestamp.
export function groupIntoThreads(rows: MailRow[]): MailThread[] {
  const byID = new Map<string, MailRow>();
  for (const r of rows) byID.set(r.id, r);

  function rootKey(r: MailRow): string {
    let cursor = r;
    const seen = new Set<string>();
    while (cursor.replyTo && !seen.has(cursor.id)) {
      seen.add(cursor.id);
      const parent = byID.get(cursor.replyTo);
      if (!parent) break;
      cursor = parent;
    }
    return cursor.threadID || cursor.id;
  }

  const threads = new Map<string, MailThread>();
  for (const r of rows) {
    const key = rootKey(r);
    const thread = threads.get(key) ?? {
      id: key,
      subject: r.subject,
      messages: [],
      lastActivity: r.timestamp,
      unreadCount: 0,
    };
    thread.messages.push(r);
    if (r.unread) thread.unreadCount += 1;
    if (r.timestamp > thread.lastActivity) thread.lastActivity = r.timestamp;
    if (!thread.subject && r.subject) thread.subject = r.subject;
    threads.set(key, thread);
  }

  for (const t of threads.values()) {
    t.messages.sort((a, b) => (a.timestamp ?? "").localeCompare(b.timestamp ?? ""));
  }

  return [...threads.values()].sort((a, b) => (b.lastActivity ?? "").localeCompare(a.lastActivity ?? ""));
}

export async function sendMail(input: {
  to: string;
  subject: string;
  body: string;
  replyTo?: string;
}): Promise<{ ok: boolean; error?: string }> {
  const city = cityScope();
  if (!city) return { ok: false, error: "no city selected" };
  const { error } = await api.POST("/v0/city/{cityName}/mail", {
    params: { path: { cityName: city } },
    body: {
      to: input.to,
      subject: input.subject,
      body: input.body,
      reply_to: input.replyTo,
    },
  });
  if (error) return { ok: false, error: error.detail ?? error.title ?? "send failed" };
  return { ok: true };
}
