import { api, cityScope } from "../api";
import { byId, clear, el } from "../util/dom";
import { formatAgentAddress, formatTimestamp } from "../util/legacy";
import { relativeTime } from "../util/time";
import { getOptions } from "./options";
import { popPause, pushPause, showToast } from "../ui";

interface MailRecord {
  body?: string;
  created_at?: string;
  from?: string;
  id?: string;
  read?: boolean;
  reply_to?: string;
  subject?: string;
  thread_id?: string;
  to?: string;
}

interface MailThread {
  id: string;
  messages: MailRecord[];
  subject: string;
  unreadCount: number;
}

let currentTab: "inbox" | "all" = "inbox";
let allMessages: MailRecord[] = [];
let currentMessage: MailRecord | null = null;

export async function renderMail(): Promise<void> {
  const city = cityScope();
  const loading = byId("mail-loading");
  const threadsEl = byId("mail-threads");
  const empty = byId("mail-empty");
  const allEl = byId("mail-all");
  if (!loading || !threadsEl || !empty || !allEl) return;
  if (!city) {
    resetMailNoCity();
    return;
  }

  loading.style.display = "block";
  threadsEl.style.display = "none";
  empty.style.display = "none";

  const { data, error } = await api.GET("/v0/city/{cityName}/mail", {
    params: { path: { cityName: city }, query: { status: "all", limit: 200 } },
  });
  loading.style.display = "none";
  if (error || !data?.items) {
    clear(threadsEl);
    threadsEl.append(el("div", { class: "panel-error" }, ["Could not load mail."]));
    threadsEl.style.display = "block";
    return;
  }

  allMessages = [...(data.items as MailRecord[])].sort((a, b) =>
    (b.created_at ?? "").localeCompare(a.created_at ?? ""),
  );
  byId("mail-count")!.textContent = String(allMessages.length);

  renderThreadedInbox(allMessages);
  renderAllTraffic(allMessages);
  restoreMailView();
}

function resetMailNoCity(): void {
  const loading = byId("mail-loading");
  const threadsEl = byId("mail-threads");
  const empty = byId("mail-empty");
  const allEl = byId("mail-all");
  if (!loading || !threadsEl || !empty || !allEl) return;

  const hadSubview = mailSubviewOpen();
  if (hadSubview) {
    switchMailView(currentTab);
    popPause();
  } else {
    switchMailView(currentTab);
  }
  currentMessage = null;
  allMessages = [];
  byId("mail-count")!.textContent = "0";
  loading.style.display = "none";
  clear(threadsEl);
  clear(allEl);
  threadsEl.style.display = "none";
  empty.querySelector("p")?.replaceChildren(document.createTextNode("Select a city to view mail"));
  empty.style.display = currentTab === "inbox" ? "block" : "none";
  allEl.append(el("div", { class: "empty-state" }, [el("p", {}, ["Select a city to view mail traffic"])]));
}

function renderThreadedInbox(messages: MailRecord[]): void {
  const threadsEl = byId("mail-threads");
  const empty = byId("mail-empty");
  if (!threadsEl || !empty) return;
  const threads = groupIntoThreads(messages);
  clear(threadsEl);
  if (threads.length === 0) {
    threadsEl.style.display = "none";
    empty.style.display = "block";
    return;
  }
  empty.style.display = "none";
  threads.forEach((thread) => {
    const last = thread.messages[thread.messages.length - 1];
    const preview = (last.body ?? "").trim().slice(0, 60);
    const card = el("div", { class: `mail-thread${thread.unreadCount > 0 ? " mail-thread-unread" : ""}` }, [
      el("div", { class: "mail-thread-header" }, [
        el("div", { class: "mail-thread-left" }, [
          el("span", { class: "mail-from" }, [formatAgentAddress(last.from)]),
        ]),
        el("div", { class: "mail-thread-center" }, [
          el("span", { class: "mail-subject" }, [thread.subject || "(no subject)"]),
          preview ? el("span", { class: "mail-thread-preview" }, [` — ${preview}`]) : null,
        ]),
        el("div", { class: "mail-thread-right" }, [
          el("span", { class: "mail-time" }, [relativeTime(last.created_at)]),
          thread.unreadCount > 0 ? el("span", { class: "badge badge-unread" }, [`${thread.unreadCount} unread`]) : null,
        ]),
      ]),
    ]);
    card.addEventListener("click", () => {
      void openThread(thread.id);
    });
    threadsEl.append(card);
  });
  threadsEl.style.display = currentTab === "inbox" ? "block" : "none";
}

function renderAllTraffic(messages: MailRecord[]): void {
  const allEl = byId("mail-all");
  if (!allEl) return;
  clear(allEl);
  if (messages.length === 0) {
    allEl.append(el("div", { class: "empty-state" }, [el("p", {}, ["No mail traffic"])]));
    return;
  }

  const tbody = el("tbody");
  messages.forEach((message) => {
    const row = el("tr", { class: `mail-row${message.read ? "" : " mail-unread"}` }, [
      el("td", { class: "mail-from" }, [formatAgentAddress(message.from)]),
      el("td", { class: "mail-to" }, [formatAgentAddress(message.to)]),
      el("td", {}, [el("span", { class: "mail-subject" }, [message.subject ?? "(no subject)"])]),
      el("td", { class: "mail-time" }, [formatTimestamp(message.created_at)]),
    ]);
    row.addEventListener("click", () => {
      if (message.id) void openMessage(message.id);
    });
    tbody.append(row);
  });

  allEl.append(el("table", { class: "mail-all-table" }, [
    el("thead", {}, [el("tr", {}, [
      el("th", {}, ["From"]),
      el("th", {}, ["To"]),
      el("th", {}, ["Subject"]),
      el("th", {}, ["Time"]),
    ])]),
    tbody,
  ]));
  allEl.style.display = currentTab === "all" ? "block" : "none";
}

async function openThread(threadID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.GET("/v0/city/{cityName}/mail/thread/{id}", {
    params: { path: { cityName: city, id: threadID } },
  });
  if (res.error || !res.data?.items || res.data.items.length === 0) {
    showToast("error", "Thread failed", res.error?.detail ?? "Could not load mail thread");
    return;
  }
  const messages = res.data.items as MailRecord[];
  const latest = messages[messages.length - 1] ?? messages[0];
  currentMessage = latest;
  showMailDetail(latest, messages);
}

async function openMessage(messageID: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.GET("/v0/city/{cityName}/mail/{id}", {
    params: { path: { cityName: city, id: messageID } },
  });
  if (res.error || !res.data) {
    showToast("error", "Message failed", res.error?.detail ?? "Could not load message");
    return;
  }
  currentMessage = res.data as MailRecord;
  await api.POST("/v0/city/{cityName}/mail/{id}/read", {
    params: { path: { cityName: city, id: messageID } },
  });
  currentMessage.read = true;
  showMailDetail(currentMessage, [currentMessage]);
  void renderMail();
}

function showMailDetail(message: MailRecord, thread: MailRecord[]): void {
  const hadSubview = mailSubviewOpen();
  byId("mail-detail-subject")!.textContent = message.subject ?? "(no subject)";
  byId("mail-detail-from")!.textContent = formatAgentAddress(message.from);
  byId("mail-detail-time")!.textContent = formatTimestamp(message.created_at);
  const bodyEl = byId("mail-detail-body");
  if (bodyEl) {
    clear(bodyEl);
    thread.forEach((item, index) => {
      if (index > 0) bodyEl.append(el("hr"));
      bodyEl.append(
        el("div", { class: "mail-thread-msg-header" }, [
          el("span", { class: "mail-from" }, [formatAgentAddress(item.from)]),
          el("span", { class: "mail-time" }, [formatTimestamp(item.created_at)]),
        ]),
        el("div", { class: "mail-thread-msg-subject" }, [item.subject ?? "(no subject)"]),
        el("pre", {}, [item.body ?? ""]),
      );
    });
  }
  syncMailDetailControls();
  switchMailView("detail");
  if (!hadSubview) pushPause();
}

function switchMailView(mode: "inbox" | "all" | "detail" | "compose"): void {
  const list = byId("mail-list");
  const all = byId("mail-all");
  const detail = byId("mail-detail");
  const compose = byId("mail-compose");
  if (!list || !all || !detail || !compose) return;

  list.style.display = mode === "inbox" ? "block" : "none";
  all.style.display = mode === "all" ? "block" : "none";
  detail.style.display = mode === "detail" ? "block" : "none";
  compose.style.display = mode === "compose" ? "block" : "none";
}

function restoreMailView(): void {
  if (byId("mail-compose")?.style.display === "block" || byId("mail-detail")?.style.display === "block") return;
  switchMailView(currentTab);
}

export function installMailInteractions(): void {
  document.querySelectorAll(".mail-tab").forEach((node) => {
    node.addEventListener("click", (event) => {
      const target = event.currentTarget as HTMLElement;
      currentTab = (target.dataset.tab as "inbox" | "all") ?? "inbox";
      document.querySelectorAll(".mail-tab").forEach((tab) => tab.classList.remove("active"));
      target.classList.add("active");
      switchMailView(currentTab);
    });
  });

  byId("mail-back-btn")?.addEventListener("click", () => {
    const wasOpen = mailSubviewOpen();
    switchMailView(currentTab);
    currentMessage = null;
    if (wasOpen) popPause();
  });

  byId("compose-mail-btn")?.addEventListener("click", () => {
    void openComposer();
  });
  byId("compose-back-btn")?.addEventListener("click", () => {
    const returningToDetail = !!currentMessage;
    const wasOpen = mailSubviewOpen();
    switchMailView(returningToDetail ? "detail" : currentTab);
    if (wasOpen && !returningToDetail) popPause();
  });
  byId("compose-cancel-btn")?.addEventListener("click", () => {
    const wasOpen = mailSubviewOpen();
    switchMailView(currentTab);
    if (wasOpen) popPause();
  });

  byId("mail-reply-btn")?.addEventListener("click", () => {
    if (currentMessage?.id) void openComposer(currentMessage);
  });

  byId("mail-send-btn")?.addEventListener("click", () => {
    void sendCurrentMessage();
  });

  byId("mail-archive-btn")?.addEventListener("click", () => {
    if (currentMessage?.id) void archiveMessage(currentMessage.id);
  });
  byId("mail-toggle-unread-btn")?.addEventListener("click", () => {
    if (currentMessage?.id) void toggleUnread(currentMessage);
  });
}

async function openComposer(replyTo?: MailRecord): Promise<void> {
  const select = byId<HTMLSelectElement>("compose-to");
  if (!select) return;
  const hadSubview = mailSubviewOpen();
  const options = await getOptions();
  clear(select);
  select.append(el("option", { value: "" }, ["Select recipient…"]));
  options.sessions.forEach((session) => {
    select.append(el("option", { value: session.id }, [session.label]));
  });

  byId<HTMLInputElement>("compose-subject")!.value = replyTo ? replySubject(replyTo.subject ?? "") : "";
  byId<HTMLTextAreaElement>("compose-body")!.value = "";
  byId<HTMLInputElement>("compose-reply-to")!.value = replyTo?.id ?? "";
  byId("mail-compose-title")!.textContent = replyTo ? "Reply" : "New Message";
  if (replyTo?.from) {
    select.value = replyTo.from;
  }
  switchMailView("compose");
  if (!hadSubview) pushPause();
}

async function sendCurrentMessage(): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const to = byId<HTMLSelectElement>("compose-to")?.value ?? "";
  const subject = byId<HTMLInputElement>("compose-subject")?.value.trim() ?? "";
  const body = byId<HTMLTextAreaElement>("compose-body")?.value ?? "";
  const replyTo = byId<HTMLInputElement>("compose-reply-to")?.value ?? "";

  if (!to || !subject) {
    showToast("error", "Missing fields", "Recipient and subject are required");
    return;
  }

  const response = replyTo
    ? await api.POST("/v0/city/{cityName}/mail/{id}/reply", {
        params: { path: { cityName: city, id: replyTo } },
        body: { body, subject },
      })
    : await api.POST("/v0/city/{cityName}/mail", {
        params: { path: { cityName: city } },
        body: { to, subject, body, from: "dashboard" },
      });

  if (response.error) {
    showToast("error", "Send failed", response.error.detail ?? "Could not send message");
    return;
  }

  showToast("success", "Message sent", subject);
  const wasOpen = mailSubviewOpen();
  switchMailView("inbox");
  currentMessage = null;
  if (wasOpen) popPause();
  await renderMail();
}

async function archiveMessage(id: string): Promise<void> {
  const city = cityScope();
  if (!city) return;
  const res = await api.POST("/v0/city/{cityName}/mail/{id}/archive", {
    params: { path: { cityName: city, id } },
  });
  if (res.error) {
    showToast("error", "Archive failed", res.error.detail ?? "Could not archive message");
    return;
  }
  showToast("success", "Archived", id);
  const wasOpen = byId("mail-detail")?.style.display === "block";
  switchMailView(currentTab);
  currentMessage = null;
  if (wasOpen) popPause();
  await renderMail();
}

async function toggleUnread(message: MailRecord): Promise<void> {
  const city = cityScope();
  if (!city || !message.id) return;
  const route = message.read
    ? "/v0/city/{cityName}/mail/{id}/mark-unread"
    : "/v0/city/{cityName}/mail/{id}/read";
  const res = await api.POST(route, {
    params: { path: { cityName: city, id: message.id } },
  });
  if (res.error) {
    showToast("error", "Update failed", res.error.detail ?? "Could not update message");
    return;
  }
  message.read = !message.read;
  currentMessage = { ...message };
  syncMailDetailControls();
  showToast("success", "Updated", message.subject ?? message.id);
  await renderMail();
}

function syncMailDetailControls(): void {
  const unreadBtn = byId<HTMLButtonElement>("mail-toggle-unread-btn");
  if (!unreadBtn) return;
  unreadBtn.textContent = currentMessage?.read ? "Mark unread" : "Mark read";
}

function mailSubviewOpen(): boolean {
  return byId("mail-detail")?.style.display === "block" || byId("mail-compose")?.style.display === "block";
}

function replySubject(subject: string): string {
  if (!subject) return "Re:";
  if (subject.toLowerCase().startsWith("re:")) return subject;
  return `Re: ${subject}`;
}

function groupIntoThreads(rows: MailRecord[]): MailThread[] {
  const byID = new Map<string, MailRecord>();
  rows.forEach((row) => {
    if (row.id) byID.set(row.id, row);
  });

  function rootKey(row: MailRecord): string {
    let cursor = row;
    const seen = new Set<string>();
    while (cursor.reply_to && cursor.id && !seen.has(cursor.id)) {
      seen.add(cursor.id);
      const parent = byID.get(cursor.reply_to);
      if (!parent) break;
      cursor = parent;
    }
    return cursor.thread_id ?? cursor.id ?? Math.random().toString(36);
  }

  const threads = new Map<string, MailThread>();
  rows.forEach((row) => {
    const key = rootKey(row);
    const thread = threads.get(key) ?? {
      id: key,
      messages: [],
      subject: row.subject ?? "",
      unreadCount: 0,
    };
    thread.messages.push(row);
    if (!row.read) thread.unreadCount += 1;
    if (!thread.subject && row.subject) thread.subject = row.subject;
    threads.set(key, thread);
  });

  const list = [...threads.values()];
  list.forEach((thread) => {
    thread.messages.sort((a, b) => (a.created_at ?? "").localeCompare(b.created_at ?? ""));
  });
  list.sort((a, b) => {
    const at = a.messages[a.messages.length - 1]?.created_at ?? "";
    const bt = b.messages[b.messages.length - 1]?.created_at ?? "";
    return bt.localeCompare(at);
  });
  return list;
}
