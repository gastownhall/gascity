import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  GC_MUTATION_HEADERS,
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
  type SupervisorApi,
} from './client';
import { setActiveCity } from '../api/cityBase';
import {
  closeSupervisorBead,
  createAndSlingSupervisorBead,
  nudgeSupervisorAgent,
} from './beadWrites';

// The operator display alias is now runtime config (OperatorConfigContext),
// no longer a shared constant (gascity-dashboard-bhvn). A bead write must still
// NEVER post it as assignee; this sample stands in for whatever the configured
// alias resolves to. Test/fixture data may carry a concrete identity.
const SAMPLE_OPERATOR_ALIAS = 'stephanie';

const baseApi: SupervisorApi = {
  baseUrl: '/gc-supervisor',
  health: vi.fn(),
  cityHealth: vi.fn(),
  cityStatus: vi.fn(),
  listCities: vi.fn(),
  listAgents: vi.fn(),
  listRigs: vi.fn(),
  listBeads: vi.fn(),
  listEvents: vi.fn(),
  getBead: vi.fn(),
  createBead: vi.fn(),
  updateBead: vi.fn(),
  closeBead: vi.fn(),
  nudgeAgent: vi.fn(),
  agentPrime: vi.fn(),
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
  mutationHeaders: () => ({ ...GC_MUTATION_HEADERS }),
};

describe('supervisor bead writes', () => {
  beforeEach(() => {
    setActiveCity('test-city');
  });

  afterEach(() => {
    resetSupervisorApiForTests();
  });

  it('closes a bead directly through the supervisor API with a trimmed reason', async () => {
    const closeBead = vi.fn(async () => ({ status: 'closed' }));
    setSupervisorApiForTests({ ...baseApi, closeBead });

    await closeSupervisorBead('td-bead-abc123', '  operator verified duplicate  ');

    expect(closeBead).toHaveBeenCalledWith('test-city', 'td-bead-abc123', {
      reason: 'operator verified duplicate',
    });
  });

  it('omits the optional close body when the reason is blank', async () => {
    const closeBead = vi.fn(async () => ({ status: 'closed' }));
    setSupervisorApiForTests({ ...baseApi, closeBead });

    await closeSupervisorBead('td-bead-abc123', '   ');

    expect(closeBead).toHaveBeenCalledWith('test-city', 'td-bead-abc123', undefined);
  });

  it('nudges an agent directly through the supervisor API with a trimmed alias', async () => {
    const nudgeAgent = vi.fn(async () => ({ status: 'ok' }));
    setSupervisorApiForTests({ ...baseApi, nudgeAgent });

    await nudgeSupervisorAgent('  mayor  ');

    expect(nudgeAgent).toHaveBeenCalledWith('test-city', 'mayor');
  });

  it('creates and slings a bead directly through the supervisor API with trimmed input', async () => {
    const createBead = vi.fn(async () => ({
      id: 'td-new-1',
      title: 'Route failing work',
      status: 'open',
      issue_type: 'task',
      created_at: '2026-06-01T00:00:00Z',
    }));
    const sling = vi.fn(async () => ({
      status: 'ok',
      bead: 'td-new-1',
      target: 'mayor',
    }));
    setSupervisorApiForTests({ ...baseApi, createBead, sling });

    const result = await createAndSlingSupervisorBead({
      title: '  Route failing work  ',
      description: '  Please investigate.  ',
      rig: '  east  ',
      target: '  mayor  ',
    });

    expect(result.bead.id).toBe('td-new-1');
    expect(createBead).toHaveBeenCalledWith('test-city', {
      title: 'Route failing work',
      description: 'Please investigate.',
    });
    expect(sling).toHaveBeenCalledWith('test-city', {
      bead: 'td-new-1',
      rig: 'east',
      target: 'mayor',
    });
  });

  it('requires a title and sling target before creating a bead', async () => {
    const createBead = vi.fn(async () => ({
      id: 'td-new-1',
      title: 'Route failing work',
      status: 'open',
      issue_type: 'task',
      created_at: '2026-06-01T00:00:00Z',
    }));
    const sling = vi.fn(async () => ({ status: 'ok', bead: 'td-new-1', target: 'mayor' }));
    setSupervisorApiForTests({ ...baseApi, createBead, sling });

    await expect(
      createAndSlingSupervisorBead({
        title: ' ',
        description: '',
        rig: 'east',
        target: 'mayor',
      }),
    ).rejects.toThrow(/title is required/i);
    await expect(
      createAndSlingSupervisorBead({
        title: 'Route failing work',
        description: '',
        rig: 'east',
        target: ' ',
      }),
    ).rejects.toThrow(/target is required/i);
    expect(createBead).not.toHaveBeenCalled();
    expect(sling).not.toHaveBeenCalled();
  });

  // gascity-dashboard-2j8e.8 (HARD CONSTRAINT): a bead write must NEVER post a
  // non-session display alias (the human operator) as assignee — the supervisor
  // rejects it ('assignee must resolve to a concrete open session bead ID'), and
  // the operator is not a session. Exercise every write helper and assert none
  // of them ever sends the operator alias as an assignee.
  it('never posts the operator display alias as a bead assignee', async () => {
    const updateBead = vi.fn(async () => ({ status: 'ok' }));
    const createBead = vi.fn(async () => ({
      id: 'td-new-1',
      title: 'Route failing work',
      status: 'open',
      issue_type: 'task',
      created_at: '2026-06-01T00:00:00Z',
    }));
    const sling = vi.fn(async () => ({ status: 'ok', bead: 'td-new-1', target: 'mayor' }));
    const closeBead = vi.fn(async () => ({ status: 'closed' }));
    const nudgeAgent = vi.fn(async () => ({ status: 'ok' }));
    setSupervisorApiForTests({
      ...baseApi,
      updateBead,
      createBead,
      sling,
      closeBead,
      nudgeAgent,
    });

    // Exercise every exported bead-write helper. (claimSupervisorBead, the only
    // helper that ever called updateBead, was removed in 2j8e.8.)
    await closeSupervisorBead('td-bead-abc123', 'done');
    await nudgeSupervisorAgent('mayor');
    await createAndSlingSupervisorBead({
      title: 'Route failing work',
      description: '',
      rig: 'east',
      target: 'mayor',
    });

    // The write surface was actually driven — without this, the assignee
    // assertions below could pass vacuously if a helper stopped issuing its
    // mutation. nudgeAgent carries no body, so it has no assignee to check.
    expect(closeBead).toHaveBeenCalled();
    expect(nudgeAgent).toHaveBeenCalled();
    expect(createBead).toHaveBeenCalled();
    expect(sling).toHaveBeenCalled();

    // updateBead has no caller now that claimSupervisorBead is gone. Assert that
    // explicitly: the assignee guard for it would otherwise pass vacuously (zero
    // calls), so a helper that reintroduces an assignee-bearing updateBead write
    // must first trip this and be wired into the exercise block above.
    expect(updateBead).not.toHaveBeenCalled();

    // No body-bearing supervisor mutation that a helper actually issues may
    // carry the operator alias as assignee.
    for (const writeSpy of [createBead, sling, closeBead]) {
      for (const call of writeSpy.mock.calls) {
        const body = call[call.length - 1] as { assignee?: unknown } | undefined;
        expect(body?.assignee).not.toBe(SAMPLE_OPERATOR_ALIAS);
      }
    }
  });
});
