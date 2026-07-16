import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router-dom';
import { GC_EVENT_PREFIX, SCOPE_REF_RE } from 'gas-city-dashboard-shared';
import type {
  FormulaRunDetail as FormulaRunDetailData,
  FormulaRunProgress,
  RunNodeStatus,
  FormulaRunPartialReason,
  RunScopeKind,
  RunSummary,
  SourceState,
} from 'gas-city-dashboard-shared';
import { Button } from '../components/Button';
import { PageHeader } from '../components/PageHeader';
import { RelatedEntities } from '../components/RelatedEntities';
import { BeadDetailModal } from '../components/BeadDetailModal';
import { FormulaRunDiagram } from '../components/run/FormulaRunDiagram';
import { FormulaRunTabs, type RunEvidenceTab } from '../components/run/FormulaRunTabs';
import { StageLadder } from '../components/run/StageLadder';
import { useNow } from '../contexts/NowContext';
import { useGcEventRefresh } from '../hooks/useGcEvents';
import { runEventIdentity, formulaRunDetailEventMatches } from '../hooks/runEventIdentity';
import { useRunNodeSelection } from '../hooks/useRunNodeSelection';
import { useFormulaRunDetail } from '../hooks/useFormulaRunDetail';
import { useRunDiff } from '../hooks/useRunDiff';
import { useEntityLinks } from '../hooks/useEntityLinks';
import { getCached } from '../api/cache';
import { getActiveCity } from '../api/cityBase';

const RUN_DETAIL_EVENT_PREFIXES = [GC_EVENT_PREFIX.bead, GC_EVENT_PREFIX.session] as const;
const NO_EVENT_PREFIXES: readonly string[] = [];

export function FormulaRunDetailPage() {
  const { runId } = useParams<{ runId: string }>();
  const [search] = useSearchParams();
  const parsedScope = parseScope(search);
  const scope = parsedScope.ok ? parsedScope.scope : undefined;
  const routeError = parsedScope.ok ? null : parsedScope.error;
  const initialNodeId = search.get('node');
  const routeSelectionKey = [
    runId ?? '',
    scope?.scopeKind ?? '',
    scope?.scopeRef ?? '',
    initialNodeId ?? '',
  ].join('\u0000');
  const runDetail = useFormulaRunDetail(
    routeError ? undefined : runId,
    scope?.scopeKind,
    scope?.scopeRef,
  );
  const readyRun = runDetail.kind === 'ready' ? runDetail : null;
  const detail = readyRun?.detail ?? null;
  // gascity-dashboard-9w3k: v1 / wisp runs are clickable in the run list but
  // have no graph.v2 step-detail view. The hook reports this as a distinct
  // 'unsupported' state (not a generic load failure) so we can render an honest
  // list-only message instead of the opaque "Formula run unavailable." dead-end.
  const unsupported = runDetail.kind === 'unsupported';
  // gascity-dashboard (Major 2): a raw 404 from the workflow endpoint (no
  // snapshot at all) is ambiguous — distinct from the reliable v1 'unsupported'
  // signal and from a generic transport failure. We surface it as its own honest
  // "detail snapshot not found" state instead of mislabeling it as v1.
  const notFound = runDetail.kind === 'not_found';
  const runDiff = useRunDiff(
    routeError || detail === null ? undefined : runId,
    detail?.executionPath,
    scope?.scopeKind,
    scope?.scopeRef,
  );
  const initialLoading = runDetail.kind === 'loading';
  const refreshing =
    (readyRun !== null && readyRun.refreshState.kind === 'refreshing') ||
    (runDiff.kind === 'ready' && runDiff.refreshState.kind === 'refreshing');
  const diffInitialLoading = detail !== null && runDiff.kind === 'loading';
  const loading = initialLoading || refreshing || diffInitialLoading;
  const loadError =
    runDetail.kind === 'failed'
      ? runDetail.error
      : readyRun !== null && readyRun.refreshState.kind === 'failed'
        ? readyRun.refreshState.error
        : null;
  // The Diff tab is the default-active view; the parent owns the tab so the P5
  // gate below can suppress the diff's git-exec read while it is hidden.
  const [activeTab, setActiveTab] = useState<RunEvidenceTab>('diff');
  const diffTabActive = activeTab === 'diff';

  // P4: the run DETAIL streams (useFormulaRunDetail owns the per-run SSE stream,
  // which pushes the whole DTO with zero refetch), so when the stream is live
  // this nudge lane refreshes ONLY the run DIFF (a separate git-exec read the
  // stream does not carry). When the stream is UNAVAILABLE — a runtime with no
  // EventSource, where detail would otherwise be fetched once then frozen — the
  // nudge must ALSO refresh the detail so its auto-refresh survives.
  //
  // P5: the diff refresh is now (a) TTL-absorbed via cheapRefresh (refresh=false,
  // the server diff TTL coalesces bursts) instead of the bypassing manual lane,
  // and (b) gated on the Diff tab being visible — a nudge fires ZERO /diff POSTs
  // while the tab is hidden. The manual Refresh button (refreshRunResources)
  // keeps the bypass. Switching TO the Diff tab refreshes it once (below) so it
  // is never stale from the nudges it skipped.
  const streamActive = runDetail.streamActive;
  useGcEventRefresh(
    routeError ? NO_EVENT_PREFIXES : RUN_DETAIL_EVENT_PREFIXES,
    () =>
      void runDetailNudgeRefresh(
        streamActive,
        diffTabActive,
        runDetail.refresh,
        runDiff.cheapRefresh,
      ),
    {
      matches: (event) => {
        const identity = runEventIdentity(event);
        // F4: before the detail loads (still warming, or the first load
        // failed), anchor the match on the ROUTE's runId. A printed deep link
        // lands here before the just-slung run's bead events exist, and
        // requiring a loaded detail meant those eventual events could never
        // nudge the page out of the failed state. An identity-less (ambient)
        // event also matches — a root bead's own events may carry no run
        // identity — mirroring the non-terminal ambient behavior below.
        if (detail === null) {
          return runId !== undefined && (identity.runIds.size === 0 || identity.runIds.has(runId));
        }
        if (detail.progress.terminal && identityIsAmbient(identity)) return false;
        return formulaRunDetailEventMatches(identity, {
          runId: detail.runId,
          rootBeadId: detail.rootBeadId,
        });
      },
    },
  );
  // P5: switching TO the Diff tab refreshes the diff once so it is not stale from
  // the nudges it skipped while hidden. The refresh lives in an effect (not the
  // tab-change handler) so it is a pure state update — React StrictMode's updater
  // double-invoke can't fire the git-exec read twice. The mount already loads the
  // diff for the default-active Diff tab, so a ref-tracked "was the Diff tab last
  // visible" flag suppresses the initial render and fires only on a genuine
  // hidden→visible transition. cheapRefresh is captured in a ref so the effect
  // depends only on visibility, never re-running on an unrelated re-render.
  const diffCheapRefreshRef = useRef(runDiff.cheapRefresh);
  diffCheapRefreshRef.current = runDiff.cheapRefresh;
  const diffTabWasActiveRef = useRef(diffTabActive);
  useEffect(() => {
    const wasActive = diffTabWasActiveRef.current;
    diffTabWasActiveRef.current = diffTabActive;
    if (diffTabActive && !wasActive) void diffCheapRefreshRef.current();
  }, [diffTabActive]);
  const handleActiveTabChange = useCallback((next: RunEvidenceTab) => setActiveTab(next), []);

  const pageError = routeError ?? loadError;
  // F4: while the initial load polls the graced warming 503 (reason
  // 'unknown_run': the projection is warm but has never seen this run — the
  // just-slung deep-link window, or a genuinely dead link), the interim copy
  // must be honest about BOTH possibilities instead of an anonymous spinner. A
  // reason-less warming 503 (the projection itself is cold-replaying) keeps
  // the plain loading copy: the run is not in doubt there.
  const warmingUnknownRun =
    runDetail.kind === 'loading' && runDetail.warming?.reason === 'unknown_run';
  const { selectedNodeId, selectedNode, toggleNode } = useRunNodeSelection(
    detail,
    initialNodeId,
    routeSelectionKey,
  );

  // Related entities (gascity-dashboard-j4x). Focus on the run's root
  // bead so the index surfaces the molecule members, sessions, and the
  // PR/issue this run is adopting.
  const links = useEntityLinks(detail?.rootBeadId ?? null);
  const [viewingBeadId, setViewingBeadId] = useState<string | null>(null);
  const now = useNow();
  const cityName = getActiveCity();

  // Optimistic skeleton (gascity-dashboard-wqsk, gascity-dashboard-i60u): when
  // the operator arrives from /runs the shared run-summary subscription has
  // already warmed this cache key with this run's lane (title + phase stages),
  // so paint that instantly instead of a blank spinner while the full detail
  // assembles. We CONSUME the cache directly and fire NO mount read of our own:
  // on a cold direct/refresh load the key is empty, so the skeleton is simply
  // absent and the lightweight loading state renders — the page no longer spends
  // a browser connection on a duplicate molecule(all=true)+feed cold scan (7-11s)
  // that would queue behind, and starve, the detail's own fast reads. The
  // always-mounted RunSummaryProvider remains the sole owner of this key's fetch.
  const [warmRunSummary] = useState<SourceState<RunSummary> | undefined>(() =>
    getCached(`runs:summary:${cityName ?? 'no-city'}`),
  );
  const skeletonLane = useMemo(() => {
    if (!runId) return null;
    const runsData =
      warmRunSummary && warmRunSummary.status !== 'error' ? warmRunSummary.data : null;
    if (runsData === null || runsData === undefined) return null;
    // gascity-dashboard-4xcv: blocked lanes live in their own bucket now, and
    // a blocked run is the most likely one the operator clicks into.
    return [...runsData.lanes, ...runsData.blockedLanes].find((lane) => lane.id === runId) ?? null;
  }, [warmRunSummary, runId]);

  const synopsis = detail
    ? `${detail.progress.visibleNodeCount} nodes. ${summarizeNodeStatuses(detail.progress)}. Local changes are shown for the run execution folder.`
    : (initialLoading && !routeError) || unsupported || notFound
      ? undefined
      : 'Formula run unavailable.';

  return (
    <section>
      <PageHeader
        title={detail?.title ?? 'Formula Run'}
        synopsis={synopsis}
        meta={
          <>
            <Link
              to="/runs"
              className="focus-mark text-label uppercase tracking-wider text-fg-muted hover:text-fg"
            >
              Runs
            </Link>
            {pageError && detail && (
              <span className="normal-case text-body text-accent" role="alert">
                {pageError}
              </span>
            )}
            {detail && (
              <span className="text-label uppercase tracking-wider text-fg-faint tnum">
                {snapshotLabel(detail)}
              </span>
            )}
            <Button
              size="sm"
              onClick={() => void refreshRunResources(runDetail.refresh, runDiff.refresh)}
              disabled={loading || Boolean(routeError)}
            >
              {refreshing ? 'Refreshing' : 'Refresh'}
            </Button>
          </>
        }
      />

      {loading && !routeError && !detail ? (
        skeletonLane ? (
          <>
            <StageLadder stages={skeletonLane.stages} label={skeletonLane.title} />
            <p className="text-body text-fg-muted italic mt-8">Loading run detail.</p>
          </>
        ) : warmingUnknownRun ? (
          <p className="text-body text-fg-muted italic" role="status">
            This run may still be being recorded — new work can take a couple of minutes to appear —
            or it may no longer exist.
          </p>
        ) : (
          <p className="text-body text-fg-muted italic">Loading formula run.</p>
        )
      ) : unsupported ? (
        <p className="text-body text-fg-muted" role="status">
          Detailed step view isn&rsquo;t available for this run (v1/wisp runs are list-only) — this
          run appears in the run list only.
        </p>
      ) : notFound ? (
        <p className="text-body text-fg-muted" role="status">
          This run&rsquo;s detail snapshot was not found. It may be a v1/wisp run, a completed run
          whose snapshot wasn&rsquo;t retained, or no longer available.
        </p>
      ) : pageError && !detail ? (
        <p className="text-body text-accent" role="alert">
          {pageError}
        </p>
      ) : readyRun ? (
        <>
          <RunMetadata detail={readyRun.detail} />
          <StageLadder stages={readyRun.detail.stages} label={readyRun.detail.title} />
          <FormulaRunPartialNotice detail={readyRun.detail} />
          <div className="mt-8 grid gap-10 lg:grid-cols-[minmax(0,0.95fr)_minmax(22rem,1.05fr)]">
            <FormulaRunDiagram
              detail={readyRun.detail}
              selectedNodeId={selectedNodeId}
              onToggleNode={toggleNode}
            />
            <FormulaRunTabs
              diff={runDiff}
              selectedNode={selectedNode}
              activeTab={activeTab}
              onActiveTabChange={handleActiveTabChange}
            />
          </div>
          <RelatedEntities
            view={links.view}
            loading={links.loading}
            error={links.error}
            now={now}
            onOpenBead={setViewingBeadId}
          />
          <BeadDetailModal
            open={viewingBeadId !== null}
            onClose={() => setViewingBeadId(null)}
            beadId={viewingBeadId}
            onOpenBead={setViewingBeadId}
          />
        </>
      ) : null}
    </section>
  );
}

async function refreshRunResources(
  refreshDetail: () => Promise<void>,
  refreshDiff: () => Promise<void>,
): Promise<void> {
  await Promise.all([refreshDetail(), refreshDiff()]);
}

/**
 * The bead/session nudge callback (P4/P5). When the per-run detail stream is live
 * it carries detail on its own, so the nudge refreshes ONLY the run diff (a
 * separate git-exec read the stream does not push) — avoiding a double detail
 * refetch. When the stream is UNAVAILABLE (a runtime with no EventSource), detail
 * would freeze after first paint, so the nudge must refresh detail AND diff.
 *
 * P5: the diff refresh is gated on the Diff tab being visible (`diffTabActive`) —
 * a hidden Diff tab fires ZERO /diff POSTs — and `refreshDiff` is the TTL-absorbed
 * cheapRefresh (refresh=false), not the bypassing manual lane. Exported so the
 * branch is unit-tested directly without the coupled EventSource harness.
 */
export function runDetailNudgeRefresh(
  streamActive: boolean,
  diffTabActive: boolean,
  refreshDetail: () => Promise<void>,
  refreshDiff: () => Promise<void>,
): Promise<void> {
  const diff = diffTabActive ? refreshDiff : noopRefresh;
  return streamActive ? diff() : refreshRunResources(refreshDetail, diff);
}

async function noopRefresh(): Promise<void> {}

function identityIsAmbient(identity: ReturnType<typeof runEventIdentity>): boolean {
  return identity.runIds.size === 0 && identity.rootBeadIds.size === 0;
}

function RunMetadata({ detail }: { detail: FormulaRunDetailData }) {
  const formulaDetail = formulaDetailLabel(detail.formulaDetail);
  return (
    <dl className="grid gap-x-8 gap-y-3 sm:grid-cols-2 lg:grid-cols-4">
      <FormulaMeta formula={detail.formula} />
      {formulaDetail !== null && <Meta label="Formula Detail" value={formulaDetail} />}
      <Meta label="Root" value={detail.rootBeadId} />
      <Meta label="Scope" value={`${detail.scopeKind}:${detail.scopeRef}`} />
      <Meta label="Store" value={detail.resolvedRootStore || detail.rootStoreRef || 'unknown'} />
    </dl>
  );
}

function Meta({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-label uppercase tracking-wider text-fg-faint">{label}</dt>
      <dd className="text-body text-fg break-all tnum">{value}</dd>
    </div>
  );
}

const TITLE_FALLBACK_TOOLTIP =
  'name inferred from bead title — supervisor did not set gc.formula on this graph.v2 root';

/**
 * Render the formula metadata cell with provenance.
 *
 * gascity-dashboard-e7hj: when the backend resolved the formula name via
 * the gated title fallback (source === 'title_fallback') we surface that
 * in a warn tone with an explanatory aside, matching the Health.tsx
 * "not reported by supervisor" precedent (PR #36). Per DESIGN.md's
 * "States have words" rule the warn tone is paired with a short textual
 * correlate ("inferred from bead title") and the longer explanation
 * lives in the native tooltip so the cell stays legible in greyscale.
 */
function FormulaMeta({ formula }: { formula: FormulaRunDetailData['formula'] }) {
  if (formula.kind !== 'known') {
    return <Meta label="Formula" value="metadata missing" />;
  }
  switch (formula.source) {
    case 'metadata':
      return <Meta label="Formula" value={formula.name} />;
    case 'title_fallback':
      return (
        <div>
          <dt className="text-label uppercase tracking-wider text-fg-faint">Formula</dt>
          <dd
            className="text-body text-warn break-all tnum"
            title={TITLE_FALLBACK_TOOLTIP}
            aria-label={`${formula.name} (${TITLE_FALLBACK_TOOLTIP})`}
          >
            {formula.name}
            <span className="ml-2 text-label uppercase tracking-wider text-warn">
              inferred from bead title
            </span>
          </dd>
        </div>
      );
    default: {
      // Exhaustiveness: a new RunFormulaSource variant must declare its
      // own render path — falling through to a default warn tone would
      // silently misrepresent its provenance.
      const _exhaustive: never = formula.source;
      return _exhaustive;
    }
  }
}

function snapshotLabel(detail: FormulaRunDetailData): string {
  return detail.snapshotEventSeq.kind === 'known'
    ? `v${detail.snapshotVersion} · seq ${detail.snapshotEventSeq.seq}`
    : `v${detail.snapshotVersion}`;
}

function formulaDetailLabel(formulaDetail: FormulaRunDetailData['formulaDetail']): string | null {
  if (formulaDetail.kind === 'available') return `available for ${formulaDetail.target}`;
  if (formulaDetail.reason === 'missing_formula_metadata') return null;
  if (formulaDetail.reason === 'missing_run_target')
    return `missing run target for ${formulaDetail.name}`;
  return `${formulaDetail.failure} for ${formulaDetail.target}`;
}

function FormulaRunPartialNotice({ detail }: { detail: FormulaRunDetailData }) {
  if (detail.completeness.kind !== 'partial') return null;
  const reasons = pageLevelPartialReasons(detail.completeness.reasons);
  if (reasons.length === 0) return null;
  return (
    <p className="mt-5 text-label uppercase tracking-wider text-warn" role="status">
      Partial run data: {partialReasonsLabel(reasons)}.
    </p>
  );
}

function pageLevelPartialReasons(
  reasons: readonly FormulaRunPartialReason[],
): FormulaRunPartialReason[] {
  return reasons.filter((reason) => !isFormulaDetailPartialReason(reason));
}

function isFormulaDetailPartialReason(reason: FormulaRunPartialReason): boolean {
  switch (reason) {
    case 'formula_detail_missing_formula_metadata':
    case 'formula_detail_missing_run_target':
    case 'formula_detail_fetch_failed':
      return true;
    case 'supervisor_snapshot_partial':
    case 'runtime_bead_read_failed':
    case 'session_list_failed':
      return false;
  }
}

function partialReasonsLabel(reasons: readonly FormulaRunPartialReason[]): string {
  return reasons.map(partialReasonLabel).join(', ');
}

function partialReasonLabel(reason: FormulaRunPartialReason): string {
  switch (reason) {
    case 'supervisor_snapshot_partial':
      return 'supervisor snapshot is partial';
    case 'runtime_bead_read_failed':
      return 'runtime bead refresh failed';
    case 'session_list_failed':
      return 'session list failed';
    case 'formula_detail_missing_formula_metadata':
      return 'formula metadata is missing';
    case 'formula_detail_missing_run_target':
      return 'formula run target is missing';
    case 'formula_detail_fetch_failed':
      return 'formula detail fetch failed';
  }
}

type ScopeParseResult =
  | { ok: true; scope?: { scopeKind: RunScopeKind; scopeRef: string } }
  | { ok: false; error: string };

function parseScope(search: URLSearchParams): ScopeParseResult {
  const rawKinds = search.getAll('scope_kind');
  const rawRefs = search.getAll('scope_ref');
  if (rawKinds.length > 1 || rawRefs.length > 1) {
    return { ok: false, error: 'Invalid run scope query.' };
  }

  const rawKind = rawKinds[0];
  const rawRef = rawRefs[0];
  if (rawKind === undefined && rawRef === undefined) return { ok: true };

  // Reject a half-specified scope (one of kind/ref present) rather than
  // silently dropping it — the backend rejects the same input as a 400, so
  // failing closed here keeps a truncated deep link from loading the WRONG
  // (default city) run instead of erroring.
  if (rawKind === undefined || rawRef === undefined) {
    return { ok: false, error: 'Invalid run scope query.' };
  }

  if (rawKind !== 'city' && rawKind !== 'rig') {
    return { ok: false, error: 'Invalid run scope query.' };
  }
  if (!SCOPE_REF_RE.test(rawRef)) {
    return { ok: false, error: 'Invalid run scope query.' };
  }

  return { ok: true, scope: { scopeKind: rawKind, scopeRef: rawRef } };
}

function summarizeNodeStatuses(progress: FormulaRunProgress): string {
  const parts = [
    statusSummaryPart(progress, ['active', 'running'], 'running'),
    statusSummaryPart(progress, ['completed', 'done'], 'done'),
    statusSummaryPart(progress, 'ready', 'ready'),
    statusSummaryPart(progress, 'blocked', 'blocked'),
    statusSummaryPart(progress, 'failed', 'failed'),
    statusSummaryPart(progress, 'skipped', 'skipped'),
    statusSummaryPart(progress, 'pending', 'pending'),
  ].filter((part): part is string => part !== null);
  return parts.length > 0 ? parts.join(', ') : 'No node status yet';
}

function statusSummaryPart(
  progress: FormulaRunProgress,
  statuses: RunNodeStatus | readonly RunNodeStatus[],
  label: string,
): string | null {
  const keys: readonly RunNodeStatus[] = typeof statuses === 'string' ? [statuses] : statuses;
  const count = keys.reduce((sum, status) => sum + (progress.statusCounts[status] ?? 0), 0);
  return count > 0 ? `${count} ${label}` : null;
}
