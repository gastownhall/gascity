import type {
  FormulaRunDetail,
  FormulaRunPartialReason,
  FormulaDetail,
  RunSnapshot,
  DashboardSession,
  RunFormulaDetailFetchFailure,
  RunFormulaDetailState,
  RunScopeKind,
} from 'gas-city-dashboard-shared';
import {
  enrichFormulaRun,
  formulaRunCompleteness,
  resolveRunFormulaIdentity,
} from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import type {
  FormulaDetailResponse,
  WorkflowSnapshotResponse,
} from 'gas-city-dashboard-shared/gc-supervisor';
import { SupervisorApiError, supervisorApi, supervisorApiForRequestBudget } from './client';
import type { SupervisorApi } from './client';
import { fetchCoreRead } from './coreRead';
import { normalizeSessions } from './sessionReads';

// The workflow snapshot is the run-detail core read — the one fetch whose
// failure blanks the whole detail view (sessions and formula detail degrade to
// 'partial'). It gets the same treatment as the runs-list core read
// (runSummary.ts): a burst-tolerant budget so a CPU spike doesn't time it out,
// and one retry on a transient timeout/5xx. A city-scoped (no-rig) fetch hits
// the supervisor's full-store scan (~12-14s, upstream gascity-dashboard#88), so
// the wider budget is what lets that path complete instead of timing out; the
// rig-scoped fetch (the common case once scope is passed) is sub-second.
const RUN_DETAIL_CORE_TIMEOUT_MS = 60_000;

export async function loadSupervisorFormulaRunDetail(
  runId: string,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): Promise<FormulaRunDetail> {
  const cityName = activeCityOrThrow('load supervisor formula run detail');
  const query = runScopeQuery(scopeKind, scopeRef);
  const coreApi = supervisorApiForRequestBudget(RUN_DETAIL_CORE_TIMEOUT_MS);
  const api = supervisorApi();
  const [raw, sessionsLookup] = await Promise.all([
    fetchCoreRead(() => coreApi.workflowRun(cityName, runId, query)),
    loadRunSessions(cityName),
  ]);
  const snapshot = toRunSnapshot(raw);
  const formulaDetailLookup = await loadRunFormulaDetail(api, cityName, snapshot, query);
  const detail = enrichFormulaRun(snapshot, {
    sessions: sessionsLookup.sessions,
    formulaDetailState: formulaDetailLookup.state,
    ...(formulaDetailLookup.kind === 'available'
      ? { formulaDetail: formulaDetailLookup.detail }
      : {}),
  });
  const reasons: FormulaRunPartialReason[] = [
    ...(detail.completeness.kind === 'partial' ? detail.completeness.reasons : []),
    ...(sessionsLookup.kind === 'unavailable' ? ['session_list_failed' as const] : []),
    ...(formulaDetailLookup.kind === 'unavailable'
      ? [formulaDetailPartialReason(formulaDetailLookup.state.reason)]
      : []),
  ];
  return {
    ...detail,
    completeness: formulaRunCompleteness(reasons),
  };
}

type RunSessionsLookup =
  | { kind: 'available'; sessions: readonly DashboardSession[] }
  | { kind: 'unavailable'; sessions: readonly DashboardSession[] };

type RunFormulaDetailLookup =
  | { kind: 'available'; detail: FormulaDetail; state: RunFormulaDetailState }
  | {
      kind: 'unavailable';
      state: Extract<RunFormulaDetailState, { kind: 'unavailable' }>;
    };

async function loadRunSessions(cityName: string): Promise<RunSessionsLookup> {
  try {
    const list = await supervisorApi().listSessions(cityName);
    return {
      kind: 'available',
      sessions: normalizeSessions(list),
    };
  } catch {
    return { kind: 'unavailable', sessions: [] };
  }
}

async function loadRunFormulaDetail(
  api: SupervisorApi,
  cityName: string,
  snapshot: RunSnapshot,
  scopeQuery: { scope_kind?: string; scope_ref?: string } | undefined,
): Promise<RunFormulaDetailLookup> {
  const root = snapshot.beads?.find((bead) => bead.id === snapshot.root_bead_id);
  const resolved = resolveRunFormulaIdentity('route', { root });
  const name = resolved.name ?? undefined;
  const target = resolved.target ?? undefined;
  if (name === undefined) {
    return {
      kind: 'unavailable',
      state: { kind: 'unavailable', reason: 'missing_formula_metadata' },
    };
  }
  if (target === undefined) {
    return {
      kind: 'unavailable',
      state: { kind: 'unavailable', reason: 'missing_run_target', name },
    };
  }
  try {
    const detail = toFormulaDetail(
      await api.formulaDetail(cityName, name, {
        target,
        ...(scopeQuery ?? {}),
      }),
    );
    return {
      kind: 'available',
      detail,
      state: { kind: 'available', name, target },
    };
  } catch (err) {
    return {
      kind: 'unavailable',
      state: {
        kind: 'unavailable',
        reason: 'fetch_failed',
        name,
        target,
        failure: formulaDetailFetchFailure(err),
      },
    };
  }
}

function toRunSnapshot(raw: WorkflowSnapshotResponse): RunSnapshot {
  const snapshot: RunSnapshot = {
    run_id: raw.workflow_id,
    root_bead_id: raw.root_bead_id,
    root_store_ref: raw.root_store_ref,
    resolved_root_store: raw.resolved_root_store,
    scope_kind: raw.scope_kind,
    scope_ref: raw.scope_ref,
    snapshot_version: raw.snapshot_version,
    partial: raw.partial,
    stores_scanned: raw.stores_scanned,
    beads: raw.beads,
    deps: raw.deps,
    logical_nodes: raw.logical_nodes as RunSnapshot['logical_nodes'],
    logical_edges: raw.logical_edges,
    scope_groups: raw.scope_groups as RunSnapshot['scope_groups'],
  };
  if (raw.snapshot_event_seq !== undefined) {
    snapshot.snapshot_event_seq = raw.snapshot_event_seq;
  }
  return snapshot;
}

function toFormulaDetail(raw: FormulaDetailResponse): FormulaDetail {
  const detail: FormulaDetail = { name: raw.name };
  const preview: NonNullable<FormulaDetail['preview']> = {};
  if (Array.isArray(raw.preview.nodes)) {
    preview.nodes = raw.preview.nodes;
  }
  if (Array.isArray(raw.preview.edges)) {
    preview.edges = raw.preview.edges;
  }
  if (preview.nodes !== undefined || preview.edges !== undefined) {
    detail.preview = preview;
  }
  if (Array.isArray(raw.steps)) {
    detail.steps = raw.steps;
  }
  if (Array.isArray(raw.deps)) {
    detail.deps = raw.deps;
  }
  return detail;
}

function formulaDetailPartialReason(
  reason: Extract<RunFormulaDetailState, { kind: 'unavailable' }>['reason'],
): FormulaRunPartialReason {
  switch (reason) {
    case 'missing_formula_metadata':
      return 'formula_detail_missing_formula_metadata';
    case 'missing_run_target':
      return 'formula_detail_missing_run_target';
    case 'fetch_failed':
      return 'formula_detail_fetch_failed';
  }
}

function formulaDetailFetchFailure(err: unknown): RunFormulaDetailFetchFailure {
  if (err instanceof SupervisorApiError && err.status === 404) return 'not_found';
  return 'upstream_error';
}

function runScopeQuery(
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): { scope_kind?: string; scope_ref?: string } | undefined {
  if (scopeKind === undefined && scopeRef === undefined) return undefined;
  const query: { scope_kind?: string; scope_ref?: string } = {};
  if (scopeKind !== undefined) query.scope_kind = scopeKind;
  if (scopeRef !== undefined) query.scope_ref = scopeRef;
  return query;
}
