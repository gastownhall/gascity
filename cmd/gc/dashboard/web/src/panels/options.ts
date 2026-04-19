// Options cache: shared across panels. The Go `/api/options`
// endpoint parallel-fetched rigs + active sessions + open beads +
// mail with a 30-second cache. The SPA keeps the same shape so
// autocomplete menus (assignee pickers, rig pickers, reply-to
// lookups) can share one backing store.

import { api, cityScope } from "../api";
import { logDebug, logWarn } from "../logger";

export interface Options {
  agents: string[];
  rigs: string[];
  sessions: { id: string; label: string; recipient: string }[];
  beads: { id: string; title: string }[];
  mail: { id: string; subject: string }[];
  fetchedAt: number;
}

const TTL_MS = 30_000;
const cached = new Map<string, Options>();
const inflight = new Map<string, Promise<Options>>();

export async function getOptions(force = false): Promise<Options> {
  const city = cityScope();
  const now = Date.now();
  const existing = cached.get(city);
  if (!force && existing && now - existing.fetchedAt < TTL_MS) return existing;
  const pending = inflight.get(city);
  if (pending) return pending;
  const next = fetchOptions(city).then((options) => {
    cached.set(city, options);
    inflight.delete(city);
    return options;
  }).catch((error) => {
    inflight.delete(city);
    throw error;
  });
  inflight.set(city, next);
  return next;
}

async function fetchOptions(city: string): Promise<Options> {
  const empty: Options = { agents: [], rigs: [], sessions: [], beads: [], mail: [], fetchedAt: Date.now() };
  if (!city) return empty;

  const [configR, rigsR, beadsR, mailR] = await Promise.all([
    api.GET("/v0/city/{cityName}/config", {
      params: { path: { cityName: city } },
    }),
    api.GET("/v0/city/{cityName}/rigs", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "open" } },
    }),
    api.GET("/v0/city/{cityName}/mail", { params: { path: { cityName: city } } }),
  ]);

  if (configR.error) {
    logWarn("options", "Config options request failed", {
      city,
      detail: configR.error.detail ?? null,
    });
  }

  const agentOptions = (configR.data?.agents ?? [])
    .map((agent) => ({
      id: agent.name ?? "",
      label: agent.name ?? "",
      recipient: agent.name ?? "",
    }))
    .filter((agent) => agent.recipient !== "");

  logDebug("options", "Fetched options", {
    agentOptions: agentOptions.map((agent) => agent.recipient),
    beads: beadsR.data?.items?.length ?? 0,
    city,
    configAgents: configR.data?.agents?.length ?? 0,
    mail: mailR.data?.items?.length ?? 0,
    rigs: rigsR.data?.items?.length ?? 0,
  });

  return {
    agents: [...new Set(agentOptions.map((agent) => agent.recipient))].sort(),
    rigs: (rigsR.data?.items ?? []).map((r) => r.name ?? "").filter(Boolean),
    sessions: agentOptions,
    beads: (beadsR.data?.items ?? []).map((b) => ({
      id: b.id ?? "",
      title: b.title ?? "",
    })),
    mail: (mailR.data?.items ?? []).map((m) => ({
      id: m.id ?? "",
      subject: m.subject ?? "",
    })),
    fetchedAt: Date.now(),
  };
}

export function invalidateOptions(): void {
  cached.clear();
  inflight.clear();
}
