# W-tick: the reconciler tick-feed refactor + endgame wave sequencing — authoritative design

**Ground truth verified at `6448e2b2a`** (docs-only commit on top of migration tip
`1b93614da`; all Go code identical to `1b93614da`). Every file:line below was read or
grepped at this HEAD. DESIGN ONLY — no code in this doc is written to the tree.

This is the contract for the remaining "stores return domain objects" endgame:
**W-tick → W-pool → W-delete → W-flip → W-unexport**. W-tick is the keystone and the
hardest wave; it is specified to implementation precision in §2. Corrections to
`/tmp/r6_finding.md` (= `engdocs/plans/store-domain-objects/r6-finding-tickfeed-keystone.md`)
and to `remainder-design.md` §5c are called out inline and collected in §1.6.

---

## 1. Ground-truth tick data-flow map (today)

### 1.1 Who feeds the tick

The reconciler root is `reconcileSessionBeadsTracedWithNamedDemand`
(`cmd/gc/session_reconciler.go:1092`), parameter `sessions []beads.Bead` (:1095), with
the work class arriving SEPARATELY as `assignedWorkBeads []beads.Bead` (:1102). Feeds
(all via the raw half of `sessionBeadSnapshot`):

| Caller | Feed | Site |
|---|---|---|
| Main controller tick | `open := sessionBeads.Open()` → `:2300` call | `city_runtime.go:2252/:2300` |
| Control-dispatcher / config-change tick | `open := filterSessionBeadsByName(updated, cfgNames)` (`:3117` iterates `snapshot.Open()` at `:3122`) → `:2976` call; also `newSessionBeadSnapshot(open)` re-wrap at `:2969` for `retainScaleCheckPartialPoolDesired` | `city_runtime.go:2962-3003` |
| Standalone `gc start` | `open := sessionBeads.Open()` (post-sync snapshot) | `cmd_start.go:929/:943 → :961` |
| Drain-ack finalize pass | `sessionBeads.Open()` → `finalizeDrainAckStopPendingSessions` | `city_runtime.go:1153-1159 → session_reconciler.go:556` |

The snapshot itself is loaded by `loadSessionBeadSnapshot(store)`
(`session_bead_snapshot.go:72`) via `sessionpkg.ListAllSessionBeads(store, ListQuery{})`
(:90) and constructed by `newSessionBeadSnapshot(beads)` (:97), which already builds
`openInfos` in lockstep (`openInfos[i] == InfoFromPersistedBead(open[i])`, :104-111).

**Aliasing fact the R6 finding missed:** `Open()` (:285) copies the *slice* but the
`Bead.Metadata` **maps are shared** with the snapshot's backing beads. Every Phase-0
in-place mirror therefore silently propagates into the snapshot's raw half (and into
anything else sharing those maps, including a CachingStore's cached rows if the store
returns un-cloned beads). One consumer *relies* on this (§1.4, stranded throttle); for
everything else it is an aliasing hazard the refactor eliminates.

### 1.2 The tick's phases and where the three codec calls live

The reconciler's `InfoFromPersistedBead` census is **3** (`typedclass_edge_guard_test.go`
row `session_reconciler.go: 3`), and the three calls are (NOT :583/:1342/:1419 — those
line anchors are stale by wave drift):

1. **`:582`** — `finalizeDrainAckStopPendingSessions` (:556) boundary projection: this
   is a *separate caller-fed pass* (city_runtime.go:1159), not the tick body. It
   projects each caller-loaded raw bead once and feeds the drain-ack helpers Info.
   Its internal "post-mutation re-read" is the NDI witness at `:439` — **already**
   `sessionFrontDoor(store).Get` (front-door, documented non-fast-path). So the
   §5c question "does :583 re-read a closed bead post-mutation?" — NO; :582 is a
   boundary projection of a caller list, and the one genuine re-read inside is
   already a sanctioned front-door Get. It needs neither a new Get nor snapshot
   plumbing; it needs the pass to take `[]Info`.
2. **`:1187`** — Phase-0 heal: `healExpiredTimers(&sessions[i],
   InfoFromPersistedBead(sessions[i]), sessFront, clk)`. `healExpiredTimers`
   (`session_reconcile.go:434`) reads only `info.HeldUntil / info.QuarantinedUntil /
   info.SleepReason`, persists `ClearExpiredHoldPatch`/`ClearExpiredQuarantinePatch`
   via `sessFront.ApplyPatch`, and mirrors the batch onto the raw bead **solely so the
   snapshot build at :1272 (which happens later) re-projects the healed values**
   (comment :1184-1186, :431-433). The mirror has no other consumer.
3. **`:1272`** — the snapshot build: `orderedIDs/orderedInfos/infoByID/beadByID` are
   built by projecting each `orderedBeads[i]` once (:1260-1277).

Between (2) and (3) sit:

- **Dedup / duplicate-retire** (:1189-1211): builds raw `bySessionName` /
  `indexBySessionName` maps from `b.Status` + `b.Metadata["session_name"]`, then
  `sessions = retireDuplicateConfiguredNamedSessionBeads(...)` (:1208; definition
  `session_beads.go:482`). It mutates `openBeads[idx]` in place (RetireNamedSessionPatch
  mirror + `Status="open"`, :543-550) and calls raw classifiers
  (`isNamedSessionBead`, `namedSessionIdentity`, `namedSessionContinuityEligible`,
  `namedSessionBeadWinsCanonicalRepair` :565) plus repair side-effects
  (`stopRuntimeBeforeSessionBeadMutation` :2408 — reads only `Metadata["session_name"]`
  + ID; `reassignWorkAssignedToRetiredSessionBead` :851 — session-side reads only
  `sessionAssignmentIdentifiers(retiredSession)` + ID, work-side walk is ClassWork;
  `reassignStateAssignedToRetiredSessionBead` :893 — IDs only). All writes already go
  through the front door (`setMetaBatch(sessionFrontDoor(store))` :534,
  `SetStatusOpen` :537). **This helper is SHARED with the class-(c) sync path** —
  second caller `session_beads.go:1137` inside `syncSessionBeadsWithSnapshotAndRigStores`,
  where the two maps DO have downstream consumers. In the reconciler caller the maps
  are dead after the call.
- **`topoOrder(sessions, deps)`** (:1238; `session_reconcile.go:1057`): reads only
  `s.Metadata["template"]` (verbatim, untrimmed — `Info.Template` is the same verbatim
  mirror, `info_store.go:38`). Returns `sessions` unchanged (no deps / cycle) or a new
  slice; either way the working set is not reallocated afterwards (:1268).

### 1.3 Phase 0.5 + the forward pass: already Info-fed

- **Phase 0.5 circuit breaker** (:1279-1339): identity reads come off `orderedInfos`;
  the *persisted breaker cluster* is read via
  `sessionpkg.CircuitStateFromMetadata(orderedBeads[i].Metadata)` (:1304, :1313) — a
  **distinct 9-key typed codec** (`internal/session/circuit_state.go:62`:
  session_circuit_{state,restarts,last_restart,last_progress,last_observed,
  progress_signature,opened_at,open_restart_count,reset_generation}). `Info` carries
  only `SessionCircuitState` (manager.go:250) **by settled design** ("the breaker
  cluster is a separate concern from Info", :1224-1226). This is the one tick read
  that `[]Info` alone cannot serve — it drives the row shape in §2.1. Writes go
  through `persistSessionCircuitBreakerMetadata(sessFront, ...)` (:1316, :1328) —
  already front-door.
- **Phase 1 forward pass** (:1381-2925): every decision read is `infoByID[session.ID]`
  and every mutation is a write-returns-Info fold (`ApplyPatch`/`ApplyPatchInfo`/
  `MarkClosed`/`applyTo`) — the blanket pre-pass is gone, STEP6-PREPASS-AUDIT groups
  1-12 all fold locally. WI-6 R4 already deleted `startCandidate.session` /
  `wakeTarget.session` (`wakeTarget` :37-45 carries only `info/tp/alive`), so §5c's
  precondition "after R4" is **already satisfied**.
- **Awake scan + Phase 2**: the scan domain is rebuilt from `orderedIDs` → `infoByID`
  (:2952-2955, order-load-bearing); the drain advance reads an `infoLookup` closure
  over `infoByID` (:3311-3317).

### 1.4 The COMPLETE inventory of remaining raw uses inside the tick

Everything the working set (`sessions`/`orderedBeads`/`beadByID`) still feeds raw,
with the Info-availability verdict:

| # | Site | Raw reads / mutations | Info status |
|---|---|---|---|
| a | Phase-0 heal :1187 | HeldUntil/QuarantinedUntil/SleepReason reads; batch mirror onto `session.Metadata` | all on Info; mirror exists only for the later :1272 projection |
| b | Dedup :1197-1210 | see §1.2 | all session-side reads on Info (`Generation`:info_store.go:108, `CreatedAt`:53, `SessionNameMetadata`, `ConfiguredNamedIdentity`); **`NamedSessionContinuityEligibleInfo` does not exist yet** (name pre-reserved by manager.go:217 comment) |
| c | Circuit :1304/:1313 | `CircuitStateFromMetadata(orderedBeads[i].Metadata)` | NOT on Info (settled); needs the row pair (§2.1) |
| d | `reconcileDrainAckStopPending(..., session, info, ...)` :1411 (+ helpers :420-466) | `session.ID`; `session.Status="closed"` mirrors (:422/:457, plus :1575/:1846 in the loop) | ID trivial; the Status mirror's only same-tick reader is the parallel `infoByID` MarkClosed fold that ALREADY exists (:437→applyTo, :1587, :1858) — the raw set is vestigial, asserted only by the telemetry close-path test |
| e | `sessionAttachedForConfigDrift(*session, ...)` :2015/:2121/:2477 (def :4109) | `session.ID` only | trivial (takes id) |
| f | `freshRestartSessionKey(tp, session.Metadata)` :2228 (def :621) | session_id_flag / resume_flag / resume_command / resume_style | ALL on Info: `SessionIDFlag` (manager.go:367-372, added expressly for this read), `ResumeFlag/ResumeStyle/ResumeCommand` (info_store.go:50-52) |
| g | `traceHealClearedPendingCreateLease(trace, *session, ...)` :1601/:2293 (def :4169) | template/session_name fallbacks via `normalizedSessionTemplate` | `normalizedSessionTemplateInfo` + `Info.Template/SessionNameMetadata` exist |
| h | `silentRebaselineSessionHashes(session, ...)` :2435/:2652/:2725 (def :4737) | `session.ID` only (mirror already dropped, :4749-4753) | trivial (takes id) |
| i | `relaunchAgentForLaunchDrift(..., session, ...)` :2522/:2593 (def :4774) + `rebaselineLaunchDriftHashesWithBatch` (:4854) | `session.ID` only | trivial (takes id) |
| j | `resetConfiguredNamedSessionForConfigDrift(session, ...)` :2535/:2745 (def :4276) | `Metadata["session_key"]`/`["started_config_hash"]` (:4314-4315) + ID | `Info.SessionKey`/`Info.StartedConfigHash` exist |
| k | `isNamedSessionBead(*session)` :2503/:2706 | named markers | `isNamedSessionInfo` exists (named_sessions.go:48) |
| l | `clearDrainTrackerForStopPending(session, dt)` :1728/:2073 (def :98) | ID only | trivial |
| m | `cycleAliveSessionForFreshReassign(beadByID[...], ...)` :3140 (def `session_bead_cycle.go:64`) | `Metadata[CurrentBeadIDKey]`, `namedSessionIdentity(*session)`, `freshRestartSessionKey(tp, session.Metadata)`; mirrors the batch (minus ResetCommittedAtKey) onto the raw map AND returns the identical fold | `CurrentlyProcessingBeadID` (info_store.go:125) + (f) + `namedSessionIdentityInfo`; the raw mirror is redundant with the returned fold (same key set, same exclusion) |
| n | `emitSessionStrandedDiagnostic(..., beadByID[...], ...)` :3243 (def :3611) | guard-reads + SETS `Metadata[strandedEventEmittedKey]` in memory BEFORE the durable `SetMarker` (:3656-3660); session-side work-scan via `sessionAssignmentIdentifiersForConfig` + `reachableStoresForSession` | `Info.StrandedEventEmittedAt` (manager.go:348-351); `...ForConfigInfo` (session_beads.go:677) and `reachableStoresForSessionInfo` (:3505) exist. **Cross-tick nuance:** today's raw write lands in the SHARED metadata map (§1.1), so a same-controller-lifetime snapshot reuse still sees the marker even when `SetMarker` failed — pinned by `TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure`. The Info fold alone does NOT reach `snapshot.openInfos`; §2.5 adds an explicit snapshot fold to preserve the pin. Work-side walk (`collectSessionAssignedWork` internals) is ClassWork — stays raw |
| o | `pruneAgentHomeWorktreeIfSafe(*beadByID[...], ...)` :3270 (def `session_worktree_prune.go:54`) | `contract.WorkerDirFromMetadata(session.Metadata)` (canonical `beadmeta.WorkerDirMetadataKey` falling back to legacy `work_dir`, `internal/beads/contract/metadata.go:52`); `lookupRigRootForSession` reads only `Metadata["template"]` (:124-137) | `Info.WorkDir` covers only the LEGACY key — **field-add required**: `Info.WorkerDir` (canonical raw mirror) + `WorkerDirFromInfo` fallback helper (BuiltinAncestor precedent, remainder-design §4) |
| p | trace fields `len(orderedBeads)` :2927/:3320/:1338/:1240 | count only | `len(rows)` |

**Main-tick caller-side raw consumers of the SAME `open` slice** (must convert with
the callers in W-tick or the slice survives):

| Site | Reads | Verdict |
|---|---|---|
| `recordReconcileTraceInputs(trace, open, ...)` city_runtime.go:2365 (called :2276) | template/session_name/state/sleep_reason | all on Info — flip to `[]Info` in W-tick |
| `recordReconcileTraceResults(trace, open, postReconcile, ...)` :2472 (called :2334) | same + `postReconcile.FindByID` (:2499) | flip to `[]Info` + `FindInfoByID` in W-tick |
| `cleanupDeadRuntimeSessionCorpses` `session_beads.go:2111` (iterates `sessionBeads.Open()` :2156; called city_runtime.go:1117) | pending_create_claim / session_name / isNamedSessionBead | all on Info — flip to `OpenInfos()` in W-tick |
| `reapRuntimesBoundToClosedBeads` city_runtime.go:1123 | (audit at impl time; sibling of the above) | W-tick if Info-expressible, else W-delete |
| `emitDueComputeFacts(ctx, sessionBeads.Open())` city_runtime.go:2159 (def `usage_compute.go:140`) | `Metadata["state"]` then hands the WHOLE bead to `emitComputeFactForBead` (usage-key reads) | usage lane — **W-delete** (audit `emitComputeFactForBead`'s keys; if any are un-projected, the usage lane gets its own edge read or an Info field-add) |
| `sessionBeadSnapshotFingerprint(snapshot)` city_runtime.go:3279 (called :3167) | hashes ID + Status + Assignee + **ALL metadata keys** of every open bead | **NOT Info-projectable** (Info deliberately drops unknown keys, info_apply_patch.go:236). W-delete: compute the fingerprint at snapshot CONSTRUCTION (the constructor holds raw beads at the edge) and store it as a field, or add an edge `Store.SessionSetFingerprint()`. Not W-tick scope |

### 1.5 The work/session `orderedBeads` split — VERDICT

**The R6 finding's §5c nuance ("orderedBeads is used for BOTH session AND work-class
scans") is STALE.** `orderedBeads` is 100% session-class. Work beads enter through the
separate `assignedWorkBeads []beads.Bead` parameter and stay raw (ClassWork) — the
parameter split already landed in WI-5 W3/W4 (`computeNamedSessionProgressSignatures
(orderedInfos, assignedWorkBeads)` :1325; `sessionHasOpenAssignedWorkForConfigInfo`
:3416 takes Info for the session side, bead-shaped work probes inside). What remains
raw on `orderedBeads` is exactly §1.4 rows (c) + (m)(n)(o) via `beadByID` — session
beads whose *whole-bead* consumers are all Info-expressible after one field-add. **No
`orderedBeads` use is a work-class scan; nothing session-shaped needs to stay raw.**

### 1.6 Corrections to /tmp/r6_finding.md and remainder-design §5c

1. Line anchors: the tick edges are **:582/:1187/:1272** (finding says :556/:582 +
   :1187/:1208 + ":1342/:1419"; the last two are stale — :1342 is now the Phase-1
   header, :1419 is loop-preamble commentary).
2. "orderedBeads serves BOTH session AND work scans" — false at HEAD (§1.5).
3. The finding's class-(a) list omits four `Open()` readers that also gate the raw
   half: `city_runtime.go:2159` (emitDueComputeFacts), `city_runtime.go:3283`
   (sessionBeadSnapshotFingerprint — the only genuinely non-Info-projectable read),
   `session_beads.go:2156` (cleanupDeadRuntimeSessionCorpses), `session_beads.go:57`
   (snapshotOrLoadSessionBeads — class (c)). Plus the two trace helpers consuming the
   main tick's `open` slice (§1.4).
4. "session_bead_snapshot.go (3)" is 1 call (:111) + 2 comment needle-hits (:34,
   :298) — the census needle counts comments; zeroing requires rewording them.
5. The finding is RIGHT that the heal/dedup in-place mutation blocks a naive Info
   feed, but the dependency is narrower than implied: the heal mirror exists *only*
   so the later :1272 projection is coherent. Re-ordering (fold-then-build, §2.3)
   dissolves it; no store semantics change.
6. `reapStaleSessionBeads` is already Info-fed (`loadOpenSessionInfos`,
   session_beads.go:2021) — the finding's class-(c) framing of "loadSessionBeads
   callers 1-3" is correct, but reap is not among them anymore.
7. remainder-design §5c's "Option 1 ... `ListAllForReconcile(opts) []Info`" — the
   return type must NOT be bare `[]Info`: Phase 0.5 needs the 9-key circuit cluster
   that Info deliberately does not carry (§1.3). §2.1 fixes the shape.
8. remainder-design §1's claim that heal "does NOT mirror onto the raw *session bead
   (WI-6 R3 dropped the raw-bead mirror)" applies to `healStateWithRollbackInfo` —
   correct — but `healExpiredTimers` (Phase 0) still mirrors; they are different
   heals. The R6 finding gets this right.

---

## 2. The W-tick design

### 2.1 The edge method and row type (`internal/session/list_all.go`)

```go
// ReconcileSession is one row of the reconciler tick feed: the session's
// domain projection paired with its persisted circuit-breaker cluster.
// The pair exists because the breaker cluster is deliberately NOT on Info
// (separate concern); the reconciler is the one consumer that needs both,
// read once per tick from the same bead.
type ReconcileSession struct {
    Info    Info
    Circuit CircuitState
}

// ListAllForReconcile returns every session bead projected to a
// ReconcileSession, using the identical type+label union, dedupe,
// IsSessionBeadOrRepairable filter, global re-sort, post-union Limit, and
// PartialResultError fold-through as ListAllSessionBeads / ListAll.
func (s *Store) ListAllForReconcile(opts ListAllOptions) ([]ReconcileSession, error)
```

- Implementation: wraps the existing shared body `listAllBeads(opts)` (:213) — the
  exact pattern `ListAll` (:178) and `ListAllWithResponses` (:194) already use — and
  projects each surviving row via `InfoFromPersistedBead(b)` +
  `CircuitStateFromMetadata(b.Metadata)`. Both projections are pure and in-package;
  no bead escapes.
- **Why a row pair and not `[]Info`:** the three candidate alternatives fail —
  per-identity `Store.CircuitState(id)` Gets (circuit_state.go:85) violate the
  pinned **0-Get** tick budget (§5.1); adding the 9 circuit keys to Info reverses the
  settled separation (:1224-1226); threading a parallel `map[id]CircuitState` breaks
  the row-lockstep discipline dedup needs (retired rows carry their circuit with
  them). The pair mirrors the `ListedSession{Info, Response}` precedent exactly.
- **Oracle (commit A):** `TestListAllForReconcileMatchesListAllSessionBeads` — same
  row set/order/error semantics as `ListAllSessionBeads` (both legs, dedupe, filter,
  sort, limit, partial fold-through), plus per-row `Info == InfoFromPersistedBead(b)`
  and `Circuit == CircuitStateFromMetadata(b.Metadata)`, over a corpus including a
  label-lost type-only bead, a label-only repairable bead, closed beads, and a
  populated 9-key circuit cluster.
- **Honest consumer note:** the `Store` method's first *production* caller is the
  W-delete flip of `loadSessionBeadSnapshot` (§4.3). In W-tick it ships oracle-pinned
  with the ROW TYPE consumed immediately by the snapshot (§2.2) — the same commit-A
  "twin lands ahead of its commit-B reader" discipline every prior wave used. Do not
  fake a consumer.

### 2.2 The snapshot carries rows (transitional, `cmd/gc/session_bead_snapshot.go`)

- `newSessionBeadSnapshot(beadsIn)` additionally builds `openCircuits []session.CircuitState`
  in lockstep with `open`/`openInfos` (one extra pure projection per bead at
  construction; `CircuitStateFromMetadata` takes a map and is NOT a policed census
  needle — verified against `typedclass_edge_guard_test.go`'s needle list).
- New reader `OpenForReconcile() []session.ReconcileSession` — assembled copy of
  `openInfos[i]` + `openCircuits[i]` under RLock, order-identical to `Open()`.
- New constructor `newSessionBeadSnapshotFromReconcileRows(rows)` — extends
  `newSessionBeadSnapshotFromInfos` (:185, already exists with the full index-map
  logic) to retain circuits. Raw `open` half stays nil, exactly like FromInfos.
- New fold hook `ApplyOpenInfoPatch(id string, patch session.MetadataPatch)` —
  mutates the matching `openInfos[i]` (and row) under Lock via `Info.ApplyPatch`.
  Sole W-tick caller: the stranded-throttle (§2.5(n)). This replaces the accidental
  shared-map propagation (§1.1) with an explicit, documented carrier.
- `add(bead)` unchanged in W-tick (pool still inserts raw beads — class (b)); it
  additionally appends the projected row so `OpenForReconcile` stays complete when
  pool creation runs mid-cycle. W-pool retypes `add` itself.

Census effect: `session_bead_snapshot.go` InfoFromPersistedBead stays 3 (the :111
call survives until W-delete). `session_reconciler.go` goes **3 → 0** (§2.3-2.6).

### 2.3 The reshaped tick entry (Phase 0 → snapshot build)

`reconcileSessionBeadsTracedWithNamedDemand` (+ its three wrappers :974/:1015/:1058
and `reconcileSessionBeads`) changes `sessions []beads.Bead` →
`rows []sessionpkg.ReconcileSession`. New Phase-0 order — **fold first, build after,
project never**:

```
// Phase 0a: heal expired timers — fold, no mirror
for i := range rows {
    rows[i].Info = healExpiredTimersInfo(rows[i].Info, sessFront, clk)
}
// Phase 0b: duplicate-retire — Info-twin, returns the folded row set
rows = retireDuplicateConfiguredNamedSessionRows(store, rigStores, sp, cfg, cityName, rows, clk.Now().UTC(), stderr)
// Topo order over rows (reads .Info.Template — verbatim mirror, byte-identical)
orderedRows := topoOrderRows(rows, deps)
// Snapshot build — NO codec call
orderedIDs := make([]string, len(orderedRows))
orderedInfos := make([]sessionpkg.Info, len(orderedRows))
infoByID := make(map[string]sessionpkg.Info, len(orderedRows))
for i := range orderedRows {
    orderedIDs[i] = orderedRows[i].Info.ID
    orderedInfos[i] = orderedRows[i].Info
    infoByID[orderedRows[i].Info.ID] = orderedRows[i].Info
}
```

- `healExpiredTimersInfo(info, sessFront, clk) sessionpkg.Info`: same two-branch body
  as :434-459; on each successful `ApplyPatch` fold via `info.ApplyPatch(batch)`
  (the hold-clear fold before the quarantine check ALREADY exists in the raw body,
  :445 — preserve that ordering: hold-clear can blank `sleep_reason` which the
  quarantine patch reads); on persist error return the input segment unchanged
  (today's `err == nil` mirror gate). Raw `healExpiredTimers` is deleted in commit B
  (single caller). This kills the **:1187** codec call.
- Because the fold happens BEFORE the infoByID build, the raw mirror's only purpose
  (coherent later projection) disappears — the **:1272** codec call dies with the
  build loop above.
- `beadByID` is **deleted** (its three consumers take Info, §2.5).
- Phase 0.5 reads `orderedRows[i].Circuit` instead of
  `CircuitStateFromMetadata(orderedBeads[i].Metadata)` (:1304/:1313). Everything else
  in Phase 0.5 already reads `orderedInfos` / writes via `sessFront`.
- Phase-1's `session := &orderedBeads[i]` disappears; the loop iterates
  `orderedIDs`/`infoByID` as the awake scan already does. Every helper in §1.4 rows
  (d)-(l) flips per its verdict column (ID param or Info param); the raw
  `session.Status = "closed"` mirrors (:1575/:1846 and inside
  `finalizeDrainAckStoppedSession` :422/:457) are deleted — their same-tick readers
  are the `infoByID` folds that already exist; the telemetry close-path test re-pins
  against the fold.

**Callers** flip mechanically: `city_runtime.go:2252/:2300` and `cmd_start.go:929/943/961`
pass `sessionBeads.OpenForReconcile()`; the config-change path (`city_runtime.go:2962-2976`)
gets `filterReconcileRowsByName` (sibling of :3117/:3134) and passes rows, and its
`newSessionBeadSnapshot(open)` re-wrap at :2969 becomes
`newSessionBeadSnapshotFromReconcileRows(filteredRows)` — safe because its consumer
`retainScaleCheckPartialPoolDesired` (build_desired_state.go:1797) reads only
`OpenInfos()` (verified). The two trace helpers + `cleanupDeadRuntimeSessionCorpses`
flip to `[]Info`/`OpenInfos()`/`FindInfoByID` (§1.4 caller table).

### 2.4 The dedup Info twin

```go
func retireDuplicateConfiguredNamedSessionRows(
    store beads.Store, rigStores map[string]beads.Store, sp runtime.Provider,
    cfg *config.City, cityName string,
    rows []sessionpkg.ReconcileSession,
    now time.Time, stderr io.Writer,
) []sessionpkg.ReconcileSession
```

Same algorithm as `session_beads.go:482-563`, expressed on rows:

- Grouping predicate: `!row.Info.Closed && isNamedSessionInfo(info) &&
  NamedSessionContinuityEligibleInfo(info) && namedSessionIdentityInfo(info) != "" &&
  spec present`. **New twin required:** `session.NamedSessionContinuityEligibleInfo`
  (raw at named_config.go:268; the Info name is already reserved by the manager.go:217
  comment). Oracle row in `TestSessionClassifierInfoEquivalence`.
- Winner rule twin `namedSessionWinsCanonicalRepairInfo(candidate, incumbent Info,
  canonicalSessionName)`: generation int-compare (`Info.Generation`, verbatim mirror),
  canonical-session-name tiebreak (`SessionNameMetadata`), `CreatedAt`, `ID` — all
  present. Oracle rows: generation pair, one-parses-one-doesn't both directions,
  canonical-name tiebreak, CreatedAt tiebreak, ID tiebreak.
- Loser processing: `stopRuntimeBeforeSessionBeadMutationInfo` (reads
  `SessionNameMetadata` + ID; new trivial twin — raw stays for sync);
  `setMetaBatch(sessionFrontDoor(store), id, RetireNamedSessionPatch(...))` +
  `SetStatusOpen(id)` (writes unchanged, already front-door);
  `reassignWorkAssignedToRetiredSessionInfo(store, rigStores, info, winnerID, stderr)`
  — session side reads `sessionAssignmentIdentifiersInfo` (twin of
  `sessionAssignmentIdentifiers`; the ForConfig variant already has one at
  session_beads.go:677 — add the plain twin beside it), work-store walk stays
  bead-shaped (ClassWork); `reassignStateAssignedToRetiredSessionBead` unchanged
  (IDs only). Fold: `rows[idx].Info = rows[idx].Info.ApplyPatch(batch)`; `Closed`
  unchanged (the raw form re-asserts `Status="open"`); `Circuit` carried untouched.
- The `bySessionName`/`indexBySessionName` parameters are dropped — dead in the
  reconciler caller (verified: built :1197-1207, never read after :1208). The raw
  form keeps them for its sync caller (session_beads.go:1137) until W-delete/W-sync.
- **Raw form + raw classifiers survive W-tick** (sync still calls them). Commit A
  adds the twins + a both-ways characterization oracle (fixture: two eligible
  duplicates + one ineligible + one closed + distinct-session-name loser requiring
  the runtime stop); commit B flips only the reconciler caller.

### 2.5 The three `beadByID` consumers (frees the map)

- **(m) `cycleAliveSessionForFreshReassign`** → takes `info sessionpkg.Info`.
  Reads flip to `info.CurrentlyProcessingBeadID`, `namedSessionIdentityInfo(info)`,
  `freshRestartSessionKeyInfo(tp, info)` (new twin of :621 over
  `Info.SessionIDFlag/ResumeFlag/ResumeCommand/ResumeStyle` — equivalence oracle with
  whitespace-padded fixtures; the raw form's other caller :2228 flips in the same
  commit, then the raw form dies). The raw metadata mirror loop (:120-127 of
  session_bead_cycle.go) is DELETED — it writes the identical key set as the returned
  fold (same `ResetCommittedAtKey` exclusion), and the caller already applies the
  fold to `infoByID` (:3141-3143); nothing else reads the raw bead after the call
  (caller `continue`s).
- **(n) `emitSessionStrandedDiagnostic`** → takes `(info sessionpkg.Info,
  snapshot *sessionBeadSnapshot, ...)` (or a fold callback). Guard reads
  `strings.TrimSpace(info.StrandedEventEmittedAt) != ""`. Ordering contract
  preserved: **fold the marker into `infoByID` AND `snapshot.ApplyOpenInfoPatch`
  BEFORE the durable `SetMarker`**, return the fold regardless of the SetMarker
  result — reproducing today's in-memory-marker-first guarantee including the
  snapshot-reuse carrier that the shared metadata map provided accidentally (§1.4(n)).
  The pin test `TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure`
  must stay green with an added assertion that a REUSED snapshot's
  `OpenForReconcile()` row carries the marker after a failed SetMarker.
  `collectSessionAssignedWork` gains an Info-taking form using
  `sessionAssignmentIdentifiersForConfigInfo` + `reachableStoresForSessionInfo`
  (both exist); the work-bead walk inside stays raw (ClassWork).
- **(o) `pruneAgentHomeWorktreeIfSafe`** → takes `info`. Requires **field-add
  `Info.WorkerDir`** (raw mirror of the canonical `beadmeta.WorkerDirMetadataKey`) +
  `session.WorkerDirFromInfo(info)` implementing the canonical→legacy(`WorkDir`)
  fallback of contract/metadata.go:52 — a one-line codec add (info_store.go +
  info_apply_patch.go + reprojection oracle), the exact BuiltinAncestor shape
  remainder-design §4 sanctioned. `lookupRigRootForSession` twin reads
  `info.Template`.

### 2.6 The finalize pass (:556) and the :582 verdict

`finalizeDrainAckStopPendingSessions` changes `sessions []beads.Bead` →
`infos []sessionpkg.Info`; caller `city_runtime.go:1159` passes
`sessionBeads.OpenInfos()` (exists). The loop drops the :582 projection and the
`session := &sessions[i]` pointer; `finalizeDrainAckStoppedSession` and
`reconcileDrainAckStopPending` drop their `*beads.Bead` parameter (uses were ID + the
vestigial Status mirror, §1.4(d)); `drainAckFinalizeResult` (:312) is untouched.
**No new Get**: the snapshot Info is sufficient for every decision read
(`isDrainAckStopPendingInfo` already takes Info), and the one genuine post-mutation
re-read — the NDI witness — is ALREADY `sessFront.Get` (:439), documented
non-fast-path, budget-exempt. Prefer the snapshot Info everywhere else; do not
convert the witness Get to a fold (a status-close is not foldable — spec §4
exception, and the comment at :441-445 already says so).

### 2.7 What W-tick does NOT touch

- `assignedWorkBeads` and every work-bead walk (ClassWork — bead IS the domain object).
- The sync path (`syncSessionBeadsWithSnapshotAndRigStores`, session_beads.go:986) —
  still produces the snapshot via `newSessionBeadSnapshot(openBeads)` (:1768); its
  OUTPUT feeds the tick as rows via `OpenForReconcile()`. Raw internals are W-delete/
  W-sync scope.
- The pool path (class (b)) — still reads `Open()` (build_desired_state.go:3607/
  :3836/:4474) and `add(bead)`s (session_name_lookup.go:301,
  session_template_start.go:110). W-pool.
- `loadSessionBeadSnapshot`'s raw union load (:90) and the raw snapshot half. W-delete.
- The circuit breaker's own logic and its `persistSessionCircuitBreakerMetadata`
  writes (already front-door).

---

## 3. Wave sequencing (two commits per wave: A additive+oracles, B migrate+delete+census)

### W-tick — the tick-feed refactor. **Risk: HIGH** (hardest wave in the migration)

Files: `internal/session/list_all.go` (+`manager.go`/`info_store.go`/
`info_apply_patch.go` for the WorkerDir field-add), `cmd/gc/session_bead_snapshot.go`,
`session_reconciler.go`, `session_reconcile.go`, `session_beads.go` (dedup twins),
`session_bead_cycle.go`, `session_worktree_prune.go`, `city_runtime.go`,
`cmd_start.go`, `named_sessions.go`. >5 files — justified the same way R3/R4 were:
one coupled cluster; commit A is additive across files, commit B flips the cluster
atomically (a partial flip strands the tick half-raw/half-rows).

- **Commit A (additive, tree green):** `ReconcileSession` + `ListAllForReconcile` +
  equivalence oracle (§2.1); `Info.WorkerDir` + `WorkerDirFromInfo` + reprojection
  oracle (§2.5o); snapshot `openCircuits`/`OpenForReconcile`/
  `FromReconcileRows`/`ApplyOpenInfoPatch` (§2.2); twins with oracles:
  `healExpiredTimersInfo`, `retireDuplicateConfiguredNamedSessionRows` (+
  `NamedSessionContinuityEligibleInfo`, `namedSessionWinsCanonicalRepairInfo`,
  `stopRuntimeBeforeSessionBeadMutationInfo`, `sessionAssignmentIdentifiersInfo`,
  `reassignWorkAssignedToRetiredSessionInfo`), `freshRestartSessionKeyInfo`,
  `topoOrderRows`, Info forms of §1.4 (e)(g)(h)(i)(j)(l)(m)(n)(o) helpers,
  `filterReconcileRowsByName`. New characterization pins (§5.2).
- **Commit B (flip + delete + census):** tick entry + finalize signatures onto
  rows/Infos; Phase-0 fold order (§2.3); Phase 0.5 onto `.Circuit`; forward pass
  drops every raw `*session` use; `beadByID` deleted; callers + trace helpers +
  `cleanupDeadRuntimeSessionCorpses` flipped; DELETE: raw `healExpiredTimers`,
  raw `freshRestartSessionKey`, the raw-taking forms of (e)(g)(h)(i)(j)(l)(m)(n)(o)
  (six-way grep each), the `session.Status="closed"` mirrors, the raw metadata mirror
  in `cycleAliveSessionForFreshReassign`. Census: `session_reconciler.go`
  `InfoFromPersistedBead` **3 → 0** — ratchet down + paste literal. Tick-budget test
  must still read 0.

### W-pool — pool selection/creation/reuse typing (class (b)). **Risk: MEDIUM-HIGH**

Scope (verified by the pool audit): the raw path is
`findOpenSessionBeadByID (bds:3603, reads only ID over Open())` →
`selectOrPlanPoolSessionBead (:3663, returns beads.Bead|plan)` →
{`reusablePoolSessionBeads` :3831 / `reusableDependencyPoolSessionBeads` :4469 —
predicates all have Info twins already (`isFailedCreateSessionInfo` :4037,
`isDrainedSessionInfo`, `isManualSessionInfo(ForAgent)`, `isNamedSessionInfo`,
`sessionBeadHasAssignedWorkInfo` :4070 — currently oracle-only per the :4063
sanction, becomes production) | `normalizeNonExpandingPoolSessionBeadForSelection`
:3901 (store `Update` + local re-merge → returns Info + fold) |
`createPoolSessionBeadWithGuardedAlias` :3965 → `createPoolSessionBeadWithAlias`
(session_name_lookup.go:217)} → `item.sessionBead` → the two projections
`build_desired_state.go:2384/:2635`.

- **Commit A:** typed create front door — `session.Store.CreateSessionInfo(spec
  CreateSpec) (Info, error)` (or `CreateSession` returns `(string, Info, error)`
  sibling): performs the create, returns the projected Info of the CREATED bead so
  `createPoolSessionBeadWithAlias` drops its post-create `store.Get(beadID)` (:281)
  + hand-mirrored `session_name` (:298) in favor of an Info fold. Today the
  front door has no create returning Info (only `CreateSession → string`,
  create.go:41) — this is the gap. Info-twin the selection helpers; snapshot
  `add(row)` (Info-built rows; circuit zero-valued at creation — correct, a fresh
  bead has no circuit metadata). `reopenClosedConfiguredNamedSessionBead`
  (session_beads.go:370) returns Info alongside/instead of the bead for the
  `session_template_start.go:110` add.
- **Commit B:** flip `selectOrPlanPoolSessionBead`+cluster to return
  `(sessionpkg.Info, int, *plan, error)`; `realizePoolDesiredSessions` Phase A/B/C
  and `ensureDependencyOnlyTemplate` carry Info; **delete** the raw predicates + the
  :4063 sanction comment; census `build_desired_state.go` InfoFromPersistedBead
  **2 → 0**. Pins: pool-slot selection precedence characterization (resume-tier
  preferred, canonical singleton, general reuse order by CreatedAt/ID), the
  normalize-returns-authoritative-value contract (:2926-2929 comment), and the
  parallel-create `add()` race test (#2319) rerun against `add(row)`.
- Risk: creation/reuse is stateful; the normalize lane's local re-merge must become
  an exact Info fold (`ApplyPatch` of the same Update batch). The two-phase
  plan/execute split is preserved as-is.

### W-delete — load-edge flip + raw-half deletion + periphery zeros. **Risk: MEDIUM (mechanical, falls out)**

Gate: W-tick + W-pool merged. Then the raw half has NO remaining reader except the
sync path's constructor rebuild and the two W-delete-scoped `Open()` readers (§1.4
caller table).

- `loadSessionBeadSnapshot` → `sessionFrontDoor(store).ListAllForReconcile(...)` +
  `newSessionBeadSnapshotFromReconcileRows` — **ListAllForReconcile's first
  production consumer**; `session_bead_snapshot.go` census: `InfoFromPersistedBead`
  3→0 (call deleted + 2 comments reworded), `ListAllSessionBeads` 1→0.
- Sync tail: `return openIndex, newSessionBeadSnapshot(openBeads)` (:1768) → re-list
  via `ListAllForReconcile` at the tail. Honest behavior delta, flagged: the rebuilt
  snapshot today reflects sync's local slice; a fresh union list reflects the store
  (every sync mutation is persisted before it is locally mirrored — verified across
  the 7 mutation sites — so the delta is only concurrent-writer visibility, which NDI
  convergence already tolerates). Cost: one extra type+label union per sync call
  (2 indexed Lists, no Gets — sync is not the reconciler fast path and already
  issues multiple internal lists). Pin: characterization that the returned snapshot
  equals a fresh load post-sync. If the perf check fails under Dolt, fallback: sync
  maintains rows alongside `openBeads` via folds (bigger edit, same result).
- `sessionBeadSnapshotFingerprint` → fingerprint computed at snapshot construction
  from the raw rows (edge-side) and stored as a field (§1.4). `emitDueComputeFacts`
  → audit `emitComputeFactForBead`'s metadata keys; migrate to Info or give the
  usage lane its own edge read.
- Pure-read accessor migrations (deliberately deferred to here per the R6 finding so
  the raw/Info equivalence pins stay load-bearing until the raw half dies):
  `city_runtime.go:2499` `FindByID`→`FindInfoByID` (lands in W-tick with
  recordReconcileTraceResults — note the overlap; whichever wave touches it first
  takes it), `providers.go:539` `FindSessionNameByNamedIdentity`→
  `FindInfoByNamedIdentity`, `providers.go:232`/`cmd_citystatus.go:393/:449`
  `newSessionBeadSnapshot(...)`→`FromReconcileRows`/`FromInfos`.
- **DELETE the raw half:** `open` slice, `Open()`, `FindByID`, `findByIDLocked`,
  `FindSessionBeadByTemplate`, `FindSessionBeadByNamedIdentity`,
  `FindSessionNameByNamedIdentity`, `add(bead)`, `replaceOpenLocked`,
  `newSessionBeadSnapshot(beads)`, raw `stampedPoolQualifiedIdentity`, the WI-6
  checklist comment :273-284. `TestSessionBeadSnapshotConstructorInfoEquivalence`
  (session_bead_snapshot_test.go:178) retires WITH its subject; its 12-bead
  index-precedence corpus is re-pointed at `FromReconcileRows` as the permanent
  constructor characterization (index-precedence bugs strand named sessions — the
  corpus survives, only the reference constructor changes).
- Periphery zeros in the same wave:
  - `cmd_stop.go:376` → new edge lister (the site's own comment specifies it: "a
    label-only, closed-excluded, unfiltered Info lister") — e.g.
    `Store.ListLabeledSessionInfosUnfiltered()` with a documented
    no-IsSessionBeadOrRepairable contract (it sweeps possibly-damaged beads).
    1→0.
  - `session_hash.go:21` → the sole raw caller is `queueAliasChangeDriftRebaseline`
    (session_beads.go:1781, sync alias lane). Flip it to `sessFront.Get(b.ID)` →
    `sessionCoreConfigForHashInfo` — a front-door Get on a rare lane (alias CHANGE
    only), outside the pinned tick fast path; bridge the Get contract (§5.4). Delete
    the raw `sessionCoreConfigForHash`. 1→0.
  - `session_logs_resolve.go:121/:127` → change the internal/session signature
    `ResolveCodexTranscriptBySessionOrder([]beads.Bead)` (transcript_lookup.go:25)
    to take `[]Info`: its per-bead reads are ID, `work_dir` (=`Info.WorkDir`),
    anchor keys `last_woke_at`/`pending_create_started_at`/`creation_complete_at`
    (all mirrored) + `awake_started_at` (**field-add `Info.AwakeStartedAt`**, raw
    mirror, same BuiltinAncestor shape), `CreatedAt`, `session_name`. Then
    `sessionLogFallbackSiblings` returns `[]Info` and both projections die. 2→0.
  - `doctor_session_model.go:149` → doctor issues its own two raw `store.List` legs
    (Type=session, Label=gc:session, IncludeClosed:true) + local ID-dedupe inline
    (~15 lines). Doctor's §5 exemption covers HOLDING raw beads; what it must not do
    is call the helper the census polices. `ListAllSessionBeads` 1→0 there.
- Census after W-delete: `InfoFromPersistedBead` interior = `cmd_session.go:1` +
  `internal/api/session_resolution.go:1` (both W-flip, next); `ListAllSessionBeads`
  interior = `session_beads.go:1` (see honest verdict §4).

### W-flip — the front-door flip (§5b of remainder-design). **Risk: MEDIUM**

As designed in remainder-design §5b (unchanged by this doc): `class_store.go`
accessors + `api.State` typed accessors, one motion per class, #4017 seam preserved
(wrap the exact `resolve*Store` outputs). This doc adds two items that belong here
by their own in-code deferral comments:

- `cmd_session.go:2296` (`cmdSessionKill`'s raw `sessStore.Get` + codec) — the census
  comment explicitly defers it to "the WI-7 front-door migration (§5b/§6)". Flip to
  the session front-door Get → Info; bridge contract (§5.4). 1→0.
- `internal/api/session_resolution.go:171` (raw retire lane over
  `ExactMetadataSessionCandidates`) — give `ExactMetadataSessionCandidates` an
  Info-returning sibling in internal/session (the lane needs only
  `SessionNameMetadata` per row) and flip the loop; alternatively fold into the
  §5d named_config needle expansion if that lands first. 1→0.

After W-flip: `InfoFromPersistedBead` interior census = **0 across all four scan
dirs**.

### W-unexport — codec unexport + guard→zero-pin (§5e). **Risk: LOW**

- **Unexports that this endgame earns:** `InfoFromPersistedBead` →
  `infoFromPersistedBead` (TRUE interior zero after W-flip; in-package uses —
  list_all.go, info_store.go, wait_store.go:557, resolve.go:82, manager.go:1838 —
  are unaffected; internal/orders:420 is a comment). `PollerKeyFromBead`,
  `PersistedResponseFromBead` — already 0-interior tripwires, unexport now-ish
  (PersistedResponseFromBead is used in-package by list_all.go:203/info_store.go:226
  — fine). The other all-zero tripwires per remainder-design §5a(a).
- **Stays exported (honest):** `ListAllSessionBeads` — blocked twice: (i)
  `session_beads.go:40` (`loadSessionBeads`) feeds the still-raw sync internals
  (`findOpenSessionBeadBySessionName` :84, `loadVisibleBySessionName` :1103,
  `snapshotOrLoadSessionBeads` :55, plus `doctor_work_option_metadata.go:109`,
  `session_lifecycle_parallel.go:2976`); (ii) `internal/mail/beadmail/beadmail.go:108/:120`
  — outside the census scan but a same-module compile dependency that unexport
  would break. Verdict: census row pinned at `session_beads.go: 1` with a header
  rationale ("raw sync/repair-lane feed; dies with W-sync"), and beadmail's
  migration (onto `ListAll(IncludeClosed:true)`-shaped reads or the mailbox front
  doors) is a W-unexport precondition to schedule separately. Full sync typing
  (**W-sync**) is real work (7 in-place `openBeads[idx]` mutation sites, ~15 raw
  helpers) and is explicitly OUT of this endgame's budget — do not pretend W-delete
  covers it.
- Orders codecs (`RunFromTrackingBead`, `MaxSeqFromLabels`) — unchanged verdict:
  gated on the WI-3 two-class graph wiring, not this endgame.
- Guard conversion per §5e: zeroed needles get deleted rows or hard `== 0` pins;
  `CircuitStateFromMetadata` is NOT added as a needle (map-taking, edge-owned, its
  interior callers die in W-tick anyway).

---

## 4. The honest residual verdict (InfoFromPersistedBead and friends)

| Site | Today | Wave | How |
|---|---|---|---|
| session_reconciler.go :582/:1187/:1272 | 3 | **W-tick → 0** | §2.3/§2.6 |
| session_bead_snapshot.go :111 (+2 comments) | 3 | **W-delete → 0** | load-edge flip §4.3 |
| build_desired_state.go :2384/:2635 | 2 | **W-pool → 0** | typed select/create returns Info |
| session_hash.go :21 | 1 | **W-delete → 0** | alias-rebaseline lane takes front-door Get |
| session_logs_resolve.go :121/:127 | 2 | **W-delete → 0** | `ResolveCodexTranscriptBySessionOrder([]Info)` + `Info.AwakeStartedAt` field-add |
| cmd_stop.go :376 | 1 | **W-delete → 0** | unfiltered label-only Info lister |
| cmd_session.go :2296 | 1 | **W-flip → 0** | front-door Get flip (its own census comment says so) |
| internal/api/session_resolution.go :171 | 1 | **W-flip → 0** | Info-returning ExactMetadataSessionCandidates sibling |

**`InfoFromPersistedBead` reaches a TRUE interior zero and unexports** — but only if
ALL of W-tick, W-pool, W-delete AND the two W-flip sites land. If W-flip's two sites
slip, the honest fallback is a pinned census at 2 (cmd_session + session_resolution)
and no unexport. There is no scenario where the reconciler sites survive: W-tick
alone zeroes them.

**`ListAllSessionBeads` does NOT unexport in this endgame** (sync raw feed +
beadmail; §3 W-unexport). **`PollerKeyFromBead`/`PersistedResponseFromBead`/the §5a(a)
tripwires unexport freely.** The `session_logs_resolve.go`/`session_resolution`
"sanction" options from remainder-design §5c are NOT needed — both migrate cleanly
(one field-add + one sibling function); prefer migration over sanction in both cases.

---

## 5. Risk register + mandatory pins (W-tick)

### 5.1 The tick-budget guard (non-negotiable)
`TestReconcileSessionBeadsFastPathGetBudget` (session_reconciler_tick_budget_test.go:50)
pins **`wantGets = 0`** on a healthy tick via a Get-counting store wrapper. W-tick
adds ZERO Gets: `ListAllForReconcile` is a List (not counted, and not even called by
the tick — the feed is snapshot rows); heal/dedup/stranded/cycle folds are
ApplyPatch + local fold; the only Gets anywhere near the tick remain the pre-existing
sanctioned ones (NDI witness :439; R4's start-execution freshness re-reads). The test
must pass UNMODIFIED against the row-fed signature (update only the call-site shape
in the test harness, never the assertion).

### 5.2 Characterization pins (commit A of W-tick, all must be green before commit B)
1. **Phase-0 heal still heals AND is snapshot-visible:**
   `TestReconcileSessionBeads_Phase0HealVisibleOnSnapshot` — expired hold + expired
   quarantine fixtures; assert the store received the clear patches AND
   `infoByID`-derived decisions in the same tick observe the healed values (today
   that coherence came from the mirror+re-project; after, from fold-then-build).
   Include the ordering fixture: expired hold whose clear blanks `sleep_reason`,
   then expired quarantine reading the post-hold `sleep_reason`.
2. **Dedup still dedups:** both-ways oracle raw-vs-rows on the retire fixture corpus
   (§2.4) + a tick-level test: two duplicate configured-named beads in, loser
   retired in store, winner's work reassigned, loser's row folded (retired identity
   visible on `infoByID`), topo/awake order unchanged for the survivors.
3. **Finalize boundary:** existing
   `TestReconcileSessionBeads_MinFloorCountReflectsMidTickCloseDrainAck`,
   `..._HealStateReflectedOnSnapshot`, `..._ZombieTerminalErrorReflectedOnSnapshot`
   stay green; the drain-ack telemetry close-path test re-pins on the MarkClosed
   fold instead of the raw `Status` field.
4. **Stranded throttle:** the SetMetadataFailure pin + the new snapshot-reuse
   assertion (§2.5n).
5. **Circuit equivalence:** Phase 0.5 restore/reset over `rows[i].Circuit` produces
   identical breaker decisions to `CircuitStateFromMetadata` on a populated 9-key
   fixture (open breaker + reset-generation + progress-signature rows).
6. **Row/order equivalence:** `TestListAllForReconcileMatchesListAllSessionBeads`
   (§2.1) + `OpenForReconcile()[i].Info == OpenInfos()[i]` lockstep pin.
7. **Twin oracles:** `freshRestartSessionKeyInfo`, `namedSessionWinsCanonicalRepairInfo`,
   `NamedSessionContinuityEligibleInfo`, `WorkerDirFromInfo` (canonical-key,
   legacy-fallback, whitespace fixtures) — rows in
   `TestSessionClassifierInfoEquivalence` / the reprojection oracle.
8. **Existing order pins stay green:** the R5-lite strict-ordering pin, wake-fairness
   (`TestWakeFairnessInfoTwinCharacterization`), ComputeAwakeSet last-write-wins
   ordering (rows order == old orderedBeads order — topoOrderRows oracle vs raw
   topoOrder on a mixed-template fixture).
9. **R6 constructor-equivalence pin** (session_bead_snapshot_test.go:178) stays
   green through W-tick and W-pool untouched — the raw constructor is still live;
   it retires only in W-delete, superseded by the re-pointed corpus (§3 W-delete).

### 5.3 Why W-tick is the highest-risk wave since R3 — and why it is smaller than it looks
It reshapes the tick's input type, deletes the working set's raw half from the tick,
and re-orders Phase 0. But the forward pass, awake scan, and drain advance are
ALREADY pure infoByID folds (WI-5/WI-6 R3-R5 did that work); W-tick extends the
established fold discipline to Phase 0 and the last three whole-bead consumers, and
swaps parameter types. The genuinely novel logic is: fold-then-build ordering (pin
5.2.1), the dedup twin (pin 5.2.2), and the stranded-throttle carrier (pin 5.2.4).
Everything else is signature mechanics over reads verified Info-present in §1.4.
The W6-drift failure mode (dropping a mirror before its last reader) is guarded by
the §1.4 inventory being EXHAUSTIVE — commit B's review checklist is that table.

### 5.4 Front-door-Get contract (for every Get this endgame moves)
`session.Store.Get`/`GetPersistedResponse` return `ErrSessionNotFound`, wrap with
`"loading session %q"`, and reject non-`IsSessionBeadOrRepairable` beads
(session_get_read.go bridge). Moved-Get sites in these waves: W-delete's
alias-rebaseline (`queueAliasChangeDriftRebaseline` — bridge + keep the lane's
best-effort stderr semantics), W-flip's `cmdSessionKill` (verify kill-path
error-text/not-found behavior on a damaged bead), W-pool's `CreateSessionInfo`
(replaces a raw post-create `store.Get` — the projection happens on the just-created
bead; define the error contract as create-succeeded-projection-failed = return the
id + error, never a silent half-create). W-tick moves NO Gets.

### 5.5 Perf note
W-tick adds one `CircuitState` projection per open bead per snapshot construction
(pure string copies) and removes one full per-tick re-projection loop (:1270-1277)
plus the per-bead heal projection (:1187) — net fewer allocations per tick. W-delete's
sync-tail re-list is the only added I/O anywhere in the plan (§3 W-delete, flagged
with its fallback).
