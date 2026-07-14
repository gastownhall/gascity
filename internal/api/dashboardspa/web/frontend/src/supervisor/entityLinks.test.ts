import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { setActiveCity } from '../api/cityBase';
import type {
  Bead,
  ListBodyBead,
  ListBodySessionResponse,
  SessionResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { resetSupervisorApiForTests, setSupervisorApiForTests, type SupervisorApi } from './client';
import { loadSupervisorEntityLinks } from './entityLinks';

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  cityUsage: vi.fn(),
  runCensus: vi.fn(),
  listCities: vi.fn(),
  listAgents: vi.fn(),
  listRigs: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  getBead: vi.fn(),
  createBead: vi.fn(),
  updateBead: vi.fn(),
  closeBead: vi.fn(),
  sling: vi.fn(),
  formulaFeed: vi.fn(),
  listMail: vi.fn(),
  markMailRead: vi.fn(),
  markMailUnread: vi.fn(),
  archiveMail: vi.fn(),
  replyMail: vi.fn(),
  sendMail: vi.fn(),
  mailThread: vi.fn(),
  cityEventStreamUrl: vi.fn(),
  sessionStreamUrl: vi.fn(),
  listSessions: vi.fn(),
  sessionPending: vi.fn(),
  respondSession: vi.fn(),
  sessionTranscript: vi.fn(),
  workflowRun: vi.fn(),
  formulaDetail: vi.fn(),
  mutationHeaders: () => ({ 'X-GC-Request': 'dashboard' }),
};

describe('loadSupervisorEntityLinks', () => {
  beforeEach(() => {
    setActiveCity('test-city');
    vi.useFakeTimers();
    vi.setSystemTime(new Date('2026-06-01T12:00:00.000Z'));
  });

  afterEach(() => {
    resetSupervisorApiForTests();
    vi.useRealTimers();
    vi.clearAllMocks();
  });

  it('builds a related-entity view from direct supervisor beads and sessions', async () => {
    const fetchSpy = vi.fn();
    vi.stubGlobal('fetch', fetchSpy);
    const listBeads = vi.fn(async () =>
      beadList([
        bead({ id: 'focus', title: 'Focus bead' }),
        bead({
          id: 'child',
          title: 'Child bead',
          status: 'in_progress',
          metadata: {
            'gc.parent_bead_id': 'focus',
            session_id: 'session-1',
            session_name: 'session one',
          },
        }),
      ]),
    );
    const listSessions = vi.fn(async () =>
      sessionList([session({ id: 'session-1', title: 'Session one', state: 'running' })]),
    );
    setSupervisorApiForTests({ ...baseApi, listBeads, listSessions });

    const view = await loadSupervisorEntityLinks('focus');

    expect(view.focus).toEqual({
      key: 'bead:city:test-city:focus',
      type: 'bead',
      ref: 'focus',
    });
    expect(view.partial).toBe(false);
    expect(view.generatedAt).toBe('2026-06-01T12:00:00.000Z');
    expect(view.nodes).toEqual(
      expect.arrayContaining([
        expect.objectContaining({ ref: 'focus', title: 'Focus bead', unresolved: false }),
        expect.objectContaining({ ref: 'child', title: 'Child bead', unresolved: false }),
      ]),
    );
    expect(view.edges).toEqual([
      {
        from: 'bead:city:test-city:focus',
        to: 'bead:city:test-city:child',
        relation: 'child',
        provenance: 'supervisor',
        resolved: true,
      },
    ]);
    expect(listBeads).toHaveBeenCalledWith('test-city', { limit: 1_000 });
    expect(listSessions).toHaveBeenCalledWith('test-city');
    expect(fetchSpy).not.toHaveBeenCalled();
  });

  it('keeps bead links available while marking the view partial when sessions fail', async () => {
    setSupervisorApiForTests({
      ...baseApi,
      listBeads: vi.fn(async () =>
        beadList([
          bead({ id: 'focus', title: 'Focus bead' }),
          bead({
            id: 'child',
            title: 'Child bead',
            metadata: { 'gc.parent_bead_id': 'focus' },
          }),
        ]),
      ),
      listSessions: vi.fn(async () => {
        throw new Error('sessions unavailable');
      }),
    });

    const view = await loadSupervisorEntityLinks('focus');

    expect(view.partial).toBe(true);
    expect(view.edges).toHaveLength(1);
  });

  it('rejects malformed refs before reading the supervisor', async () => {
    const listBeads = vi.fn(async () => beadList([]));
    setSupervisorApiForTests({
      ...baseApi,
      listBeads,
      listSessions: vi.fn(async () => sessionList([])),
    });

    await expect(loadSupervisorEntityLinks('bad id !!')).rejects.toThrow('unrecognised ref');
    expect(listBeads).not.toHaveBeenCalled();
  });
});

function bead(overrides: Partial<Bead> = {}): Bead {
  return {
    id: 'focus',
    title: 'Focus bead',
    issue_type: 'task',
    status: 'open',
    created_at: '2026-06-01T11:00:00.000Z',
    ...overrides,
  };
}

function beadList(items: Bead[], partial = false): ListBodyBead {
  return {
    items,
    partial,
    total: items.length,
  };
}

function session(overrides: Partial<SessionResponse> = {}): SessionResponse {
  return {
    id: 'session-1',
    template: 'codex',
    session_name: 'session-1',
    title: 'Session one',
    state: 'running',
    created_at: '2026-06-01T11:00:00.000Z',
    attached: true,
    running: true,
    provider: 'codex',
    ...overrides,
  };
}

function sessionList(items: SessionResponse[], partial = false): ListBodySessionResponse {
  return {
    items,
    partial,
    total: items.length,
  };
}
