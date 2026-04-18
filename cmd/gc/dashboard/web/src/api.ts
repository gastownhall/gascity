// Typed API client for the Gas City supervisor.
//
// `openapi-fetch` is a tiny typed `fetch` wrapper; every call is
// path-, body-, and response-typed against `schema.d.ts`, which
// `openapi-typescript` generates from `internal/api/openapi.json`.
// No hand-written URL construction, JSON serialization, or response
// parsing lives in this file — that is the whole point of the
// migration.

import createClient from "openapi-fetch";
import type { components, paths } from "./generated/schema";
import { logDebug, logError, logWarn } from "./logger";
import { cityScope as currentCityScope } from "./state";

// The Go static server injects the supervisor's base URL into a
// `<meta name="supervisor-url">` tag at page-load time so the SPA
// can call the supervisor cross-origin. Same-origin-only dashboards
// (dev with the Vite proxy) set this to an empty string and rely on
// relative URLs.
function supervisorBaseURL(): string {
  const meta = document.querySelector<HTMLMetaElement>(
    'meta[name="supervisor-url"]',
  );
  const raw = meta?.content ?? "";
  return raw.replace(/\/+$/, "");
}

// cityScope reads the dashboard's current city from the `?city=...`
// query parameter. Every per-city API call uses this value; callers
// must handle the empty case (no city selected → supervisor-scope
// operations only).
export function cityScope(): string {
  return currentCityScope();
}

export function hasCityScope(): boolean {
  return cityScope() !== "";
}

export type DashboardSchema = components["schemas"];
export type CityEventRecord = DashboardSchema["Event"];
export type CityEventStreamEnvelope = DashboardSchema["EventStreamEnvelope"];
export type SupervisorEventRecord = DashboardSchema["TaggedEvent"];
export type SupervisorEventStreamEnvelope = DashboardSchema["TaggedEventStreamEnvelope"];
export type HeartbeatEvent = DashboardSchema["HeartbeatEvent"];
export type SessionRecord = DashboardSchema["SessionResponse"];
export type MailRecord = DashboardSchema["Message"];
export type BeadRecord = DashboardSchema["Bead"];
export type RigRecord = DashboardSchema["RigResponse"];
export type ServiceStatusRecord = DashboardSchema["Status"];
export type CityInfoRecord = DashboardSchema["CityInfo"];

// The supervisor's CSRF middleware requires `X-GC-Request: true` on
// every mutation. Attaching it as a default request editor means
// callers never have to remember the header.
export const api = createClient<paths>({
  baseUrl: supervisorBaseURL(),
  headers: {
    "X-GC-Request": "true",
  },
});

api.use({
  async onError({ error, request, schemaPath }) {
    logError("api", "Request failed", {
      error,
      method: request.method,
      schemaPath,
      url: request.url,
    });
    return error instanceof Error ? error : new Error(String(error));
  },
  async onRequest({ params, request, schemaPath }) {
    logDebug("api", "Request start", {
      method: request.method,
      params,
      schemaPath,
      url: request.url,
    });
    return undefined;
  },
  async onResponse({ request, response, schemaPath }) {
    const details = {
      method: request.method,
      ok: response.ok,
      schemaPath,
      status: response.status,
      url: request.url,
    };
    if (!response.ok || response.status >= 400) {
      logWarn("api", "Request response", details);
      return undefined;
    }
    logDebug("api", "Request response", details);
    return undefined;
  },
});

export const supervisorAPI = {
  cities() {
    return api.GET("/v0/cities");
  },

  events(query: { actor?: string; since?: string; type?: string } = {}) {
    return api.GET("/v0/events", { params: { query } });
  },

  health() {
    return api.GET("/health");
  },
};

export function cityAPI(cityName: string) {
  return {
    bead(id: string) {
      return api.GET("/v0/city/{cityName}/bead/{id}", { params: { path: { cityName, id } } });
    },

    beadAssign(id: string, assignee: string) {
      return api.POST("/v0/city/{cityName}/bead/{id}/assign", {
        params: { path: { cityName, id } },
        body: { assignee },
      });
    },

    beadClose(id: string) {
      return api.POST("/v0/city/{cityName}/bead/{id}/close", { params: { path: { cityName, id } } });
    },

    beadDeps(id: string) {
      return api.GET("/v0/city/{cityName}/bead/{id}/deps", { params: { path: { cityName, id } } });
    },

    beadReopen(id: string) {
      return api.POST("/v0/city/{cityName}/bead/{id}/reopen", { params: { path: { cityName, id } } });
    },

    beadUpdate(id: string, body: { labels?: string[]; priority?: number }) {
      return api.POST("/v0/city/{cityName}/bead/{id}/update", {
        params: { path: { cityName, id } },
        body,
      });
    },

    beads(query: { label?: string; limit?: number; status?: string } = {}) {
      return api.GET("/v0/city/{cityName}/beads", {
        params: { path: { cityName }, query },
      });
    },

    createBead(body: {
      assignee?: string;
      description?: string;
      priority?: number;
      rig?: string;
      title: string;
    }) {
      return api.POST("/v0/city/{cityName}/beads", {
        params: { path: { cityName } },
        body,
      });
    },

    convoy(id: string) {
      return api.GET("/v0/city/{cityName}/convoy/{id}", { params: { path: { cityName, id } } });
    },

    convoyAdd(id: string, items: string[]) {
      return api.POST("/v0/city/{cityName}/convoy/{id}/add", {
        params: { path: { cityName, id } },
        body: { items },
      });
    },

    convoys(limit = 200) {
      return api.GET("/v0/city/{cityName}/convoys", {
        params: { path: { cityName }, query: { limit } },
      });
    },

    createConvoy(title: string, items: string[]) {
      return api.POST("/v0/city/{cityName}/convoys", {
        params: { path: { cityName } },
        body: { title, items },
      });
    },

    events(query: { actor?: string; limit?: number; since?: string; type?: string } = {}) {
      return api.GET("/v0/city/{cityName}/events", {
        params: { path: { cityName }, query },
      });
    },

    mail(query: { limit?: number; status?: string } = {}) {
      return api.GET("/v0/city/{cityName}/mail", {
        params: { path: { cityName }, query },
      });
    },

    rigs(options: { git?: boolean } = {}) {
      return api.GET("/v0/city/{cityName}/rigs", {
        params: {
          path: { cityName },
          query: { git: options.git ? "true" : undefined },
        },
      });
    },

    rigAction(name: string, action: string) {
      return api.POST("/v0/city/{cityName}/rig/{name}/{action}", {
        params: { path: { cityName, name, action } },
      });
    },

    services() {
      return api.GET("/v0/city/{cityName}/services", { params: { path: { cityName } } });
    },

    serviceRestart(name: string) {
      return api.POST("/v0/city/{cityName}/service/{name}/restart", {
        params: { path: { cityName, name } },
      });
    },

    sessions(query: { peek?: boolean; state?: string } = {}) {
      return api.GET("/v0/city/{cityName}/sessions", {
        params: {
          path: { cityName },
          query: {
            peek: query.peek ? "true" : undefined,
            state: query.state,
          },
        },
      });
    },

    sling(body: { bead: string; rig?: string; target: string }) {
      return api.POST("/v0/city/{cityName}/sling", {
        params: { path: { cityName } },
        body,
      });
    },

    status() {
      return api.GET("/v0/city/{cityName}/status", { params: { path: { cityName } } });
    },
  };
}
