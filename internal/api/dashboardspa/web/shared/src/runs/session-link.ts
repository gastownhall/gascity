import type { RunSnapshotBead } from '../run-snapshot.js';
import type { DashboardSession } from '../dashboard-sessions.js';
import type { RunNodeStatus, RunSessionLink } from '../run-detail.js';
import { SESSION_ID_RE } from '../session-id.js';
import { meta, nonEmpty } from './bead-fields.js';

export interface RunSessionIndex {
  byId: Map<string, DashboardSession>;
  byName: Map<string, DashboardSession>;
  byTemplate: Map<string, DashboardSession[]>;
}

export interface RunSessionLinkContext {
  sessionIndex?: RunSessionIndex;
  scopeRef?: string;
}

export function buildRunSessionIndex(sessions: readonly DashboardSession[]): RunSessionIndex {
  const byId = new Map<string, DashboardSession>();
  const byName = new Map<string, DashboardSession>();
  const byTemplate = new Map<string, DashboardSession[]>();

  for (const session of sessions) {
    remember(byId, session.id, session);
    remember(byName, session.alias, session);
    remember(byName, session.title, session);
    remember(byName, session.session_name, session);
    const template = nonEmpty(session.template);
    if (template) byTemplate.set(template, [...(byTemplate.get(template) ?? []), session]);
  }

  return { byId, byName, byTemplate };
}

export function runSessionLinkFor(
  bead: RunSnapshotBead,
  status: RunNodeStatus,
  context: RunSessionLinkContext = {},
): RunSessionLink | undefined {
  if (status === 'pending' || status === 'ready') return undefined;
  const assignee = nonEmpty(bead.assignee);
  const sessionId = sessionIdFromBead(bead, assignee);
  const sessionName = sessionNameFromBead(bead, assignee, sessionId);
  if (!sessionId && !sessionName) return undefined;
  const rawLink = rawLinkFrom(sessionId, sessionName, assignee);
  const link = resolveRunSessionLink(rawLink, context.sessionIndex);
  // Final gate: link.sessionId is fed straight to the supervisor session
  // routes, which reject anything outside SESSION_ID_RE as "invalid session
  // id". When the index could not resolve the run to a real session (a
  // completed pool/rig-store run has dropped out of the live index) and the
  // recorded handle carries no extractable supervisor id, drop the link so the
  // Session tab degrades to a clean "session not available" state instead of
  // leaking an unvalidated handle into the route.
  if (!SESSION_ID_RE.test(link.sessionId)) return undefined;
  return link;
}

// The bead's recorded session id can be a pool-qualified session NAME
// (e.g. a polecat run records `polecat-gc-333573`, whose real supervisor
// session id is `gc-333573`). Normalize every candidate through
// supervisorSessionIdFrom so the link carries the id the session routes
// expect — otherwise a completed session (absent from the live index, so
// unresolvable by name) leaks its name into the id slot and the Session
// tab rejects it as "invalid session id".
function sessionIdFromBead(
  bead: RunSnapshotBead,
  assignee: string | undefined,
): string | undefined {
  const rawSessionId =
    meta(bead, 'session_id') ??
    meta(bead, 'gc.session_id') ??
    meta(bead, 'gc.sessionId') ??
    assignee;
  return supervisorSessionIdFrom(rawSessionId) ?? rawSessionId;
}

function sessionNameFromBead(
  bead: RunSnapshotBead,
  assignee: string | undefined,
  sessionId: string | undefined,
): string | undefined {
  return (
    meta(bead, 'session_name') ??
    meta(bead, 'gc.session_name') ??
    meta(bead, 'gc.sessionName') ??
    assignee ??
    sessionId
  );
}

function rawLinkFrom(
  sessionId: string | undefined,
  sessionName: string | undefined,
  assignee: string | undefined,
): RunSessionLink {
  const name = sessionName ?? sessionId ?? '';
  return {
    sessionId: sessionId ?? sessionName ?? '',
    sessionName: name,
    assignee: assignee ?? name,
  };
}

function supervisorSessionIdFrom(value: string | undefined): string | undefined {
  const clean = nonEmpty(value);
  if (!clean) return undefined;
  if (SESSION_ID_RE.test(clean)) return clean;
  const suffix = clean.match(/(?:^|[-_/])((?:gc|td|th|[a-z]{4})-[a-z0-9-]{1,32})$/)?.[1];
  if (!suffix || !SESSION_ID_RE.test(suffix)) return undefined;
  return suffix;
}

function resolveRunSessionLink(
  rawLink: RunSessionLink,
  sessionIndex: RunSessionIndex | undefined,
): RunSessionLink {
  if (!sessionIndex) return rawLink;
  const session = resolveRunSessionSummary(rawLink, sessionIndex);
  if (!session) return rawLink;
  return linkForSession(session, rawLink);
}

function resolveRunSessionSummary(
  link: RunSessionLink,
  sessionIndex: RunSessionIndex,
): DashboardSession | null {
  for (const candidate of [link.sessionId, link.sessionName, link.assignee]) {
    const key = nonEmpty(candidate);
    if (!key) continue;
    const exact =
      sessionIndex.byId.get(key) ??
      sessionIndex.byName.get(key) ??
      uniquePreferredSession(sessionIndex.byTemplate.get(key) ?? []);
    if (exact) return exact;
  }
  return null;
}

function linkForSession(session: DashboardSession, rawLink: RunSessionLink): RunSessionLink {
  return {
    sessionId: session.id,
    sessionName:
      nonEmpty(session.alias) ??
      nonEmpty(session.title) ??
      nonEmpty(session.session_name) ??
      nonEmpty(session.template) ??
      rawLink.sessionName,
    assignee:
      rawLink.assignee ||
      nonEmpty(session.template) ||
      nonEmpty(session.alias) ||
      nonEmpty(session.title) ||
      nonEmpty(session.session_name) ||
      session.id,
  };
}

function uniquePreferredSession(sessions: readonly DashboardSession[]): DashboardSession | null {
  if (sessions.length === 0) return null;
  const active = sessions.filter(
    (session) => session.state === 'active' || session.running === true,
  );
  if (active.length === 1) return active[0] ?? null;
  if (sessions.length === 1) return sessions[0] ?? null;
  return null;
}

function remember(
  store: Map<string, DashboardSession>,
  key: string | undefined,
  session: DashboardSession,
): void {
  const clean = nonEmpty(key);
  if (!clean || store.has(clean)) return;
  store.set(clean, session);
}
