# Event-Sourced Runs View — Design

## 1. Problem and goal

The dashboard **Runs** view is slow because there is no server-side run
projection. The SPA reconstructs runs entirely client-side: on every wide
refresh it fans out to four supervisor `/v0` reads — `molecule(all=true)`
(~6.8s over ~340k rows, capped at `MOLECULE_HISTORY_TIMEOUT_MS=3000`),
`formulaFeed` (~10s), one `task(all=true)` per discovered rig, and the core
active `listBeads` — then folds flat bead lists into run lanes via
`buildRunSummary` (`shared/src/runs/summary.ts`) and `enrichRunSummary`
(`frontend/src/supervisor/runSummary.ts`). The `molecule(all=true)` scan
exists *purely to surface historical run roots* that the view already caps at
50.

The hosted product is fast because it reads ClickHouse, which is fed by
exported events. The OSS-local analog of that ClickHouse is the per-city
append log `.gc/events.jsonl`. **Goal: source the Runs view from
events.jsonl, mirroring the hosted fold, so the expensive bead scans
disappear.**

## 2. Recommended architecture

Add a **per-city background run-projection tailer** inside
`internal/api/dashboardbff`, modeled on the existing `citySampler`
(`samplers.go`). It:

1. Resolves the city's on-disk root via `Deps.Resolver.CityPath(name)`
   (already available; this is how `resolveCityPath` works).
2. Opens `<root>/.gc/events.jsonl` **read-only** using the
   `transientCityEventProvider` pattern (`cmd/gc/city_registry.go`): reads go
   through `events.ReadFiltered`, the watcher through
   `recorder.Watch(...)` then `recorder.Close()` — the watcher only needs the
   path and holds **no second writer**, so it never contends with the
   controller's appender.
3. Does a **one-time cold replay** from cursor zero, folding the latest
   `bead.created`/`bead.updated`/`bead.closed`/`bead.deleted` snapshot per
   bead id (delete removes), then **live-tails** via `Watch(ctx, lastSeq)`.
4. Rebuilds the bead-derived `RunSummary` (a Go port of `buildRunSummary`)
   off the fold and publishes it under a brief lock — identical
   off-request-path discipline to `citySampler.refresh`.
5. Serves a new **`GET /api/city/{cityName}/runs/summary`** endpoint that
   returns the cached projection, then layers session-dependent health/census
   on at request time from the live `/v0/.../sessions` read (the same data the
   SPA fetches today).

```
.gc/events.jsonl ──(read-only Watch/ReadFiltered)──▶ runProjection (per city, warm)
   bead.created/updated/closed/deleted               fold: map[beadID]Bead
                                                      └▶ buildRunSummary (Go)
                                                            │ cached snapshot
GET /api/city/{city}/runs/summary ──▶ snapshot + live /v0/.../sessions enrich
                                                            ▼
                                                    RunSummary DTO (unchanged shape)
```

### Why the BFF plane, not a new supervisor `/v0` endpoint

| Consideration | BFF `/api` plane (recommended) | Supervisor `/v0` endpoint |
|---|---|---|
| OpenAPI/Huma contract | **Untouched** — the plane is the documented non-Huma exception | Requires a new Huma op + regenerated `openapi.json` + TS types |
| Upstream alignment (AGENTS.md) | New file in a fork-friendly package; no edit to upstream `internal/api` wire code | Edits upstream-owned wire surface |
| SPA wiring | Same-origin `/api/city/...`, identical to existing run-diff + sampler calls (`cityBase.ts`) | New typed client method |
| Background warm cache | `citySampler` lifecycle (lazy start, Start/Stop) already wired in `supervisor_dashboard.go` | Would need new lifecycle plumbing |
| Reuse of read substrate | `transientCityEventProvider` read-only pattern drops in | Same |

The BFF plane already serves `POST /api/city/{cityName}/runs/{runId}/diff`
(`rundiff.go`), so a `runs/summary` sibling is idiomatic and the SPA's
`cityPath()` helper already targets `/api/city/:cityName/*`.

### Why Go-side fold, not ship-events-to-browser

Folding in TS would require streaming tens of thousands of event lines to
each client on each refresh to reproduce a fold that costs **0.9s / 27MB
once** server-side (measured below). The fold belongs where the log is. Cost:
`buildRunSummary` must be reimplemented in Go and pinned by a parity test
(see Phase 2).

## 3. RunSummary field → event source mapping

The fold keeps the latest `beads.Bead` snapshot per id. `buildRunSummary`'s
inputs are bead fields + `gc.*` metadata, **all of which are in the bead.*
payload** (`internal/api/event_payloads.go` `BeadEventPayload` =
`json.Marshal(beads.Bead)`; the controller emits the full bead via
`caching_store_events.go notifyChange`).

| RunSummary field | Source | In events.jsonl? |
|---|---|---|
| Run grouping (`runRootId`) | `pr_review.*` / `bugflow.*` / `design_review.*` / `gc.root_bead_id` / `gc.kind` / `issue_type==molecule` / `molecule_id` metadata | **Yes** — bead.* payload metadata |
| Run-group promotion (`isRunGroup`) | `gc.formula_contract==graph.v2` / `issue_type==molecule` / `gc.kind==run` / `gc.formula` | **Yes** |
| `lanes` / `historicalLanes` / `blockedLanes` split | folded bead `status` + `gc.phase` (`mapRunPhase`) | **Yes** |
| `totalActive` / `totalHistorical` | counts over folded lanes | **Yes** |
| lane `title` | `pr_review.github_title` / root `title` | **Yes** |
| lane `formula` | `gc.formula` / `resolveRunFormulaIdentity` | **Yes** |
| lane `scope` | `gc.root_store_ref` / `gc.scope_ref` | **Yes** (replaces the `formulaFeed` discovery read) |
| lane `external` | `pr_review.pr_url` / `bugflow.github_issue_url` | **Yes** |
| `statusCounts` / `activeAssignees` | folded bead `status` / `assignee` | **Yes** |
| `updatedAt` / `recentChanges` | bead `updated_at` (present in payload) | **Yes** |
| `stages` / `progress` / `formulaStageResolved` | `gc.step_id` / `gc.step_ref` / `gc.phase` / `gc.attempt` + formula stage tables | **Yes** for the metadata; stage tables are static code |
| `runCounts` | derived from lanes | **Yes** |
| **lane `health`** (phaseConfidence, needsOperator, stuckNode, thrashingDetected, session) | `deriveRunHealth` over the **live sessions list** | **NO — genuine gap** (see below) |
| **`census`** | `buildCensus` over enriched lanes | **NO — depends on health** |
| `thrashing` / progress marks | `advanceProgressMarks` cross-generation state | Server-derivable but currently client-only (open question) |

### The one genuine gap: session-derived health

`session.woke`/`session.stopped` events carry only `{subject: session-name}`
(verified in a real log). They do **not** carry `lastActive`, `running`, or
`activity` — the `DashboardSession` fields `deriveRunHealth` needs. Those are
**live process facts**, consistent with the project rule "no status files —
query live state." So health/census **cannot be event-sourced** and must come
from the live `/v0/.../sessions` read. The endpoint layers them on at request
time (cheap; sessions is already the fast read). When sessions is unavailable,
health degrades to `unavailable` and `phaseConfidence` to `inferred`, exactly
as today.

## 4. Read mechanism, cursor, rotation, cost

- **Backfill:** on city-first-view, `events.ReadFiltered(path,
  Filter{})` (or a `Type`-filtered pass for the four bead types) yields one
  chronological stream across any gzip archives; fold to `map[beadID]Bead`,
  record `lastSeq`.
- **Live tail:** `Watch(ctx, lastSeq)`; the file watcher polls 250ms,
  advances a byte offset, dedupes by seq, and detects rotation by inode change
  (resets offset, honors the `events.rotated` anchor). Apply each bead.* event
  to the fold, republish the snapshot under a brief lock.
- **Cursor:** the `uint64` Seq. No persistence needed for v1 (replay is
  sub-second); a future compacted checkpoint is an open question.
- **Memory/startup cost (measured):** folding the 70MB / **59,165-event**
  `my-city/.gc/events.jsonl` took **0.9s wall, 27MB RSS** in Python; Go will
  be faster. Distinct beads after fold: 3,546 (a ~16x event→bead ratio). This
  is paid **once per city at first view**, then the tail is incremental. The
  warm snapshot is the in-memory fold (a few MB of beads) plus the built
  `RunSummary`.
- **Lazy + bounded:** start the tailer lazily per city (like `citySampler`),
  so cities nobody views cost nothing.

## 5. Historical question, answered

**Does events.jsonl retain enough to replace `molecule(all=true)`? Yes, with
one caveat.**

- **Retention:** rotation is OFF by default (`maxSize=0` unless
  `[events.rotation] enabled`); `archive_retain_age` defaults to empty →
  reaping is a no-op → **archives kept forever**. `ReadFiltered` walks them
  transparently. The log retains run roots indefinitely — *more* than the
  SPA's `MAX_HISTORICAL_LANES=50` wire cap.
- **Reconstruction proven on real data:** folding
  `/data/projects/daytona-trial-city/.gc/events.jsonl` produced **10 run
  lanes** (mol-dog-compactor, mol-dog-backup, …) with correct formula names —
  the `gc.root_bead_id`/`gc.kind`/`gc.step_ref` keys are present in real
  bead.created metadata.
- **The caveat:** the projection reconstructs exactly the runs whose
  lifecycle events are in the log. A city running current code records them
  completely → no backfill needed. But runs that occurred **before the city
  ever recorded bead events** (e.g. the legacy `my-city` log folded to **0 run
  groups** — that workload never ran graph.v2 formulas) are not in the log and
  cannot be event-sourced. That history is equally invisible to a fresh
  controller; only beads-as-system-of-record has it.
- **Conclusion:** steady-state needs **no** beads backfill. If product
  requires deep pre-event history for legacy installs, add an **optional,
  flag-gated** one-time `molecule(all=true)` read to seed a historical
  checkpoint at cursor zero — not a structural requirement, and not run on
  every city.

## 6. Conceptual alignment with the hosted ClickHouse fold

The hosted run plane builds runs at query time, not as a materialized view:
- **Run step-structure** (`forgebff Store.RunSteps`): `GROUP BY ref` over
  `city_events.events FINAL`, pairing `bead.created`/`bead.closed` (minIf/maxIf
  on ts) within `org_id + run_id`. A step = one bead's created/closed pair.
- **Run list/cost** (`manifold list_runs_sql`): `GROUP BY
  coalesce(nullIf(run_id,''),agent)` over the spend fact table.

The OSS local fold mirrors the **same shape**: group by run key, fold bead
lifecycle. Differences to keep documented so the two stay in sync:
- Local **groups by re-derived `runRootId`** from the folded snapshot
  metadata (not by envelope `run_id`), because envelope `run_id` is stamped at
  record time and an early event can carry `run_id=self` before
  `molecule_id`/`workflow_id` was stamped — re-deriving from the latest
  snapshot is the correctness-safe key (matches `buildRunSummary`).
- Local has **`bead.updated`** (status transitions); the hosted export
  allowlist **excludes** it, so ClickHouse sees only created/closed boundaries.
  The local projection is therefore *richer* (real per-lane status), and the
  two are intentionally not byte-identical. Cost/token data lives only in the
  hosted spend plane and is out of scope locally.
- Keep the run-key precedence and `isRunGroup` rule identical to
  `summary.ts` so a future hosted run-list-over-city_events can reuse the same
  predicate.

## 7. Fidelity gaps vs the molecule scan, and SPA degradation

| Aspect | molecule scan today | event-sourced projection | SPA degrade |
|---|---|---|---|
| Historical depth | beads system-of-record (all roots ever) | log-resident roots (complete for current-code cities) | Pre-event-history runs absent until optional backfill |
| Health/census | live sessions read (client) | live sessions read at request time | `unavailable` when sessions down (same as today) |
| Progress/thrashing marks | per-browser, in-memory | TBD: client-side (v1) or relocated to tailer | Resets on reload if left client-side |
| Dependency edges (parent/child) | full bead read | `ParentID` in payload, but `caching_store_events` can drop deps after removals | Step→root child edges may be incomplete; lane still renders from grouping |
| Freshness | per-refresh fan-out | 250ms tail latency | Snapshot up to one tick stale (negligible) |
| Cold-start | n/a (always re-fetches) | one 0.9s replay at first view | First view slightly slower than a warm cache, far faster than 6.8s scan |

Degradation contract: a tailer/log error returns the endpoint with
`lanesPartial: true` (existing convention) rather than blanking; the SPA can
keep the `/v0` path as a fallback behind a flag during rollout.

## 8. Phased implementation plan (≤5 files/phase, each verifiable)

**Phase 1 — Go fold + buildRunSummary port (no wiring).**
Files: `internal/api/dashboardbff/runprojection.go` (new: fold + run grouping
+ `RunSummary` build), `runprojection_test.go` (new),
`testdata/runs/daytona.jsonl` (new fixture, trimmed from the real log),
optionally a shared `runmodel.go` for the DTO structs.
Verify: golden test folds the fixture and asserts the same lanes/counts the TS
`buildRunSummary` produces for the same beads (capture the TS output once as
the golden). `go test ./internal/api/dashboardbff/ -run RunProjection`.

**Phase 2 — Parity test against TS.**
Files: `runprojection_parity_test.go` (new), a shared fixture under
`shared/src/runs/__fixtures__/` consumed by both a TS test and the Go test.
Verify: TS `summary.test.ts` and Go parity test build the **same RunSummary**
(modulo health/census) from one fixture. This is the contract that lets the Go
reimplementation track `summary.ts`.

**Phase 3 — Per-city tailer (read substrate).**
Files: `runprojection_tailer.go` (new: lazy per-city loop, cold replay +
`Watch`, snapshot publish under brief lock, mirroring `citySampler`),
`runprojection_tailer_test.go` (new), small edit to `plane.go` (add the
tailer manager to `Plane`, enable in `Start`, drain in `Stop`).
Verify: test writes a temp `.gc/events.jsonl`, starts the tailer, asserts the
snapshot reflects appended bead events after a tick; asserts no second writer
(file still appendable by a separate recorder).

**Phase 4 — Endpoint + session enrichment.**
Files: `runs_summary.go` (new: `GET /api/city/{cityName}/runs/summary`,
reads tailer snapshot, fetches live `/v0/.../sessions` over loopback like
`fetchStatus`, applies health/census), `runs_summary_test.go` (new), edit
`plane.go` `registerRoutes` to call `registerRunsSummary`.
Verify: handler test returns the DTO with available health when sessions
present, `unavailable` health + `lanesPartial` when sessions read fails;
`make dashboard-check` for the wire contract.

**Phase 5 — SPA cutover behind a flag.**
Files: `frontend/src/supervisor/runSummary.ts` (add a BFF fetcher that calls
`/api/city/{city}/runs/summary` via `cityPath()`), `runSummarySubscription.tsx`
(select BFF vs `/v0` path by flag), `runSummary.test.ts` (cover the new
fetcher).
Verify: SPA tests green; manual `npm run preview` shows the Runs view
populated from the BFF path; shadow-compare DTOs from both paths for a period.

## 9. Risks

- **Go↔TS drift** in `buildRunSummary`: mitigated by the Phase 2 shared-fixture
  parity test; treat `summary.ts` as the spec.
- **Replay growth** on very long-lived cities under keep-forever retention:
  0.9s today, but unbounded; the compacted-checkpoint open question hedges it.
- **Incomplete dependency edges** in some bead.updated payloads
  (`updateEventDepsLocked` can drop deps): lanes still render from grouping;
  document that step child-edge completeness is best-effort, matching the
  existing client behavior.

---

## Locked decisions (operator, 2026-06-28)

1. **Historical: pure event-sourcing.** Runs = whatever `events.jsonl` contains (complete for any city on current code). No beads backfill; runs predating the city's first recorded bead event are not shown.
2. **Server scope: full server-side, including session enrich.** Go owns the event fold + `buildRunSummary` AND `enrichRunSummary` (per-lane health + census). The BFF reads `/v0/.../sessions` at request time (loopback) to layer session state; the SPA just renders the returned `RunSummary`.
3. **Rollout: direct cutover.** Repoint the SPA's run-summary source to the new endpoint and delete the 4-read `/v0` path, gated by a Go↔TS golden-parity test (Go `RunSummary` == current TS `RunSummary` on shared fixtures).
4. **Restart: cold-replay each supervisor start** (~0.9s) + live-tail. No persisted checkpoint in v1.
5. (defaulted) `progressStateByCity` monotonicity marks move server-side into the tailer (shared across viewers, survive reload) since the fold is now server-owned.
