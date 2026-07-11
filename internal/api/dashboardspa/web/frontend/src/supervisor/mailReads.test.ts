import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  SupervisorApiError,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
  type SupervisorApi,
} from './client';
import {
  fetchSupervisorMailThread,
  listSupervisorMail,
  type MailOperatorIdentity,
  type SupervisorMailItem,
} from './mailReads';

// Test operator identity: display alias maps to the gc wire alias for mailbox
// filtering (gascity-dashboard-bhvn). Mirrors the production operator config.
const operator: MailOperatorIdentity = {
  operatorAlias: 'stephanie',
  operatorWireAlias: 'human',
};

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  cityUsage: vi.fn(),
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

describe('supervisor mail reads', () => {
  afterEach(() => {
    resetSupervisorApiForTests();
  });

  it('filters the operator inbox using the supervisor wire alias while preserving Message objects', async () => {
    const listMail = vi.fn(async () => ({
      items: [
        mail({ id: 'a', from: 'mayor', to: 'human', subject: 'operator inbox' }),
        mail({ id: 'b', from: 'mechanic', to: 'mayor', subject: 'someone else' }),
        mail({ id: 'c', from: 'human', to: 'mechanic', subject: 'operator sent' }),
      ],
      total: 3,
    }));
    setSupervisorApiForTests({ ...baseApi, listMail });

    const result = await listSupervisorMail('inbox', 'stephanie', operator);

    expect(listMail).toHaveBeenCalledWith('test-city', { limit: 100 });
    expect(result.items.map((m) => m.id)).toEqual(['a']);
    expect(result.items[0]).toMatchObject({ to: 'human', subject: 'operator inbox' });
    expect(result.upstream_total).toBe(3);
  });

  it('passes an explicit supervisor mail history depth through the generated query', async () => {
    const listMail = vi.fn(async () => ({
      items: [mail({ id: 'a', from: 'mayor', to: 'human', subject: 'operator inbox' })],
      total: 1,
    }));
    setSupervisorApiForTests({ ...baseApi, listMail });

    await listSupervisorMail('inbox', 'stephanie', operator, 1000);

    expect(listMail).toHaveBeenCalledWith('test-city', { limit: 1000 });
  });

  it('filters fetched supervisor mail by a typed clock window after mailbox projection', async () => {
    const listMail = vi.fn(async () => ({
      items: [
        mail({
          id: 'recent-inbox',
          from: 'mayor',
          to: 'human',
          created_at: '2026-06-01T10:00:00.000Z',
        }),
        mail({
          id: 'old-inbox',
          from: 'mayor',
          to: 'human',
          created_at: '2026-05-30T10:00:00.000Z',
        }),
        mail({
          id: 'recent-other',
          from: 'mechanic',
          to: 'mayor',
          created_at: '2026-06-01T10:00:00.000Z',
        }),
      ],
      total: 3,
    }));
    setSupervisorApiForTests({ ...baseApi, listMail });

    const result = await listSupervisorMail(
      'inbox',
      'stephanie',
      operator,
      1000,
      '24h',
      Date.parse('2026-06-01T12:00:00.000Z'),
    );

    expect(result.items.map((m) => m.id)).toEqual(['recent-inbox']);
    expect(result.upstream_total).toBe(3);
    expect(result.upstream_fetched).toBe(3);
    expect(result.total).toBe(1);
  });

  it('loads a thread through the supervisor thread endpoint and sorts messages oldest first', async () => {
    const mailThread = vi.fn(async () => ({
      items: [
        mail({ id: 'new', created_at: '2026-06-01T10:01:00Z', thread_id: 'thread-1' }),
        mail({ id: 'old', created_at: '2026-06-01T10:00:00Z', thread_id: 'thread-1' }),
      ],
      total: 2,
    }));
    setSupervisorApiForTests({ ...baseApi, mailThread });

    const result = await fetchSupervisorMailThread('thread-1', 'stephanie', operator);

    expect(mailThread).toHaveBeenCalledWith('test-city', 'thread-1');
    expect(result.items.map((m) => m.id)).toEqual(['old', 'new']);
  });

  it('falls back to a direct supervisor list/filter when the thread endpoint is unavailable', async () => {
    const mailThread = vi.fn(async () => {
      throw new SupervisorApiError(404, 'not found', undefined);
    });
    const listMail = vi.fn(async () => ({
      items: [
        mail({ id: 'thread-hit', thread_id: 'thread-1' }),
        mail({ id: 'thread-miss', thread_id: 'thread-2' }),
      ],
      total: 2,
    }));
    setSupervisorApiForTests({ ...baseApi, listMail, mailThread });

    const result = await fetchSupervisorMailThread('thread-1', 'stephanie', operator);

    expect(mailThread).toHaveBeenCalledWith('test-city', 'thread-1');
    expect(listMail).toHaveBeenCalledWith('test-city', { limit: 100 });
    expect(result.items.map((m) => m.id)).toEqual(['thread-hit']);
  });
});

function mail(overrides: Partial<SupervisorMailItem>): SupervisorMailItem {
  return {
    id: 'mail-1',
    from: 'mayor',
    to: 'human',
    subject: 'subject',
    body: 'body',
    created_at: '2026-06-01T10:00:00Z',
    read: false,
    ...overrides,
  };
}
