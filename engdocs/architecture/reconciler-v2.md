---
title: "Session Reconciler v2"
---


> Last verified against code: 2026-10-03

## Summary

The v2 session reconciler replaces the legacy tick reconciler, the one
function that reads every store and decides every session on each
controller tick. v2 is keyed. Each trigger is mapped to the keys it
concerns: one session row, or the city-wide allocator. Workers drain
those keys from a queue, and each key is reconciled on its own.

v2 is built in phases. This page describes what is on main today: the P2
skeleton. The queue, the router, the lanes, the reload barrier, the worker
FS-pressure gate and the maintenance split are all live. The controllers
are trace-only: they record what they were asked to do and return. P3
replaces the allocator, and P4 replaces the session controller in four
behavior groups. Until both are in the build, a city cannot select v2
(see [Admission](#admission)).

This page also imports the A1-A17 guarantees into the repository
([Guarantees](#guarantees-a1-a17)). Until now they existed only outside it.

## The switch

`[daemon] session_reconciler` selects the reconciler. It accepts:

- `legacy` (the default): the tick reconciler.
- `v2`: the keyed reconciler.
- `off`, `auto` and `require`: the gc-enterprise spellings. They are
  deprecated aliases for `legacy`, and each one loads with a non-fatal
  warning.

The controller resolves the mode once, at boot (`latchReconcilerMode`,
`cmd/gc/reconcile_mode.go`), in `newControllerWiring`
(`cmd/gc/reconcile_wiring.go`). Both entry points (`gc start` and the
supervisor) call it before the controller socket starts. The latched mode
never changes for the life of the process. When a reload finds a different
value on disk, it warns once per transition ("pending restart") and keeps
running the latched mode. An unknown value refuses to start the city.
`runsV2()` is the single predicate every v2 branch reads.

### Admission

A city that selects `v2` is refused at start until the build carries both
controller groups. Those are the allocator (P3) and the first session
group (P4.1). `v2ControllersInBuild` and `defaultV2Controllers` derive
from the same two constants, so admission and the runtime cannot disagree.

For smoke runs of the skeleton on a throwaway city, the developer
override `GC_RECONCILER_V2_SKELETON=1` admits v2 anyway. The override is
read through an injected `lookupEnv`, and it is deleted at P4.1. While it
exists it is process-wide: under the supervisor it admits every city whose
config selects v2. A skeleton city starts, restarts and scales nothing.

`gc doctor` has a `daemon-session-reconciler` check:

| Value | Result |
|---|---|
| `legacy` or unset | OK |
| an alias | WARN |
| `v2`, admitted by the developer override | WARN |
| `v2`, not admissible in this build | ERROR |
| an unknown value | ERROR |

## Model: keys, queue, router

### Keys

A key names what reconciles. There are two kinds:

- A **session key** (`rowKey{Leg, ID}`) is one session row: the census
  label of its store, plus its bead ID. In P2 the leg is always the
  sessions-class store, and `[storage]` is boot-latched, so the label is
  stable for the process.
- The **allocator** is one city-wide pass. It runs as a paced lane, not as
  a queue key.

Every trigger carries a reason. The reason kind is a short code from a
closed set (`event`, `replay`, `api`, `socket`, `inventory`, `resync`,
`boot`, `retry`, ...). Operator intents (`api`, `socket`) and
`supervisor-reload` are urgent: they bypass a key's backoff gate.

### The queue (`internal/workqueue`)

The queue is a generic, stdlib-only keyed queue, used for session keys
only.

- **Dedupe.** Adding a key that is already queued merges its reasons into
  the queued item, up to 8 reason kinds.
- **Per-key serialization.** A key is never handed to two workers. A key
  added while it is processing is marked dirty, and it requeues at `Done`.
- **Two lanes.** Hot carries events, timers, API and snapshot diffs.
  Resync carries the boot enqueue-all and the periodic resync. Hot goes
  first, except that every 8th dequeue takes the oldest resync key, so a
  sweep finishes under sustained hot load. A hot add promotes a queued
  resync key.
- **Timers.** Each key has one timer, which keeps the earliest deadline.
- **Backoff.** A failed key backs off from 500ms, doubling to a 5m cap,
  with ±10% jitter. Until the retry time, non-urgent adds and dirty
  requeues are deferred (the backoff gate).
- **Holds.** While any named hold is active, `Get` hands out nothing. The
  reload barrier and the FS gate use holds.
- **Shutdown.** `ShutDown` stops admission, and queued work never starts.
- **Sequence numbers.** Every accepted add gets a queue-wide, monotonic
  sequence number. An item carries the numbers of its first and latest
  merged add, which coverage tracking uses (boot readiness, sweep time).

### The router (`cmd/gc/reconcile_router.go`)

The router maps every trigger to keys. It owns two reverse indexes:

- identity to session rows: every name a work bead can be assigned under;
- work bead to its last seen assignee.

Bead events maintain the indexes incrementally, and the resync lane
rebuilds them from cached census reads. The router has no write path:
everything it does is an enqueue. That is why cache-reconcile replays can
be routed like live events without re-creating the ga-yoix1 self-echo
loop (INC-006). Replays are counted separately from live events.

| Trigger | Keys |
|---|---|
| Session bead event or replay (sessions store) | that row, plus the allocator |
| Work bead event or replay | the sessions of the old and the new assignee, plus the allocator |
| Undecodable bead event | the allocator |
| Inventory pass flip | for each changed name: the owning session row, else the rows the name resolves to (and a reused name's previous owner); plus the allocator. A primed or health change alone wakes only the allocator |
| Session ID key (API, socket, lanes) | that row, with no index lookup |
| Session name key (provider events, gone names) | the rows the name resolves to, else rows closed under it, else the allocator plus a resync |
| Allocator, key-less or control-dispatch key | the allocator (MAINT-056) |
| Supervisor reload | the allocator, plus a full resync |
| Bead event tail gap | a full resync |

A mapping panic is recovered. The panic is counted (`router_panics`), its
trigger is dropped, and a resync is forced (F7).

### The wake

There is one wake sink (`controllerWake`, `cmd/gc/reconcile_wake.go`).
Every enqueue in the process goes through it: the API, the socket, the
config watcher, the lanes, the provider event pump and the bead event
watcher. Under legacy the wake folds keys onto the tick's channels,
exactly as before. Under v2 it hands them to the router.

### The runtime (`cmd/gc/reconcile_runtime.go`)

- **Workers.** Eight session workers drain the queue. Each reconcile runs
  under `safeTick` with a 5m context deadline. A panic or an error backs
  the key off; success forgets its failures and arms any requested
  requeue.
- **Allocator lane.** The allocator runs as a paced lane: a 100ms minimum
  gap, a busy fraction of at most about 50%, and a backstop at the patrol
  interval. Triggers that arrive during a pass fold into the next pass. A
  failed pass backs off from 1s to 30s.
- **Resync lane.** The resync lane has a 5m backstop and a 30s minimum gap.
  A full pass rebuilds the router, adds every open session row on the
  resync lane and wakes the allocator. A failed work census leg or an
  overflow needs only a rebuild.
- **Environment.** Workers read an immutable, per-generation
  `reconcileEnv` (config, provider, config revision). They never read
  `CityRuntime` fields, which a reload writes without a lock (F2).
  `TestV2RuntimeDoesNotReferenceCityRuntime` enforces this.
- **Boot.** Boot runs the first resync pass synchronously. It retries a
  failed sessions census, because a failed read is not an empty city. Then
  it starts the workers and lanes, and reports ready once every boot key
  has been reconciled once and the allocator has completed a pass. Boot is
  idempotent (MAINT-005). While it waits, it records the queue at every
  patrol interval (see [Observability](#observability)). A city with no
  bead store latches `no-store` and never boots (MAINT-003).
- **Shutdown.** Shutdown cancels, closes admission, then joins the workers
  and lanes, bounded by the shutdown timeout and outside any lock.

### Reload barrier and FS gate (`cmd/gc/reconcile_barrier.go`)

A config reload runs on the maintenance tick, under the barrier:

1. Hold the queue and both lanes.
2. Wait up to 30s for in-flight work to finish.
3. Apply the reload with the unchanged `reloadConfigTraced`.
4. Publish the next env.
5. Run the hooks.
6. Reply to a manual reload.
7. Release the holds.

If work is still running at the deadline, the reload aborts: the old
config stays, and the reload retries at the next patrol.

The worker FS gate is armed after readiness. Under sustained IO pressure
it holds the queue and both lanes. After five held patrol intervals it
releases for one interval (a forced window).

## Exclusivity

Under v2 no legacy session path runs. Each one is unreachable by
construction, and guarded besides.

| Legacy path | Under v2 |
|---|---|
| Startup step: corpse cleanup, stale-creating reap, desired state and sync, boot `beadReconcileTick` | Not run. The startup step runs the closed-bead reaper, the process-table orphan sweep and the boot maintenance sub-steps, then boots v2 |
| Tick session phases: corpse cleanup, stale reap, drain-ack finalize, demand and sync, desired-state refresh, soft-reload acceptance, `beadReconcileTick`, chat auto-suspend | Not run. `v2TickPhases` is `maintenancePhases(legacyTickPhases)` |
| Control dispatcher (`controlDispatcherTick`) | Its select arm reads a nil channel. The control-dispatch key is allocator work |
| In-memory drain tracker and provider-health gate | Not constructed |
| Poke-triggered full tick | Keys never reach `pokeCh`. Under v2 it carries maintenance wakes (reload) only |
| `tick_debounce` | Ignored, with one warning at boot (MAINT-017) |

The guard is `legacySessionEntry` (`cmd/gc/reconcile_maintenance.go`).
Under v2 it refuses every way into the legacy session reconciler, counts
the refusal in `legacySessionEntries` and logs each site once. The count
must stay 0, and the queue record reports it.

These run unchanged in both modes:

- the adoption barrier;
- the orders, inventory, route-recovery, completions and detached-orphan
  lanes;
- on_death;
- the maintenance reapers;
- convergence;
- the nudge dispatcher;
- operator commands;
- the shutdown stop.

### The maintenance tick

Under v2 the tick runs at patrol cadence and on maintenance wakes only.
It traces as `maintenance_tick`. Its phases are the legacy tick's
non-session phases, plus the maintenance sub-steps of `beadReconcileTick`:

| Sub-step | Row |
|---|---|
| `emit_due_compute_facts` | MAINT-045 |
| `start_historical_transcript_meta_reconcile` | MAINT-046 |
| `sweep_detached_handoff_orphans` | MAINT-048 |
| `nudge_dispatch_tick` | part of MAINT-055 |

Two of those sub-steps still trace under their `bead_reconcile.*` names,
so a trace query written for legacy finds them in both modes.

## Observability

### The `reconcile_queue` record

Every v2 maintenance tick records one `reconcile_queue` operation (site
`reconcile.queue`) in its trace, however the tick ends. Maintenance ticks
start only after readiness, so while boot waits, the startup step records
one in its own trace (`initial_reconcile`) at every patrol interval. A
legacy tick records none. Adds are counted since the previous record.
Every other field is cumulative or current.

| Fields | What |
|---|---|
| `boot` | What boot still waits on: `census`, `coverage` (a boot key not yet reconciled), `allocator` (no successful pass yet), `ready`, or `no-store` (MAINT-003: the startup step found no bead store, so no workers; latched) |
| `keys`, `depth_hot`, `depth_resync`, `processing`, `dirty`, `deferred`, `timers` | Queue state |
| `oldest_hot_ms`, `oldest_resync_ms`, `longest_in_flight_ms` | The oldest queued key per lane, and the longest reconcile in flight |
| `adds`, `dropped_adds` | Accepted adds by reason kind since the last record; adds refused after shutdown |
| `reconciles`, `reconcile_failures`, `reconcile_panics` | Reconcile outcomes |
| `latency_p50_ms`, `latency_p99_ms` | Enqueue-to-start latency of first attempts, every reason: the polled inputs of target 3, and queue health for target 6 |
| `bead_event_latency_p50_ms`, `bead_event_latency_p99_ms` | The same, for reconciles a live bead event queued: the item's first reason is `event`, the add its wait is timed from. This is the bead-event half of target 3's evented inputs; provider events, API and socket keys are not split out |
| `work_p50_ms`, `work_p99_ms` | Reconcile duration |
| `session_reasons` | Reason kinds the trace-only session controller was handed (P2 only; P4 replaces it) |
| `allocator_passes`, `allocator_failures`, `allocator_last_pass_ms`, `allocator_duty`, `allocator_wakes` | The allocator lane; the duty is the busy fraction over the last 5m (target 4); wakes are the passes' reasons by kind |
| `resync_sweep_ms`, `resync_superseded`, `resync_requests`, `boot_ms` | The last sweep's duration, superseded sweeps, resync wakes by reason, and the boot duration (target 6) |
| `router_events`, `router_replays`, `router_undecodable`, `router_keys_out`, `router_unresolved`, `router_panics` | Router counters; `router_events` includes undecodable live events. A recovered mapping panic drops its trigger and forces a resync |
| `holds`, `fs_gate` | Active holds; the worker FS gate (`unarmed`, `open` or `held`) |
| `legacy_session_entries` | Refused legacy session entries. Must be 0 |

The latency samples are windowed: the last 1024 reconciles.
`TestV2MaintenanceTraceRecordsReconcileQueue` pins the field names.

### Alerts and banners

- **Stuck reconcile.** When the record finds a session reconcile in flight
  for at least twice the reload deadline (60s), it alerts on stderr, once
  per stuck reconcile. A reload has already aborted on it by then, and
  reloads keep aborting until it returns. The alert is raised whether or
  not the tick is traced, and during boot too. It watches session
  reconciles only; allocator and resync passes are P3's.
- **Startup watchdog.** Under v2 the readiness watchdog (MAINT-004) prints
  what boot waits on, the queue depth, the reconciles in flight and the
  longest one, before its goroutine dump.
- **Boot banner.** "session reconciler: v2 (skeleton: trace-only
  controllers)".
- **Barrier.** The barrier logs every aborted reload.
- **FS gate.** The FS gate logs each pressure episode.

`gc status` and API fields wait for P4.1. Per-reconcile trace records
(MAINT-021) come with P3.

### Bead event feed

The bead event watcher (`startBeadEventWatcher`) feeds the caches and the
router. When the tail breaks, the watcher re-watches from the last seq it
read. `events.Provider.Watch` replays every retained event after that seq,
so a resumed tail loses nothing and reports no gap. It reports an event
gap, which forces a full resync, in these cases:

- a re-watch that fails;
- a watcher that breaks before reading past its cursor;
- a sequence that goes backwards, from a provider that delivers one;
- a city with no feed at all.

After the first two it waits 1s before watching again.

A log reset is not a gap the watcher can see. FileRecorder's watcher drops
every event at or below the highest seq it has delivered, so after a reset
it delivers nothing until the new log passes the old head, and a re-watch
from the cursor does the same. Until P3-7 adds provider-level reset
detection, v2 relies on the resync lane's 5m backstop to repair the
router's indexes after one.

The legacy reconciler ignores gaps.

## BEHAVIORS rows touched by P2

The status values mean:

- **P2-n**: built and tested in that PR.
- **inherited**: an unchanged code path, now asserted under v2.
- **off until Px**: not run under v2 until that owner lands.
- **n.a.**: no P2 effect.

| Row | Status | v2 test |
|---|---|---|
| MAINT-001 boot owns city, watcher before lanes | inherited | `TestV2Boot_OwnsCityAndArmsWatcherBeforeLanes` |
| MAINT-002 order-tracking sweep before lock | n.a. | - |
| MAINT-003 no store, no session reconcile | P2-8, P2-9 | `TestCityRuntimeV2NoStoreDisablesWorkers`, `TestCityRuntimeV2NoStoreQueueRecordSaysNoStore` |
| MAINT-004 readiness watchdog | inherited; P2-9 adds the v2 boot line and boot patrol records | `TestV2BootPatrolRecordsWhatBootWaitsOn` |
| MAINT-005 startup retry only on panic | inherited (`v2.boot` is idempotent) | `TestV2Boot_FirstPassRetriesOnPanicBoundedByMaxRestarts` |
| MAINT-006/007 adoption barrier and startup reload first | P2-8 | `TestCityRuntimeV2AdoptionAndStartupReloadPrecedeBootEnqueue` |
| MAINT-008/009 orders and route lanes before first pass | inherited | - |
| MAINT-010/012 boot enqueue-all; ready after the first full pass | P2-6, P2-8 | `TestV2BootReadyOnlyAfterEveryKeyProcessedOnceAndAllocatorPrimed` |
| MAINT-011 convergence startup before ready | inherited | - |
| MAINT-013 dirty at boot applies immediately | inherited (barrier) | - |
| MAINT-014 nudge wake socket | inherited | - |
| MAINT-015 provider events, session key | P2-5, P2-8 | `TestCityRuntimeV2ProviderEventEnqueuesSessionKey` |
| MAINT-016 reload accept never blocks | inherited | - |
| MAINT-017 dedupe replaces debounce | P2-2, P2-8 | `TestQueueAddDedupesAndMergesReasons`, `TestCityRuntimeV2TickDebounceIgnoredWithOneWarning` |
| MAINT-018 maintenance at patrol cadence, off workers | P2-4, P2-8 | `TestCityRuntimeTickV2RunsMaintenancePhasesOnly` |
| MAINT-019 convergence requests | inherited | - |
| MAINT-020 panic recovery everywhere | P2-6 | `TestV2WorkerPanicIsRecoveredAndKeyRequeuedWithBackoff` |
| MAINT-021 per-reconcile trace ids | P3 (P2-9 records a per-tick queue summary) | `TestV2MaintenanceTraceRecordsReconcileQueue` |
| MAINT-022 on_death edges | done (P1.4) | - |
| MAINT-023 barrier replies before resume | P2-7, P2-8 | `TestCityRuntimeV2ReloadReplyBeforeResume` |
| MAINT-024 provider swap needs a complete listing | inherited inside the barrier; effect wait in P4 | - |
| MAINT-025 soft drift rewrite | off until P4.4 | `TestCityRuntimeV2SoftReloadRepliesUnavailable` |
| MAINT-026 FS gate pauses workers | P2-7 | `TestWorkerFSGateHoldsAndForcesAWindowAfterFivePatrols` |
| MAINT-027 Dolt preflight | inherited | - |
| MAINT-028/029 orders lane, route delta | done / inherited | - |
| MAINT-030 snapshots feed decisions | off until P3 (the v2 tick reads a cached snapshot for maintenance only) | - |
| MAINT-031 corpse close | off until P4.4 | - |
| MAINT-032/033/035/037 closed-bead reaper, process orphans, worktree reaper, extmsg reapers | inherited | `TestCityRuntimeTickV2RunsMaintenancePhasesOnly` |
| MAINT-034 stale creating | off until P4.1 | - |
| MAINT-036 drain-ack finalize | off until P4.2 | - |
| MAINT-038 `beadReconcileTick` gating | P2-4, P2-8, P2-9 | `TestCityRuntimeV2StartupSkipsLegacyBootReconcile`, `TestLegacySessionEntryGuardBlocksAndCountsUnderV2`, `TestV2LegacySessionEntriesReportedInTrace` |
| MAINT-039/040/041/043 completions, wisp GC, services, convergence | inherited | `TestCityRuntimeTickV2RunsMaintenancePhasesOnly` |
| MAINT-042 chat auto-suspend | off until P4.4 | - |
| MAINT-044/050 partial census, pool desired | P3 | - |
| MAINT-045/046/048 usage facts, transcript meta, detached delta | P2-4 (sub-steps in the v2 list) | `TestCityRuntimeTickV2RunsMaintenancePhasesOnly`, `TestCityRuntimeStartupV2RunsMaintenancePhasesOnly` |
| MAINT-047 orphan release | off until P3 | - |
| MAINT-049 squatter probe | off until P3 | - |
| MAINT-051/052/053 pool sweep, wait deps, reconcile | P3/P4 | - |
| MAINT-054 follow-up pokes | P2-3 (through the wake; legacy-only callers) | `TestEveryEnqueueSourceKeepsItsLegacyWake` |
| MAINT-055 nudges | patrol fallback inherited; wait and stalled nudges off until P4.3 | - |
| MAINT-056 control dispatcher | P2-5, P2-8 | `TestRouterControlDispatchKeyMapsToAllocator`, `TestCityRuntimeV2ControlDispatcherSignalNeverRunsLegacyPath` |
| MAINT-057 one-shot start | n.a. | - |
| API-001/011 key-less and sling, allocator only | P2-5 | `TestRouterKeylessAndAllocatorKeysWakeAllocatorOnly` |
| API-002 replays enqueue, never write | P2-5 | `TestRouterReplayRoutesEnqueueOnlyAndNeverWrites` |
| API-003/013/017 config mutation, socket reload, watcher: barrier | P2-3, P2-8 | `TestCityRuntimeV2ConfigMutationRunsBarrierThenWakesAllocator` |
| API-004 supervisor reload | P2-3, P2-5 | `TestRouterSupervisorReloadWakesAllocatorAndResync` |
| API-005/016 service poke, trace arm | P2-3 | `TestEveryEnqueueSourceKeepsItsLegacyWake` |
| API-006..010 session keys | P2-5, P2-8 | `TestRouterSessionIDKeyNeedsNoIndex`, `TestCityRuntimeV2KeyedEnqueueReachesQueueNotTick` |
| API-012 socket optional key | done (P1.2); routed by P2-3 | `TestControllerSocketPokeAcceptsOptionalKey` |
| API-014 control-dispatcher socket | P2-5, P2-8 | as MAINT-056 |
| API-015 circuit-reset socket | unchanged operator path; session key in P4.1 | - |
| API-018 provider events | P2-3, P2-8 | as MAINT-015 |
| API-019 doctor lock probe | inherited | - |
| GUAR-012 trace status | P5 | - |
| GUAR-013 shutdown | P2-2, P2-6 | `TestV2StopClosesAdmissionJoinsWorkersWithoutStartingQueued` |
| GUAR-014 superseded reload | P2-7 | `TestReloadBarrierSupersededReloadPublishesNothing` |
| GUAR-015 provider swap | the barrier wraps it (P2-7); effects in P4 | - |
| INC-001 panic | P2-6 | as MAINT-020 |
| INC-006 replay loop | P2-5 | as API-002 |
| INC-022 stale cache on poke | P4 (P2 carries `Item.Reasons`) | - |
| INC-026 stop outside the lock | P2-6 | as GUAR-013 |
| INC-031 per-store CAS | P4.1 | - |
| INC-033/034 reload atomicity, swap drains starts | P2-7 / P4 | - |

## Guarantees (A1-A17)

A1-A17 are the user-visible guarantees that the gc-enterprise keyed-lane
tests encode. Until now their only definition was a distill-analysis
report outside git (BEHAVIORS OQ-8). Decision 58 imports them here, with
the P2 PR. Each one is mapped to its BEHAVIORS row (GUAR-001..016) and
given a verdict for main.

The "Main today" column names the main tests that come closest to each
guarantee, where any exist. A guarantee is a v2 acceptance requirement
from the phase its owner lands in. Before then, legacy keeps providing
whatever main already has.

| ID | Guarantee | Row | Main today | Verdict |
|---|---|---|---|---|
| A1 | A keyed reconcile of one session reads only that session's row. It never lists the session fleet | GUAR-001 | Legacy lists the fleet every tick, by design. No analog | **Adopt**: the core v2 property. The session controller (P4.1) must pass a list-rejecting store test. In P2 the router lists only in the resync pass, from cached census reads |
| A2 | Exactly one provider Start per admission: no double start, no duplicate runtime, no restart of a live runtime | GUAR-002 | `session_lifecycle_parallel_test.go` (parallel start waves) | **Adopt** at P4.1. Assert on provider call counts |
| A3 | Named and pinned sessions keep one canonical identity (bead ID, `session_name`, configured identity, `pin_awake`) across wake, unpin and public kill | GUAR-003 | `TestCanonicalDuplicateSessionBead`, `TestAdoptionBarrier_StampsCanonicalIdentity` | **Adopt**. Wake at P4.1; duplicate verdicts at P4.4, where only the loser's own session key retires it (N5) |
| A4 | Stopping a durably suspended user-hold session keeps the row open and suspended, keeps `sleep_intent` and `held_until`, adds no drain-ack stamps, and makes exactly one token-bound unattended stop | GUAR-004 | No analog (BEHAVIORS marks it enterprise-only) | **Adopt with the stop path** (P4.2). There is no main behavior to preserve; the enterprise behavior becomes the v2 requirement |
| A5 | A drain-ack stop finalizes only on proven, complete runtime absence. Every re-kill re-certifies first. The close is atomic and comes before the stop | GUAR-005 | The drain-ack tests in `session_reconciler_test.go` | **Adopt** at P4.2. A partial or uncertain observation never finalizes |
| A6 | Never terminate an attached user's pane, and kill only the exact certified pane | GUAR-006 | `TestAutoSuspendSkipsAttachedSessions` | **Adopt** at P4.2 (drain and stop) and P4.4 (chat auto-suspend) |
| A7 | Nudges never inject unguarded keystrokes: every tmux send-keys is wrapped in an if-shell token guard | GUAR-007 | None. **Unmet on main under both reconcilers**: tmux nudges call send-keys with no guard (`SendKeysDebounced` for the text and the Enter, and `sendLiteralText`, in `internal/runtime/tmux/tmux.go`) | **Adopt; owner P4 (nudge delivery)**. The gap is in the effect path, so v2 does not widen it, but P4.3's nudge effects must add the guard, and A7 is unmet until they do |
| A8 | Each due nudge is delivered exactly once, to the right session, across a controller restart | GUAR-008 | The store-backed nudge queue: `TestClaimDueQueuedNudgesClaimsOnceUntilAck`, `TestBeadsNudgeQueueClaimHasExactlyOneWinner`, `TestDispatchAllQueuedNudgesDeliversAndAcks`. None crosses a restart | **Adopt** at P4.3. Until then the nudge patrol fallback runs as maintenance in both modes |
| A9 | Routed work gets a pool member: reuse the oldest idle member, else grow, else fall back at the cap. Manual and dependency-only sole members are refused | GUAR-009 | `build_desired_state_test.go` | **Adopt** at P3 (allocator). Caps count the cache plus the intent ledger |
| A10/A11 | A session waiting on a dependency starts once when the dependency closes. A canceled or revoked wait starts nothing | GUAR-010 | None | **Adopt** at P4.3, through a router dependency-to-waiter index. The cancellation is re-read before the start |
| A12 | On ambiguous input, refuse with zero effects: no writes, no runtime calls | GUAR-011 | Scattered fail-closed checks; no zero-effect counters | **Adopt as house style** for every P3/P4 decide. Each refusal test asserts a zero-effect counter. The P2 skeleton meets it vacuously (`TestV2TraceOnlyControllersWriteNothing`) |
| A13 | `gc trace status` gives a live, read-only, race-safe view of keyed state, with human and JSON output in parity | GUAR-012 | `gc trace status` exists, but it reports the tracer (schema, head seq, active arms), not reconciler state | **Defer to P5**. Until then there is no live keyed view. The per-tick `reconcile_queue` record (P2-9) is the nearest substitute: a queue summary in the trace, written once per patrol, with no per-key state |
| A14 | Controller shutdown drains in-flight keyed work before teardown. Stop closes admission, joins workers and cannot deadlock | GUAR-013 | Legacy has no keyed admission | **Met for the skeleton** (P2-2, P2-6): `TestQueueShutDownStopsAdmissionAndNeverStartsQueued`, `TestV2StopClosesAdmissionJoinsWorkersWithoutStartingQueued`. The in-flight effect drain arrives with P4's effect executor |
| A15 | A config reload never publishes a superseded snapshot and never blesses an incoherent store. A pack mutation rolls back fully | GUAR-014 | `TestCityRuntimeReloadRejectsRevisionSupersededDuringPreparation`, `TestCityRuntimeProviderReloadSupersededDuringCensusDoesNotPublishSwap` | **Inherited**. v2 runs the unchanged `reloadConfigTraced` inside the barrier, and a superseded reload publishes no env (`TestReloadBarrierSupersededReloadPublishesNothing`) |
| A16 | A provider swap neither loses nor duplicates keyed sessions. It drains starts before listing the old provider and restores it on a list failure | GUAR-015 | `TestCityRuntimeReloadProviderSwapFailsOnPartialSessionListing` | **Partly inherited**. The barrier holds every reconcile across the swap (P2-7). The wait for in-flight effects is P4's `beforeProviderSwap` |
| A17 | An agent-start latency benchmark (30 clean samples) guards the performance goal | GUAR-016 | None | **Adopt; the harness is kept** (owner decision). The 30-sample agent-start latency harness, `TestAgentStartLatencyPerf_V2`, is a P5 acceptance obligation, on top of the soak targets |

## Code map

| File | Role |
|---|---|
| `cmd/gc/reconcile_mode.go` | Boot latch, admission, the drift warning, the doctor check |
| `cmd/gc/reconcile_wiring.go` | `newControllerWiring`: channels, the mode, the wake, the v2 runtime |
| `cmd/gc/reconcile_wake.go` | The one wake sink |
| `cmd/gc/reconcile_router.go` | The router and its reverse indexes |
| `cmd/gc/reconcile_runtime.go` | Workers, the allocator and resync lanes, boot, shutdown |
| `cmd/gc/reconcile_barrier.go` | Reload barrier, lane holds, the worker FS gate |
| `cmd/gc/reconcile_metrics.go` | Metrics and the `reconcile_queue` record |
| `cmd/gc/reconcile_maintenance.go` | `maintenancePhases` and the `legacySessionEntry` guard |
| `cmd/gc/city_runtime_v2.go` | The city runtime's side: `v2Host` (F2), boot, the barrier call, the queue record call |
| `internal/workqueue/` | The keyed queue |

## See Also

- [Controller](controller.md) -- the controller loop that hosts both
  reconcilers
- [Health Patrol](health-patrol.md) -- the legacy reconciliation state
  machine that v2 replaces
- [Reconciler debugging](../contributors/reconciler-debugging.md) -- `gc
  trace` workflows. The `reconcile_queue` record is an operation record in
  its tick's cycle, so `gc trace cycle --tick <tick_id>` shows it
