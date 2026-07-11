// Static "test-city" supervisor fixture — a deterministic, seeded snapshot of
// GC-supervisor state (rigs/agents/sessions/beads/runs/mail/events/health)
// across many lifecycle states. It exists to drive the dashboard's
// supervisor-backed tabs (Agents/Runs/Beads/Mail/Sessions/Health) WITHOUT a
// live `gc` supervisor or a live city — filling the gap the dashboard-owned
// `SNAPSHOT_USE_FIXTURES` path leaves untouched (it only covers `/api/*`, not
// the `/gc-supervisor/*` proxy).
//
// Everything is typed against the GENERATED supervisor client types, so a wire
// shape change at OpenAPI-regeneration time surfaces here as a compile error,
// and `test-city.test.ts` re-validates every payload against the generated zod
// schemas as a runtime regression gate.
//
// Timestamps are derived from an injected `nowMs` so seeded mail/events land
// inside the views' relative time windows (24h/7d). Pass a fixed `nowMs` for
// deterministic assertions; omit it (defaults to `Date.now()`) when serving a
// live dashboard.

import type {
  AgentResponse,
  Bead,
  CityInfo,
  Dep,
  HealthOutputBody,
  Message,
  MonitorFeedItemResponse,
  SessionPendingResponse,
  SessionResponse,
  StatusBody,
  SupervisorHealthOutputBody,
  TypedEventStreamEnvelope,
} from '../../generated/gc-supervisor-client/index.js';
import {
  AGENT_SPECS,
  type AgentSpec,
  BEAD_SPECS,
  type BeadSpec,
  type BeadStatus,
  MAIL_SPECS,
  type MailSpec,
  RUN_SPECS,
  type RunSpec,
  WORKFLOW_GROUPS,
} from './seeds.js';

export const TEST_CITY_NAME = 'test-city';

const MIN = 60_000;
const HOUR = 60 * MIN;

/** The five seeded rigs the fixture spreads work across. */
export const TEST_CITY_RIGS = ['web', 'api', 'data', 'ops', 'docs'] as const;

function specToBead(spec: BeadSpec, iso: (offsetMs: number) => string): Bead {
  const dependencies: Dep[] = (spec.dependsOn ?? []).map((depId) => ({
    depends_on_id: depId,
    issue_id: spec.id,
    type: 'blocks',
  }));
  const bead: Bead = {
    id: spec.id,
    title: spec.title,
    status: spec.status,
    issue_type: spec.issue_type,
    created_at: iso(spec.agedH * HOUR),
  };
  if (spec.updatedH !== undefined) bead.updated_at = iso(spec.updatedH * HOUR);
  if (spec.assignee !== undefined) bead.assignee = spec.assignee;
  if (spec.priority !== undefined) bead.priority = spec.priority;
  if (spec.labels !== undefined) bead.labels = spec.labels;
  if (spec.parent !== undefined) bead.parent = spec.parent;
  if (spec.description !== undefined) bead.description = spec.description;
  if (spec.ephemeral !== undefined) bead.ephemeral = spec.ephemeral;
  if (spec.metadata !== undefined) bead.metadata = spec.metadata;
  if (dependencies.length > 0) bead.dependencies = dependencies;
  return bead;
}

function specToAgent(spec: AgentSpec, iso: (offsetMs: number) => string): AgentResponse {
  const agent: AgentResponse = {
    name: spec.name,
    state: spec.state,
    available: spec.available,
    running: spec.running,
    suspended: spec.suspended,
    provider: spec.provider,
    model: spec.model,
    rig: spec.rig,
    pack_derived: false,
  };
  if (spec.activeBead !== undefined) agent.active_bead = spec.activeBead;
  if (spec.contextPct !== undefined) agent.context_pct = spec.contextPct;
  if (spec.activity !== undefined) agent.activity = spec.activity;
  if (spec.unavailableReason !== undefined) agent.unavailable_reason = spec.unavailableReason;
  if (spec.sessionName !== undefined) {
    agent.session = {
      name: spec.sessionName,
      attached: spec.sessionAttached ?? false,
      ...(spec.lastActivityMin !== undefined
        ? { last_activity: iso(spec.lastActivityMin * MIN) }
        : {}),
    };
  }
  return agent;
}

/** Sessions are derived from the agents that carry a `sessionName`. */
function buildSessions(
  agents: readonly AgentSpec[],
  iso: (offsetMs: number) => string,
): SessionResponse[] {
  return agents
    .filter((a) => a.sessionName !== undefined)
    .map((a) => {
      const session: SessionResponse = {
        id: a.sessionName as string,
        session_name: a.sessionName as string,
        title: `${a.rig} · ${a.name.split('/')[1] ?? a.name}`,
        template: 'pool-worker',
        state: a.state,
        provider: a.provider,
        attached: a.sessionAttached ?? false,
        running: a.running,
        created_at: iso(6 * HOUR),
        rig: a.rig,
        model: a.model,
      };
      if (a.activeBead !== undefined) session.active_bead = a.activeBead;
      if (a.contextPct !== undefined) session.context_pct = a.contextPct;
      if (a.activity !== undefined) session.activity = a.activity;
      if (a.lastActivityMin !== undefined) session.last_active = iso(a.lastActivityMin * MIN);
      return session;
    });
}

function specToMessage(spec: MailSpec, iso: (offsetMs: number) => string): Message {
  const msg: Message = {
    id: spec.id,
    from: spec.from,
    to: spec.to,
    subject: spec.subject,
    body: spec.body,
    read: spec.read,
    created_at: iso(spec.agedMin * MIN),
  };
  if (spec.threadId !== undefined) msg.thread_id = spec.threadId;
  if (spec.rig !== undefined) msg.rig = spec.rig;
  if (spec.priority !== undefined) msg.priority = spec.priority;
  return msg;
}

function specToRun(spec: RunSpec, iso: (offsetMs: number) => string): MonitorFeedItemResponse {
  return {
    id: spec.id,
    workflow_id: spec.workflowId,
    root_bead_id: spec.rootBeadId,
    root_store_ref: `rig:${spec.rig}`,
    store_ref: `rig:${spec.rig}`,
    scope_kind: 'city',
    scope_ref: TEST_CITY_NAME,
    title: spec.title,
    target: spec.target,
    type: spec.type,
    status: spec.status,
    started_at: iso(spec.startedH * HOUR),
    updated_at: iso(spec.updatedH * HOUR),
    detail_available: true,
    run_detail_available: true,
  };
}

function buildEvents(
  beadById: Map<string, Bead>,
  iso: (offsetMs: number) => string,
): TypedEventStreamEnvelope[] {
  const bead = (id: string): Bead => {
    const b = beadById.get(id);
    if (b === undefined)
      throw new Error(`test-city fixture: unknown bead id "${id}" in event seed`);
    return b;
  };
  // seq descending in recency; the Events read sorts by seq desc.
  return [
    {
      type: 'session.crashed',
      seq: 108,
      ts: iso(40 * MIN),
      actor: 'docs/writer',
      subject: 'gc-docs-1',
      message: 'session crashed (exit 1) during tc-docs-api-ref',
      payload: { session_id: 'gc-docs-1', reason: 'exit 1' },
    },
    {
      type: 'order.failed',
      seq: 107,
      ts: iso(38 * MIN),
      actor: 'supervisor',
      subject: 'order-4821',
      message: 'order dispatch failed: no available agent in rig docs',
      payload: {},
    },
    {
      type: 'bead.updated',
      seq: 106,
      ts: iso(6 * HOUR),
      actor: 'api/worker',
      subject: 'tc-tax-service',
      message: 'tc-tax-service moved to in_progress',
      payload: { bead: bead('tc-tax-service') },
    },
    {
      type: 'mail.sent',
      seq: 105,
      ts: iso(6 * MIN),
      actor: 'api/worker',
      subject: 'tc-mail-1',
      message: 'BLOCKED: tax-rating service missing credentials',
      payload: { rig: 'api' },
    },
    {
      type: 'session.suspended',
      seq: 104,
      ts: iso(95 * MIN),
      actor: 'data/etl',
      subject: 'data/etl',
      message: 'suspended pending tc-data-warehouse',
      payload: {},
    },
    {
      type: 'bead.created',
      seq: 103,
      ts: iso(6 * HOUR),
      actor: 'web/builder',
      subject: 'tc-convoy-input-1',
      message: 'input convoy created for tc-feat-search',
      payload: { bead: bead('tc-convoy-input-1') },
    },
    {
      type: 'bead.closed',
      seq: 102,
      ts: iso(9 * 24 * HOUR),
      actor: 'web/builder',
      subject: 'tc-checkout-cart',
      message: 'tc-checkout-cart closed',
      payload: { bead: bead('tc-checkout-cart') },
    },
    {
      type: 'session.woke',
      seq: 101,
      ts: iso(2 * HOUR),
      actor: 'web/builder',
      subject: 'gc-web-1',
      message: 'session resumed work on tc-bug-login-loop',
      payload: {},
    },
  ];
}

function countByStatus(beads: readonly Bead[], status: BeadStatus): number {
  return beads.filter((b) => b.status === status).length;
}

/** The fully-built, typed snapshot the matcher and stream renderer consume. */
export interface TestCitySupervisorData {
  cities: CityInfo[];
  beads: Bead[];
  agents: AgentResponse[];
  sessions: SessionResponse[];
  mail: Message[];
  formulaFeed: MonitorFeedItemResponse[];
  events: TypedEventStreamEnvelope[];
  /**
   * Pending-interaction poll responses keyed by session id. The shell polls
   * `/session/<id>/pending` for every session; the stuck agent's session has a
   * real pending approval, everything else is "supported, nothing pending".
   */
  pendingBySession: Record<string, SessionPendingResponse>;
  cityHealth: HealthOutputBody;
  supervisorHealth: SupervisorHealthOutputBody;
  status: StatusBody;
}

/** Session id of the stuck agent that has a pending operator approval. */
const PENDING_SESSION_ID = 'gc-api-1';

function buildPendingBySession(
  sessions: readonly SessionResponse[],
): Record<string, SessionPendingResponse> {
  const out: Record<string, SessionPendingResponse> = {};
  for (const session of sessions) {
    out[session.id] =
      session.id === PENDING_SESSION_ID
        ? {
            supported: true,
            pending: {
              kind: 'approval',
              request_id: 'tc-req-tax-key',
              prompt: 'Approve using the staging tax key until the prod key lands?',
              options: ['approve', 'deny'],
            },
          }
        : { supported: true };
  }
  return out;
}

/**
 * Build the seeded snapshot. Timestamps are offsets from `nowMs` so seeded
 * mail/events stay inside the views' relative windows. Pass a fixed `nowMs`
 * for deterministic test assertions.
 */
export function buildTestCitySupervisorData(nowMs: number = Date.now()): TestCitySupervisorData {
  const iso = (offsetMs: number): string => new Date(nowMs - offsetMs).toISOString();

  const beads = [...BEAD_SPECS, ...WORKFLOW_GROUPS].map((s) => specToBead(s, iso));
  const beadById = new Map(beads.map((b) => [b.id, b]));
  const agents = AGENT_SPECS.map((s) => specToAgent(s, iso));
  const sessions = buildSessions(AGENT_SPECS, iso);
  const mail = MAIL_SPECS.map((s) => specToMessage(s, iso));
  const formulaFeed = RUN_SPECS.map((s) => specToRun(s, iso));
  const events = buildEvents(beadById, iso);

  const cities: CityInfo[] = [
    { name: TEST_CITY_NAME, path: `/tmp/${TEST_CITY_NAME}`, running: true, status: 'ok' },
  ];
  const cityHealth: HealthOutputBody = {
    city: TEST_CITY_NAME,
    status: 'ok',
    uptime_sec: 4 * 3600,
    version: 'test-fixture',
  };
  const supervisorHealth: SupervisorHealthOutputBody = {
    status: 'ok',
    uptime_sec: 6 * 3600,
    cities_running: 1,
    cities_total: 1,
    build_id: 'test-fixture',
    version: 'test-fixture',
  };
  const status: StatusBody = {
    name: TEST_CITY_NAME,
    path: `/tmp/${TEST_CITY_NAME}`,
    suspended: false,
    uptime_sec: 4 * 3600,
    version: 'test-fixture',
    agent_count: agents.length,
    running: agents.filter((a) => a.running).length,
    rig_count: TEST_CITY_RIGS.length,
    agents: {
      total: agents.length,
      running: agents.filter((a) => a.running).length,
      suspended: agents.filter((a) => a.suspended).length,
      quarantined: 0,
    },
    rigs: { suspended: 0, total: TEST_CITY_RIGS.length },
    mail: { total: mail.length, unread: mail.filter((m) => !m.read).length },
    work: {
      open: countByStatus(beads, 'open'),
      ready: countByStatus(beads, 'open'),
      in_progress: countByStatus(beads, 'in_progress'),
      hooked: 0,
      review: 0,
    },
  };

  return {
    cities,
    beads,
    agents,
    sessions,
    mail,
    formulaFeed,
    events,
    pendingBySession: buildPendingBySession(sessions),
    cityHealth,
    supervisorHealth,
    status,
  };
}
