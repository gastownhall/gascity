// Options cache: shared across panels. The Go `/api/options`
// endpoint parallel-fetched rigs + active sessions + open beads +
// mail with a 30-second cache. The SPA keeps the same shape so
// autocomplete menus (assignee pickers, rig pickers, reply-to
// lookups) can share one backing store.

import { api, cityScope } from "../api";

export interface Options {
  agents: string[];
  rigs: string[];
  sessions: { id: string; label: string }[];
  beads: { id: string; title: string }[];
  mail: { id: string; subject: string }[];
  fetchedAt: number;
}

const TTL_MS = 30_000;
let cached: Options | null = null;
let inflight: Promise<Options> | null = null;

export async function getOptions(force = false): Promise<Options> {
  const now = Date.now();
  if (!force && cached && now - cached.fetchedAt < TTL_MS) return cached;
  if (inflight) return inflight;
  inflight = fetchOptions().then((o) => {
    cached = o;
    inflight = null;
    return o;
  }).catch((e) => {
    inflight = null;
    throw e;
  });
  return inflight;
}

async function fetchOptions(): Promise<Options> {
  const city = cityScope();
  const empty: Options = { agents: [], rigs: [], sessions: [], beads: [], mail: [], fetchedAt: Date.now() };
  if (!city) return empty;

  const [rigsR, sessionsR, beadsR, mailR] = await Promise.all([
    api.GET("/v0/city/{cityName}/rigs", { params: { path: { cityName: city } } }),
    api.GET("/v0/city/{cityName}/sessions", {
      params: { path: { cityName: city }, query: { peek: "true" } },
    }),
    api.GET("/v0/city/{cityName}/beads", {
      params: { path: { cityName: city }, query: { status: "open" } },
    }),
    api.GET("/v0/city/{cityName}/mail", { params: { path: { cityName: city } } }),
  ]);

  return {
    agents: [...new Set((sessionsR.data?.items ?? []).map((s) => s.template ?? "").filter(Boolean))].sort(),
    rigs: (rigsR.data?.items ?? []).map((r) => r.name ?? "").filter(Boolean),
    sessions: (sessionsR.data?.items ?? []).map((s) => ({
      id: s.id ?? "",
      label: s.template ?? s.session_name ?? s.id ?? "",
    })),
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
  cached = null;
}
