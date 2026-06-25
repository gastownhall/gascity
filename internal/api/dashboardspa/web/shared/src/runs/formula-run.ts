import type { FormulaDetail, RunSnapshotBead, RunSnapshot } from '../run-snapshot.js';
import type { DashboardSession } from '../dashboard-sessions.js';
import type {
  RunControlBadge,
  RunDisplayEdge,
  RunDisplayLane,
  RunDisplayNode,
  RunExecutionPath,
  RunFormula,
  RunFormulaDetailState,
  RunNodeStatus,
  FormulaRunProgress,
  RunScopeKind,
  RunSnapshotSequence,
} from '../run-detail.js';
import type { RunPhase, RunStage } from '../snapshot/types.js';
import { mapRunPhase, stageProgress, type RunIssue } from './phaseMapping.js';
import { meta } from './bead-fields.js';
import { resolveRunFormulaIdentity } from './formula-name.js';
import { applyDisplayNodeStates } from './display-state.js';
import { buildRunDisplayEdges } from './edges.js';
import {
  buildRunDisplayNode,
  latestIterationsByLoop,
  type RunNodeGroup,
} from './execution-instances.js';
import { resolveRunExecutionPath } from './execution-path.js';
import { orderRunNodeGroups } from './formula-order.js';
import { groupRunBeads } from './groups.js';
import { buildRunDisplayLanes } from './lanes.js';
import {
  buildRunSessionIndex,
  type RunSessionIndex,
  type RunSessionLinkContext,
} from './session-link.js';

export interface RunningFormulaRunInput {
  raw: RunSnapshot;
  runId: string;
  rootBeadId: string;
  rootStoreRef: string;
  resolvedRootStore: string;
  scopeKind: RunScopeKind;
  scopeRef: string;
  root?: RunSnapshotBead;
  beads: RunSnapshotBead[];
  rigRoot?: string;
  sessions?: readonly DashboardSession[];
  formulaDetail?: FormulaDetail;
  formulaDetailState?: RunFormulaDetailState;
}

/**
 * Backend-owned projection of a running graph.v2 formula.
 *
 * The React detail page should render this projection, not infer runtime
 * state from raw run beads or the global sessions list. This is the
 * single aggregation point for supervisor run shape, live bead state,
 * session summaries, loop instances, and display graph state.
 */
export interface RunningFormulaRun {
  raw: RunSnapshot;
  runId: string;
  rootBeadId: string;
  rootStoreRef: string;
  resolvedRootStore: string;
  scopeKind: RunScopeKind;
  scopeRef: string;
  title: string;
  formula: RunFormula;
  formulaDetail: RunFormulaDetailState;
  executionPath: RunExecutionPath;
  root?: RunSnapshotBead;
  beads: RunSnapshotBead[];
  nodeGroups: RunNodeGroup[];
  physicalToSemantic: Map<string, string>;
  badgesByTarget: Map<string, RunControlBadge[]>;
  latestIterationByLoop: Map<string, number>;
  sessionIndex: RunSessionIndex;
  sessionContext: RunSessionLinkContext;
  nodes: RunDisplayNode[];
  edges: RunDisplayEdge[];
  lanes: RunDisplayLane[];
  progress: FormulaRunProgress;
  phase: RunPhase;
  stages: RunStage[];
}

export function buildRunningFormulaRun(input: RunningFormulaRunInput): RunningFormulaRun {
  const {
    groups: unorderedGroups,
    physicalToSemantic,
    badgesByTarget,
  } = groupRunBeads(input.beads, input.rootBeadId);
  const groups = orderRunNodeGroups(unorderedGroups, input.formulaDetail, input.rootBeadId);
  // Prefer supervisor-owned compiled formula order when available. If a run
  // does not expose a formula name yet, preserve snapshot order rather than
  // reading formula files locally.
  const latestIterationByLoop = latestIterationsByLoop(groups);
  const sessionIndex = buildRunSessionIndex(input.sessions ?? []);
  const sessionContext = {
    sessionIndex,
    scopeRef: input.scopeRef,
  };
  const rawNodes = groups.map((group) =>
    buildRunDisplayNode(
      group,
      badgesByTarget.get(group.semanticNodeId) ?? [],
      latestIterationByLoop.get(group.loopControlNodeId ?? ''),
      sessionContext,
    ),
  );
  const edges = buildRunDisplayEdges(input.raw, physicalToSemantic, rawNodes);
  const nodes = applyDisplayNodeStates(rawNodes, edges);
  const progress = buildFormulaRunProgress(input.raw, nodes, edges);
  const formula = runFormulaState(input.root, input.formulaDetail);
  const formulaDetail =
    input.formulaDetailState ?? runFormulaDetailState(input.root, input.formulaDetail);
  const executionPath = resolveRunExecutionPath(input.root, input.beads, input.rigRoot);

  // gascity-dashboard-ud6j: compute the dashboard phase ladder from this
  // run's OWN beads through the SAME fromDashboardBead → mapRunPhase → stageProgress
  // pipeline the snapshot lane uses, so the run-detail ladder cannot drift
  // from the lane's. mapRunPhase keys off bead status + title, which run
  // beads carry. The resolved formula name (when known) selects the
  // formula-specific stage set; otherwise the generic 5-stage ladder applies.
  const issues = input.beads.map(fromRunSnapshotBead);
  const phaseMapping = mapRunPhase(issues);
  const formulaName = formula.kind === 'known' ? formula.name : null;
  const stages = stageProgress(phaseMapping, formulaName, issues);

  const run: RunningFormulaRun = {
    raw: input.raw,
    runId: input.runId,
    rootBeadId: input.rootBeadId,
    rootStoreRef: input.rootStoreRef,
    resolvedRootStore: input.resolvedRootStore,
    scopeKind: input.scopeKind,
    scopeRef: input.scopeRef,
    title: input.root?.title.trim() || input.runId,
    formula,
    formulaDetail,
    executionPath,
    beads: input.beads,
    nodeGroups: groups,
    physicalToSemantic,
    badgesByTarget,
    latestIterationByLoop,
    sessionIndex,
    sessionContext,
    nodes,
    edges,
    lanes: buildRunDisplayLanes(nodes),
    progress,
    phase: phaseMapping.phase,
    stages,
  };
  if (input.root !== undefined) run.root = input.root;
  return run;
}

/**
 * Adapt a supervisor run-snapshot bead to the phase classifier's
 * RunIssue input. The phase pipeline's own fromDashboardBead adapter consumes the
 * city-wide DashboardBead shape (issue_type, created_at); the run-snapshot wire row
 * is a different shape (kind, no created_at). mapRunPhase only reads
 * status / title / metadata / issue_type / parent, so this maps the run-bead
 * `kind` onto `issue_type` and leaves updated_at empty (the snapshot carries
 * no per-bead timestamp — latestStepId's ordering degrades gracefully to
 * input order, which the phase classifier does not depend on).
 */
function fromRunSnapshotBead(bead: RunSnapshotBead): RunIssue {
  const parent = meta(bead, 'gc.parent_bead_id');
  const issue: RunIssue = {
    id: bead.id,
    title: bead.title,
    status: bead.status,
    issue_type: bead.kind,
    updated_at: '',
    metadata: bead.metadata,
  };
  if (bead.assignee !== undefined) issue.assignee = bead.assignee;
  if (parent !== undefined) issue.parent = parent;
  return issue;
}

function runFormulaState(
  root: RunSnapshotBead | undefined,
  formulaDetail: FormulaDetail | undefined,
): RunFormula {
  // Provenance precedence (gascity-dashboard-e7hj + sadp). The supervisor's
  // canonical signals win over the graph.v2 bead-title heuristic; the title
  // fallback only fires when none of them are present:
  //   1. `gc.formula` / `gc.formula_name` metadata   → source: 'metadata'
  //   2. supervisor formula detail name               → source: 'metadata'
  //      (canonical even when the root metadata key is absent)
  //   3. graph.v2 title fallback (resolveRunFormulaIdentity) → 'title_fallback'
  // The title fallback shares the mode-aware identity resolver with the
  // route-side formula-detail fetch (routes/runs.ts) so both agree on which
  // graph.v2 roots get a title-derived name. See gascity-dashboard-sadp.
  const resolved = resolveRunFormulaIdentity('state', { root, formulaDetail });
  if (resolved.name !== null) {
    const source = resolved.source === 'title_fallback' ? 'title_fallback' : 'metadata';
    return {
      kind: 'known',
      name: resolved.name,
      source,
    };
  }
  return {
    kind: 'unavailable',
    reason: 'missing_formula_metadata',
  };
}

function runFormulaDetailState(
  root: RunSnapshotBead | undefined,
  formulaDetail: FormulaDetail | undefined,
): RunFormulaDetailState {
  const resolved = resolveRunFormulaIdentity('detail', { root, formulaDetail });
  const name = resolved.name;
  if (name === null) return { kind: 'unavailable', reason: 'missing_formula_metadata' };
  const target = resolved.target;
  if (!target) return { kind: 'unavailable', reason: 'missing_run_target', name };
  if (formulaDetail !== undefined) return { kind: 'available', name, target };
  return {
    kind: 'unavailable',
    reason: 'fetch_failed',
    name,
    target,
    failure: 'upstream_error',
  };
}

function buildFormulaRunProgress(
  raw: RunSnapshot,
  nodes: readonly RunDisplayNode[],
  edges: readonly RunDisplayEdge[],
): FormulaRunProgress {
  const visibleNodes = nodes.filter((node) => node.visibleInGraph);
  const streamableSessionIds = new Set<string>();
  let executionInstanceCount = 0;
  let sessionLinkCount = 0;
  let streamableSessionCount = 0;

  for (const node of nodes) {
    for (const instance of node.executionInstances) {
      executionInstanceCount += 1;
      if (instance.session.kind === 'attached') {
        sessionLinkCount += 1;
      }
      if (instance.session.kind === 'attached' && instance.session.streamable) {
        streamableSessionCount += 1;
        streamableSessionIds.add(instance.session.link.sessionId);
      }
    }
  }

  return {
    snapshotVersion: raw.snapshot_version,
    snapshotEventSeq: runSnapshotSequence(raw.snapshot_event_seq),
    snapshotPartial: raw.partial,
    totalNodeCount: nodes.length,
    visibleNodeCount: visibleNodes.length,
    edgeCount: edges.length,
    executionInstanceCount,
    sessionLinkCount,
    streamableSessionCount,
    streamableSessionIds: [...streamableSessionIds],
    statusCounts: countNodeStatuses(visibleNodes),
    allStatusCounts: countNodeStatuses(nodes),
  };
}

function runSnapshotSequence(raw: number | null | undefined): RunSnapshotSequence {
  return typeof raw === 'number'
    ? { kind: 'known', seq: raw }
    : { kind: 'unavailable', reason: 'supervisor_omitted' };
}

function countNodeStatuses(
  nodes: readonly RunDisplayNode[],
): Partial<Record<RunNodeStatus, number>> {
  const counts: Partial<Record<RunNodeStatus, number>> = {};
  for (const node of nodes) {
    counts[node.status] = (counts[node.status] ?? 0) + 1;
  }
  return counts;
}
