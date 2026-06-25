import type { FormulaDetail, RunSnapshotBead } from '../run-snapshot.js';
import type { RunFormulaSource } from '../run-detail.js';
import { meta, nonEmpty } from './bead-fields.js';

export type RunFormulaIdentityMode = 'detail' | 'lane' | 'route' | 'state';
export type RunFormulaIdentitySource = RunFormulaSource | 'formula_detail';

interface RunFormulaRootLike {
  title: string;
  status: string;
  assignee?: string;
  metadata?: Record<string, string>;
}

export interface ResolvedRunFormulaIdentity {
  name: string | null;
  source: RunFormulaIdentitySource | null;
  target: string | null;
}

export interface ResolveRunFormulaIdentityInput {
  root?: RunFormulaRootLike | undefined;
  formulaDetail?: Pick<FormulaDetail, 'name'> | undefined;
  issues?: readonly RunFormulaRootLike[];
}

/**
 * Resolved workflow formula name plus the provenance of that name.
 *
 * `source` carries which resolution path produced `name`:
 *  - `'metadata'` — the workflow root carried an explicit `gc.formula` key.
 *  - `'title_fallback'` — gc.formula was absent and the resolver derived
 *    the name from the bead title under the graph.v2 + `gc.run_target`
 *    gate. The dashboard surfaces this provenance to the operator in a
 *    warn tone instead of letting it pass as canonical metadata. See
 *    gascity-dashboard-e7hj for the precedent.
 */
export interface ResolvedRunFormulaName {
  name: string;
  source: RunFormulaSource;
}

/**
 * Resolve a workflow root bead to its formula name and the provenance of
 * that name.
 *
 * gascity-dashboard-sadp: the live supervisor does NOT set `gc.formula` on
 * graph.v2 workflow roots — the formula name lives in the bead title by
 * convention (verified against live city data: title equals the
 * registered formula name for every observed graph.v2 root, including
 * `mol-focus-review`, `mol-dashboard-graphv2-smoke`). Without this
 * fallback, both the formula-detail fetch in routes/runs.ts and the
 * presentation-enrichment in formula-run.ts would treat graph.v2 lanes as
 * missing a formula, collapsing every graph.v2 run-detail page to an
 * empty `formula_detail_unavailable` state.
 *
 * The gate on `gc.run_target` is what keeps the fallback honest: only
 * fully-instantiated runnable roots set both `gc.formula_contract` and
 * `gc.run_target`. Operator-edited descriptive titles on closed roots
 * without a target won't be mis-surfaced as formula names.
 *
 * gascity-dashboard-xfb7 (sadp follow-up): terminal graph.v2 roots are
 * additionally excluded from the title fallback even when they retain
 * `gc.run_target`. After a run completes operators sometimes retitle the
 * root to a descriptive summary (e.g. 'investigation: foo bug'); a terminal
 * run cannot be re-fetched against the supervisor's formula registry to
 * refute a bad name, so the safer behavior is to defer — return null and
 * let the consumer render 'unavailable' rather than a false attribution.
 * Operators can override by setting `gc.formula` in metadata; the metadata
 * path remains canonical regardless of run state.
 *
 * Returns `{name, source}`, or `null` if neither the explicit key nor the
 * gated title fallback yields one. Callers may layer additional fallbacks
 * (e.g. `formulaDetail?.name` for the rare case where the detail fetch
 * succeeds despite missing root metadata) — those fallback paths attach
 * their own `source` value.
 *
 * Returning a single object (rather than `name()` + `source()`) keeps the
 * call atomic so a caller cannot accidentally surface a name from one
 * resolution path with the source label of another.
 */
export function resolveRunFormulaName(
  root: RunSnapshotBead | undefined,
): ResolvedRunFormulaName | null {
  if (!root) return null;
  const explicit = meta(root, 'gc.formula');
  if (explicit !== undefined) return { name: explicit, source: 'metadata' };
  if (
    meta(root, 'gc.formula_contract') === 'graph.v2' &&
    meta(root, 'gc.run_target') !== undefined &&
    !isTerminalRunRootStatus(root.status)
  ) {
    const title = root.title.trim();
    if (title.length > 0) return { name: title, source: 'title_fallback' };
  }
  return null;
}

export function resolveRunFormulaIdentity(
  mode: RunFormulaIdentityMode,
  { root, formulaDetail, issues = [] }: ResolveRunFormulaIdentityInput,
): ResolvedRunFormulaIdentity {
  const target = runFormulaTarget(root);
  const metadata = runFormulaMetadataName(mode, root, issues);
  if (metadata !== null) return { name: metadata, source: 'metadata', target };

  if (mode === 'detail' || mode === 'state') {
    const detailName = nonEmpty(formulaDetail?.name);
    if (detailName !== undefined) {
      return { name: detailName, source: 'formula_detail', target };
    }
  }

  const title = runFormulaTitleFallback(mode, root);
  if (title !== null) return { name: title, source: 'title_fallback', target };

  return { name: null, source: null, target };
}

function runFormulaMetadataName(
  mode: RunFormulaIdentityMode,
  root: RunFormulaRootLike | undefined,
  issues: readonly RunFormulaRootLike[],
): string | null {
  if (mode === 'lane') {
    return (
      metadataString(issues, 'pr_review.workflow_formula') ??
      metadataString(issues, 'gc.formula') ??
      null
    );
  }
  return rootMeta(root, 'gc.formula') ?? rootMeta(root, 'gc.formula_name') ?? null;
}

function runFormulaTitleFallback(
  mode: RunFormulaIdentityMode,
  root: RunFormulaRootLike | undefined,
): string | null {
  if (root === undefined) return null;
  if (
    rootMeta(root, 'gc.formula_contract') !== 'graph.v2' ||
    rootMeta(root, 'gc.run_target') === undefined ||
    isTerminalRunRootStatus(root.status)
  ) {
    return null;
  }
  const title = nonEmpty(root.title);
  if (title === undefined) return null;
  return mode === 'lane' && !title.startsWith('mol-') ? null : title;
}

function isTerminalRunRootStatus(status: string): boolean {
  switch (status.trim().toLowerCase()) {
    case 'closed':
    case 'completed':
    case 'done':
    case 'failed':
    case 'skipped':
      return true;
    default:
      return false;
  }
}

function runFormulaTarget(root: RunFormulaRootLike | undefined): string | null {
  return (
    rootMeta(root, 'gc.run_target') ??
    rootMeta(root, 'gc.routed_to') ??
    nonEmpty(root?.assignee) ??
    null
  );
}

function rootMeta(root: RunFormulaRootLike | undefined, key: string): string | undefined {
  return nonEmpty(root?.metadata?.[key]);
}

function metadataString(issues: readonly RunFormulaRootLike[], key: string): string | undefined {
  for (const issue of issues) {
    const value = rootMeta(issue, key);
    if (value !== undefined) return value;
  }
  return undefined;
}
