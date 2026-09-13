# WI-6 remainder + WI-7 — implementation DESIGN (the delete-heavy tail + codec-unexport endgame)

**Ground truth verified on branch `worktree-ref` @ `e02175188`** (WI-0..WI-5, WI-1,
WI-6 W0–W5, WI-6 W6 batch-cluster slice integrated). Every file:line below was
`git grep`'d at HEAD. Where this contradicts `/tmp/remainder_context.md` or the
original `/tmp/wi6_design.md`, the correction is called out inline — the W6 wave
already proved the original W6 Commit-B "delete the 6 classifiers" premise FALSE,
so this doc is grounded on code, not the wave reports.

## 0. Corrections to the starting map (`/tmp/remainder_context.md`)

- **`pendingCreateSessionStillLeased` (raw, `session_reconciler.go:708`) is ALREADY
  DEAD** — zero live callers (all callers use `pendingCreateSessionStillLeasedInfo`;
  the raw form survives only as an oracle sibling). The context doc lists it as a
  live consumer chain (`sessionStartRequested ← … pendingCreateSessionStillLeased`).
  It is not. It can be deleted with its oracle in the very first wave.
- **`pendingResumePreservingNamedRestart` (raw, `session_reconciler.go:1008`) is
  ALSO DEAD** — the only live caller is `pendingResumePreservingNamedRestartInfo`
  (`session_reconciler.go:2875`). Deleting it removes a `pendingCreateLeaseActive`
  raw caller for free.
- The context doc's "6 classifiers" is really **one entangled raw family of ~11
  lease/wake classifiers** (`sessionStartRequested`, `sessionMetadataState`,
  `staleCreatingState`, `pendingCreateAttemptStale`, `pendingCreateStartInFlight`,
  `pendingCreateLeaseActive`, `pendingCreateLeaseExpiredForRollback`,
  `pendingCreateNeverStartedExpired`, `pendingCreateNeverStartedLeaseExpired`,
  `shouldRollbackPendingCreate`, `runningSessionMatchesPendingCreate`), plus the two
  DEAD forms above. They cannot be deleted piecemeal — they call each other raw, so
  the family collapses only when its last raw *root reader* migrates. Deletion is
  distributed across the waves that retire each root, not a single Commit-B.
- **`InfoFromPersistedBead` census at HEAD is 13 hits / 10 files** (verified against
  the checked-in `typedClassCodecCensus`), not "3 residuals." The 3 the context doc
  names (`session_reconciler.go` :583/:1342/:1419) are the *hard tick-collection*
  subset; the other 10 are periphery/display holds that migrate first.

---

## 1. Ground-truth dependency graph (raw readers → what they read raw)

### 1a. The two transitional W6 mirrors (must die WHEN their last raw reader types)

**Mirror #1 — `quarantined_until`** at `session_reconciler.go:2526-2533`
(after `clearWakeFailures`):
```
session.Metadata["quarantined_until"] = infoByID[session.ID].QuarantinedUntil
```
Sole reason it exists: **`pendingInteractionKeepsAwake(*session, sp, name, clk)`**
(`session_sleep.go:119`) reads `quarantined_until` raw (via
`LifecycleInputFromMetadata` → `BlockerQuarantined`) at the SAME-tick downstream
decisions: config-drift drain (`session_reconciler.go:2219`, `:2277`), max-age kill
region (`:2724`), idle kill (`:2948`, `:3031`), and `:4423`.
→ **Dies when `pendingInteractionKeepsAwake` takes `Info`** (sleep cluster, Wave R3).

**Mirror #2 — 5 keys** at `session_reconciler.go:2054-2058` (after the zombie
`markProviderTerminalError`): mirrors `state`, `sleep_reason`, `last_woke_at`,
`pending_create_claim`, `pending_create_started_at` onto `*session`.
Reasons it exists (three same-tick raw readers of `*session`/`target.session`):
- **`healStatePatchWithRollback(*session)`** (`session_reconcile.go:915`, via
  `healStateWithRollback` at `session_reconciler.go:2462`) reads `state`,
  `pending_create_claim`, `pending_create_started_at`, `last_woke_at`.
- **`persistSleepPolicyMetadata(target.session)`** (`session_sleep.go:263`, called
  `session_reconciler.go:3224`) reads `state`, `sleep_reason`, `sleep_intent`,
  `sleep_policy_fingerprint`.
- **`configWakeSuppressed(*target.session)`** (`session_sleep.go:218`, called
  `session_reconciler.go:3168`) reads `sleep_reason`, `sleep_policy_fingerprint`,
  idle refs.
→ **Dies when all three take `Info`** (heal + sleep clusters land together — the
coordinated Wave R3).

### 1b. The 4 START-EXECUTION coupling mirrors (die WITH `startCandidate.session`/`wakeTarget.session`)

Comment marker + raw `for k,v := range batch { session.Metadata[k]=v }` loops at:
- `session_reconciler.go:2411` — restart-handoff `restartFold` (RestartRequestPatch).
- `session_reconciler.go:2994` — max-age kill `SleepPatch` mirror.
- `session_reconciler.go:3080` — idle kill `SleepPatch` mirror.
- `session_reconciler.go:4509` — `resetConfiguredNamedSessionForConfigDrift`
  (ConfigDriftResetPatch).

(The original doc's line numbers `:2405/:2978/:3063/:4488` are stale by ~5–20 lines.)
They are retained ONLY because the start-execution path still holds the raw
`*beads.Bead` via `startCandidate.session`/`wakeTarget.session` and the write helpers
that mutate it. Wake-fairness itself already reads the Info twin
(`wakeFairnessTime(c)` → `c.info.LastWokeAt`). → **Die in Wave R4** when the raw
fields + the raw-taking write helpers are removed.

### 1c. The raw ROOT readers (the leaves whose migration frees the classifier family)

Each holds a raw bead / raw list and is the *entry point* into the classifier tree.

| Root | Site | Reads raw | Calls classifiers (raw) |
|---|---|---|---|
| **A. `evaluateWakeReasons` / `wakeReasons`** | `session_reconcile.go:76`; only caller `wakeReasons:73` ← **`cmd_session.go:1337`** (gc session REASON column) | held_until, quarantined_until, wait_hold, session_name | `resolveSessionSleepPolicy`, `sessionStartRequested`, `sessionWithinDesiredConfig`, `sessionMetadataState`, `configWakeSuppressed`, `namedSessionMode`, `sessionKeepWarmEligible` |
| **B. `healStatePatchWithRollback`** | `session_reconcile.go:915` ← `healState`/`healStateWithRollback` ← reconciler forward pass `session_reconciler.go:1746`, `:2462` | whole `Metadata`, `Status`, `CreatedAt` | `sessionStartRequested`, `pendingCreateLeaseActive`, `pendingCreateLeaseExpiredForRollback`, `isNamedSessionBead` |
| **C. `dependencySessionStartInFlight`** | `session_lifecycle_parallel.go:666` ← `dependencyTemplateAlive` ← `:737`, `session_reconciler.go:687` | iterates `store.ListByMetadata({session_name})` raw beads; `Status`, `isSessionBead` | `pendingCreateStartInFlight` |
| **D. `reapStaleSessionBeads`** | `session_beads.go:2004` (feeds off `loadSessionBeads` raw) | `state`, `session_name`, `pending_create_claim`, `last_woke_at`, `staleReapStartBoundary(b)` | `pendingCreateNeverStartedLeaseExpired` (`:2070`) |
| **E. `markCityStopSessionSleepReason`** | `cmd_stop.go:362` (iterates `sessFront.Store().ListByLabel("gc:session")`) | `sleep_reason` | `sessionMetadataState` |
| **F. GCSweep pool-slot** | `city_runtime.go:2837` `pendingCreateLeaseActive(bead,…)` (Info twin `:2843` already exists + used `:2729`) | pending-create lease keys | `pendingCreateLeaseActive` |
| **G. start-execution cluster** | `session_lifecycle_parallel.go` async commit path | see 1d | `shouldRollbackPendingCreate`, `runningSessionMatchesPendingCreate`, `asyncStart*` |
| **H. sleep-cluster READ side** | `session_sleep.go` `resolveSessionSleepPolicy:32`, `configWakeSuppressed:218`, `sessionIdleReference:195`, `sessionKeepWarmEligible:245`, `pendingInteractionKeepsAwake:119` | whole-bead sleep/idle/detach metadata + **runtime probes (STAY RAW, §7)** | (leaf; no classifier deps except each other) |
| **I. sleep/lifecycle WRITE helpers** | `session_sleep.go` `persistSleepPolicyMetadata:263`, `markIdleSleepPending:316`, `recoverPendingIdleSleep:330`, `reconcileDetachedAt:145`; `session_bead_cycle.go recordCurrentBeadIDOnWake:27` | take `*beads.Bead`, mirror onto `session.Metadata` | — |

### 1d. Classifier → live raw-caller map (from `git grep`, non-Info, non-comment)

```
sessionStartRequested(raw)              ← A(:108), B(:949)          [pendingCreateSessionStillLeased DEAD:727]
sessionMetadataState(raw)               ← A(:114), E(cmd_stop:362)
staleCreatingState(raw)                 ← sessionStartRequested(:192)                 (delete WITH it)
pendingCreateAttemptStale(raw)          ← staleCreatingState(:1066), pendingCreateLeaseActive(sr:844),
                                          pendingCreateLeaseExpiredForRollback(sr:953,:961)
pendingCreateStartInFlight(raw)         ← C(slp:686), reconciler raw lease helpers(sr:838,:955)
pendingCreateLeaseActive(raw)           ← F(cr:2837), B(:962), [pendingCreateSessionStillLeased DEAD:714],
                                          [pendingResumePreservingNamedRestart DEAD:1026]
pendingCreateLeaseExpiredForRollback    ← B(:987)
pendingCreateNeverStartedExpired(raw)   ← pendingCreateLeaseExpiredForRollback(sr:951,:959)
pendingCreateNeverStartedLeaseExpired   ← D(sb:2070), pendingCreateLeaseActive(sr:842), pendingCreateNeverStartedExpired(sr:880)
shouldRollbackPendingCreate(raw)        ← G(asyncStartSessionStillCurrent:1711, asyncStartStaleRuntimeCleanupAllowed:1750)
runningSessionMatchesPendingCreate(raw) ← G(stopStaleAsyncStartRuntime:1670)
```

**Resulting deletion gates (which root must clear each classifier):**
- `sessionMetadataState`: A **and** E → **R2** (E lands R1, A lands R2).
- `sessionStartRequested` + `staleCreatingState`: A **and** B → **R3** (B is R3).
- `pendingCreateLeaseActive` + `pendingCreateLeaseExpiredForRollback` +
  `pendingCreateNeverStartedExpired` + `pendingCreateNeverStartedLeaseExpired` +
  `pendingCreateAttemptStale` + `pendingCreateStartInFlight`: B + F + C + D + the two
  DEAD forms → all cleared by end of **R3** (F/C/D land R1; B lands R3; DEAD forms
  deleted R1).
- `shouldRollbackPendingCreate` + `runningSessionMatchesPendingCreate`: G → **R4**.

**The tick-collection `InfoFromPersistedBead` holds** (the hard subset):
- `session_reconciler.go:583` — finalize/close boundary.
- `session_reconciler.go:1342` — Phase-0 raw bead load (builds `orderedBeads`).
- `session_reconciler.go:1419` — `infoByID` snapshot build (per-bead projection).
These are the tick's raw→Info projection edge; the reconciler holds `orderedBeads
[]beads.Bead` and projects each into `infoByID`. They are addressed in R5/WI-7
(§5, §7 verdict).

---

## 2. Wave plan (leaf-first; two commits per wave; ≤~5 files where the file count allows)

**Discipline (every wave):** Commit A = additive twins/vocab/oracle rows + the
characterization pin, tree green. Commit B = migrate the reads + delete the now-dead
raw forms + move the census ratchet DOWN in the same commit. No mirror is dropped
before its last raw reader migrates (the W6 fail-safe drift MUST NOT recur — the
mirrors in §1a/§1b are load-bearing until the named wave). Each commit is
independently green under `make test-cmd-gc-process-parallel` +
`make test-integration-shards-parallel`; `make test-local-full-parallel` once before
the final merge.

Because the reconciler files (`session_reconcile.go` 1242 LOC, `session_reconciler.go`
5093 LOC, `session_lifecycle_parallel.go` 3471 LOC) are huge, "≤5 files" is honored
by *touching few files per wave*, not by splitting a coupled edit — R3 and R4 each
legitimately touch 3 big reconciler files at once (they are one cluster).

### Wave R1 — leaf sweeps + dead-form deletion (mechanical, no coupling)  **[≤5 files]**
Files: `session_beads.go`, `cmd_stop.go`, `session_lifecycle_parallel.go` (dep site
only), `city_runtime.go`, `session_reconciler.go` (dead-form deletes only).

- **Commit A:** Info twins for the leaf roots that lack them:
  `dependencySessionStartInFlightInfo` (iterate `sessFront.ListAll({})` → Info,
  call `pendingCreateStartInFlightInfo`); `reapStaleSessionBeadsInfo` feed via
  `loadOpenSessionInfos` (already exists, `session_beads.go:70`);
  `markCityStopSessionSleepReason` onto `ListAll` + `sessionMetadataStateInfo`;
  GCSweep `city_runtime.go:2837` already has `pendingCreateLeaseActiveInfo:2843` used
  at `:2729` — flip the one raw `:2837` site. Add oracle rows only where a twin is new.
- **Commit B:** migrate D/E/F/C reads onto the Info twins; **delete the two DEAD raw
  forms** `pendingCreateSessionStillLeased` (`:708`) + `pendingResumePreservingNamedRestart`
  (`:1008`) and their oracle siblings (six-way grep first). Census: no `InfoFromPersistedBead`
  movement yet (these roots read via `ListAll`/`loadOpenSessionInfos`, edge-package, unscanned);
  `ListAllSessionBeads` unchanged.
- **Front-door-Get flags:** `cmd_stop.go:362` moves from `ListByLabel("gc:session")`
  (label-only) to `ListAll({})` (type+label union) — **behavior delta**: the sweep
  now also sees label-lost type-only beads. Original W6 doc flagged this (risk 2).
  DECISION: keep byte-identity via `sessFront.Store().ListByLabel` unless the widen is
  explicitly wanted; recommend byte-identity for a mechanical wave.
- **Risk:** low. `dependencySessionStartInFlight` moves from `ListByMetadata` to a
  full `ListAll` scan + filter — verify the metadata-filter equivalence
  (`session_name == X`) and the `Live` tier is NOT needed (it isn't; this is a
  desired-state read).

### Wave R2 — the display reason lane (root A) + sleep-read twins (additive)  **[≤5 files]**
Files: `cmd_session.go`, `session_reconcile.go`, `session_sleep.go`, `named_sessions.go`.

- **Commit A:** ADD Info twins for the whole read side that `evaluateWakeReasons`
  drives (raw forms STAY — reconciler still uses them until R3):
  `resolveSessionSleepPolicyInfo`, `configWakeSuppressedInfo`,
  `sessionKeepWarmEligibleInfo`, `sessionIdleReferenceInfo`,
  `pendingInteractionKeepsAwakeInfo` (§3), `sessionWithinDesiredConfigInfo`,
  `namedSessionModeInfo`, plus `evaluateWakeReasonsInfo`/`wakeReasonsInfo`. Runtime
  probes inside them STAY RAW (§3, §7). Oracle rows for every new twin
  (`TestSessionClassifierInfoEquivalence`).
- **Commit B:** migrate `cmd_session.go:1337` `wakeReasons(b,…)` →
  `wakeReasonsInfo(info,…)` fed by `loadSessionBeadSnapshot(...).OpenInfos()` (already
  exists) instead of the raw `Open()` half; **delete `wakeReasons`/`evaluateWakeReasons`
  (raw)** (only caller migrated). That removes `sessionStartRequested`/`sessionMetadataState`
  raw caller #A. Combined with R1's E, **delete `sessionMetadataState` (raw) + oracle**
  (no callers left). Census: `cmd_session.go` `InfoFromPersistedBead` 2→0
  (the reason projection folds onto Info); `ListAllSessionBeads` unchanged (still fed
  via snapshot). Paste the regen literal.
- **Front-door-Get flags:** none new — the snapshot `OpenInfos()` feed already exists;
  no per-bead `Get` moves here.
- **Risk:** medium. The display path must reproduce the exact REASON column; the sleep
  twins read whole-bead state — the `MetadataState` vs normalized `State` trap
  (`sessionMetadataStateInfo` reads `Info.MetadataState`) and the untrimmed
  `DependencyOnlyMetadata`/`ManualSessionMetadata` mirrors matter. Oracle rows must
  include closed, drained, always-named, and whitespace-padded fixtures.

### Wave R3 — reconciler HEAL + sleep-write coordinated unit (DROPS BOTH TRANSITIONAL MIRRORS)  **[3 big reconciler files]**
Files: `session_reconcile.go`, `session_reconciler.go`, `session_sleep.go`
(+ `session_bead_cycle.go` if `recordCurrentBeadIDOnWake` signature changes).

This is the load-bearing wave. It types EVERY same-tick raw reader that the two
transitional mirrors feed, so both mirrors drop in one Commit B.

- **Commit A (additive):**
  - `healStatePatchWithRollbackInfo(info, alive, clk, startupTimeout, rollbackAvailable)`
    — reads all state/lease keys off `Info` (`MetadataState`, `PendingCreateClaim`,
    `PendingCreateClaimMetadata`, `PendingCreateStartedAt`, `LastWokeAt`, sleep_reason);
    calls the `*Info` classifier twins already present (`sessionStartRequestedInfo`,
    `pendingCreateLeaseActiveInfo`, `pendingCreateLeaseExpiredForRollbackInfo`,
    `isNamedSessionInfo`). `healStateWithRollback` keeps writing via `sessFront.ApplyPatch`
    + returns the batch (unchanged); only its *read* switches to the `Info` snapshot
    entry (`infoByID[session.ID]`), which the forward pass already holds coherent.
  - `persistSleepPolicyMetadataInfo(info, sessFront, policy, configSuppressed) (Info, …)`
    — §3. `configWakeSuppressedInfo`/`resolveSessionSleepPolicyInfo`/
    `pendingInteractionKeepsAwakeInfo` reused from R2-A.
  - `markIdleSleepPending`/`recoverPendingIdleSleep`/`reconcileDetachedAt` gain
    Info-taking forms (they already return the batch to fold; only drop the `*session`
    param and mirror loop).
  - Oracle rows for `healStatePatchWithRollbackInfo` (the biggest — closed,
    failed-create, stale-creating, drained, rollback-available fixtures).
- **Commit B (migrate + drop mirrors + delete family):**
  1. Reconciler forward pass: `healStateWithRollback(session,…)` →
     `healStateWithRollbackInfo(infoByID[id],…)`; the awake scan
     `persistSleepPolicyMetadata(target.session,…)` → `…Info(infoByID[id],…)`;
     `configWakeSuppressed(*target.session,…)` → `…Info(info,…)`;
     `resolveSessionSleepPolicy(*target.session/*session,…)` → `…Info(info,…)`;
     `pendingInteractionKeepsAwake(*session,…)` → `…Info(info,…)` at every site
     (`:2219`, `:2277`, `:2724`, `:2948`, `:3031`, `:4423`).
  2. **DROP transitional mirror #1** (`quarantined_until`, `:2526-2533`) and
     **mirror #2** (5 keys, `:2054-2058`) — every raw reader they fed now reads `Info`.
  3. **DELETE the raw sleep-read forms** (`resolveSessionSleepPolicy`,
     `configWakeSuppressed`, `sessionKeepWarmEligible`, `sessionIdleReference`,
     `pendingInteractionKeepsAwake`) — reconciler was their last raw user (display
     migrated R2). Their oracle siblings go too.
  4. **DELETE the raw classifier family** now that B is gone: `sessionStartRequested`,
     `staleCreatingState`, `pendingCreateAttemptStale`, `pendingCreateStartInFlight`,
     `pendingCreateLeaseActive`, `pendingCreateLeaseExpiredForRollback`,
     `pendingCreateNeverStartedExpired`, `pendingCreateNeverStartedLeaseExpired` +
     their oracle rows (six-way grep each name first).
  5. `healStatePatchWithRollback`/`healState`/`healStateWithRollback` raw forms:
     delete/reduce to the Info body (`healState` becomes a thin `*session`-free
     wrapper only if a raw caller remains — none should).
- **Census:** `session_reconciler.go` `InfoFromPersistedBead` 3→3 (the tick-collection
  edges are untouched here); `ListAllSessionBeads` unchanged. The win is the mirror
  drop + family deletion (Tier-1 comment-needle counts fall as the classifier
  comments go). Regen + paste.
- **Front-door-Get flags:** none — this wave is all `ApplyPatch`/`ApplyPatchInfo`
  folds on the coherent `infoByID` snapshot (no re-Get; the tick-budget guard
  `TestReconcileSessionBeadsFastPathGetBudget` MUST stay at its pinned count).
- **Risk:** HIGH — this is the wave that most resembles the W6 fail-safe drift. The
  two mirrors and their ~10 reader sites must ALL flip in one commit. `persistSleepPolicyMetadata`'s
  no-op-on-error swallow contract MUST survive (§3). Pin: a same-tick zombie→heal→sleep
  characterization test that the awake-scan reads the post-heal/post-mark `Info`, not a
  stale mirror; plus `TestReconcileSessionBeads_ZombieTerminalErrorReflectedOnSnapshot`
  and `..._HealStateReflectedOnSnapshot` (already exist) must stay green with the
  mirrors gone.

### Wave R4 — start-execution cluster (DROPS THE 4 COUPLING MIRRORS + `startCandidate.session`/`wakeTarget.session`)  **[2–3 files]**
Files: `session_lifecycle_parallel.go`, `session_reconciler.go` (append sites +
coupling-mirror loops), `session_bead_cycle.go`.

- **Commit A (additive):** the async-gate Info twins already exist
  (`asyncStartPreparedCommandStaleInfo`, `asyncStartSessionStillCurrentInfo`,
  `asyncStartStaleRuntimeCleanupAllowedInfo`, `asyncStartIdentityMatchesInfo`,
  `runningSessionMatchesPendingCreateInfo`, `shouldRollbackPendingCreateInfo`). ADD:
  - `refreshAsyncStartResult` returns Info-only (drop the raw bead half of
    `GetBeadWithInfo`; use `sessFront.GetPersistedResponse(id)` → Info) — **kills the
    single `InfoFromPersistedBead` in `session_lifecycle_parallel.go`** (the honest
    in-lock re-projection at `prepareStartCandidateForCity`) by making the re-Get
    return `Info` directly.
  - the write helpers `clearPendingStartInFlightLease`, `rollbackPendingCreate`,
    `rollbackPendingCreateClearingClaim` take `(handle string, sessFront)` +
    return the batch (they already return the batch); the caller folds onto
    `infoByID`/`candidate.info`. `recordCurrentBeadIDOnWake` takes handle + returns
    the patch (already does; drop `*session`).
  - `ProviderFamilyFromInfo` + `sessionProviderFamily(*session)` at
    `session_lifecycle_parallel.go:1078` → Info form (§4).
- **Commit B (migrate + drop + delete):**
  1. Every executor read moves onto `candidate.info` (they largely already do — W5
     landed this). The re-Get sites (`prepareStartCandidateForCity:780`,
     `refreshAsyncStartResult:1606`) return Info; `preWakeCommit` takes handle+store.
  2. **DELETE `startCandidate.session` + `wakeTarget.session`** raw fields;
     `clonePreparedStartForAsync`'s bead deep-copy (`:1820-1834`) collapses to an
     Info value copy.
  3. **DROP the 4 coupling mirrors** (`:2411`, `:2994`, `:3080`, `:4509`) — nothing
     reads the raw bead after the append now.
  4. **DELETE the raw `shouldRollbackPendingCreate` + `runningSessionMatchesPendingCreate`
     + `asyncStart*` raw forms** + oracle siblings (G was their last user;
     `stopStaleAsyncStartRuntime` moves onto `runningSessionMatchesPendingCreateInfo`).
- **Census:** `session_lifecycle_parallel.go` `InfoFromPersistedBead` 1→0 (tripwire).
  Add `GetBeadWithInfo(` to the census-needle list at its new count (or 0 if fully
  retired). Paste literal.
- **Front-door-Get flags:** `refreshAsyncStartResult`'s re-read stays a real
  front-door `GetPersistedResponse` — the documented sanctioned cross-goroutine
  freshness re-read (NOT a per-patch re-Get). It keeps the `ErrSessionNotFound` +
  `"loading session %q"` wrap + `IsSessionBeadOrRepairable` rejection; the existing
  `TestRefreshAsyncStartRejectsNonSessionBead` pins the delta. `prepareStartCandidateForCity`'s
  in-lock re-Get likewise.
- **Risk:** HIGH — wake-fairness (`TestWakeFairnessInfoTwinCharacterization` already
  pins it), the value-Info staleness window (capture-at-append), and the
  cross-goroutine `commitStartFailure` (must fold into the local result chain, NEVER
  `infoByID`). `rollbackPendingCreate`/`clearPendingStartInFlightLease` lose their
  `*session` write mirror — they already return the batch; verify every caller folds
  it (grep the ~8 call sites).

### Wave R5 — periphery honest holds (empties the remaining scanned census)  **[≤5 files/subwave; may split into R5a/R5b]**
Files: `session_bead_snapshot.go`, `session_beads.go`, `session_hash.go`,
`session_template_start.go`, `session_logs_resolve.go`, `build_desired_state.go`,
`doctor_session_model.go`, `internal/api/session_resolution.go`, `cmd_prime.go`,
`cmd_wait.go`.

- **Delete the raw `sessionBeadSnapshot` half** (`Open()`, `FindByID`, the `open`
  slice, `newSessionBeadSnapshot(beads)`): after R2 migrated `cmd_session`, its last
  raw consumer, the raw half has no reader — verify via the in-file WI-6 checklist
  (`session_bead_snapshot.go:212-223`) + six-way grep. This zeroes
  `session_bead_snapshot.go` `InfoFromPersistedBead` (3→0) and `ListAllSessionBeads`
  (1→0), and `session_beads.go` `ListAllSessionBeads` (1→0 once `loadSessionBeads` is
  the only raw feed and it is reimplemented over the edge / its remaining raw callers
  drop).
- `session_hash.go:21` → Info form (WI-5 deferral); `session_template_start.go` →
  Info; `session_logs_resolve.go` (2) → feed `ResolveCodexTranscriptBySessionOrder`
  an Info-shaped input OR sanction (see §5 — it takes `[]beads.Bead`);
  `build_desired_state.go` :2380/:2631 → type the feeder beads or sanction (§5);
  `doctor_session_model.go:149` → `ListAll` (the census header explicitly requires
  this for the Tier-3 zero, even though doctor is a §5 exemption for *holding* beads —
  it must not *call the codec*); `internal/api/session_resolution.go:1` retire lane →
  migrate or sanction with the `named_config.go` family (WI-7).
- **§4 cmd_prime:** `cmd_prime.go:600` — add `Info.BuiltinAncestor` + route through
  `sessionFrontDoor(sessStore).Get` → Info + `ProviderFamilyFromInfo` (§4). Front-door-Get
  flag: cmd_prime is a hook path; the front-door `Get` now rejects non-session beads
  and wraps errors — verify the `warn("loading session bead …")` text + the codex-only
  guard still behave.
- **`cmd_wait.go` `PollerKeyFromBead`** (`:1266` `waitNudgePollerKey`): reduce to a
  `WaitInfo`/`Info`-based poller key so the `PollerKeyFromBead(` needle zeroes.
- **Census:** drives `InfoFromPersistedBead` to its irreducible tail (the 3
  tick-collection edges + any sanctioned holds), `ListAllSessionBeads` → 0,
  `PollerKeyFromBead` → 0. Paste literal after each subwave.
- **Risk:** medium; mostly mechanical, but the snapshot-half deletion is load-bearing
  (index-precedence bugs strand named sessions — pin the constructor-equivalence test).

---

## 3. The sleep cluster as a coordinated unit

The sleep cluster is roots **H** (read side) + **I** (write side). It is NOT
migratable piecemeal because its readers are split across two consumers that share
transitional mirror #2: the CLI display (root A, Wave R2) and the reconciler awake
scan / heal (Wave R3). Sequencing: **read-side twins ADD in R2-A** (display needs
them), **the reconciler read/write side flips in R3** (which also drops mirror #1
and mirror #2 and deletes the raw sleep forms).

### 3a. Info fields — ALL PRESENT (verified `internal/session/manager.go:60-437`)
The 7-key sleep-policy vocab WI-1 promised is on `Info` and in `Info.ApplyPatch`
(`internal/session/info_apply_patch.go:207-219`) at HEAD:
`SleepPolicyFingerprint`, `RequestedSleepAfterIdle`, `EffectiveSleepAfterIdle`,
`SleepPolicySource`, `SleepCapability`, `SleepPolicyAdjustmentReason`,
`ConfigWakeSuppressedMetadata`. The idle/detach/intent keys are present too:
`DetachedAt`, `SleepIntent`, `SleepReason`, `HeldUntil`, `QuarantinedUntil`,
`WaitHold`, `MetadataState`, `SessionNameMetadata`. **No edge field-add is required
for the sleep cluster** — this is a pure signature collapse. (Contrast §4, which
DOES need a field-add.)

The one thing to verify per twin: `configWakeSuppressed` compares
`sleep_policy_fingerprint` EXACTLY against the freshly-`resolveSessionSleepPolicy`'d
`policy.Fingerprint` — the Info form reads `info.SleepPolicyFingerprint` (raw mirror)
and the policy fingerprint is computed from `cfg`/`agent`/`sp` (unchanged). Byte-identical.

### 3b. Runtime probes STAY RAW (spec §7 live edge)
Inside the sleep resolvers, these are provider/runtime calls, NOT bead reads, and
must remain exactly as-is when the signature flips to `Info`:
- `resolveSleepCapability(sp, info.SessionNameMetadata)` — `sp.SleepCapability` /
  `sp.Capabilities()`.
- `sessionIdleReference`: `workerSessionTargetLastActivityWithConfig(…, sp, …,
  info.SessionNameMetadata)`.
- `reconcileDetachedAt`: `workerSessionTargetAttachedWithConfig(…, store, sp, …)`
  (takes `store` + `session.ID`; keep the store/handle, drop only the raw `*session`
  metadata reads — `detached_at` read/write goes through `Info.DetachedAt` +
  `sessFront.SetMarker`).
- `pendingInteractionKeepsAwake`: `pendingInteractionReady(sp, name)` +
  `ProjectLifecycle(LifecycleInputFromInfo(info))` (already exists at
  `lifecycle_projection.go:225`) instead of `LifecycleInputFromMetadata`.

### 3c. `persistSleepPolicyMetadata`'s no-op-on-error swallow contract MUST survive
Current body (`session_sleep.go:263-311`): it computes `changed` off the diff, and on
`sessFront.ApplyPatch(id, changed)` error it **`return`s silently** (no fold, no
mutation). The Info form MUST preserve this exactly: on ApplyPatch error, return the
INPUT `Info` unchanged (which `ApplyPatchInfo` already guarantees — it returns the
folded Info only on success). The preserve-fingerprint branch (`:294-300`, keeps the
in-flight idle-drain fingerprint) reads `info.MetadataState` (== "asleep"),
`info.SleepReason` (== "idle"), `info.SleepIntent` (== "idle-stop-pending"),
`info.SleepPolicyFingerprint` — all present. Pin: an oracle row where ApplyPatch
returns an error → the returned Info equals the input, byte-for-byte, and no partial
fold leaked.

### 3d. `reconcileDetachedAt` / `markIdleSleepPending` / `recoverPendingIdleSleep`
These already return the mirrored batch for the reconciler to fold (`session_sleep.go`).
The collapse only drops the `*beads.Bead` param + the trailing `session.Metadata[k]=v`
mirror loop; the `sessFront.SetMarker`/`ApplyPatch` write + batch return are unchanged.
`recoverPendingIdleSleep` reads `info.SleepIntent` + `info.SleepPolicyFingerprint`;
`reconcileDetachedAt` reads `info.DetachedAt` + `info.SessionNameMetadata`.

---

## 4. `cmd_prime` `builtin_ancestor` — DECISION: add the edge field

`cmd_prime.go:600` `sessionProviderFamily(sessionBead)` →
`session.ProviderFamilyFromMetadata(meta, "")` (`internal/session/submit.go:308`),
which reads `builtin_ancestor` → `provider_kind` → `provider` in that precedence.
`session.Info` carries `Provider` and `ProviderKind` but **NOT `builtin_ancestor`**
(verified — no `BuiltinAncestor` field on `Info`).

**Verdict: ADD `Info.BuiltinAncestor` (raw mirror of `builtin_ancestor`) + a
`ProviderFamilyFromInfo(info, fallback)` helper.** Rationale over a §5 exemption:
1. It is a one-line codec add (`info_store.go` + `info_apply_patch.go` + the
   reprojection oracle) — the same shape as the dozens of raw mirrors already on Info.
2. `Info` already carries the other two precedence rungs (`Provider`, `ProviderKind`);
   the family-resolution vocab is 2/3 present, so this COMPLETES an existing partial
   projection rather than introducing a new concern.
3. It unblocks **three** `sessionProviderFamily(raw)` call sites at once:
   `cmd_prime.go:600`, `cmd_wait.go:1130`, and `session_lifecycle_parallel.go:1078`
   (the R4 start-exec `sessionProviderFamily(*session)`). A §5 exemption would have to
   sanction all three permanently.
4. It lets `cmd_prime` route through `sessionFrontDoor(sessStore).Get` → `Info` and
   drop its `InfoFromPersistedBead(sessionBead).SessionKey` read (census
   `cmd_prime.go` 1→0), instead of holding a raw bead.

A permanent §5 exemption would be justified ONLY if `builtin_ancestor` were a
runtime-derived value — it is not; it is durable session-bead metadata stamped at
creation (`session_beads.go:225`, `session_template_start.go:151`), exactly the kind
of persisted value the codec is meant to project. So the field-add is correct.

Lands in **R5** (with the cmd_prime migration). Front-door-Get flag: cmd_prime is a
hot hook path (`loadCityConfigWithoutBuiltinPackRefresh`); moving to the front-door
`Get` adds the `ErrSessionNotFound` + `"loading session %q"` wrap + non-session
rejection — verify the existing `warn("loading session bead %q: …")` diagnostic and
the `!= "codex"` guard still behave for a damaged/foreign bead.

---

## 5. WI-7 honest scope — per-codec classification + the front-door flip

### 5a. Codec-by-codec unexport verdict (against the checked-in `typedClassCodecNeedles`)

**(a) Already all-zero interior tripwires → UNEXPORT NOW (WI-7 W7b, independent of the remainder):**
- `SessionInfoFromBead` (0 — retired W3), `GetWithBead` (0 — W3),
  `SessionByLoadedBead` (0 — W3), `ResolveSessionBeadByExactID` (0 — W3+W6),
  `ListFullFromBeads` (0 — W2), `ListSessionWaitBeads` (0), `WaitInfoFromBead`
  (0 interior; `internal/api/client_waits.go` is edge-excluded during the /v0/waits
  deprecation window — unexport blocked until that window closes, so treat as
  "unexport after the client_waits legacy rungs go").
- Nudges: `DecodeShadow`, `.FindBead`, `.FindBeadIncludingTerminal`,
  `StaleCandidatesBefore` — all 0 (WI-1 nudges closeout). UNEXPORT/DELETE NOW.
- Messaging: `.ReadMessagesBefore`, `ReadMessageWispEntries` — 0. NOW.

**(b) Reducible to zero by this remainder → UNEXPORT AFTER the named wave:**
- `ListAllSessionBeads` — census `doctor_session_model.go:1`, `session_bead_snapshot.go:1`,
  `session_beads.go:1`. Zeroes in **R5** (doctor migrates; snapshot raw half deleted;
  `loadSessionBeads` becomes the edge `ListAll` or its raw callers drop). The
  edge-package consumer `internal/mail/beadmail.go:108/:120` is NOT scanned, so it
  does not block unexport — but `ListAllSessionBeads` will still be *referenced* by
  the edge `ListAll` internally, so it is inlined/unexported into the edge, not
  deleted. UNEXPORT after R5.
- `PollerKeyFromBead` — `cmd_wait.go:1`. Reducible in R5 (WaitInfo/Info poller key).
- `GetWithPersistedResponse` — `internal/worker/catalog.go:1`. This is the worker
  catalog boundary (cmd/gc routes through it per the worker-boundary migration).
  Reimplement `catalog.GetWithPersistedResponse` over `Store.GetPersistedResponse` +
  `EnrichInfo` and rename/inline so the needle zeroes — WI-7 cleanup.

**(c) Legitimate residual edges → MIGRATE or SANCTION before unexport:**
- `RunFromTrackingBead` (`internal/api/huma_handlers_orders.go:1`) + `MaxSeqFromLabels`
  (`cmd_order.go:1`, `huma_handlers_orders.go:1`) — **orders class, deferred WI-3
  residuals**. These need the orders front-door `Get`/`Cursor` + wire-DTO work
  (`orders.Store.Get` exists at `internal/orders/store_reads.go:43`;
  `orders.Store.Cursor` at `:378`). Their unexport is gated on the WI-3 two-class
  graph wiring (`resolveGraphStore` into `orderFrontDoorsForStores`) — NOT on this
  remainder. Keep them exported; do the orders unexport in a dedicated WI-7 orders
  sub-slice.
- **`InfoFromPersistedBead` — the hard one.** After R1–R5 it reaches its irreducible
  tail: the 3 tick-collection edges (`session_reconciler.go:583/:1342/:1419`) plus
  whatever R5 cannot cleanly migrate (`session_logs_resolve.go` feeds
  `ResolveCodexTranscriptBySessionOrder([]beads.Bead)` — an edge signature that takes
  raw beads; `build_desired_state.go:2380/:2631` sweep projections;
  `internal/api/session_resolution.go` retire lane). See §5c for the concrete plan.

### 5b. The front-door flip (`cmd/gc/class_store.go` + `internal/api` State)
`sessionFrontDoor(store)` (`session_beads.go:1940`) already constructs a fresh
`*session.Store` per call from `session.NewStore(beads.SessionStore{Store: store})` —
a stateless one-field wrapper (verified). The flip:
- `cmd/gc/class_store.go`: the `sessionsBeadStore()`/`ordersBeadStore()`/
  `nudgesBeadStore()`/`mailBeadStore()` accessors (on both `controllerState` and
  `CityRuntime`) return `beads.XStore` wrappers today. Flip them to domain-store
  front doors (`sessionsFrontDoor() *session.Store`, `ordersFrontDoor() *orders.Store`
  — `orders.Store.Get` exists, `:43`; `nudgesFrontDoor() *nudgequeue.Store` —
  `Find`/`FindIncludingTerminal` exist, `internal/nudgequeue/store.go:356/:368`; mail
  via `newCityMailProvider`, `class_store.go:288`), built from the `resolve*Store`
  outputs (`resolveSessionStore:269`, `resolveOrderStore:253`, `resolveNudgesStore:261`,
  `resolveGraphStore:278`) — wrapping the EXACT cached store value so the #4017
  create-side capability assertions (`GraphApplyFor`/`HandlesFor`/`Counter`) still pass
  (spec §7). Per-call construction is safe (stateless wrappers).
- `internal/api`: `state.go:146` exposes `SessionsBeadStore() beads.SessionStore`;
  the flip adds a typed `session.Store`-returning accessor and moves the 15+ handler
  sites off the raw wrapper — in one motion per class (spec §7). The permission-mode
  raw lane (`huma_handlers_sessions_command.go:486` `// WI-6 residual`) and the mail
  identity resolver (`handler_mail.go:218` `// WI-6 residual`) migrate here.
- Every read the flip moves to a per-class `Get` MUST bridge the front-door-Get
  contract (§6).

### 5c. Concrete plan for `InfoFromPersistedBead` → true interior zero (or sanction)
The 3 tick-collection edges are the deciding factor. `session_reconciler.go` is a
**mixed work+session file** that the spec (§6 tier 2) explicitly keeps OFF
`frontDoorStoreFreeFiles` with an in-code census — so a `beads.Bead` occurrence there
is allowed, but a `InfoFromPersistedBead(` CALL is what blocks unexport.

Two options, honestly:
- **Option 1 (drive to zero — recommended IF R4 lands):** After R4 deletes
  `startCandidate.session`/`wakeTarget.session`, the tick no longer needs raw
  `*beads.Bead` pointers for the write helpers. Add
  `session.Store.ListAllForReconcile(opts) []Info` in the edge (it is just the
  existing `ListAll` union) and reshape the tick entry so `:1342` loads `[]Info`
  directly and `:1419` builds `infoByID` from that — the raw→Info projection moves
  INTO the edge. `:583` (finalize boundary) then either uses the snapshot Info or, if
  it genuinely re-reads a single closed bead, uses `sessFront.Get`. This reaches a
  TRUE interior zero → `InfoFromPersistedBead` → `infoFromPersistedBead` (unexport).
  Cost: a real reconciler-tick refactor (the `orderedBeads []beads.Bead` slice is
  load-bearing for work-class assigned-work scans — audit whether those scans need
  raw beads; if they hold WORK-class beads that is fine and stays, but SESSION beads
  must not be re-projected via the codec).
- **Option 2 (sanction — the honest fallback):** if the tick-feed refactor is out of
  budget, the 3 sites stay and `InfoFromPersistedBead` stays EXPORTED. The census
  pins those exact 3 hits as a **permanent count (not zero)** with a header rationale
  ("tick-collection edge: the reconciler's once-per-tick raw→Info projection lives
  here; §5-exempt as class-generic machinery"). This does NOT achieve the compiler
  endgame for `InfoFromPersistedBead` — it is a documented, ratcheted, non-zero pin.

**Recommendation:** attempt Option 1 for `:1342`/`:1419` (pure session-snapshot
construction — high value, they are THE reason the codec stays exported). Sanction
`:583` only if it is a close/finalize boundary that legitimately re-reads a single
bead post-mutation. `session_logs_resolve.go` (feeds `ResolveCodexTranscriptBySessionOrder([]beads.Bead)`)
is a genuine edge-signature hold: either change that internal/session signature to
take `[]Info` (edge-internal, cheap) or add `session_logs_resolve.go` to
`typedClassCodecEdgeFiles` with rationale. `build_desired_state.go:2380/:2631` and
`internal/api/session_resolution.go:1` migrate with the `named_config.go` needle
expansion (below) or sanction.

### 5d. Deferred WI-7 companions (tracked, not this remainder)
- `named_config.go` family needle expansion (`Find*NamedSessionBead`,
  `NamedSessionResolution*`, `ExactMetadataSessionCandidates*`) — add needles +
  migrate the `session_resolution.go`/`session_resolve.go` callers.
- WI-3 two-class graph wiring + orders residuals (`RunFromTrackingBead`/`MaxSeqFromLabels`).
- The permission-mode raw lane (`huma_handlers_sessions_command.go:486`).

### 5e. Guard → permanent-zero-pin conversion
`typedclass_edge_guard_test.go` is a *ratchet* today (increase fails; decrease fails
until the census is re-pasted). WI-7 W7b: for each codec whose census reaches 0 AND
whose interior compiler-unexport lands, DELETE its needle row entirely from
`typedClassCodecNeedles` (the tripwire is now compiler-enforced — the unexported name
cannot be referenced from the interior, so the runtime scan is redundant) OR keep the
needle with a hard `== 0` assertion (belt-and-suspenders). The `frontdoor_di_guard_test.go`
transition lists (`frontDoorStoreFreeFiles`, `snapshotInfoOnlyFiles`,
`metadataInfoOnlyFiles`, `sessionRelocationRoutedFiles`) become permanent (the files
never leave them). For `InfoFromPersistedBead` under Option 2, the census row stays at
its sanctioned non-zero count with a permanent-pin comment.

---

## 6. Front-door-Get contract — flag every moved read

`session.Store.Get` / `GetPersistedResponse` / `GetBeadWithInfo` differ from raw
`store.Get`: they return `ErrSessionNotFound`, wrap with `"loading session %q"`, and
REJECT beads failing `IsSessionBeadOrRepairable`. This bit W2/W3/W5. Every read this
remainder moves to a front-door `Get` MUST mirror the `session_get_read.go:60`
bridge (`bridgeSessionGetError` / `bridgeSessionRecordError`). The moved-Get sites:

| Wave | Site | Move | Bridge needed |
|---|---|---|---|
| R1 | `cmd_stop.go:362` | `ListByLabel` → `ListAll` (or keep `ListByLabel` for byte-identity) | list-tier: `ListAll` already applies `IsSessionBeadOrRepairable`; **widen delta flagged** (§2 R1) |
| R1 | `dependencySessionStartInFlight` | `ListByMetadata` → `ListAll`+filter | list-tier; verify metadata filter equivalence |
| R4 | `refreshAsyncStartResult:1606` | `GetBeadWithInfo` → `GetPersistedResponse` (Info-only) | YES — keeps existing `TestRefreshAsyncStartRejectsNonSessionBead`; sanctioned cross-goroutine freshness re-read, NOT per-patch re-Get |
| R4 | `prepareStartCandidateForCity:780` in-lock re-Get | raw `store.Get`+`InfoFromPersistedBead` → `GetPersistedResponse` | YES — this is the honest whole-bead re-Get (`template_overrides` can change out of band); returns Info directly, killing the `session_lifecycle_parallel.go` codec hit |
| R5 | `cmd_prime.go:600` | `cliSessionStore.Get` (raw) → `sessionFrontDoor().Get` (Info) | YES — hook path; verify `warn(...)` text + codex guard on a foreign/damaged bead |
| R5 | periphery single-Get projections (`session_hash`, `session_template_start`, `session_resolution` retire lane) | raw `Get`+codec → `sessFront.Get` | YES per site — grep each for error-text assertions + not-found handling before swapping |
| WI-7 | the 15+ `internal/api` handler Get sites (front-door flip) | `beads.SessionStore` wrapper → `session.Store.GetPersistedResponse`+`EnrichInfo` | YES — the API 400→500 race W3 hit; bridge each |

**Tick-budget invariant:** `TestReconcileSessionBeadsFastPathGetBudget` pins Gets/tick.
R3 adds ZERO Gets (all `ApplyPatch`/`ApplyPatchInfo` folds on `infoByID`). R4's two
Gets are the already-sanctioned freshness re-reads (no net increase). Do NOT introduce
a "convenient re-Get" anywhere — the guard fails CI.

---

## 7. Realistic assessment

**Wave count: 5 remainder waves (R1–R5) + 2 WI-7 waves (W7a front-door flip, W7b
codec unexport) = 7 waves total.** R5 likely splits into R5a/R5b to honor the
≤5-file rule (10 files touched), so call it **7–8 landings**.

**Mechanical vs risky:**
- **R1 (leaf sweeps + dead-form deletes):** LOW. Info twins mostly exist; the only
  judgment call is the `cmd_stop` label-only-vs-union widen (recommend byte-identity).
- **R2 (display lane + sleep-read twins):** MEDIUM. Additive twins are safe; the
  display equivalence + `MetadataState`-vs-`State` trap need oracle coverage. Deletes
  `sessionMetadataState` + the raw wake-reason forms.
- **R3 (heal + sleep coordinated unit — DROPS BOTH TRANSITIONAL MIRRORS):** HIGH. This
  is the wave most able to reproduce the W6 fail-safe drift: ~10 reader sites across 3
  files must flip in one Commit B so mirror #1 and mirror #2 drop together. Deletes the
  whole pending-create lease classifier family + raw sleep forms + `sessionStartRequested`/
  `staleCreatingState`. `persistSleepPolicyMetadata` swallow contract must survive.
- **R4 (start-execution — DROPS 4 COUPLING MIRRORS + `startCandidate.session`/
  `wakeTarget.session`):** HIGH. Wake-fairness, value-Info staleness, cross-goroutine
  `commitStartFailure` fold discipline, the two sanctioned re-Gets. Deletes
  `shouldRollbackPendingCreate`/`runningSessionMatchesPendingCreate`/`asyncStart*` raw.
- **R5 (periphery holds):** MEDIUM. Mostly mechanical; the snapshot raw-half deletion
  is load-bearing (constructor-equivalence pin). Adds `Info.BuiltinAncestor` (§4).
- **W7a/W7b (flip + unexport):** MEDIUM. The flip is a wide-but-shallow accessor swap;
  the risk is the front-door-Get bridge across 15+ API sites (§6).

**Which mirrors/fields/classifiers die where (summary):**
- Mirror #1 (`quarantined_until`) + Mirror #2 (5 keys) → **R3**.
- 4 start-execution coupling mirrors + `startCandidate.session` + `wakeTarget.session`
  + `clonePreparedStartForAsync` bead deep-copy → **R4**.
- `sessionMetadataState` + raw wake-reason forms → **R2**. `sessionStartRequested`,
  `staleCreatingState`, and the entire pending-create lease family + raw sleep forms →
  **R3**. `shouldRollbackPendingCreate` + `runningSessionMatchesPendingCreate` +
  `asyncStart*` → **R4**. Each with its oracle sibling in the same Commit B.
- Two DEAD forms (`pendingCreateSessionStillLeased`, `pendingResumePreservingNamedRestart`)
  → **R1**.

**Honest `InfoFromPersistedBead` unexport verdict:** The OTHER session codecs
(`SessionInfoFromBead`, `GetWithBead`, `SessionByLoadedBead`,
`ResolveSessionBeadByExactID`, `ListFullFromBeads`, `PersistedResponseFromBead` once
0, `WaitInfoFromBead` after the /v0/waits window, `ListAllSessionBeads`,
`PollerKeyFromBead`) unexport CLEANLY after R1–R5. **`InfoFromPersistedBead` does NOT
reach a true interior zero for free** — it bottoms out on the 3 tick-collection edges
(`session_reconciler.go:583/:1342/:1419`) plus 2–3 edge-signature holds. Full unexport
is achievable ONLY via the Option-1 tick-feed refactor (add
`session.Store.ListAllForReconcile() []Info`, reshape the tick to hold `infoByID`
from the edge, retire `orderedBeads`' session-projection). If that refactor is funded,
`InfoFromPersistedBead` unexports. If not, the honest outcome is a **permanent
sanctioned census pin at count 3** (Option 2) with `session_reconciler.go` +
`session_logs_resolve.go` documented as the tick-collection / codex-transcript edge —
and `InfoFromPersistedBead` stays exported. Do not paper over this: it is the one hard
residual, and the decision (fund the tick-feed refactor vs sanction) is the single
open call the impl lead must make before W7b.

**Orders codecs** (`RunFromTrackingBead`, `MaxSeqFromLabels`) are NOT in this
remainder's reach — they need the deferred WI-3 two-class graph wiring first.
