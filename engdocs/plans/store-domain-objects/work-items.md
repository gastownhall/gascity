# Work items: stores return domain objects

Ordered strangler migration. Each item: **Fable design → Opus impl (TDD) → Fable
red-team before commit.** Acceptance = the item's checks pass AND the CI census
ratchet (WI-0) records progress (never regresses). See `spec.md` for the contract.

Status legend: `[ ]` todo · `[~]` in progress · `[x]` done.

Open PRs from the earlier leak-cleanup fold into this plan (do NOT double-build):
- **Merge as-is / keepers:** O1 #4055 (dead wake helpers — deletes ~500 LOC of the
  session surface first), O9 #4048 (constant), O3 #4049 (graph-class field codec,
  out-of-metric), O5 #4050 (orders opener), O7 #4051 (session codec vocabulary).
- **Fold as steps:** O2 #4057, O4 #4058, O6 #4056.
- **Rework:** O8 #4052 (keep `SweepStale`; replace the `DecodeShadow(b).ID` reads).

---

## WI-0 — CI census ratchet (enforcement baseline) `[x]`
`cmd/gc/typedclass_edge_guard_test.go`, Tier-1 only (§6.1). Checked-in
`map[file]count` of typed-class codec/raw-export needles across the interior dirs,
excluding the edge set; increase or new file fails; decrease fails until ratcheted.
Header documents the §5 exemption census.
**Acceptance:** test passes on current tree (pins today's baseline); a synthetic
added `InfoFromPersistedBead(` in an interior file makes it fail.

## WI-1 — Nudges class `[x]`  (smallest blast radius; pilot)
<!-- COMPLETE: body landed e0eec587f (merge fa1f95edc); wait-residual closed with WI-4 A2 (c08eb2505); closeout (guard un-exclusion + dead-alias deletion + this marker) dfe8a0878. Marker was stale. -->
Rework O8. Add `NudgeShadow.Open` (bead-authoritative) + `Store.StaleShadowsBefore(before, limit, liveExcludeIDs) -> []NudgeShadow` (carries the live-flock-queue exclusion; the count/dry-run twin lives at the cmd/gc caller `countStaleNudgeMail` — the shared close budget spans two classes so it cannot live inside a single-class store method). Keep `Store.SweepStale`. Migrate `nudge_mail_sweep.go` sweep+count loops onto the typed reads; delete `FindBead`/`FindBeadIncludingTerminal`/`DecodeShadow`/`StaleCandidatesBefore` (zero non-test callers after rework). `nudge_beads.go` survives as needle-free wiring (store-open seam + flock-callable write adapters), un-excluded from the census guard in the closeout. `Find`/`FindIncludingTerminal(nudgeID)` stay as this class's `Get(handle)` (handle = durable nudge ID). Preserve nil-receiver no-op + flock-transaction callability.
**Residual (closed in WI-4 A2, c08eb2505):** `blockedQueuedNudgeReason`/`nextWaitDeliveryAttempt` now read the session-class wait via the typed `session.Store` front door (`GetWait`, `WaitInfo`).
**Acceptance:** nudges census → 0 for its needles (minus the documented session residual); typed reads pin `NudgeShadow` fields; byte-identical terminal writes.

## WI-2 — Messaging class `[x]`
Add whole-operation retention methods to `beadmail` returning **counts**:
`SweepReadMessagesBefore(cutoff, limit, closeReason)`, `CountReadMessagesBefore(cutoff, limit)`, `PurgeReadMessageWisps(cutoff)`. Export an `IsMessageBead` predicate (or use `coordclass.Classify`). Migrate `nudge_mail_sweep.go` mail phases + split the mail arm OUT of `wisp_gc.go`'s graph-owned `purgeExpiredBeadRoots` onto `PurgeReadMessageWisps`; swap `order_dispatch.go:1680` inline `Type=="message"` for the predicate. Delete `beadmail.ReadMessagesBefore`/`ReadMessageWispEntries`.
**Residual (owned by WI-4/6):** mail identity/recipient resolution over raw session beads in `cmd_mail.go`/`handler_mail.go` converges on the typed session mailbox surface (O7 vocabulary).
**Acceptance:** messaging retention loops live inside `beadmail`; the two raw exports gone; graph GC undisturbed (mail arm already runs against `mailStore` separately).

## WI-3 — Orders class `[x]`
Land O5 first. Then on `orders.Store`: `Get(handle) -> OrderRun`; `RunDetail(handle) -> {OrderRun, convergence.GateOutput}`; bulk **Live**-tier `RecentRunsAll(limit)`/`OpenRuns()` (fold the perf-critical tracking index onto `OrderRun`, NOT per-handle Gets); sweep reads `StaleOpenRuns`/`OrphanedOpenRuns`/`ClosedRunsForRetention` + `CloseRuns(ids, reason)` batch-with-verify + `DeleteRun`; `MarkFailed(runID, outcome, cursor)` (one Update, byte-identical to `markTrackingFailure`). `OrderRun` grows `UpdatedAt` + legacy `order:<title>` name fallback.
**MANDATORY (critique correction 1):** `HasOpenWork(scoped)`, `LastRun`, `Cursor` are **mixed orders+graph reads** (event seq labels are stamped on graph wisp roots) — implement them as two-class edge reads taking `(OrdersStore, GraphStore)`; the union List + wisp-descendant walk stay inside the edge; only typed verdicts escape. **Do NOT** "rebase onto `beads.OrdersStore`" as a single class. Characterization test: an order whose only evidence is a wisp/molecule root (no tracking bead) still reports correct last-run + cursor against two DISTINCT stores.
Migrate `order_dispatch.go` index/sweeps/close-verify, `cmd_order.go` cursor reads, `internal/api` orders read path; rebase `LastRunFuncForStore`/`CursorFuncForStore` as two-class; delete `unwrapOrdersStores`.
**Acceptance:** orders census → 0; every new read declares its tier (Live pinned by a bypass test); the two-class characterization test passes.

## WI-4 — Sessions / Waits (greenfield; unblocks WI-1 & WI-2 residuals) `[x]`
Land O6. Promote to `session.Store` handle-taking methods: `GetWait(handle) -> WaitInfo`, `WaitsForSession(sessionID)`, `ListWaits(state, session)`, `CreateWait(spec) -> WaitInfo`; move `CancelWaits`/`ReassignWaits`/`WakeSession(sessionID)` from package funcs taking `(beads.Store, bead)` to Store methods taking **handles** (`WakeSession` becomes a store-internal transaction: lifecycle-conflict check + wait cancel + metadata batch, replacing four callers that fetch the raw bead first). Move O6's residual write codecs (`retryClosedWait`, `setWaitTerminalState`, `cmdSessionWait` meta map) into the store.
**WIRE:** typed Huma `/v0/waits` endpoint + DTO replacing `Client.ListBeads(label=gc:wait)`/`GetBead` in `cmd_wait.go`. **(critique correction):** make 404-on-new-route a `ShouldFallbackForRead`-eligible/capability-probed condition (rolling-deploy safety); keep the label read serving through a deprecation window; carry `AgeSeconds` in the typed `CachedRead` envelope; migrate the local `doWaitListFallback` leg onto the session front door in the same step.
**Acceptance:** wait census → 0 in `cmd_wait.go`/`waits.go`; `/v0/waits` + fallback both typed; WI-1 & WI-2 wait residuals close.

## WI-5 — Sessions / Reconciler core (large; already mid-flight) `[x]`
<!-- COMPLETE: W0-W5 integrated (merge f2742d35e). Marker was stale. -->


> WI-5 waves: W0 (fold O1+O2+O4) ✅ · W1 (ApplyPatchInfo cutover) ✅ · W2 (leaf reads) ✅ → W3 (mixed splits) → W4 (ordered-slice/snapshot) → W5 (lockstep drop + oracle-sibling deletion). Relocation-guard regression from WI-4 fixed (5fb00e5d3).
Fold O2 + O4. `ApplyPatch` **returns the refreshed `Info` as a LOCAL fold** (not re-Get); status-close keeps a `Get`. Migrate the remaining ~37 `session_reconcile.go` decision helpers + the `session_wake.go` drain family + `session_lifecycle_parallel.go` async-start commit protocol onto `infoByID` (Info first grows the enumerable vocabulary those compares need). Retire the ordered `[]beads.Bead` working set (`session_reconciler.go:1411-1433`) onto `infoByID`; delete the `sessionBeadSnapshot` raw half + the ~20 single-site `InfoFromPersistedBead` wrappers + `infoLookupFromBeadLookup` shim. Every migrated read gets the `*_info_equiv_test.go` oracle treatment; the raw classifier oracle siblings are deleted last (unblocks Tier-3 unexport). **Do NOT attempt in one PR** — leaf-first waves.
**Acceptance:** `session_reconcile.go`/`session_wake.go` bead-free (mixed files stay off Tier-2 with in-code census); tick budget preserved (no re-Get); oracles green.

## WI-6 — Sessions / API + Worker + Periphery `[~]`

> WI-6 waves: W0 (fold O7) ✅ · W1 (edge vocabulary + ListAll union pin) ✅ ·
> W2 (API read-model cutover) ✅ (merge `cf77967bd`; Fable red-team caught 4
> blockers — incl. census-gaming via inlined `Metadata["agent_name"]` magic
> strings — all fixed + re-approved) · W3 (worker boundary) ✅ · W5 (start-exec
> feed typing) ✅ · W4 (periphery ListAll + snapshot raw-half) ✅ (W4 merged
> `c9e59d17c` — full W2+W3+W4+W5 integrated: 6 shards + session/worker/api green;
> red-team zero blockers, 5 nits closed incl. a primed silent-empty `FindInfo*`
> trap + a latent nil-store panic) · W6 **PARTIAL** ✅ (merge `e02175188`, 6 shards
> green): landed the SAFE half — the 10 wake/churn/stability write helpers collapsed
> onto `Store.ApplyPatchInfo`, and `ResolveSessionBeadByExactID` retired from the
> reconciler (census→0). Two TRANSITIONAL lockstep raw mirrors kept
> (`clearWakeFailures` `quarantined_until`; zombie `markProviderTerminalError` 5 keys)
> because deferred same-tick raw readers survive — red-team caught a fail-safe drift
> (mid-tick quarantine clear losing the pending-interaction kill/drain deferral),
> fixed + pinned. **The delete-heavy tail is DEFERRED** (the W6 brief under-scoped it):
> the 6 raw classifiers have live production consumers (`healStatePatchWithRollback`,
> `dependencySessionStartInFlight`, lease helpers) that must migrate first; the sleep
> + lifecycle clusters are same-tick coupled → migrate as a coordinated unit; then
> drop the 2 transitional + 4 coupling mirrors, remove `startCandidate.session`/
> `wakeTarget.session`, delete the classifiers + oracle siblings.
>
> **WI-6 remainder + WI-7 coordinated plan: `remainder-design.md`** (this dir).
> User approved the FULL endgame (R1–R5 + WI-7) with the **tick-feed refactor** for
> the `InfoFromPersistedBead` unexport (`Store.ListAllForReconcile() []Info` reshapes
> the reconcile tick so the 3 tick-collection edges :583/:1342/:1419 stop calling the
> codec → full unexport). Remainder waves:
> - **R1** ✅ (merge `7d0758f35`): leaf sweeps (roots C/D/E/F → Info) + deleted 3 dead
>   raw forms. `cmd_stop` byte-identical (census +1, tracked). Red-team fixed a
>   non-load-bearing reap-boundary oracle (recently-woken creating bead was silently
>   reapable after the raw sibling's deletion).
> - **R2** ✅ (merge `3df383d2f`): display reason lane (`cmd_session` `wakeReasons`→Info) +
>   additive sleep-read twins → deleted `sessionMetadataState`/`wakeReasons`/`evaluateWakeReasons`
>   (raw). cmd_session census 2→1 (residual = `cmdSessionKill` raw Get, a WI-7 front-door flip;
>   design's 2→0 double-counted). NOTE: `session_circuit_state` absent from `Info` →
>   `LifecycleDisplayReasonWithLiveness` stays raw; R5 needs `Info.SessionCircuitState` + a twin
>   before the snapshot raw `Open()` half fully retires.
> - **R3** (HIGH) ✅ (merge `e3e2cc74c`): reconciler heal + sleep-write coordinated unit;
>   DROPPED both transitional mirrors atomically; deleted the ~8-member pending-create lease
>   family + raw sleep-reads + raw heal forms (−656 net). Red-team: zero blockers, mirror-drop
>   audit VERIFIED COMPLETE (the audit itself caught a design-omitted reader recoverRunningPendingCreate);
>   4 nits fixed (3 stale coherence comments + self-sufficient lease oracle). Anti-drift pin added.
> - **R4** (HIGH) ✅ (merge `a1ff223ff`): start-execution cluster; DROPPED the 4 coupling mirrors +
>   deleted `startCandidate.session`/`wakeTarget.session` + `shouldRollbackPendingCreate`/
>   `runningSessionMatchesPendingCreate`/`asyncStart*` + retired `GetBeadWithInfo`; added
>   `Info.BuiltinAncestor`/`LiveHash`/`StartupDialogVerified`. `session_lifecycle_parallel`
>   `InfoFromPersistedBead` 1→0 (−287 net). Red-team: 1 blocker fixed (buildPreparedStart-error
>   residue fold carried pre-prep values → same-tick config-drift gate could kill an alive session;
>   threaded the post-mutation Info out on error) + 3 nits. The 2 non-known integration timeouts
>   independently confirmed as contention flakes, not R4.
> - **R5** RE-SCOPED to **R5-lite** 🔨 impl (the R5 agent honestly STOPPED: the design's premise
>   "after R2 the snapshot raw half has no reader" is FALSE — `Open()`/`FindByID`/
>   `newSessionBeadSnapshot(beads)` still have many consumers in `city_runtime`/`cmd_start`/
>   `providers`/`build_desired_state`/`session_name_lookup`, several out of R5's scope). R5-lite =
>   the in-scope wins: add `Info.SessionCircuitState` + `LifecycleDisplayReasonWithLivenessInfo`
>   twin + migrate cmd_session's display (removes ONE raw consumer); `cmd_prime` front-door Get;
>   `session_hash`/`session_template_start` → Info; `cmd_wait` PollerKeyFromBead→0. Net:
>   `PollerKeyFromBead → 0` + 3 `InfoFromPersistedBead` drops. `ListAllSessionBeads` UNCHANGED.
> - **R6 STOPPED + RE-SEQUENCED** (second honest design-vs-reality stop): the raw
>   `sessionBeadSnapshot` half CANNOT be deleted in isolation — it exists to serve 3 load-bearing
>   consumer classes the design deferred: (a) the reconciler tick MUTATES the raw `[]beads.Bead` in
>   place (Phase-0 heal `session_reconciler.go:1187`, dedup :1208) + projects it at the tick edges
>   (:1342/:1419); (b) the pool path REUSES/CREATES raw beads (selection/creation/`add`); (c) the
>   sync/heal path mutates raw `openBeads` in place + rebuilds the snapshot. So the raw-half deletion is
>   DOWNSTREAM of the tick-feed refactor + pool typing (the code's own in-line sanctions at
>   `session_bead_snapshot.go:273-284` + `build_desired_state.go:4063-4069` say so). R6's constructor-
>   equivalence pin proven load-bearing (mutation stranded a named session). See `/tmp/r6_finding.md`.
>
> **Corrected remaining endgame (= WI-7 expanded; tick-feed refactor is the KEYSTONE the user approved):**
> - **W-tick** ✅ (merge `1d0260f90`, the keystone): `ListAllForReconcile() []ReconcileSession{Info,Circuit}` +
>   fold-then-build reshape; `session_reconciler` `InfoFromPersistedBead` 3→0; 0-Get budget held. Red-team
>   (hardest of the migration): reshape byte-identical; 3 blockers fixed (dedup-stop + fold-visible pins
>   made load-bearing; trace-recorder/cleanup/cmd_start row flips landed). Added `Info.WorkerDir`.
> - **W-pool** ✅ (merge `507f7bf4a`): pool selection/creation/reuse path typing (class b). Added
>   typed create front door `session.Store.CreateSessionInfo` (projects the created bead, no
>   post-create Get); flipped `selectOrPlanPoolSessionBead`+normalize/reuse cluster +
>   `realizePoolDesiredSessions` to carry Info; `snapshot.add`→`addInfo`. `build_desired_state`
>   `InfoFromPersistedBead` 2→0. Red-team (changes-needed→approved): fixed a real regression —
>   `addInfo` updated only the snapshot typed half while `syncSessionBeadsWithSnapshotAndRigStores`
>   reads the raw `Open()` half on the SAME snapshot (3 no-reload windows), so poolSlot-0 creates
>   minted a DUPLICATE session bead; fix = `snapshotOrLoadSessionBeads` reloads from store on
>   typed/raw cardinality skew (byte-identical on no-create path), pinned load-bearing by
>   `TestSyncDoesNotMintDuplicateForSameCycleSingletonCreate` (fail-then-pass verified). Nits:
>   collapsed a duplicated staleness twin; pinned create-echo==Get across backends
>   (beadstest `CreateEchoMatchesGetOnMetadata`). Commits a62f1b6b8/abb79e1fb/db9b3e4ac.
> - **W-delete** ✅ (merge `e0c186205`, net −378): deleted the `sessionBeadSnapshot` raw half
>   (`loadSessionBeadSnapshot`→`ListAllForReconcile`, its first production consumer; the W-pool skew
>   reload retired with the raw half). Census zeros: `session_bead_snapshot` IFP 3→0 + LASB 1→0,
>   `session_hash` 1→0, `session_logs_resolve` 2→0, `cmd_stop` 1→0, `doctor_session_model` LASB 1→0.
>   `session_beads` LASB STAYS 1 (honest sync/beadmail floor). Config-change fingerprint computed
>   edge-side (`SessionSetFingerprint`+`ListAllForReconcileWithFingerprint`), byte-parity-pinned;
>   sync-tail = fresh re-list (NDI delta, pinned). Field-adds `Info.AwakeStartedAt`+`Info.UsageComputeEmittedAt`;
>   deleted the dead 16-fn raw pool cluster + re-pointed oracles. Red-team approve-with-nits (0 blockers);
>   6 nits fixed in commit C incl. 2 mandatory oracle-regression restorations (fail-then-pass verified).
>   Commits cfdc94e30/b7ef1af77/bec1a2be3. **Interior IFP now = cmd_session 1 + session_resolution 1 (W-flip).**
> - **W-flip + W-unexport** ✅ (merge `13c0ff6f9`, combined final wave): zeroed the LAST TWO interior
>   `InfoFromPersistedBead` sites — `cmd_session.go` cmdSessionKill (raw Get+codec → `sessionFrontDoor().Get`→Info,
>   bridge preserved; the `infoErr` best-effort branch is defensive, `resolveSessionIDWithConfig` gates
>   foreign/missing first) and `internal/api/session_resolution.go` retire lane (→ `ExactMetadataSessionCandidatesInfo`
>   + exported Info classifiers + new `LifecycleIdentityReleasedInfo`). **Interior (non-test) `InfoFromPersistedBead`
>   = TRUE ZERO across all 4 scan dirs** (census-enforced; no gaming). Retired the `GetWithPersistedResponse`
>   needle (deleted the dead zero-caller `SessionCatalog.GetWithPersistedResponse`). Red-team changes-needed→resolved
>   (the mandated kill pin tested an UNREACHABLE branch — proven by mutation; added the reachable foreign/missing
>   + candidate-Info oracles). Commits ffada9ce8/325d5b877/493691763.
> - **W-unexport (compiler rename) DEFERRED — honest under-reach.** `InfoFromPersistedBead` is NOT renamed to
>   `infoFromPersistedBead`: it is a test-fixture constructor at **~444 external call sites / 51 external test files**
>   (cmd/gc, internal/api, internal/worker) — the compiler rename breaks them all. The census ratchet ALREADY
>   enforces the non-test interior boundary at true zero; the needle stays as a permanent interior-zero-pin.
>   Achieving the compiler boundary needs a separate mechanical **W-test-fixture** wave (migrate the ~523 test
>   sites to a `sessiontest` shim / lowercase internal-package tests), then unexport all session codecs together.
>   `ListAllSessionBeads` (session_beads.go:1, sync/beadmail floor) + orders codecs (`RunFromTrackingBead`/
>   `MaxSeqFromLabels`, WI-3) stay exported+pinned per the honest endgame verdict.
>
> Every session-store wave (W2/W3/W5) tripped the SAME front-door-Get contract
> subtlety (session.Store.Get/GetPersistedResponse returns `ErrSessionNotFound` +
> `"loading session"` wrap + rejects non-`IsSessionBeadOrRepairable` beads, unlike
> raw `store.Get`); each swap must bridge it (W2 established `bridgeSessionGetError`
> at `session_get_read.go:60`). Red-teams caught it in W2 (API), W3 (factory lane,
> 400→500 in a resolve-then-Get race), W5 (front-door rejection of type+label-lost
> beads). W5's coherence-Gets were also converted to `ApplyPatch` folds (no re-Get,
> spec §7). Cross-wave merge conflicts are confined to the census guard alone;
> resolve by regen-from-tree.

`session.Store`: `ListAll(opts)` (carries `IncludeClosed`/`Sort`/`Live`/`Limit`; cache-first union ported from `cache_read_model.go`; characterization-pinned) + `GetPersistedResponse(handle)` (retire `Manager.GetWithPersistedResponse`/`GetWithBead`). Migrate `cache_read_model.go`/`handler_sessions.go`/`huma_handlers_sessions_query.go`/`session_resolution.go`/`handler_status.go` (fold O7). Worker: `Factory.SessionByHandle`/`SessionByInfo`, catalog off bead feeds; Manager stops accepting bead feeds and returning `(Info, Bead)` pairs. Periphery: `build_desired_state`/`pool` cluster (per-parameter split: session params → `Info`, work slices stay `[]beads.Bead`; `bindPoolSessionTriggerBead` returns a typed patch + fixes its write routing), `session_beads` repair lane, sleep/idle/name-lookup collapse; mail identity residual onto the typed session mailbox surface (closes WI-2 residual).
**Acceptance:** session interior (minus §5 exemptions) bead-free; API/worker on typed Store; dashboard perf tier preserved (`make dashboard-check` + no per-request bd hit regression).

## WI-7 — Front-door flip + compiler endgame `[ ]`
`cmd/gc/class_store.go` + `api.State` accessors flip from `beads.XStore` wrappers to domain stores (`sessionsFrontDoor() *session.Store`, `ordersFrontDoor() *orders.Store`, `nudgesFrontDoor() *nudgequeue.Store`; mail already via `newCityMailProvider`), built from `resolve*Store` outputs (preserve capability assertions). Unexport the per-class codecs; convert the WI-0 ratchet guards into permanent zero-count pins; `frontdoor_di_guard_test.go` transition lists become permanent.
**Acceptance:** typed-class codecs unexported (compiler-enforced boundary); census tests are zero-pins; work/graph accessors unchanged.

## Deferred follow-ups (tracked, not yet done)
- **WI-3 two-class graph wiring:** the orders `LastRun`/`Cursor`/`HasOpenWork` edge is built to take an orders leg + a graph leg, but every call site currently passes the orders store as its own graph leg and `resolveGraphStore` is not wired in — so graph-split correctness is deferred (byte-identical to before for single-store cities). Wire `resolveGraphStore` into `orderFrontDoorsForStores`/`orderFrontDoorsForTypedStores` + the `order_dispatch`/`cmd_order`/`huma_handlers_orders` call sites, with a split-city characterization test, before Tier-3 unexport of the order codecs.
- **WI-3 residuals** (order-class debt in the census): `RunFromTrackingBead(` in `huma_handlers_orders.go` and `MaxSeqFromLabels(` in `cmd_order.go`/`huma_handlers_orders.go` — the API history/detail federation + `bdCursor` path; close with the WI-6 API read-model + wire-DTO work.
- **WI-0 census guard blind spot (found by W2 red-team, blocker 2 — HARDEN in WI-7):** the ratchet counts codec-call needles (`InfoFromPersistedBead(` …) but is BLIND to raw `bead.Metadata["<key>"]` inline reads, so a needle can be driven to zero *dishonestly* by inlining the magic string (the worse form of the leak). W2's impl agent did exactly this at 3 internal/api sites; the red-team caught it and the fix restored the honest codec lane. Until hardened, red-team every wave's census delta against the actual diff, not just the needle counts. Hardening: add a second census dimension counting raw session-class metadata-key string literals (`"session_name"`, `"agent_name"`, `"state"`, `"sleep_reason"`, the beadmeta.* session keys) in the interior scan dirs outside the edge set, ratcheted to zero alongside the codec needles. See `/tmp/wi6_census_blindspot.md` (working note).
- **WI-6 W2 residual (permission-mode raw lane):** `internal/api/huma_handlers_sessions_command.go:updateSessionPermissionMode` still validates via raw `store.Get` because `legacySessionKind(b.Metadata)`/`resolveProviderForSessionOptions(info, b.Metadata, cfg)` read the raw metadata map downstream (not projected onto `Info`) — carries a `// WI-6 residual:` comment; convert in WI-7 alongside the front-door flip (or once those provider-resolution helpers take `Info`/`PersistedResponse`).
