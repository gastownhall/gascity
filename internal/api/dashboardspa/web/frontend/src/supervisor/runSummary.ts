import type {
  DashboardBead,
  DashboardSession,
  RunFeedScope,
  RunFeedScopeMap,
  RunSummary,
  SourceAvailableState,
  SourceState,
} from 'gas-city-dashboard-shared';
import {
  advanceProgressMarks,
  buildCensus,
  buildRunSummary,
  deriveRunHealth,
  fromFeedScope,
  fromDashboardBead,
  fromRootMetadataScope,
  fromStoreRef,
  isStaleSessionlessLatch,
  runBeadFilter,
  runCounts,
  SCOPE_REF_RE,
  type LaneProgressMark,
} from 'gas-city-dashboard-shared';
import { activeCityOrThrow } from '../api/cityBase';
import type { Bead, FormulaFeedBody, ListBodyBead } from 'gas-city-dashboard-shared/gc-supervisor';
import { supervisorApiForRequestBudget } from './client';
import { fetchCoreRead } from './coreRead';
import { listIsIncomplete, listIsPartial } from './listPartial';
import { normalizeSessions } from './sessionReads';

// Pre-exposure load bound (gascity-dashboard-q89b): the primary in-flight
// bead fetch refreshes on SSE bursts (10s debounce floor in the shared
// run-summary subscription) per client. listIsIncomplete keeps truncation
// visible in the lanes-partial signal.
const RUNS_FETCH_LIMIT = 500;
// gascity-dashboard-4xcv: the supervisor's bead list has no sort/recency
// guarantee, so a small `all=true` window can drop run roots and step beads
// arbitrarily — observed live as an empty first paint and a history list
// missing most completed runs (a busy rig store holds hundreds of closed
// task beads). 500 covers the largest observed store with headroom; if a
// store outgrows it the symptom returns as silently missing lanes, so a
// real fix beyond raising the cap means cursor pagination.
const RECENT_RUN_FETCH_LIMIT = 500;
const RUNS_STALE_AFTER_MS = 60 * 1000;
// The CORE active-bead read is the one fetch whose failure blanks the whole runs
// view (everything else degrades to `partial`). The proxy path is ~0.02s
// normally, but the box runs at load avg ~30 under a slung-pipeline burst (the
// supervisor reconciler alone hits ~274% CPU), and during a spike a single core
// read occasionally crosses the old 5s budget → a first load with no last-good
// snapshot blanks to "Run data unavailable". Raise this read's ceiling to absorb
// a burst (the higher bound only costs time when actually slow), and retry once
// on a transient timeout/5xx before giving up. A real outage still surfaces an
// error after the retries are spent — the resilience only hides a brief spike,
// never a sustained failure (upstream gascity-dashboard#88).
const REQUIRED_RUN_SUMMARY_TIMEOUT_MS = 15_000;
// Optional run-summary enrichment (recent-bead / formula-feed / session reads)
// is best-effort: a miss degrades the lanes to "partial" rather than failing the
// load. The budget is split by call site (gascity-dashboard-4bol). The preview
// runs on first paint and blocks it, so it keeps a tight bound and the tab paints
// fast even when the supervisor is slow. The full source runs only as the shared
// run-summary subscription's background refreshFetcher, so it can afford a far
// wider budget matching the 30s
// background status samplers: on the bloated store the supervisor's list/feed
// reads run ~10-38s (upstream gascity-dashboard#88), so a 2.5s interactive bound
// always times out and latches a spurious "runs partial" badge. The wider refresh
// budget lets those slow-but-available reads land and clear it, without the
// first-paint spinner a single global raise would cause.
const PREVIEW_ENRICHMENT_TIMEOUT_MS = 2_500;
const REFRESH_ENRICHMENT_TIMEOUT_MS = 30_000;
// gascity-dashboard-9rk2: the molecule(all=true) read scans the full (large,
// ~340k-row) molecule history purely to surface HISTORICAL run roots, which the
// view already caps at MAX_HISTORICAL_LANES (50). Live it runs ~6.8s — past the
// 5s required-fetch budget — and the supervisor exposes no recency-ordered or
// bounded molecule query, so it cannot be made cheaper server-side without a new
// endpoint. It is therefore the FIRST optional read to dominate the wider refresh
// budget. Bound it well under REQUIRED_RUN_SUMMARY_TIMEOUT_MS so a slow scan
// degrades the historical lanes to "partial" fast instead of holding the refresh
// (and so it can never out-wait the active set, which paints from the fast
// open/active read). It still rides the surrounding enrichment budget too via the
// min() at the call site, so the tight first-paint path is never loosened.
const MOLECULE_HISTORY_TIMEOUT_MS = 3_000;

interface LoadedRunBeads {
  beads: DashboardBead[];
  feedScopes: RunFeedScopeMap;
  partial: boolean;
}

type RecentFetchOutcome = { ok: true; items: DashboardBead[]; partial: boolean } | { ok: false };

type RunSessionsLookup =
  | { kind: 'available'; sessions: DashboardSession[] }
  | { kind: 'unavailable'; sessions: DashboardSession[] };

interface ProgressState {
  marks: Map<string, LaneProgressMark>;
  fetchedAt: string;
}

const progressStateByCity = new Map<string, ProgressState>();

// The wide-budget run-summary source (gascity-dashboard-4bol): the wide
// enrichment budget lets slow-but-available list/feed reads land and clear the
// spurious "runs partial" badge (upstream gascity-dashboard#88). It is the
// authoritative refresh snapshot for the shared run-summary subscription
// (runs/runSummarySubscription), which both the /runs page and the nav attention
// badge read — so the badge counts the same genuinely-blocked runs the page
// shows, by construction, off a single fan-out (gascity-dashboard-2j8e.7).
//
// Do NOT use this as a ROUTE first-paint fetcher — it can block a route view for
// up to REFRESH_ENRICHMENT_TIMEOUT_MS; route mount consumers (Home, Formula Run
// Detail) use loadSupervisorRunSummaryMountSource on the tight budget instead.
// The shared subscription is exempt: it is cache-backed and the always-mounted
// header reads its result, so its latency never blocks a route view.
export async function loadSupervisorRunSummarySource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource(REFRESH_ENRICHMENT_TIMEOUT_MS);
}

// Mount / first-paint full source for Home and Formula Run Detail: the same data
// as the refresh source (lanes + sessions) but on the tight first-paint budget,
// so a cold navigation to those routes never blocks on slow optional enrichment
// reads (gascity-dashboard-4bol).
export async function loadSupervisorRunSummaryMountSource(): Promise<SourceState<RunSummary>> {
  return loadRunSummarySource(PREVIEW_ENRICHMENT_TIMEOUT_MS);
}

async function loadRunSummarySource(enrichmentBudgetMs: number): Promise<SourceState<RunSummary>> {
  const cityName = activeCityOrThrow('load supervisor run summary');
  const fetchedAt = new Date().toISOString();
  try {
    const [loaded, sessions] = await Promise.all([
      loadRunBeads(cityName, RUNS_FETCH_LIMIT, enrichmentBudgetMs),
      loadRunSessions(cityName, enrichmentBudgetMs),
    ]);
    const summary = buildRunSummary(
      loaded.beads.filter(runBeadFilter).map(fromDashboardBead),
      loaded.feedScopes,
      loaded.partial,
    );
    const source: SourceAvailableState<RunSummary> = {
      source: 'runs',
      status: 'fresh',
      fetchedAt,
      staleAt: new Date(Date.parse(fetchedAt) + RUNS_STALE_AFTER_MS).toISOString(),
      error: { kind: 'none' },
      data: summary,
    };
    return {
      ...source,
      data: enrichRunSummary(cityName, source, sessions),
    };
  } catch (err) {
    return {
      source: 'runs',
      status: 'error',
      error: errorMessage(err, 'formula runs unavailable'),
    };
  }
}

// gascity-dashboard: the CHEAP SSE-refresh source. The wide source's loadRunBeads
// fires the two expensive HISTORICAL/discovery reads (molecule(all=true) ~6.8s,
// city formulaFeed ~10s) plus per-rig task reads on every SSE burst, saturating
// the browser ~6-conn/host cap so the run-detail's fast workflowRun read queues
// behind them. The active/blocked set and its scope/health/counts/census do NOT
// need those reads: runScope takes the bead's own gc.root_store_ref/gc.scope_ref
// metadata first (feedScopes is only a fallback), and runRigNames derives the
// active rig set from that same per-bead metadata. So this fetches only the core
// active read + sessions, skipping molecule + city feed + per-rig task reads.
// historicalLanes/totalHistorical come back empty here; the subscription merges
// them from the last wide snapshot so History is never blanked.
export async function loadSupervisorRunSummaryActiveSource(): Promise<SourceState<RunSummary>> {
  const cityName = activeCityOrThrow('load supervisor run summary active');
  const fetchedAt = new Date().toISOString();
  try {
    const [loaded, sessions] = await Promise.all([
      loadActiveRunBeads(cityName, RUNS_FETCH_LIMIT),
      loadRunSessions(cityName, PREVIEW_ENRICHMENT_TIMEOUT_MS),
    ]);
    const summary = buildRunSummary(
      loaded.beads.filter(runBeadFilter).map(fromDashboardBead),
      loaded.feedScopes,
      loaded.partial,
    );
    const source: SourceAvailableState<RunSummary> = {
      source: 'runs',
      status: 'fresh',
      fetchedAt,
      staleAt: new Date(Date.parse(fetchedAt) + RUNS_STALE_AFTER_MS).toISOString(),
      error: { kind: 'none' },
      data: summary,
    };
    return {
      ...source,
      data: enrichRunSummary(cityName, source, sessions),
    };
  } catch (err) {
    return {
      source: 'runs',
      status: 'error',
      error: errorMessage(err, 'formula runs unavailable'),
    };
  }
}

// Core active read only — no molecule history scan, no city formula feed, no
// per-rig task reads. Truncation still reads as partial lanes. feedScopes is
// empty: a run root whose OWN bead carries no scope metadata falls back to
// 'unavailable' scope until the next wide refresh repairs it (minority path —
// runScope prefers per-bead metadata).
async function loadActiveRunBeads(cityName: string, limit: number): Promise<LoadedRunBeads> {
  const activeList = await fetchCoreActiveBeads(cityName, limit);
  const active = normalizeBeads(activeList.items ?? []);
  return {
    beads: active,
    feedScopes: new Map(),
    partial: listIsIncomplete(activeList, active.length),
  };
}

export async function loadSupervisorRunSummaryPreviewSource(): Promise<SourceState<RunSummary>> {
  const cityName = activeCityOrThrow('load supervisor run summary preview');
  const fetchedAt = new Date().toISOString();
  try {
    const loaded = await loadRunBeads(cityName, RUNS_FETCH_LIMIT, PREVIEW_ENRICHMENT_TIMEOUT_MS);
    const summary = buildRunSummary(
      loaded.beads.filter(runBeadFilter).map(fromDashboardBead),
      loaded.feedScopes,
      loaded.partial,
    );
    return {
      source: 'runs',
      status: 'fresh',
      fetchedAt,
      staleAt: new Date(Date.parse(fetchedAt) + RUNS_STALE_AFTER_MS).toISOString(),
      error: { kind: 'none' },
      // First paint has no sessions yet, so no latch demotion. `lanes` carries
      // the full active set; RunMap applies the collapsed window.
      data: summary,
    };
  } catch (err) {
    return {
      source: 'runs',
      status: 'error',
      error: errorMessage(err, 'formula runs unavailable'),
    };
  }
}

export function resetSupervisorRunSummaryStateForTests(): void {
  progressStateByCity.clear();
}

// Exposed for tests: the raised core-read budget that absorbs a CPU-burst spike.
export const CORE_RUN_SUMMARY_TIMEOUT_MS = REQUIRED_RUN_SUMMARY_TIMEOUT_MS;

function requiredRunSummaryApi() {
  return supervisorApiForRequestBudget(REQUIRED_RUN_SUMMARY_TIMEOUT_MS);
}

// The core active-bead read, with a bounded retry on a transient failure
// (fetchCoreRead). This is the one read whose rejection blanks the whole runs
// view, so a brief CPU burst that times out the first attempt should not lose
// the load; a sustained failure still propagates after the retry is spent.
// Optional enrichment reads keep their own degrade-to-partial handling and are
// NOT routed here.
async function fetchCoreActiveBeads(cityName: string, limit: number): Promise<ListBodyBead> {
  return fetchCoreRead(() => requiredRunSummaryApi().listBeads(cityName, { limit }));
}

function optionalRunSummaryApi(budgetMs: number) {
  return supervisorApiForRequestBudget(budgetMs);
}

async function loadRunBeads(
  cityName: string,
  limit: number,
  enrichmentBudgetMs: number,
): Promise<LoadedRunBeads> {
  // The molecule-history read gets its own tight bound (gascity-dashboard-9rk2),
  // capped to the surrounding enrichment budget so the first-paint path is never
  // loosened: a slow scan folds to `partial` instead of dominating the refresh.
  const moleculeFetch = settledRecentFetch(
    cityName,
    {
      limit: RECENT_RUN_FETCH_LIMIT,
      type: 'molecule',
      all: true,
    },
    Math.min(MOLECULE_HISTORY_TIMEOUT_MS, enrichmentBudgetMs),
  );
  const [activeList, feedDiscovery] = await Promise.all([
    fetchCoreActiveBeads(cityName, limit),
    discoverFromFeed(cityName, enrichmentBudgetMs),
  ]);
  const active = normalizeBeads(activeList.items ?? []);
  const rigNames = unionRigNames(runRigNames(active), feedDiscovery.rigNames);

  const rigFetches = rigNames.map((rig) =>
    settledRecentFetch(
      cityName,
      {
        limit: RECENT_RUN_FETCH_LIMIT,
        type: 'task',
        rig,
        all: true,
      },
      enrichmentBudgetMs,
    ),
  );

  const settled = await Promise.all([moleculeFetch, ...rigFetches]);
  const recentItems: DashboardBead[] = [];
  // Truncation at the bounded fetch reads as partial lanes, not complete
  // (gascity-dashboard-q89b).
  let partial = feedDiscovery.partial || listIsIncomplete(activeList, active.length);

  for (const outcome of settled) {
    if (outcome.ok) {
      recentItems.push(...outcome.items);
      partial ||= outcome.partial;
      continue;
    }
    partial = true;
  }

  return {
    beads: uniqueBeads([...active, ...recentItems]),
    feedScopes: feedDiscovery.scopes,
    partial,
  };
}

async function settledRecentFetch(
  cityName: string,
  query: { limit: number; type: string; all: true; rig?: string },
  budgetMs: number,
): Promise<RecentFetchOutcome> {
  try {
    const list = await withOptionalReadBudget(
      optionalRunSummaryApi(budgetMs).listBeads(cityName, query),
      `recent ${query.type} beads`,
      budgetMs,
    );
    return {
      ok: true,
      items: normalizeBeads(list.items ?? []),
      partial: listIsPartial(list),
    };
  } catch {
    return { ok: false };
  }
}

interface FeedDiscovery {
  rigNames: string[];
  scopes: RunFeedScopeMap;
  partial: boolean;
}

async function discoverFromFeed(cityName: string, budgetMs: number): Promise<FeedDiscovery> {
  try {
    const runs = await withOptionalReadBudget(
      optionalRunSummaryApi(budgetMs).formulaFeed(cityName, {
        scope_kind: 'city',
        scope_ref: cityName,
      }),
      'formula feed',
      budgetMs,
    );
    const rigNames = new Set<string>();
    const scopes = new Map<string, RunFeedScope>();
    for (const run of runs.items ?? []) {
      if (run.type !== 'formula') continue;
      const storeScope = fromStoreRef(run.root_store_ref ?? null);
      if (storeScope?.scopeKind === 'rig') {
        rigNames.add(storeScope.scopeRef);
      }
      const rootId = run.root_bead_id ?? run.workflow_id ?? null;
      // gascity-dashboard-q89b (detail scope leak): the feed's top-level
      // scope_kind is always 'city', so fromFeedScope alone makes a rig lane's
      // detail href drop to city scope — and a city-scoped workflow fetch hits
      // the supervisor's full-store scan (~12-14s, upstream #88) instead of the
      // sub-second single-store rig fetch. Recover the rig from root_store_ref
      // first (store-ref-first — deliberately the inverse of
      // fromRootMetadataScope's pair-first rule: the metadata edge trusts its
      // explicit pair, but the feed's pair is unreliable here, always 'city').
      // Fall back to the feed (city) scope only when the store ref names no rig.
      // The store ref is validated against SCOPE_REF_RE before it is emitted as
      // a lane scope: unlike fromFeedScope/fromRootMetadataScope, fromStoreRef
      // does not validate, so a malformed root_store_ref must fall back rather
      // than emit a scope_ref the detail route would reject.
      const scope =
        storeScope?.scopeKind === 'rig' && SCOPE_REF_RE.test(storeScope.scopeRef)
          ? storeScope
          : fromFeedScope(run);
      if (rootId !== null && scope !== null) {
        scopes.set(rootId, {
          scopeKind: scope.scopeKind,
          scopeRef: scope.scopeRef,
          rootStoreRef: run.root_store_ref ?? `${scope.scopeKind}:${scope.scopeRef}`,
        });
      }
    }
    return { rigNames: [...rigNames], scopes, partial: feedIsPartial(runs) };
  } catch {
    return { rigNames: [], scopes: new Map(), partial: true };
  }
}

async function loadRunSessions(cityName: string, budgetMs: number): Promise<RunSessionsLookup> {
  try {
    const list = await withOptionalReadBudget(
      optionalRunSummaryApi(budgetMs).listSessions(cityName),
      'run sessions',
      budgetMs,
    );
    return {
      kind: 'available',
      sessions: normalizeSessions(list),
    };
  } catch {
    return { kind: 'unavailable', sessions: [] };
  }
}

function enrichRunSummary(
  cityName: string,
  source: SourceAvailableState<RunSummary>,
  sessionsLookup: RunSessionsLookup,
): RunSummary {
  // gascity-dashboard-4xcv: blocked lanes are enriched alongside active
  // ones so they carry derived health (needsOperator) for the attention
  // layer, then split back out — they are not part of the Active set.
  const inFlight = [...source.data.lanes, ...source.data.blockedLanes];
  const state = progressStateByCity.get(cityName);
  const generationMs = Date.parse(source.fetchedAt);
  let marks = state?.marks ?? new Map<string, LaneProgressMark>();
  if (state === undefined || generationMs > Date.parse(state.fetchedAt)) {
    marks = advanceProgressMarks(marks, inFlight);
    progressStateByCity.set(cityName, { marks, fetchedAt: source.fetchedAt });
  }

  const sessionsAvailable = sessionsLookup.kind === 'available';
  const { lanes } = deriveRunHealth({
    lanes: inFlight,
    sessions: sessionsLookup.sessions,
    sessionsAvailable,
    marks,
  });

  const blockedLanes = lanes.filter((lane) => lane.phase === 'blocked');
  const activeEnriched = lanes.filter((lane) => lane.phase !== 'blocked');

  // gascity-dashboard-s4rp: sessions only resolve here at enrichment, so this is
  // the earliest seam with enough information to demote stale session-less
  // latches (the gc-1920 phantom: no live session, no in_progress step, days
  // stale) out of the Active set. buildRunSummary hands us the FULL active set
  // (not the capped window), so totalActive is recomputed exactly from the
  // surviving lanes — a phantom past the 8th slot is demoted too. Staleness is
  // judged against the snapshot generation time, not a live clock, so the result
  // is stable for a snapshot.
  const liveActive = activeEnriched.filter(
    (lane) => !isStaleSessionlessLatch(lane, generationMs, sessionsAvailable),
  );
  const census = buildCensus([...liveActive, ...blockedLanes]);

  // gascity-dashboard: `lanes` carries the FULL active set; RunMap owns the
  // collapsed window (MAX_VISIBLE_ACTIVE_LANES) and its "Show N more runs"
  // expander, mirroring the historical section.
  return {
    ...source.data,
    totalActive: liveActive.length,
    lanes: liveActive,
    blockedLanes,
    runCounts: runCounts(liveActive, liveActive.length, blockedLanes.length),
    census: { status: 'available', data: census },
  };
}

function runRigNames(beads: readonly DashboardBead[]): string[] {
  const names = new Set<string>();
  for (const bead of beads) {
    const storeScope = fromStoreRef(bead.metadata?.['gc.root_store_ref']);
    if (storeScope?.scopeKind === 'rig') {
      names.add(storeScope.scopeRef);
      continue;
    }

    const metadataScope = fromRootMetadataScope(bead.metadata);
    if (metadataScope?.scopeKind === 'rig') {
      names.add(metadataScope.scopeRef);
    }
  }
  return Array.from(names).sort();
}

function unionRigNames(a: readonly string[], b: readonly string[]): string[] {
  const all = new Set<string>();
  for (const name of a) all.add(name);
  for (const name of b) all.add(name);
  return [...all];
}

function withOptionalReadBudget<T>(
  promise: Promise<T>,
  label: string,
  budgetMs: number,
): Promise<T> {
  let timeout: ReturnType<typeof setTimeout> | null = null;
  const budget = new Promise<T>((_, reject) => {
    timeout = setTimeout(() => {
      reject(new Error(`${label} timed out after ${budgetMs}ms`));
    }, budgetMs);
  });
  return Promise.race([
    promise.finally(() => {
      if (timeout !== null) clearTimeout(timeout);
    }),
    budget,
  ]);
}

function uniqueBeads(beads: readonly DashboardBead[]): DashboardBead[] {
  const byId = new Map<string, DashboardBead>();
  for (const bead of beads) {
    if (!byId.has(bead.id)) byId.set(bead.id, bead);
  }
  return Array.from(byId.values());
}

function normalizeBeads(beads: readonly Bead[]): DashboardBead[] {
  return beads.map(normalizeBead);
}

function normalizeBead(bead: Bead): DashboardBead {
  const normalized: DashboardBead = {
    id: bead.id,
    title: bead.title,
    status: bead.status,
    issue_type: bead.issue_type,
    priority: bead.priority ?? null,
    created_at: bead.created_at,
  };
  if (bead.description !== undefined) normalized.description = bead.description;
  if (bead.assignee !== undefined) normalized.assignee = bead.assignee;
  if (Array.isArray(bead.labels)) normalized.labels = bead.labels;
  if (bead.metadata !== undefined) normalized.metadata = bead.metadata;
  if (bead.ref !== undefined) normalized.ref = bead.ref;
  if (bead.parent !== undefined) normalized.parent = bead.parent;
  if (bead.from !== undefined) normalized.from = bead.from;
  if (bead.ephemeral !== undefined) normalized.ephemeral = bead.ephemeral;
  if (bead.needs !== undefined) normalized.needs = bead.needs;
  if (bead.dependencies !== undefined) normalized.dependencies = bead.dependencies;
  if (bead.updated_at !== undefined) normalized.updated_at = bead.updated_at;
  return normalized;
}

function feedIsPartial(feed: FormulaFeedBody): boolean {
  return feed.partial === true || (feed.partial_errors?.length ?? 0) > 0;
}

function errorMessage(err: unknown, fallback: string): string {
  return err instanceof Error && err.message.trim().length > 0 ? err.message : fallback;
}
