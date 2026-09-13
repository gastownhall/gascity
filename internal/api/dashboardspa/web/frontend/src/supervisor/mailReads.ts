import type { MailListBody, Message } from 'gas-city-dashboard-shared/gc-supervisor';
import { activeCityOrThrow } from '../api/cityBase';
import { SupervisorApiError, supervisorApi } from './client';

/**
 * Operator identity needed to resolve which mailbox a viewing alias maps to
 * (gascity-dashboard-bhvn). The operator's mail is addressed to/from the WIRE
 * alias, not the display name, so the inbox/sent filter swaps the display alias
 * for the wire alias. `OperatorConfig` from OperatorConfigContext is structurally
 * compatible, so callers pass it directly without a downward UI-layer import.
 */
export interface MailOperatorIdentity {
  readonly operatorAlias: string;
  readonly operatorWireAlias: string;
}

export const MAIL_HISTORY_LIMITS = [100, 500, 1000] as const;
export type MailHistoryLimit = (typeof MAIL_HISTORY_LIMITS)[number];
export const DEFAULT_MAIL_HISTORY_LIMIT: MailHistoryLimit = 100;
export const MAIL_HISTORY_WINDOWS = ['24h', '7d', 'all'] as const;
export type MailHistoryWindow = (typeof MAIL_HISTORY_WINDOWS)[number];
export const DEFAULT_MAIL_HISTORY_WINDOW: MailHistoryWindow = 'all';

const MAIL_WINDOW_MS: Record<Exclude<MailHistoryWindow, 'all'>, number> = {
  '24h': 24 * 60 * 60 * 1000,
  '7d': 7 * 24 * 60 * 60 * 1000,
};

export type SupervisorMailItem = Message;
export type SupervisorMailBox = 'inbox' | 'sent' | 'all';

export type SupervisorMailList = Omit<MailListBody, 'items'> & {
  items: SupervisorMailItem[];
  upstream_total?: number;
  upstream_fetched?: number;
  fetch_limit?: number;
};

export async function listSupervisorMail(
  box: SupervisorMailBox,
  alias: string,
  operator: MailOperatorIdentity,
  limit: MailHistoryLimit = DEFAULT_MAIL_HISTORY_LIMIT,
  window: MailHistoryWindow = DEFAULT_MAIL_HISTORY_WINDOW,
  nowMs: number = Date.now(),
): Promise<SupervisorMailList> {
  const cityName = activeCityOrThrow('list supervisor mail');
  const mailList = await supervisorApi().listMail(cityName, { limit });
  const rawItems = mailList.items ?? [];
  const filtered = filterByClockWindow(filterByBox(rawItems, box, alias, operator), window, nowMs);
  filtered.sort(sortNewestFirst);
  return {
    ...mailList,
    items: filtered,
    total: filtered.length,
    upstream_total: rawItems.length,
    upstream_fetched: rawItems.length,
    fetch_limit: limit,
  };
}

export async function fetchSupervisorMailThread(
  threadId: string,
  alias: string,
  operator: MailOperatorIdentity,
  limit: MailHistoryLimit = DEFAULT_MAIL_HISTORY_LIMIT,
): Promise<SupervisorMailList> {
  const cityName = activeCityOrThrow('fetch supervisor mail thread');
  try {
    const thread = await supervisorApi().mailThread(cityName, threadId);
    return normalizeThread(thread);
  } catch (err) {
    if (!(err instanceof SupervisorApiError) || err.status !== 404) throw err;
    const mailList = await listSupervisorMail('all', alias, operator, limit);
    const items = mailList.items.filter((mail) => mail.thread_id === threadId);
    return normalizeThread({
      ...mailList,
      items,
      total: items.length,
    });
  }
}

function normalizeThread(mailList: MailListBody): SupervisorMailList {
  const items = dedupeById(mailList.items ?? []).sort(sortOldestFirst);
  return {
    ...mailList,
    items,
    total: items.length,
  };
}

function filterByBox(
  items: ReadonlyArray<SupervisorMailItem>,
  box: SupervisorMailBox,
  alias: string,
  operator: MailOperatorIdentity,
): SupervisorMailItem[] {
  const resolvedAlias = supervisorMailAlias(alias, operator);
  if (box === 'all') return [...items];
  if (box === 'inbox') {
    return items.filter((mail) => mail.to.toLowerCase() === resolvedAlias);
  }
  return items.filter((mail) => mail.from.toLowerCase() === resolvedAlias);
}

function filterByClockWindow(
  items: ReadonlyArray<SupervisorMailItem>,
  window: MailHistoryWindow,
  nowMs: number,
): SupervisorMailItem[] {
  if (window === 'all') return [...items];
  const cutoffMs = nowMs - MAIL_WINDOW_MS[window];
  return items.filter((mail) => {
    const createdMs = Date.parse(mail.created_at);
    return Number.isFinite(createdMs) && createdMs >= cutoffMs;
  });
}

function supervisorMailAlias(alias: string, operator: MailOperatorIdentity): string {
  const lower = alias.toLowerCase();
  return lower === operator.operatorAlias.toLowerCase() ? operator.operatorWireAlias : lower;
}

function dedupeById(items: ReadonlyArray<SupervisorMailItem>): SupervisorMailItem[] {
  const seen = new Set<string>();
  const deduped: SupervisorMailItem[] = [];
  for (const item of items) {
    if (seen.has(item.id)) continue;
    seen.add(item.id);
    deduped.push(item);
  }
  return deduped;
}

function sortNewestFirst(a: SupervisorMailItem, b: SupervisorMailItem): number {
  return b.created_at.localeCompare(a.created_at);
}

function sortOldestFirst(a: SupervisorMailItem, b: SupervisorMailItem): number {
  return a.created_at.localeCompare(b.created_at);
}
