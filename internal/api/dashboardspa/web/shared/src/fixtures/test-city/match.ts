// Pure request → response matcher for the test-city supervisor fixture. Given a
// prebuilt snapshot and an inbound `/gc-supervisor/v0/city/<city>/...` request,
// it returns the JSON the dashboard's supervisor reads expect, or `null` when
// the path isn't part of the fixture (the caller decides how to surface that —
// the Playwright installer answers `null` with an explicit 501 so an unseeded
// endpoint is visible rather than silently passing).
//
// The SSE event stream (`/events/stream`) and per-session stream are handled by
// the caller, not here — they need a streaming content-type, not a JSON body.

import type {
  ListBodyAgentResponse,
  ListBodyBead,
  ListBodySessionResponse,
  ListBodyWireEvent,
  MailListBody,
  FormulaFeedBody,
  SupervisorCitiesOutputBody,
  Bead,
} from '../../generated/gc-supervisor-client/index.js';
import type { TestCitySupervisorData } from './data.js';

export interface FixtureResponse {
  status: number;
  contentType: string;
  body: string;
}

const JSON_CONTENT_TYPE = 'application/json';

function json(value: unknown, status = 200): FixtureResponse {
  return { status, contentType: JSON_CONTENT_TYPE, body: JSON.stringify(value) };
}

function applyLimit<T>(items: readonly T[], search: URLSearchParams): T[] {
  const raw = search.get('limit');
  if (raw === null) return [...items];
  const limit = Number.parseInt(raw, 10);
  if (!Number.isFinite(limit) || limit <= 0) return [...items];
  return items.slice(0, limit);
}

function beadMatchesRig(bead: Bead, rig: string): boolean {
  if (bead.labels?.includes(`rig:${rig}`)) return true;
  return bead.assignee?.startsWith(`${rig}/`) ?? false;
}

function filterBeads(data: TestCitySupervisorData, search: URLSearchParams): Bead[] {
  let beads: readonly Bead[] = data.beads;
  const rig = search.get('rig');
  if (rig !== null && rig.length > 0) beads = beads.filter((b) => beadMatchesRig(b, rig));
  const type = search.get('type');
  if (type !== null && type.length > 0) beads = beads.filter((b) => b.issue_type === type);
  const status = search.get('status');
  if (status !== null && status.length > 0) beads = beads.filter((b) => b.status === status);
  return applyLimit(beads, search);
}

function beadIdFrom(pathname: string): string | null {
  // .../bead/<id> — return the id segment (the caller only routes bead detail).
  const match = pathname.match(/\/bead\/([^/]+)\/?$/);
  return match?.[1] ?? null;
}

function threadIdFrom(pathname: string): string | null {
  const match = pathname.match(/\/mail\/thread\/([^/]+)\/?$/);
  return match?.[1] ?? null;
}

const isCityScoped = (pathname: string): boolean => pathname.includes('/v0/city/');

/**
 * Resolve a GET request against the fixture. Returns `null` for unknown paths,
 * non-GET methods, and the streaming endpoints (handled by the caller).
 */
export function matchTestCitySupervisorRequest(
  data: TestCitySupervisorData,
  method: string,
  pathname: string,
  search: URLSearchParams = new URLSearchParams(),
): FixtureResponse | null {
  if (method.toUpperCase() !== 'GET') return null;
  // Streaming endpoints are the caller's responsibility.
  if (pathname.endsWith('/stream')) return null;

  // Non-city-scoped supervisor resource: the city list the shell uses to
  // resolve the active city (redirecting `/` → `/city/test-city/`).
  if (pathname.endsWith('/v0/cities')) {
    const body: SupervisorCitiesOutputBody = {
      items: data.cities,
      total: data.cities.length,
    };
    return json(body);
  }
  if (pathname.endsWith('/health')) {
    return isCityScoped(pathname) ? json(data.cityHealth) : json(data.supervisorHealth);
  }
  if (pathname.endsWith('/status')) {
    return json(data.status);
  }
  // Per-session pending-interaction poll (the shell polls this for every
  // session). Unknown sessions get a benign "supported, nothing pending".
  const pendingMatch = pathname.match(/\/session\/([^/]+)\/pending\/?$/);
  if (pendingMatch !== null) {
    const sessionId = pendingMatch[1] ?? '';
    return json(data.pendingBySession[sessionId] ?? { supported: true });
  }
  if (pathname.endsWith('/agents')) {
    const items = applyLimit(data.agents, search);
    const body: ListBodyAgentResponse = { items, total: items.length };
    return json(body);
  }
  if (pathname.endsWith('/sessions')) {
    const items = applyLimit(data.sessions, search);
    const body: ListBodySessionResponse = { items, total: items.length };
    return json(body);
  }
  if (pathname.endsWith('/formulas/feed')) {
    const body: FormulaFeedBody = { items: data.formulaFeed, partial: false };
    return json(body);
  }
  if (pathname.endsWith('/events')) {
    const items = applyLimit(data.events, search);
    const body: ListBodyWireEvent = { items, total: items.length };
    return json(body);
  }
  // Mail thread detail must be checked before the bare `/mail` collection.
  const threadId = threadIdFrom(pathname);
  if (threadId !== null) {
    const items = data.mail.filter((m) => m.thread_id === threadId);
    const body: MailListBody = { items, total: items.length };
    return json(body);
  }
  if (pathname.endsWith('/mail')) {
    const items = applyLimit(data.mail, search);
    const body: MailListBody = { items, total: items.length };
    return json(body);
  }
  // Bead detail (`.../bead/<id>`) — checked last so collection routes win.
  if (/\/bead\/[^/]+\/?$/.test(pathname)) {
    const id = beadIdFrom(pathname);
    const bead = id === null ? undefined : data.beads.find((b) => b.id === id);
    if (bead === undefined) return json({ error: `bead ${id ?? '?'} not found` }, 404);
    return json(bead);
  }
  if (pathname.endsWith('/beads')) {
    const items = filterBeads(data, search);
    const body: ListBodyBead = { items, total: items.length };
    return json(body);
  }
  return null;
}

/**
 * Render the seeded events as a finite Server-Sent-Events body. The supervisor
 * names its events `event` (not the default `message`); `useGcEvents` listens
 * for both. A long `retry` keeps EventSource from reconnect-storming during a
 * short test once this finite body closes.
 */
export function renderTestCityEventStream(data: TestCitySupervisorData, maxEvents = 4): string {
  const lines: string[] = ['retry: 30000', '', ': test-city fixture stream', ''];
  for (const event of data.events.slice(0, maxEvents)) {
    lines.push('event: event', `data: ${JSON.stringify(event)}`, '');
  }
  return lines.join('\n');
}
