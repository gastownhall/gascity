// Regression gate for the static test-city supervisor fixture.
//
// Two jobs:
//   1. Wire-shape conformance — every seeded payload and every matched response
//      envelope is validated against the GENERATED zod schemas. When the
//      supervisor OpenAPI is regenerated and a shape drifts, this fails in CI,
//      forcing the fixture to be brought back into line (the whole point of
//      keeping the seed typed against the generated client).
//   2. Coverage invariants — the seed must actually exercise the lifecycle
//      states the dashboard tabs render (blocked beads, a stuck agent, unread
//      mail, an epic with children, runs across statuses, an attention event),
//      so a future edit can't quietly hollow the fixture out.

import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  buildTestCitySupervisorData,
  matchTestCitySupervisorRequest,
  renderTestCityEventStream,
  TEST_CITY_NAME,
} from './index.js';
import {
  zAgentResponse,
  zBead,
  zFormulaFeedBody,
  zHealthOutputBody,
  zListBodyAgentResponse,
  zListBodyBead,
  zListBodySessionResponse,
  zListBodyWireEvent,
  zMailListBody,
  zMessage,
  zMonitorFeedItemResponse,
  zSessionResponse,
  zStatusBody,
  zSupervisorCitiesOutputBody,
  zSupervisorHealthOutputBody,
  zTypedEventStreamEnvelope,
} from '../../generated/gc-supervisor-client/zod.gen.js';

// Fixed clock so assertions are deterministic. Date.parse (not Date.now) keeps
// this reproducible across runs.
const NOW = Date.parse('2026-06-06T12:00:00.000Z');
const data = buildTestCitySupervisorData(NOW);

const cityPath = (suffix: string): string => `/gc-supervisor/v0/city/${TEST_CITY_NAME}${suffix}`;

void test('every seeded item conforms to the generated supervisor zod schemas', () => {
  for (const bead of data.beads) zBead.parse(bead);
  for (const agent of data.agents) zAgentResponse.parse(agent);
  for (const session of data.sessions) zSessionResponse.parse(session);
  for (const message of data.mail) zMessage.parse(message);
  for (const run of data.formulaFeed) zMonitorFeedItemResponse.parse(run);
  for (const event of data.events) zTypedEventStreamEnvelope.parse(event);
  zHealthOutputBody.parse(data.cityHealth);
  zSupervisorHealthOutputBody.parse(data.supervisorHealth);
  zStatusBody.parse(data.status);
});

void test('matched list envelopes conform to their generated zod schemas', () => {
  const parseMatched = (suffix: string, schema: { parse: (v: unknown) => unknown }): void => {
    const res = matchTestCitySupervisorRequest(data, 'GET', cityPath(suffix));
    assert.ok(res !== null, `expected a fixture response for ${suffix}`);
    schema.parse(JSON.parse(res.body));
  };
  parseMatched('/beads', zListBodyBead);
  parseMatched('/agents', zListBodyAgentResponse);
  parseMatched('/sessions', zListBodySessionResponse);
  parseMatched('/mail', zMailListBody);
  parseMatched('/events', zListBodyWireEvent);
  parseMatched('/formulas/feed', zFormulaFeedBody);
  parseMatched('/status', zStatusBody);
  parseMatched('/health', zHealthOutputBody);

  const cities = matchTestCitySupervisorRequest(data, 'GET', '/gc-supervisor/v0/cities');
  assert.ok(cities !== null, 'expected a fixture response for /v0/cities');
  const citiesBody = zSupervisorCitiesOutputBody.parse(JSON.parse(cities.body));
  assert.ok(
    citiesBody.items?.some((c) => c.name === TEST_CITY_NAME),
    'cities list must advertise the test city so the shell can resolve it',
  );
});

void test('bead seed exercises every status and the key issue types', () => {
  const statuses = new Set(data.beads.map((b) => b.status));
  for (const s of ['open', 'in_progress', 'blocked', 'closed']) {
    assert.ok(statuses.has(s), `bead seed is missing status "${s}"`);
  }
  const types = new Set(data.beads.map((b) => b.issue_type));
  for (const t of ['epic', 'feature', 'bug', 'task', 'chore', 'decision']) {
    assert.ok(types.has(t), `bead seed is missing issue_type "${t}"`);
  }
  // ~30-50 beads per the bead's scope.
  assert.ok(data.beads.length >= 30, `expected >=30 beads, got ${data.beads.length}`);

  // An epic with children wired by `parent`.
  const epic = data.beads.find((b) => b.issue_type === 'epic');
  assert.ok(epic, 'expected at least one epic');
  const children = data.beads.filter((b) => b.parent === epic?.id);
  assert.ok(children.length >= 2, 'expected the epic to have >=2 children');

  // A blocked bead wired by real dependencies.
  const blockedWithDeps = data.beads.find(
    (b) => b.status === 'blocked' && (b.dependencies?.length ?? 0) > 0,
  );
  assert.ok(blockedWithDeps, 'expected a blocked bead with dependencies');
});

void test('agent seed surfaces a stuck "needs you" agent and varied states', () => {
  const stuck = data.agents.find(
    (a) => a.state === 'stuck' || (a.unavailable_reason?.includes('needs you') ?? false),
  );
  assert.ok(stuck, 'expected a stuck / needs-you agent');
  const states = new Set(data.agents.map((a) => a.state));
  assert.ok(states.size >= 4, `expected varied agent states, got ${[...states].join(',')}`);
  assert.ok(
    data.agents.some((a) => a.suspended),
    'expected at least one suspended agent',
  );
});

void test('mail seed has unread and threaded messages', () => {
  assert.ok(
    data.mail.some((m) => !m.read),
    'expected at least one unread message',
  );
  const threaded = data.mail.filter((m) => m.thread_id !== undefined);
  assert.ok(threaded.length >= 2, 'expected a threaded conversation');
  const threadId = threaded[0]?.thread_id;
  assert.ok(threadId);
  const res = matchTestCitySupervisorRequest(data, 'GET', cityPath(`/mail/thread/${threadId}`));
  assert.ok(res !== null);
  const body = JSON.parse(res.body) as { items: unknown[] };
  assert.ok(body.items.length >= 2, 'thread detail should return the threaded messages');
});

void test('run feed spans running, blocked, failed and done', () => {
  const statuses = new Set(data.formulaFeed.map((r) => r.status));
  for (const s of ['running', 'blocked', 'failed', 'done']) {
    assert.ok(statuses.has(s), `run feed is missing status "${s}"`);
  }
});

void test('every run feed item resolves to a real seeded bead', () => {
  // Guards against dangling root_bead_id/workflow_id references as the feed
  // grows — a feed item that points at a bead the seed never created.
  const beadIds = new Set(data.beads.map((b) => b.id));
  for (const run of data.formulaFeed) {
    assert.ok(
      run.root_bead_id !== undefined && beadIds.has(run.root_bead_id),
      `run ${run.id} has root_bead_id "${run.root_bead_id ?? '(none)'}" with no matching bead`,
    );
  }
});

void test('per-session pending poll is seeded with one real pending approval', () => {
  // The stuck agent's session carries a pending approval; others are benign.
  const stuckSession = data.sessions.find(
    (s) => data.pendingBySession[s.id]?.pending !== undefined,
  );
  assert.ok(stuckSession, 'expected one session with a pending interaction');
  const res = matchTestCitySupervisorRequest(
    data,
    'GET',
    cityPath(`/session/${stuckSession?.id ?? ''}/pending`),
  );
  assert.ok(res !== null);
  const body = JSON.parse(res.body) as { supported: boolean; pending?: { request_id: string } };
  assert.equal(body.supported, true);
  assert.ok(body.pending?.request_id, 'pending interaction must carry a request_id');

  // An unknown session still resolves to a benign supported/no-pending body.
  const other = matchTestCitySupervisorRequest(data, 'GET', cityPath('/session/unknown/pending'));
  assert.ok(other !== null);
  assert.equal((JSON.parse(other.body) as { pending?: unknown }).pending, undefined);
});

void test('event seed includes an attention-class event', () => {
  assert.ok(
    data.events.some((e) => e.type === 'session.crashed' || e.type === 'order.failed'),
    'expected an attention-class event (session.crashed / order.failed)',
  );
  // The rendered SSE body is non-empty and frames events as named `event`s.
  const stream = renderTestCityEventStream(data);
  assert.match(stream, /event: event/);
  assert.match(stream, /retry: 30000/);
});

void test('matcher distinguishes city vs supervisor health and 404s unknown beads', () => {
  const supervisor = matchTestCitySupervisorRequest(data, 'GET', '/gc-supervisor/health');
  assert.ok(supervisor !== null);
  zSupervisorHealthOutputBody.parse(JSON.parse(supervisor.body));

  const city = matchTestCitySupervisorRequest(data, 'GET', cityPath('/health'));
  assert.ok(city !== null);
  zHealthOutputBody.parse(JSON.parse(city.body));

  const missing = matchTestCitySupervisorRequest(data, 'GET', cityPath('/bead/does-not-exist'));
  assert.ok(missing !== null);
  assert.equal(missing.status, 404);

  // Non-GET and unknown paths fall through to null.
  assert.equal(matchTestCitySupervisorRequest(data, 'POST', cityPath('/beads')), null);
  assert.equal(matchTestCitySupervisorRequest(data, 'GET', cityPath('/unknown')), null);
});

void test('builds are deterministic for a fixed clock', () => {
  const a = buildTestCitySupervisorData(NOW);
  const b = buildTestCitySupervisorData(NOW);
  assert.deepEqual(a, b);
});
