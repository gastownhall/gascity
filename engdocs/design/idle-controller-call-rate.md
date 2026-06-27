---
title: "Idle Controller Call-Rate Reduction"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-16 |
| Author(s) | Claude (Opus 4.8) |
| Issue | #3543, #2463 |
| Supersedes | N/A |

# Idle Controller Call-Rate Reduction

> **Status: Proposed.** Layer-3 (Gas City scheduling) companion to the already
> merged/in-flight store-layer work. Tracks #3543 (the idle-saturation data
> point) and the "Gas City Layer" the reporter of #2463 called *"the most
> immediately beneficial option."*

## Summary

An idle Gas City issues a high, roughly fixed rate of `bd`/Dolt reads — ~7 `bd`
subprocesses/sec → ~463 Dolt `Com_select`/sec on a single idle city (#2463),
and enough to push a small **multi-tenant** host to load 8→14 even with a tiny,
retention-bounded store (#3543). The rate comes from the controller running a
**full per-pass sweep** — every session, every order, every rig — on a fixed
tick, regardless of whether anything changed.

The store layer is being fixed in parallel: the native in-process store + the
`CachingStore` (re-enabled by #3505/#3270 for #3248), `beads#4107`'s read
optimisation, and the DoltLite backend all reduce **per-call cost**. This
design is deliberately orthogonal: it reduces the controller's **logical call
rate**. Both are required. As @coffeegoddd noted on #2463, cheaper queries
*raise throughput and refill the CPU* unless the call rate also drops; as
@mmlac demonstrated empirically (pruned 543k rows, "CPU did not move"), the
saturation is **rate-bound, not size-bound**.

The proposal is four pillars, ordered by confidence:

1. **Demand-gated ticking** *(lead, high-confidence, store-layer-independent)* —
   back off the controller tick when a pass observes no demand delta; wake
   immediately on a real signal.
2. **Single-snapshot per-pass evaluation** *(contingent on measurement)* —
   collapse residual per-order / per-session read fan-out within a pass into one
   snapshot read, reusing #3492's `ListQuery.LabelPrefix` primitive.
3. **Quiescent-scope skipping + cursor sanity** — *largely shipped by #3097 +
   `order_dispatch.go` suspended-skip; only a small residual remains* (see
   pillar). Kept for the record and the cursor-sanity gap.
4. **Snapshot-served hot hooks** *(secondary)* — serve `gc hook` / `gc mail
   check` / `gc rig list --json` from a cached controller snapshot instead of a
   fresh per-call fan-out.

A cross-cutting **bd-call-rate budget + doctor guard** makes the win measurable
and prevents regression — directly implementing @coffeegoddd's "GC/GT should
rate limit usage of bd commands." The *measurement* half already exists
(`GC_BD_TRACE_JSON`, #2485); the new part is a budget + a `bd-call-rate` doctor
check.

## Decision Log / Status

- **Proposed** (this revision). No new optimisation code landed. Phase 0
  (baseline measurement on a native-store build) **gates** the rest: pillar
  ordering and the Phase-3 go/no-go depend on the measured residual rate, not on
  the pre-fix CLI-fallback numbers. **Phase 0 is partially complete:** the `bd`-
  subprocess A/B (CLI-fallback vs native store) is done and reshaped the scope
  (below), but the gating measurement for Pillars 1–2 — a clean controller-only
  `Com_select`/sec on a long-running idle supervisor — is **still pending**
  (housekeeping-dog contamination; see the Phase-0 plan). Until that lands, the
  pillar go/no-go calls remain provisional.
- **Revision after re-checking `origin/main` (a2b890dd7):** three relevant fixes
  already landed and reshape this design:
  - **#3097 (merged 2026-06-05)** — "cut supervisor reconcile CPU": skips the
    beads-cache reconcile loop for suspended rigs (~167-253% → ~19% on a
    5-active/8-suspended city), memoizes pack-hash + remote-cache across ticks,
    and honors `event_hooks=false`. Together with the order-dispatcher's existing
    suspended-skip (`cmd/gc/order_dispatch.go:423,476`), **Pillar 3 is essentially
    already shipped** — see that pillar for the small residual.
  - **#3270 + #2928 (merged), #3505 (merged in `gastownhall/gascity` main — this
    PR's base)** — native store runs with gc-owned hooks; per-write event hooks
    are toggleable; #3505 drops the now-redundant forwarder hooks. #3270 alone
    already makes the native store eligible, so the Phase-0 *build* (the fork's
    `origin/main`, which predated #3505) is a **valid native-store baseline**;
    #3505 is a further cleanup, not a prerequisite.
  - **#2485 (merged)** — `internal/beads/bdtrace.go`: a scope-classified,
    tick-reason-attributed `bd`-call JSONL tracer gated on `GC_BD_TRACE_JSON`.
    **Phase 0's instrumentation already exists**; Phase 0 only adds an aggregator
    + repro procedure on the post-#3097 baseline (see `engdocs/plans/`).
- **Phase-0 measurement (2026-06-16, native-store build of `origin/main`):** an
  A/B on `gc order check` showed **43 `bd` subprocesses in CLI-fallback → 1 with
  the native store** (a single order-eval pass). Two conclusions reshape the
  scope:
  1. The #2463/#3543 **`bd`-subprocess flood is overwhelmingly a CLI-fallback
     artifact** (#3248), already solved by the native store (#3270/#3505). Our
     #3543 incident ran in CLI-fallback (binary predated #3505) — confirmed by
     reproducing its exact `gate=bd_context_agreement` warning.
  2. **Metric pivot:** on the native store, reads are in-process and `bd`/sec is
     no longer the right meter. The residual this design targets is **in-process
     Dolt query volume** — measure via `Com_select`/sec, not `GC_BD_TRACE_JSON`.
     Pillars 1/2 are re-justified on that axis; the Com_select baseline is the
     outstanding gate (see `engdocs/plans/idle-controller-call-rate-phase0.md`).

## Problem Statement

### Observed behaviour

`#2463` (idle, single city, default Gas Town pack), 10-minute aggregate:

| subcmd | calls | /sec | % |
|---|--:|--:|--:|
| list | 1,597 | 2.83 | 38.7% |
| query | 1,118 | 1.98 | 27.1% |
| show | 645 | 1.14 | 15.6% |
| update | 358 | 0.63 | 8.7% |
| dep | 191 | 0.34 | 4.6% |
| create / close | 74 / 74 | 0.13 / 0.13 | 1.8% / 1.8% |
| gate | 34 | 0.06 | 0.8% |

Reads (`list`+`query`+`show`) are **81%** of calls. Corroborating data points:

- **#3543 (our case):** a small **multi-tenant** host (4 cores, two cities under
  one systemd supervisor) reached load 8→14 with a *retention-bounded* store
  (~1,083 wisps total; 15 of 16 rig DBs held **0** wisps yet were still
  patrolled), ~36–40 `order:*` events/min. Suspending the active agent did **not**
  relieve it — the idle controller sweep continued. Only stopping the city did.
- **@mmlac (#2463):** ~442–462 `Com_select`/sec, Dolt 250–450% CPU; **rate-bound
  not size-bound** (pruned 543k orphaned rows, CPU unchanged).
- **@Cdfghglz (#2463):** 37-rig city; `gc rig list --json` pegs a core on per-rig
  runtime probes; a 60 s-TTL flock cache "noticeably reduced sustained load."
- **@julianknutsen (#2463):** the tmux status line shells out **every 5 s per
  session** to `gc hook` + `gc mail check`.

### Where the rate comes from (code)

- **Fixed tick.** Supervisor patrol ticker (`cmd/gc/cmd_supervisor.go:1344-1370`)
  → per-city `CityRuntime.tick()` (`cmd/gc/city_runtime.go:~914-1271`) →
  `dispatchOrders()` (`:~1273-1289`). The city patrol default is ~30 s
  (`internal/config/config.go`, `PatrolIntervalDuration`). Every tick runs the
  **full sweep** whether or not anything changed.
- **Per-order read fan-out.** `order_dispatch.go` (~`:460-640`) already loads a
  `trackingIndex` snapshot once, but still issues residual **per-order** reads:
  `bdCursorAcrossStores()` for each event order, and `CheckTriggerWithOptions` →
  `checkEvent` → `events.Provider.List(...)` per event order
  (`internal/orders/triggers.go:~238-258`).
- **Per-session reads/writes.** The session reconciler iterates every session
  each pass, reading/writing session metadata (e.g. the restart-handoff and
  idle-timeout paths around `cmd/gc/session_reconciler.go` — flagged by
  @sjarmak on #2463 as the *top* idle-read candidates; line numbers drift across
  `main`).
- **Subprocess multiplier.** When the native store is unavailable each read
  shells out via `exec.CommandContext(ctx, name, args…)` in
  `ExecCommandRunnerWithEnv` (`internal/beads/bdstore.go:~64`) — spawn + connect
  + query per call.

### The #3248 multiplier (and why #3543's own diagnosis was incomplete)

`#3543` attributed our saturation to call volume but did **not** identify that
our binary was running with the **native store disabled**. It was: build
`721a42f0d` (2026-06-14 16:41 Z) is *behind* #3505's merge commit (2026-06-14
23:57 Z), so it predates the #3248 fix. The `WARN native_store_unavailable
gate=bd_context_agreement` we observed is the exact #3248 signature — every bead
op fell back to a CLI `bd` subprocess. **CLI-fallback was plausibly the dominant
multiplier for our specific numbers**, and it is already fixed upstream
(#3505/#3270). This document completes #3543's diagnosis and then designs for the
**residual** rate that remains *after* the store layer is healthy — see Goals.

## Goals

- Reduce the controller's steady-state **logical** `bd`/Dolt call rate on an
  idle or quiescent city, measured **on a post-#3505 binary** (native store +
  cache active), not on CLI-fallback numbers.
- Make the rate **scale with demand**, not with `tick_count × orders × rigs`;
  in particular, suspended/empty rigs must contribute ~0 reads.
- Make the call rate **observable** and add a regression guard.
- Preserve order-firing and event-order latency guarantees
  (`order-firing-current` doctor stays green; event-order p95 bounded).

## Non-goals (fenced off — owned elsewhere)

- **Native-store re-enablement / hook gating (#3248).** Done in #3505 + #3270.
  This design *assumes* the native store + `CachingStore` are active and
  optimises the rate on top of them.
- **Per-call query cost / Beads read optimisation.** `beads#4107`, the hot
  6-LEFT-JOIN aggregation, covering indexes, and using `ready_issues` are the
  **Beads/Dolt layers (1 & 2)** of #2463 — not this doc.
- **DoltLite embedded backend** (#2989/#3147/#3233/#3449) — store backend, not
  scheduling.
- **`bd`↔lib version-skew flood** (@mmlac on #2463) — a separate fail-loud /
  auto-pin issue.
- **The bd+Dolt contract** — owned by the Accepted `beads-dolt-contract-redesign`.
- **Wisp history cascade-delete / orphan prune** — retention/GC, already shipped
  for accumulation (#3424) and tracked separately for `wisp_events`/`wisp_labels`.

## Upstream Alignment

- **`beads-dolt-contract-redesign` (Accepted):** this is the Layer-3 consumer of
  that contract; no contract changes proposed.
- **`idle-session-sleep` (Accepted, implemented):** Pillar 1 lifts the same
  demand-driven principle from the *session* level to the *controller-tick*
  level. The session reconciler already sleeps/wakes individual sessions on
  demand; the controller still sweeps them all on a fixed tick.
- **#3492 (draft):** introduces `ListQuery.LabelPrefix` and fixes the orders-
  *feed* N+1. Pillar 2 **reuses that primitive** for the dispatch path, which
  #3492 does not touch. **#3511 (draft):** indexed order-run lookups in doctor —
  complementary.
- **#3097 (merged):** "cut supervisor reconcile CPU" — suspended-rig cache-
  reconcile skip + pack-hash/remote-cache memoization + `event_hooks` gate.
  **Supersedes most of Pillar 3** and reduces per-tick *cost*; this design's
  Pillars 1/2 reduce per-tick *count*, which #3097 does not address.
- **#2485 (merged):** `GC_BD_TRACE_JSON` scope-classified call tracer. Phase 0's
  meter; the cross-cutting `bd-call-rate` doctor check builds on it.
- **#3270 (merged) + #2928:** native store runs with gc-owned hooks +
  `event_hooks` toggle — establishes the native-store baseline this design
  optimises on top of.

## Design

### Pillar 1 — Demand-gated ticking *(lead)*

Replace the fixed-cadence full sweep with a **demand-gated** one. Each pass
computes a cheap **demand signal**; if nothing changed, the next city tick backs
off; any real signal wakes it immediately.

- **No-demand predicate (all true ⇒ idle):** event cursor unchanged since last
  pass (one tail read, see Pillar 2); no order due (cooldown/cron not elapsed,
  no matching event); no session-state delta requiring action; no pending
  poke / mail / `gc reload`.
- **Backoff:** exponential from the configured base (~30 s) to a capped ceiling
  (e.g. 5 min), reset to base on any wake signal. A **heartbeat floor** (one
  cheap liveness pass at the ceiling) guarantees forward progress even if a
  wake signal is missed.
- **Wake triggers (immediate):** new event appended (`events.Provider`),
  controller poke (existing `tick("poke")` path, `city_runtime.go`), inbound
  mail, config reload, session lifecycle request.

Why lead: this pillar is **robust to every store-layer win**. Even with a
perfect in-process cache, a fixed tick still re-evaluates orders, polls events,
and runs the cache reconciler at cadence; backing off when idle removes the
*number of passes*, which is the multiplier on all per-pass reads.

### Pillar 2 — Single-snapshot per-pass evaluation *(contingent)*

Within a pass, collapse residual per-order/per-session fan-out into **one**
snapshot read:

- Extend the existing `trackingIndex` to also carry, per scope: latest
  `seq:` cursors for all event orders and the relevant event tail — loaded with
  **one** `ListQuery.LabelPrefix` scan (the #3492 primitive) instead of
  `bdCursorAcrossStores()` per order.
- Evaluate every order's trigger against the in-memory snapshot; evaluate
  session decisions against a single session-metadata snapshot.

**Contingent:** with the `CachingStore` active post-#3505, some of these reads
are already cache hits. Phase 0 must measure the residual per-pass fan-out
*after* #3505 before investing here; if the cache already serves them, Pillar 2
is low-value and is dropped.

### Pillar 3 — Quiescent-scope skipping + cursor sanity *(largely shipped)*

**Mostly done in `origin/main`** — verify against the Phase-0 trace before
proposing any further work here:

- **Skip suspended rigs.** Already implemented: #3097 skips the beads-cache
  reconcile loop for suspended rigs, and the order dispatcher skips suspended
  cities and suspended-rig-targeted orders (`cmd/gc/order_dispatch.go:423,476`).
  This is the bulk of the #3543 win (our 15-of-16 idle rig DBs are now skipped).
- **Residual to confirm:** per-rig *runtime probes* on the
  `gc rig list --json` path (@Cdfghglz measured a core pegged); these may not be
  covered by the dispatch/reconcile skip. Phase 0 will show whether they still
  fire for suspended rigs.
- **Cursor sanity (likely still open).** The phantom event-cursor backlog where
  a suspended, 0-issue rig reports hundreds of thousands of "pending
  `bead.updated`" events (@mmlac) — initialise/clamp cursors so a quiescent scope
  yields an empty event match without a scan. Confirm against the trace.

### Pillar 4 — Snapshot-served hot hooks *(secondary)*

Serve the per-session-per-5 s status-line calls (`gc hook`, `gc mail check`) and
`gc rig list --json` from a controller-maintained snapshot with a short TTL,
rather than a fresh `bd`/runtime fan-out each call (validated informally by
@Cdfghglz's 60 s-TTL flock cache). Bounded staleness is acceptable for a status
line.

### Cross-cutting — bd-call-rate budget + meter

- A process-level **calls/sec meter** (by subcommand + caller), reusing the
  trace approach from PR #2485.
- A **doctor check** (`bd-call-rate`) that fails when idle rate exceeds a
  threshold — the regression guard implementing @coffeegoddd's "rate limit"
  directive.
- *(Optional, later)* a soft budget that defers non-critical reads when over
  budget.

## Implementation Plan

TDD / red-green; each phase independently shippable behind a flag, default-off
until validated, then defaults flipped.

- **Phase 0 — Baseline (gating). *In progress.***
  Instrumentation already exists (`GC_BD_TRACE_JSON`, #2485). This phase adds an
  **aggregator + repro procedure** (`engdocs/plans/idle-controller-call-rate-phase0.md`,
  `scripts/bd-call-rate/`) and measures the idle rate on a **native-store build of
  `origin/main`** (post-#3097/#3270 — `/tmp/gc-baseline` builds clean), single-rig
  and multi-rig. *Output:* the residual rate per subcommand **and per trace scope**
  (order-dispatch vs tick-body vs bead-event-watcher vs hook-cascade), which sets
  thresholds, confirms the Pillar-3 residual, and decides the Pillar-2 go/no-go.
  *Nothing else proceeds without these numbers.*

- **Phase 1 — Demand-gated ticking (Pillar 1).**
  Tests: a pass with no demand schedules a backed-off next tick; each wake
  trigger (event/poke/mail/reload/session-request) resets to base within one
  pass; the heartbeat floor fires at the ceiling. Flag `controller.idle_backoff`.

- **Phase 2 — Quiescent-scope skipping + cursor sanity (Pillar 3).**
  Tests: suspended rig ⇒ 0 order-eval reads, 0 runtime probes; a 0-issue
  quiescent scope reports 0 pending events (no phantom backlog).

- **Phase 3 — Per-pass snapshot coalescing (Pillar 2). *Blocked on #3492 merge.***
  *Only if Phase 0 shows residual per-pass fan-out matters.* Extend `trackingIndex`
  with cursors/event tail via `ListQuery.LabelPrefix` — the `LabelPrefix` primitive
  does **not** yet exist in `origin/main`; it is introduced by #3492 (still draft),
  so this phase cannot start until #3492 lands. Then remove per-order
  `bdCursorAcrossStores`. Test: N event orders ⇒ O(1) snapshot reads per pass
  (regression test in the spirit of #3492's 500-order test).

- **Phase 4 — Snapshot-served hot hooks (Pillar 4).**
  Tests: status-line calls served from snapshot within TTL ⇒ 0 store reads;
  correctness within bounded staleness.

- **Phase 5 — Rate-budget doctor check + telemetry; flip defaults.**
  Ship the `bd-call-rate` doctor check + telemetry; enable Phase 1/2 defaults
  after soak.

## Risks & Mitigations

- **Missed wake ⇒ stalled dispatch (Pillar 1).** Conservative, comprehensive
  wake triggers; a capped backoff ceiling; a heartbeat-floor pass guarantees
  eventual progress. Validate `order-firing-current` stays green under backoff.
- **Event-order latency regression (Pillars 1–3).** Event append is an explicit
  immediate wake trigger; assert an event-order p95 latency bound in tests.
- **Interaction with `idle-session-sleep`.** Pillar 1 gates the *tick*, not
  session wake demand; ensure an on-demand session wake is a controller wake
  trigger so a backed-off controller still services it promptly.
- **Stale hot-hook snapshot (Pillar 4).** Bounded TTL; mail/hook *writes* still
  invalidate; status-line reads only.
- **Optimising the wrong baseline.** Mitigated by Phase 0 gating on post-#3505
  measurements (the central lesson from #3543's incomplete diagnosis).

## Acceptance / Metrics

- Idle single-rig city (post-#3505): steady-state `bd`/sec drops from the Phase-0
  baseline to a target set by that baseline (goal: an idle city trends toward
  near-zero reads between real events, not a fixed floor).
- Multi-rig idle: total rate scales with *active* rigs, ~flat in suspended-rig
  count (our 16-rig case ⇒ ≈ single active-rig cost).
- No regression: `order-firing-current` doctor green; event-order p95 within the
  asserted bound; `bd-call-rate` doctor check green at the new threshold.

## References

- Issues: #3543 (this design's trigger), #2463 (umbrella; three-layer framing),
  #3248 (native-store gating — fixed by **#3505**/#3270), `beads#4107` (read opt).
- PRs: #3492 (`LabelPrefix` primitive + orders-feed N+1; reused by Pillar 2),
  #3511 (indexed order-run lookups), #2485 (`bd` trace instrumentation).
- Design: `idle-session-sleep` (Accepted), `beads-dolt-contract-redesign`
  (Accepted), `architecture/health-patrol.md`.
- Code anchors: `cmd/gc/cmd_supervisor.go:~1344-1370`,
  `cmd/gc/city_runtime.go:~914-1271,~1273-1289`, `cmd/gc/order_dispatch.go:~460-640`,
  `internal/orders/triggers.go:~238-258`, `cmd/gc/session_reconciler.go` (idle/
  restart paths), `internal/beads/bdstore.go:~64` (`ExecCommandRunnerWithEnv`),
  `internal/beads/caching_store_reconcile.go`, `internal/beads/factory.go:42-150`.
