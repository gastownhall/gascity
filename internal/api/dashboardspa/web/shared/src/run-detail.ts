import type { RunPhase, RunStage } from './snapshot/types.js';

export type RunScopeKind = 'city' | 'rig';

/**
 * Validation pattern for a run `scope_ref` query value. Lives in shared so
 * the backend route guard and the frontend deep-link parser validate against
 * one source of truth — a wire rule that crosses the boundary must not be
 * copy-pasted on each side (they would silently drift).
 */
export const SCOPE_REF_RE = /^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$/;

export type RunNodeStatus =
  | 'pending'
  | 'ready'
  | 'running'
  | 'active'
  | 'done'
  | 'completed'
  | 'failed'
  | 'blocked'
  | 'skipped';

export type RunConstructKind =
  | 'run-root'
  | 'step'
  | 'retry'
  | 'check-loop'
  | 'scope'
  | 'condition'
  | 'fanout'
  | 'expansion'
  | 'scope-check'
  | 'run-finalize'
  | 'spec'
  | 'control'
  | 'unknown';

export interface RunSessionLink {
  sessionId: string;
  sessionName: string;
  assignee: string;
}

export type RunIteration = { kind: 'base' } | { kind: 'loop'; value: number };

export type RunAttempt = { kind: 'untracked' } | { kind: 'attempt'; value: number };

export type RunSessionAttachment =
  | { kind: 'attached'; link: RunSessionLink; streamable: boolean }
  | { kind: 'none'; reason: 'not_started' | 'session_unresolved' };

export interface RunExecutionInstance {
  id: string;
  semanticNodeId: string;
  beadId: string;
  iteration: RunIteration;
  attempt: RunAttempt;
  label: string;
  status: RunNodeStatus;
  session: RunSessionAttachment;
  currentIteration: boolean;
  historical: boolean;
}

export interface RunControlBadge {
  id: string;
  label: string;
  status: RunNodeStatus;
}

export interface RunDisplayNode {
  id: string;
  semanticNodeId: string;
  title: string;
  kind: string;
  constructKind: RunConstructKind;
  status: RunNodeStatus;
  currentBeadId: string;
  scope: RunNodeScope;
  /** False when this semantic node is transcript history only and should not render in the left graph. */
  visibleInGraph: boolean;
  /** True when all execution instances are from older loop iterations or stale expansion output. */
  historicalOnly: boolean;
  iterationSummary: RunIterationSummary;
  attemptSummary: RunAttemptSummary;
  visibleExecutionInstanceId: string;
  executionInstances: RunExecutionInstance[];
  controlBadges: RunControlBadge[];
}

export type RunNodeScope = { kind: 'run' } | { kind: 'scoped'; ref: string };

export type RunIterationSummary =
  | { kind: 'single' }
  | {
      kind: 'stacked';
      visibleIteration: number;
      iterationCount: number;
      control: { kind: 'known'; id: string } | { kind: 'unknown' };
    };

export type RunAttemptSummary =
  | { kind: 'none' }
  | {
      kind: 'tracked';
      count: number;
      badge: { kind: 'bounded'; label: string } | { kind: 'count-only' };
      active: { kind: 'running'; value: number } | { kind: 'idle' };
    };

export interface RunDisplayEdge {
  from: string;
  to: string;
  kind: string;
}

export interface RunDisplayLane {
  id: string;
  label: string;
  nodeIds: string[];
}

export interface FormulaRunProgress {
  snapshotVersion: number;
  snapshotEventSeq: RunSnapshotSequence;
  snapshotPartial: boolean;
  totalNodeCount: number;
  visibleNodeCount: number;
  edgeCount: number;
  executionInstanceCount: number;
  sessionLinkCount: number;
  streamableSessionCount: number;
  streamableSessionIds: string[];
  statusCounts: Partial<Record<RunNodeStatus, number>>;
  allStatusCounts: Partial<Record<RunNodeStatus, number>>;
}

export type FormulaRunPartialReason =
  | 'supervisor_snapshot_partial'
  | 'runtime_bead_read_failed'
  | 'session_list_failed'
  | 'formula_detail_missing_formula_metadata'
  | 'formula_detail_missing_run_target'
  | 'formula_detail_fetch_failed';

export type FormulaRunCompleteness =
  | { kind: 'complete' }
  | { kind: 'partial'; reasons: FormulaRunPartialReason[] };

export interface FormulaRunDetail {
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
  snapshotVersion: number;
  snapshotEventSeq: RunSnapshotSequence;
  completeness: FormulaRunCompleteness;
  progress: FormulaRunProgress;
  /**
   * gascity-dashboard-ud6j: the dashboard-derived phase ladder
   * (intake → implementation → review → approval → finalization) — the SAME
   * stages the snapshot lane renders, computed from this run's OWN beads via
   * the shared fromDashboardBead → mapRunPhase → stageProgress pipeline (no
   * recompute drift). Lets a single-root run with no materialized step DAG
   * still show live phase progression instead of a dead "1 node" line.
   */
  phase: RunPhase;
  stages: RunStage[];
  nodes: RunDisplayNode[];
  edges: RunDisplayEdge[];
  lanes: RunDisplayLane[];
}

/**
 * `source` records where the formula `name` came from:
 *
 * - `metadata` — the supervisor reported the formula identity. Either the
 *   run root carries `gc.formula`, OR the formula-detail fetch from the
 *   supervisor returned a name (the supervisor is canonical even when the
 *   root metadata key is absent).
 * - `title_fallback` — the supervisor did NOT set `gc.formula` on a
 *   graph.v2 root and the formula-detail fetch yielded nothing, so the
 *   resolver derived the name from the bead title. Per the project's
 *   "Don't Swallow Errors" posture this case is surfaced to the operator
 *   in a warn tone rather than rendered as if it were canonical metadata.
 *   See gascity-dashboard-e7hj for the precedent.
 */
export type RunFormula =
  | { kind: 'known'; name: string; source: RunFormulaSource }
  | { kind: 'unavailable'; reason: 'missing_formula_metadata' };

export type RunFormulaSource = 'metadata' | 'title_fallback';

export type RunFormulaDetailFetchFailure =
  | 'timeout'
  | 'not_found'
  | 'invalid_payload'
  | 'empty_response'
  | 'upstream_error';

export type RunFormulaDetailState =
  | { kind: 'available'; name: string; target: string }
  | { kind: 'unavailable'; reason: 'missing_formula_metadata' }
  | { kind: 'unavailable'; reason: 'missing_run_target'; name: string }
  | {
      kind: 'unavailable';
      reason: 'fetch_failed';
      name: string;
      target: string;
      failure: RunFormulaDetailFetchFailure;
    };

export type RunExecutionPath =
  | { kind: 'known'; path: string }
  | { kind: 'unavailable'; reason: 'missing_cwd_and_rig_root' };

export interface RunDiffRequest {
  executionPath: RunExecutionPath;
}

export type RunSnapshotSequence =
  | { kind: 'known'; seq: number }
  | { kind: 'unavailable'; reason: 'supervisor_omitted' };

export type RunDiffKind = 'ok' | 'not_git' | 'path_unknown' | 'error';

export type RunChangedFileKind = 'code' | 'test' | 'docs' | 'config' | 'other';

export interface RunChangedFile {
  path: string;
  status: string;
  kind: RunChangedFileKind;
}

export type RunDiffComparison =
  | { kind: 'upstream'; ref: string; mergeBase: string }
  | { kind: 'head'; reason: 'no_upstream' | 'upstream_lookup_failed' }
  | { kind: 'unavailable'; reason: 'path_unknown' | 'not_git' | 'error' };

interface RunDiffBase {
  rootPath: RunDiffRootPath;
  comparison: RunDiffComparison;
  status: string[];
  changedFiles: RunChangedFile[];
  patch: string;
  truncated: boolean;
}

export type RunDiffResponse =
  | (RunDiffBase & { kind: 'ok' })
  | (RunDiffBase & { kind: 'not_git' | 'path_unknown' })
  | (RunDiffBase & { kind: 'error'; error: string });

export type RunDiffRootPath =
  | { kind: 'known'; path: string }
  | { kind: 'unavailable'; reason: 'path_unknown' | 'not_git' | 'error' };
