/**
 * Golden-parity generator for the run-view TS pipeline (Phase 0 safety net).
 *
 * Loads the cross-boundary bead fixture (internal/runproj/testdata/beads_fixture.json,
 * in the `beads.Bead` event-payload JSON shape that the Go projection will fold
 * from .gc/events.jsonl), runs it through the CURRENT TS pipeline, and writes
 * the bead-derived RunSummary and one FormulaRunDetail as committed goldens.
 *
 *   beads.Bead JSON → DashboardBead → fromDashboardBead → buildRunSummary  → runsummary_golden.json
 *   beads.Bead JSON → RunSnapshotBead → enrichFormulaRun  → rundetail_golden.json
 *
 * The future Go event-sourced projection is tested against these exact outputs.
 *
 * Determinism: the captured RunSummary/RunDetail are BEAD-DERIVED only — no
 * session enrich (health/census), no Date.now(). The builder's default
 * census is captured as-is (status: 'unavailable'). See README notes in the
 * task report for the determinism guarantees.
 *
 * Usage:
 *   tsx scripts/gen-run-goldens.mts            # regenerate committed goldens
 *   tsx scripts/gen-run-goldens.mts --check    # verify committed goldens are current (CI guard)
 */
import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

import { buildRunSummary, runCounts } from '../shared/src/runs/summary.js';
import { fromDashboardBead } from '../shared/src/runs/phaseMapping.js';
import { enrichFormulaRun } from '../shared/src/runs/enrich.js';
import {
  advanceProgressMarks,
  buildCensus,
  deriveRunHealth,
  type LaneProgressMark,
} from '../shared/src/runs/health.js';
import { isStaleSessionlessLatch } from '../shared/src/runs/liveness.js';
import type { DashboardBead } from '../shared/src/dashboard-beads.js';
import type { DashboardSession } from '../shared/src/dashboard-sessions.js';
import type { RunSnapshot, RunSnapshotBead } from '../shared/src/run-snapshot.js';
import type { FormulaRunDetail } from '../shared/src/run-detail.js';
import type { RunSummary } from '../shared/src/snapshot/types.js';

const here = dirname(fileURLToPath(import.meta.url));
const testdataDir = resolve(here, '../../../../runproj/testdata');
const fixturePath = resolve(testdataDir, 'beads_fixture.json');
const sessionsFixturePath = resolve(testdataDir, 'sessions_fixture.json');
const summaryGoldenPath = resolve(testdataDir, 'runsummary_golden.json');
const enrichedGoldenPath = resolve(testdataDir, 'runsummary_enriched_golden.json');
const detailGoldenPath = resolve(testdataDir, 'rundetail_golden.json');

// The graph.v2 run captured as the FormulaRunDetail golden.
const DETAIL_RUN_ID = 'dt-adopt1';

// Fixed snapshot-generation time for the enriched golden: the stale-session-less
// latch demotion judges age against this, not a live clock, so the golden is
// deterministic. The Go enrich test parses the same instant.
const ENRICHED_GENERATION_MS = Date.parse('2026-06-09T00:00:00.000Z');

/**
 * The `beads.Bead` event-payload JSON shape (internal/beads/beads.go). Only the
 * keys the run pipeline reads are typed; the JSON may carry more. This is the
 * cross-boundary input shape the Go projection folds.
 */
interface BeadPayload {
  id: string;
  title: string;
  status: string;
  issue_type: string;
  priority?: number | null;
  created_at: string;
  updated_at?: string;
  assignee?: string;
  from?: string;
  parent?: string;
  ref?: string;
  needs?: string[] | null;
  description?: string;
  labels?: string[];
  metadata?: Record<string, string>;
  ephemeral?: boolean;
}

function loadFixture(): BeadPayload[] {
  const raw = readFileSync(fixturePath, 'utf8');
  const parsed: unknown = JSON.parse(raw);
  if (!Array.isArray(parsed)) {
    throw new Error(`beads_fixture.json must be a JSON array, got ${typeof parsed}`);
  }
  return parsed as BeadPayload[];
}

/**
 * Map a `beads.Bead` event payload to the dashboard's DashboardBead wire shape.
 *
 * The Go JSON tags already match DashboardBead field names (`issue_type`,
 * `parent`, `created_at`, `updated_at`, `metadata`, ...) — there is NO
 * snake/camel rename on the wire. The only adaptation is `priority`: the Go
 * payload omits it when nil, while DashboardBead requires `priority: number |
 * null`, so a missing payload priority becomes null. Optional payload keys are
 * forwarded only when present so exactOptionalPropertyTypes is satisfied.
 */
function toDashboardBead(b: BeadPayload): DashboardBead {
  const bead: DashboardBead = {
    id: b.id,
    title: b.title,
    status: b.status,
    issue_type: b.issue_type,
    priority: b.priority ?? null,
    created_at: b.created_at,
  };
  if (b.updated_at !== undefined) bead.updated_at = b.updated_at;
  if (b.description !== undefined) bead.description = b.description;
  if (b.assignee !== undefined) bead.assignee = b.assignee;
  if (b.from !== undefined) bead.from = b.from;
  if (b.parent !== undefined) bead.parent = b.parent;
  if (b.ref !== undefined) bead.ref = b.ref;
  if (b.labels !== undefined) bead.labels = b.labels;
  if (b.metadata !== undefined) bead.metadata = b.metadata;
  if (b.needs !== undefined) bead.needs = b.needs;
  if (b.ephemeral !== undefined) bead.ephemeral = b.ephemeral;
  return bead;
}

/**
 * Map a `beads.Bead` event payload to the supervisor run-snapshot bead row
 * (RunSnapshotBead). The detail pipeline consumes the supervisor wire shape,
 * where `issue_type` becomes `kind`, the step ref is the structural identity,
 * and metadata is required (never undefined). This mirrors how the supervisor's
 * /workflow/{id} endpoint projects molecule beads into a run snapshot.
 */
function toRunSnapshotBead(b: BeadPayload): RunSnapshotBead {
  const kind = b.metadata?.['gc.original_kind'] ?? b.issue_type;
  const bead: RunSnapshotBead = {
    id: b.id,
    title: b.title,
    status: b.status,
    kind,
    metadata: b.metadata ?? {},
  };
  if (b.ref !== undefined) bead.step_ref = b.ref;
  if (b.assignee !== undefined) bead.assignee = b.assignee;
  const scopeRef = b.metadata?.['gc.scope_ref'];
  if (scopeRef !== undefined) bead.scope_ref = scopeRef;
  const logical = b.metadata?.['gc.logical_bead_id'];
  if (logical !== undefined) bead.logical_bead_id = logical;
  return bead;
}

/**
 * Synthesize a deterministic RunSnapshot for one run from the bead fixture —
 * the run root plus every bead that belongs to it (root_bead_id / parent /
 * id-prefix). The snapshot identity is taken from the root's scope metadata so
 * enrichFormulaRun's graph.v2 + scope gates pass without any live supervisor.
 */
function snapshotForRun(beads: BeadPayload[], rootId: string): RunSnapshot {
  const root = beads.find((b) => b.id === rootId);
  if (root === undefined) {
    throw new Error(`detail run root ${rootId} not found in fixture`);
  }
  const members = beads.filter(
    (b) =>
      b.id === rootId ||
      b.parent === rootId ||
      b.metadata?.['gc.root_bead_id'] === rootId ||
      b.id.startsWith(`${rootId}.`),
  );
  const rootStoreRef = root.metadata?.['gc.root_store_ref'] ?? '';
  const scopeKind = root.metadata?.['gc.scope_kind'] ?? '';
  const scopeRef = root.metadata?.['gc.scope_ref'] ?? '';

  return {
    run_id: rootId,
    root_bead_id: rootId,
    root_store_ref: rootStoreRef,
    resolved_root_store: rootStoreRef,
    scope_kind: scopeKind,
    scope_ref: scopeRef,
    snapshot_version: 1,
    snapshot_event_seq: 100,
    partial: false,
    stores_scanned: [rootStoreRef],
    beads: members.map(toRunSnapshotBead),
    deps: depsForMembers(members),
    logical_nodes: null,
    logical_edges: null,
    scope_groups: null,
  };
}

/**
 * Structural dependency edges between a run's beads: each step depends on the
 * root. Deterministic (driven by member order in the fixture), and enough for
 * buildRunDisplayEdges to wire the detail graph.
 */
function depsForMembers(members: BeadPayload[]): RunSnapshot['deps'] {
  const rootId = members[0]?.id;
  if (rootId === undefined) return [];
  return members
    .filter((b) => b.id !== rootId)
    .map((b) => ({ from: rootId, to: b.id, kind: 'parent' }));
}

function buildSummaryGolden(beads: BeadPayload[]): RunSummary {
  const issues = beads.map(toDashboardBead).map(fromDashboardBead);
  return buildRunSummary(issues);
}

function loadSessions(): DashboardSession[] {
  const raw = readFileSync(sessionsFixturePath, 'utf8');
  const parsed: unknown = JSON.parse(raw);
  if (!Array.isArray(parsed)) {
    throw new Error(`sessions_fixture.json must be a JSON array, got ${typeof parsed}`);
  }
  return parsed as DashboardSession[];
}

/**
 * The bead-derived summary with session enrichment layered on, captured as the
 * oracle for the Go EnrichRunSummary port. This replicates the frontend
 * enrichRunSummary composition (supervisor/runSummary.ts) for a single snapshot:
 * a cold mark advance (no prior generation), deriveRunHealth against a fixture
 * sessions list (sessionsAvailable: true), the blocked-lane split, the
 * stale-session-less-latch demotion at a fixed generation time, then recomputed
 * counts and census. The per-city progressStateByCity gating is a multi-call
 * frontend concern that, for one generation from empty marks, reduces to a single
 * advanceProgressMarks — exactly what the per-city tailer does server-side.
 */
function buildEnrichedGolden(beads: BeadPayload[], sessions: DashboardSession[]): RunSummary {
  const base = buildSummaryGolden(beads);
  const inFlight = [...base.lanes, ...base.blockedLanes];
  const marks = advanceProgressMarks(new Map<string, LaneProgressMark>(), inFlight);
  const { lanes } = deriveRunHealth({
    lanes: inFlight,
    sessions,
    sessionsAvailable: true,
    marks,
  });
  const blockedLanes = lanes.filter((lane) => lane.phase === 'blocked');
  const activeEnriched = lanes.filter((lane) => lane.phase !== 'blocked');
  const liveActive = activeEnriched.filter(
    (lane) => !isStaleSessionlessLatch(lane, ENRICHED_GENERATION_MS, true),
  );
  const census = buildCensus([...liveActive, ...blockedLanes]);
  return {
    ...base,
    totalActive: liveActive.length,
    lanes: liveActive,
    blockedLanes,
    runCounts: runCounts(liveActive, liveActive.length, blockedLanes.length),
    census: { status: 'available', data: census },
  };
}

function buildDetailGolden(beads: BeadPayload[]): FormulaRunDetail {
  const snapshot = snapshotForRun(beads, DETAIL_RUN_ID);
  // No sessions / no formulaDetail: capture the BEAD-DERIVED detail only, so the
  // golden is independent of any live supervisor or session list.
  return enrichFormulaRun(snapshot, {});
}

function stableJson(value: unknown): string {
  return `${JSON.stringify(value, null, 2)}\n`;
}

function main(): void {
  const check = process.argv.includes('--check');
  const beads = loadFixture();
  const sessions = loadSessions();

  const summary = stableJson(buildSummaryGolden(beads));
  const enriched = stableJson(buildEnrichedGolden(beads, sessions));
  const detail = stableJson(buildDetailGolden(beads));

  if (check) {
    const drift: string[] = [];
    for (const [path, next] of [
      [summaryGoldenPath, summary],
      [enrichedGoldenPath, enriched],
      [detailGoldenPath, detail],
    ] as const) {
      let current = '';
      try {
        current = readFileSync(path, 'utf8');
      } catch {
        current = '';
      }
      if (current !== next) drift.push(path);
    }
    if (drift.length > 0) {
      console.error('run goldens are stale; re-run `npm run gen:run-goldens`. Drifted files:');
      for (const path of drift) console.error(`  ${path}`);
      process.exit(1);
    }
    console.log('run goldens are up to date');
    return;
  }

  writeFileSync(summaryGoldenPath, summary);
  writeFileSync(enrichedGoldenPath, enriched);
  writeFileSync(detailGoldenPath, detail);
  console.log(`wrote ${summaryGoldenPath}`);
  console.log(`wrote ${enrichedGoldenPath}`);
  console.log(`wrote ${detailGoldenPath}`);
}

main();
