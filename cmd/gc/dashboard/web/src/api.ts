// Typed API client for the Gas City supervisor.
//
// `openapi-fetch` is a tiny typed `fetch` wrapper; every call is
// path-, body-, and response-typed against `schema.d.ts`, which
// `openapi-typescript` generates from `internal/api/openapi.json`.
// No hand-written URL construction, JSON serialization, or response
// parsing lives in this file — that is the whole point of the
// migration.

import createClient from "openapi-fetch";
import type { paths } from "./generated/schema";

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
  const params = new URLSearchParams(window.location.search);
  return (params.get("city") ?? "").trim();
}

// The supervisor's CSRF middleware requires `X-GC-Request: true` on
// every mutation. Attaching it as a default request editor means
// callers never have to remember the header.
export const api = createClient<paths>({
  baseUrl: supervisorBaseURL(),
  headers: {
    "X-GC-Request": "true",
  },
});
