import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  resetSupervisorApiForTests,
  setSupervisorApiForTests,
  SupervisorApiError,
  type SupervisorApi,
} from './client';
import { loadSupervisorFormulaRunDetail } from './runDetail';

vi.mock('../api/cityBase', () => ({
  getActiveCity: () => 'test-city',
  activeCityOrThrow: () => 'test-city',
}));

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
  mutationHeaders: () => ({ 'X-GC-Request': 'dashboard' }),
};

describe('loadSupervisorFormulaRunDetail', () => {
  const workflowRun = vi.fn();
  const formulaDetail = vi.fn();
  const listSessions = vi.fn();

  beforeEach(() => {
    workflowRun.mockResolvedValue(workflowSnapshot());
    formulaDetail.mockResolvedValue(formulaDetailResponse());
    listSessions.mockResolvedValue({ items: [], total: 0 });
    setSupervisorApiForTests({
      ...baseApi,
      workflowRun,
      formulaDetail,
      listSessions,
    });
  });

  afterEach(() => {
    resetSupervisorApiForTests();
    vi.clearAllMocks();
  });

  it('fetches formula detail when the root exposes formula metadata and a run target', async () => {
    const detail = await loadSupervisorFormulaRunDetail('wf-1', 'city', 'test-city');

    expect(detail.formula).toEqual({
      kind: 'known',
      name: 'mol-test',
      source: 'metadata',
    });
    expect(detail.formulaDetail).toEqual({
      kind: 'available',
      name: 'mol-test',
      target: 'test-city/codex',
    });
    expect(detail.completeness).toEqual({ kind: 'complete' });
    expect(formulaDetail).toHaveBeenCalledWith('test-city', 'mol-test', {
      target: 'test-city/codex',
      scope_kind: 'city',
      scope_ref: 'test-city',
    });
  });

  it('resolves formula detail when the supervisor omits the version field (3eo8, mol-focus-review)', async () => {
    formulaDetail.mockResolvedValue(versionlessFormulaDetailResponse());

    const detail = await loadSupervisorFormulaRunDetail('wf-1', 'city', 'test-city');

    expect(detail.formulaDetail).toEqual({
      kind: 'available',
      name: 'mol-test',
      target: 'test-city/codex',
    });
    expect(detail.completeness).toEqual({ kind: 'complete' });
  });

  it('reports missing formula metadata without calling the formula endpoint', async () => {
    workflowRun.mockResolvedValue(
      workflowSnapshot({
        status: 'closed',
        metadata: {
          'gc.kind': 'workflow',
          'gc.formula_contract': 'graph.v2',
          'gc.run_target': 'test-city/codex',
        },
      }),
    );

    const detail = await loadSupervisorFormulaRunDetail('wf-1');

    expect(detail.formula).toEqual({
      kind: 'unavailable',
      reason: 'missing_formula_metadata',
    });
    expect(detail.formulaDetail).toEqual({
      kind: 'unavailable',
      reason: 'missing_formula_metadata',
    });
    expect(detail.completeness).toEqual({
      kind: 'partial',
      reasons: ['formula_detail_missing_formula_metadata'],
    });
    expect(formulaDetail).not.toHaveBeenCalled();
  });

  it('does not title-fallback into formula detail for completed supervisor runs', async () => {
    workflowRun.mockResolvedValue(
      workflowSnapshot({
        status: 'completed',
        metadata: {
          'gc.kind': 'workflow',
          'gc.formula_contract': 'graph.v2',
          'gc.run_target': 'test-city/codex',
        },
      }),
    );

    const detail = await loadSupervisorFormulaRunDetail('wf-1');

    expect(detail.formula).toEqual({
      kind: 'unavailable',
      reason: 'missing_formula_metadata',
    });
    expect(detail.formulaDetail).toEqual({
      kind: 'unavailable',
      reason: 'missing_formula_metadata',
    });
    expect(detail.completeness).toEqual({
      kind: 'partial',
      reasons: ['formula_detail_missing_formula_metadata'],
    });
    expect(formulaDetail).not.toHaveBeenCalled();
  });

  it('reports missing run target without calling the formula endpoint', async () => {
    workflowRun.mockResolvedValue(
      workflowSnapshot({
        metadata: {
          'gc.kind': 'workflow',
          'gc.formula_contract': 'graph.v2',
          'gc.formula': 'mol-test',
        },
      }),
    );

    const detail = await loadSupervisorFormulaRunDetail('wf-1');

    expect(detail.formula).toEqual({
      kind: 'known',
      name: 'mol-test',
      source: 'metadata',
    });
    expect(detail.formulaDetail).toEqual({
      kind: 'unavailable',
      reason: 'missing_run_target',
      name: 'mol-test',
    });
    expect(detail.completeness).toEqual({
      kind: 'partial',
      reasons: ['formula_detail_missing_run_target'],
    });
    expect(formulaDetail).not.toHaveBeenCalled();
  });

  it('preserves supervisor formula endpoint failures as partial formula detail', async () => {
    formulaDetail.mockRejectedValue(new SupervisorApiError(404, 'not found', undefined));

    const detail = await loadSupervisorFormulaRunDetail('wf-1');

    expect(detail.formulaDetail).toEqual({
      kind: 'unavailable',
      reason: 'fetch_failed',
      name: 'mol-test',
      target: 'test-city/codex',
      failure: 'not_found',
    });
    expect(detail.completeness).toEqual({
      kind: 'partial',
      reasons: ['formula_detail_fetch_failed'],
    });
  });

  // Fix A: the workflow snapshot is the run-detail core read; a transient
  // timeout/5xx is retried once before it blanks the view, mirroring the runs
  // list core read. A 4xx is the caller's fault and is never retried.
  it('retries the workflow core read once on a transient timeout', async () => {
    let attempts = 0;
    workflowRun.mockImplementation(async () => {
      attempts += 1;
      if (attempts === 1) {
        throw new SupervisorApiError(
          undefined,
          'gc supervisor request timed out after 60000ms',
          undefined,
        );
      }
      return workflowSnapshot();
    });

    const detail = await loadSupervisorFormulaRunDetail('wf-1', 'rig', 'app');

    expect(attempts).toBe(2);
    expect(detail.completeness).toEqual({ kind: 'complete' });
  });

  it('does not retry the workflow core read on a non-transient (4xx) failure', async () => {
    let attempts = 0;
    workflowRun.mockImplementation(async () => {
      attempts += 1;
      throw new SupervisorApiError(400, 'bad request', undefined);
    });

    await expect(loadSupervisorFormulaRunDetail('wf-1', 'rig', 'app')).rejects.toThrow(
      'bad request',
    );
    expect(attempts).toBe(1);
  });
});

function workflowSnapshot(
  overrides: {
    status?: string;
    metadata?: Record<string, string>;
  } = {},
) {
  return {
    workflow_id: 'wf-1',
    root_bead_id: 'wf-1',
    root_store_ref: 'city:test-city',
    resolved_root_store: 'city:test-city',
    scope_kind: 'city',
    scope_ref: 'test-city',
    snapshot_version: 1,
    snapshot_event_seq: 1,
    partial: false,
    stores_scanned: ['city:test-city'],
    beads: [
      {
        id: 'wf-1',
        title: 'Direct supervisor run',
        status: overrides.status ?? 'in_progress',
        kind: 'workflow',
        metadata: overrides.metadata ?? {
          'gc.kind': 'workflow',
          'gc.formula_contract': 'graph.v2',
          'gc.formula': 'mol-test',
          'gc.run_target': 'test-city/codex',
        },
      },
    ],
    deps: [],
    logical_nodes: [],
    logical_edges: [],
    scope_groups: [],
  };
}

function formulaDetailResponse() {
  return {
    name: 'mol-test',
    description: 'formula detail',
    version: 'v1',
    preview: {
      nodes: [{ id: 'wf-1', title: 'Direct supervisor run', kind: 'workflow' }],
      edges: [],
    },
    steps: [],
    deps: [],
    var_defs: [],
  };
}

// Inferred/title-based formulas (e.g. mol-focus-review) come back from the
// supervisor with no `version` key — `{ name, description, var_defs, steps,
// deps, preview }`. The dashboard must resolve these to `available`, not
// degrade the Formula Detail panel (3eo8).
function versionlessFormulaDetailResponse() {
  const { version: _version, ...rest } = formulaDetailResponse();
  return rest;
}
