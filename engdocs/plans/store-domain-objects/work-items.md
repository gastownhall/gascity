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
> - **R5** 🔨 impl: periphery honest holds (snapshot raw-half delete — needs
>   `Info.SessionCircuitState` + `LifecycleDisplayReasonWithLivenessInfo` twin per the R2 finding;
>   hash/template; `cmd_prime` front-door Get; `cmd_wait` PollerKeyFromBead→0). Zeroes
>   `ListAllSessionBeads` + `PollerKeyFromBead`. Leaves the `InfoFromPersistedBead` tail (3 tick
>   edges + build_desired_state + session_logs_resolve + session_resolution) for WI-7.
> - **WI-7 W7a**: front-door flip (`class_store.go` + `api.State` → domain stores).
> - **WI-7 W7b**: unexport the codecs (tick-feed refactor for `InfoFromPersistedBead`);
>   guards → permanent zero-pins. Orders codecs gated on deferred WI-3 two-class wiring.
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
