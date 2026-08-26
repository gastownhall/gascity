# Legacy-ownership ledger — every way a row stays legacy's under `auto`

Owner-directed systematic audit (Julian, 2026-08-26): *"why aren't you taking
every legacy classification and adding a keyed version of it?"* This document
is that enumeration. It lists **every distinct way a session row or action can
end up legacy-owned** (or owned by nobody) while `daemon.session_reconciler =
"auto"`, and for each: whether a keyed version exists (with the handler site),
whether one is NEEDED (bead filed), whether the arrangement is
designed-until-WE (coexistence lattice, retires with the cutover), or whether
the legacy arm simply dies at WE with nothing to port.

The bar this ledger must meet: **at WE, someone can walk it row by row and
prove nothing is orphaned.** Every row cites its enumeration source
(file:line, campaign class, or fleet census) and its tracking bead where one
exists.

Companion documents: `engdocs/plans/reconciler-distillation/DETECTOR.md`
(§1 site dispositions, §3b divergence classes, §5 WE deletions),
`WE-SIGNOFF.md` (§3 classification table, §5.2 orphan decisions),
`DESIGN.md` (§3 ledger, §7 owner decisions). Field evidence:
ga-f7v2ft.161 soak censuses (2026-08-24 v1, 2026-08-25/26 v2–v3) and the
WD.15 campaign readout (`evidence/we-final-yield-join.json`).

## How to read the disposition column

- **EXISTS@site** — a keyed handler/mechanism owns the shape; the citation is
  the production site. Legacy involvement is at most the designed coexistence
  hand-back.
- **DESIGNED-UNTIL-WE** — the row is legacy's *on purpose* while both engines
  coexist (yield lattice, capability fallback, admission refusal contract).
  The arrangement retires at WE; the row names what replaces it.
- **DIES-AT-WE** — the legacy arm/record has no successor and needs none;
  it dies with the god function (`reconcileSessionBeadsTracedWithNamedDemand`,
  cmd/gc/session_reconciler.go) per DETECTOR.md §5.
- **NEEDED** — no keyed counterpart exists and either one must be built or a
  retirement must be *recorded, not defaulted* before WE. Every NEEDED row has
  a P1/P2 bead.

---

## 0. Entry #1 — the poolDesired demand-derivation circularity (win3's; RESOLVED 2026-08-26, ported)

| # | Class | Trigger | Keyed version | Evidence | Bead |
|---|---|---|---|---|---|
| 1 | **Pool demand derived from session existence** — a keyed stop erases its own regeneration demand | Pool desired-state (`cmd/gc/pool_desired_state.go` `ComputePoolDesiredStates*`, fed by `cmd/gc/build_desired_state.go`) derives fill demand from open session rows / live snapshot; when the keyed engine stops or closes a seat, desired drops with it, so the pool-fill arm sees no deficit to materialize | EXISTS@site for the *arms* — pool-fill materialize `cmd/gc/pool_allocation_controller.go:1261-1278` (ungated, `effect_owner=keyed` hardcoded) and start commit (`TraceSiteLifecycleStartCommit`, `cmd/gc/session_start_reconcile.go`, owner-stamped since `a29e5e3142`) — but the *demand producer* is the circular part. **win3's team is actively working this ONE instance — do not touch; cross-reference their findings on ga-f7v2ft.161 when they land** | Both always-on sinks read **ZERO fleet-wide across the entire soak**: census v1 (2026-08-24, C3: "honest NO-DEMAND") and census v3 (2026-08-26 Table B: pool-fill materialize 0, start commit 0, "no-demand"), while D-DRAIN/D-ORPHAN/D-STRANDED applied 97 effects on the same fleet. Adjacent field shapes: the `.194` Finding-3 / asleep-identity deadlock ("session name already exists … (skipping)" with poolDesired=1..2 and ZERO tmux sessions, ga-f7v2ft.161 2026-08-23) shows the allocator side of the same identity/demand coupling | ga-f7v2ft.161 (win3 coordination channel — their findings land there) |

### Disposition: RESOLVED — five fixes, all ported to OSS (2026-08-26)

win3's findings landed as the `6o`→`6s` deploy chain on maintainer-city. The
circularity was never one defect: it was a demand-side blindness plus a
four-arm release wedge that kept the drained seats from ever coming back. Both
halves reproduce on the OSS lane — `cmd/gc/pool_allocation_controller.go` and
`cmd/gc/pool_allocation_shadow.go` were **byte-identical** (md5) to the
enterprise pre-chain baseline, so none of this is enterprise-only.

| Arm | Defect | OSS fix |
|---|---|---|
| Demand | `filterAssignedWorkBeadsForPoolDemand` matched a work bead's census ref against the agent's configured **rig**, so graph-resident work carrying a relocated binding's `class:*` ref was structurally invisible. The wake filter got this widening in ga-whzrt; the demand half never did — the city drained a slot and then refused to refill it | `assignedWorkIndexReachableFromAgentOnClaimRefs` + `assignedWorkRelocatedClaimRefs` (empty for a single-store city, so unsplit cities stay byte-identical) |
| Release 1 | `authorizeRoutedWorkPoolDrainAck` gated a **release** on the **allocation** predicate `supported()`, which is false for every capacity shape. Any city with `[workspace] max_active_sessions` (or a min floor, rig cap, or namepool) refused *every* agent drain-ack it ever saw, permanently | `releaseEligible()` reads `contributionPresent`; the singleton and source-store clauses lifted out of `supported()`'s early return |
| Release 2 | The source-store clause used agent-rig equality, but a city that relocates its work class serves claims at **city** scope — `rig:<name>` never equals `city:<name>`, so every rig-scoped member's ack refused forever | `routedWorkStoreRefConfigured` — the pure-config half of `routedWorkStore`; asks the CITY's reachability |
| Release 3 | The work-closed proof read the trigger from the raw work store; `gcg-` triggers live in the graph binding, so `ErrNotFound` surfaced as *unavailable* and retried forever | `ErrNotFound` fallback through the graph binding at city scope (no-op on a single-store city, where `graphStore == sourceStore`) |
| Release 4 | Triggers **deleted** after scope finalize are permanently absent, but absence was reported as transient-unavailable and retried forever | a deleted trigger satisfies the work-closed check; `sessionHasAwakeAssignedWorkForReachableStore` remains the fence |

Field evidence (maintainer-city, live): 26 rows wedged in `draining` holding 26
distinct slot names, and a lifetime `drain_ack_source` split of 271
keyed-superseded against **zero** agent acks, ever — the only path that had ever
finalized a drain in that city was the pane dying. That is the sink behind rows
46/47 reading true-zero fleet-wide: the arms were never no-demand, the demand
could not be seen and the seats could not be given back.

Held back deliberately: the enterprise `edcaf9619` ByID-federation refactor of
the release-3 fallback. It fixes no observed defect, swaps a narrow live-verified
two-leg rule for a broad multi-leg resolver, adds a new `storeref.Plan` error
path onto drain-ack authorization, and is **undeployed** — the fleet tip is the
release-4 commit. Revisit once it has fleet time.

---

## 1. Handler yield/park lattice — every `yieldOrPark` / `errSessionStartLegacyFallbackRequired` reason

Under `auto`, each of these returns `exactSessionStartLegacyOwner` (the row is
handed back for legacy to act on this tick); under `require` the same reason
parks the key for re-admission. The tri-state + 24 legacy-owner returns are
themselves scheduled deletions (DETECTOR.md §5 "call-site scaffolding").
Each distinct REASON string is a class.

| # | Class (reason) | Trigger | Keyed version | Evidence | Bead |
|---|---|---|---|---|---|
| 2 | **Active legacy drain fence** — "has an active legacy drain" (+ "entered an active legacy drain before stop") | A legacy-initiated drain is in the drainTracker for the key when a keyed handler wants it: deadline `session_deadline_reconcile.go:275,:348`, orphan-close `session_orphan_close_reconcile.go:125`, stale-create `session_stale_create_reconcile.go:86`, stranded `session_stranded_reconcile.go:127`, suspended-stop `session_start_reconcile.go:1991,:2032` | DESIGNED-UNTIL-WE. The fence exists only because two engines share the tracker; at WE all drains are keyed-initiated and the fence has no referent | Coexistence census (DETECTOR.md, `withLegacy*` bridges ~400 LOC); zero field yields observed (yields invisible on unarmed cities — census v3 caveat) | dies with WE deletion ledger (WE-SIGNOFF §5.1) |
| 3 | **Provider cannot prove fresh liveness** (`FreshLivenessObserver` unimplemented) | subprocess/acp/k8s/exec/ssh providers: deadline `:278`, orphan-close `:128`, stranded `:130`, suspended `session_start_reconcile.go:1994`, sleep-drain `session_sleep_reconcile.go:188`, orphan-drain `session_orphan_drain_reconcile.go:126`, drain-advance `session_drain_reconcile.go:106`, drain-ack stop `session_start_reconcile.go:2252` (with rollback) | DESIGNED-UNTIL-WE → **typed refusal at WE** per owner decision **D2** (DESIGN.md §7: hard provider-capability requirement, no degraded mode). Until WE, incapable-provider cities are wholly legacy-owned for lifecycle stops | DESIGN.md §6 trap 4; sweep-side twin is `refused_provider_incapable` (row 12) | D2 (recorded owner decision); capability contract program ga-f7v2ft.164 |
| 4 | **Provider cannot prove unattended stop** (`UnattendedSessionStopper` unimplemented) | deadline `:281`, suspended `session_start_reconcile.go:1997` | Same as row 3 — D2 typed refusal at WE | same | same |
| 5 | **Liveness observation incomplete** — "liveness observation is incomplete" | `Liveness.Complete = cacheComplete && scanComplete` false: deadline `:333`, orphan-close `session_orphan_close_reconcile.go:179`, stranded `:158`, sleep-drain `session_sleep_reconcile.go:230`, drain-advance `session_drain_reconcile.go:146`, suspended `session_start_reconcile.go:2022` | EXISTS@site — and the gate was the **ga-bxa8r trap**: pre-fix, a LIVE target withheld its own absence license by construction, so six keyed arms silently treadmilled to legacy forever under `auto` (fixed `2310019ca6`); permanently-unreadable/zombie residue then kept it incomplete (ga-lp5w6 fixed `bd620c1605`, .194 fixed `425e319b0b`). Post-fix residue fails closed onto the `.173` bounded escalation — under `auto` that residue is legacy-owned; at WE it is park+escalate | Census v3: 97 applied keyed effects prove the gate now passes on the live fleet; pre-fix soak showed the treadmill (ga-f7v2ft.161, 2026-08-21 stall verdict: 33-row drain pile) | Residue: **ga-9lgxo** (container-cgroup shapes, OPEN), ga-f7v2ft.201 (escalation population, closed), ga-f7v2ft.178 (retry-bound census) |
| 6 | **Conditional writer unavailable / errored** — the fence itself is missing | `beads.RequiredConditionalWriter(snapshot.Store)` fails (`cmd/gc/city_runtime_session_start.go:129`; StatusWriterError consumed at deadline `:290`, drain-ack `session_start_reconcile.go:2118-2121`, pool recovery `:3242-3245`, and every writer-using handler) | DESIGNED-UNTIL-WE, but this is the **silent-degrade trap of the split-store flip**: a store without a conditional writer turns `auto` into a no-op swap (every family yields), flagged in the .161 split-store verdict. WE direction: required store contract at boot (`cmd/gc/store_contract_preflight.go:52-59` `contractConditionalWriter`) instead of per-row capability resolution | .161 OPEN QUESTION (2026-08-19) + SAFE-WITH-CONDITIONS verdict; mc verified to carry sqlite conditional writes | **ga-f7v2ft.164** (de-conditionalize, P1), .164.1, **ga-f7v2ft.165** (served-city ConditionalWriter half-refusal, P1), .193 (fence-capability ERROR→WARN when off), .150/.151 (fidelity/census guards) |
| 7 | **Drain tracker / reset store not wired** | drift restart-in-place `session_drift_reconcile.go:516` ("no store to reset through"), drift drain `:541`, sleep-drain `session_sleep_reconcile.go:175`, orphan-drain `session_orphan_drain_reconcile.go:120` ("no tracker to record drain intent in") | EXISTS@site — defensive wiring guards; on the production controller both are always wired. DIES-AT-WE as legacy-fallback (becomes a hard wiring error) | No field occurrences | WE deletion ledger |
| 8 | **Drain-ack authorization refusals** — `not_agent_stamped`, `member_not_occupied`, `runtime_gone`, `unavailable`, and the 17 `lease_invalid/*` sub-codes | `authorizeRoutedWorkPoolDrainAck` (`cmd/gc/pool_allocation_controller.go:105-152` vocabulary; policy arm `:440-442` `policy_unsupported`); refusal → `handed_back=true` (`session_start_reconcile.go:1645`) → legacy `:2102` | EXISTS@site — and field-proven to be a **routing boundary, not a terminal yield**: the drain-ack arm declines on policy and D-ORPHAN close→drain picks the row up (census v3 Y1: all 3 mc refusal rows resolved KEYED, incl. a >26h stuck row; fleet `refused_and_unresolved = 0`). One sub-code is special: **`not_agent_stamped` is "the only one the auto-mode legacy fallback may still serve"** (`:107-110`) — at WE that server disappears (see row 30) | Census v1 C2 (13 refusals, `lease_invalid/policy_unsupported`), census v3 Y1 resolution ledger; live refusal-cycle specimen ga-f7v2ft.199 | **ga-f7v2ft.199** (supersede-fence refusal cycle, P2); row 30 for the WE decision |
| 9 | **Drain-ack post-transition rollback → legacy** | Stop-pending patch applied, then a precondition fails; `drainAckStopPendingRollback` restores and hands back (`session_start_reconcile.go:2139`; rollback `:1696`) | DESIGNED-UNTIL-WE — the rollback exists so legacy can still serve the ack this tick. Explicit WE deletion (DETECTOR.md §5: `drainAckStopPendingRollback` `:997-1046`) | Campaign D-DRAIN family 100% (3 skews all twin-proven, owner-signed class) | WE deletion ledger |
| 10 | **Reset recycle refusal → legacy** | The exact reset's own refusal arms (capability gap, incomplete liveness, failed stop/confirm-dead, read error) leave the row reset-current; `auto` returns it (`session_start_reconcile.go:2660`) | EXISTS@site (keyed reset recycle, .103 machinery); refusal → legacy is lattice-only, DIES-AT-WE (park + re-admission) | Census v3: reset stop 0 applied = no-demand (trigger-starved, not wedged) | — |
| 11 | **Pool recovery yield** — "exact pool recovery yielded" | Recovery of an in-flight allocation fails a precondition (`session_start_reconcile.go:3239`, `fail()` at `:3235-3240`); also the allocation hint channel dropping on overflow (`pool_allocation_controller.go:393-398`) | EXISTS@site; overflow recovery is **Q2-RESOLVED**: census-owed sweep re-detection of unallocated routed work (`ReadyLive` promoted to a declared sweep input), WD.10b | DETECTOR.md §6 Q2 resolution (ga-f7v2ft.117) | — |
| 12 | **Missing exact identity** — "stop lacks exact session identity" (empty `session_name` / `instance_token`) | Pre-keyed legacy rows without the durable fence fields (`session_start_reconcile.go` drain-ack stop leg) | DESIGNED-UNTIL-WE: legacy owns rows the keyed engine cannot fence. At WE these must be healed or converge via close/recreate — this is exactly the flip-transition question | Flip-transition convergence is the untested shape | **ga-f7v2ft.176** (flip-transition fixture, P1) |

## 2. Sweep admission refusals — detected but not routed

The sweep is zero-write; a condition it detects but does not route stays
legacy's by contract (`session_detector_sweep.go:2083-2133`).

| # | Class | Trigger | Keyed version | Evidence | Bead |
|---|---|---|---|---|---|
| 13 | **`refused_uncertifiable`** — D-WAKE's own admission declines to route the wake target | `detectorAdmissionRefusedUncertifiable` (`session_detector_sweep.go:392-396`, applied `:2094,:2106,:2129`); contract text: *"the row stays legacy's and is re-detected next sweep"* | **NEEDED at WE** — the contract's second half loses its referent when legacy dies: an uncertifiable wake target would be re-detected forever and started by nobody. Campaign class `wake_admission_refused_row_stays_legacy` (act-vs-non-act, incomparable) blessed the coexistence arrangement, not a WE story | 135 campaign records (WE-SIGNOFF §3); census: mc 39 d-unknown-state shadow records adjacent | **ga-f7v2ft.203 (P1, filed by this audit)** |
| 14 | **`refused_provider_incapable`** | `session_detector_sweep.go:383-388`, applied `:2083` — sweep-side twin of rows 3-4 | DESIGNED-UNTIL-WE → D2 typed refusal at WE | — | D2; ga-f7v2ft.164 |
| 15 | **`refused_error`** — the Admit call itself rejected | `session_detector_sweep.go:389-391`, applied `:2102` | EXISTS@site — re-detected next sweep; retry semantics unchanged at WE | — | ga-f7v2ft.178 (retry bounds) |
| 16 | **`refused_overflow`** — bounded controller queue full | `session_detector_sweep.go:402-406`, applied `:2133` | EXISTS@site — census-owed re-detection (Q2), backpressure not loss | — | — |

## 3. Row-level exclusions — rows neither engine (or only legacy) reasons about

| # | Class | Trigger | Keyed version | Evidence | Bead |
|---|---|---|---|---|---|
| 17 | **Unknown-state rows** — skipped before any family evaluates them | `isKnownStateInfo` (`cmd/gc/session_reconcile.go:1308-1334`); sweep global guard 1 (`session_detector_sweep.go:611-631`, `detector_unknown_state_skipped`); legacy skips the same rows (§1 site 18 UnknownState, `session_reconciler.go:1802-1814`) | **Not legacy-owned — NOBODY-owned**: both engines skip. Whether the class is designed (forward-compat rollback vocabulary) or a state-vocabulary gap is exactly the .200 question. Note the map's own comment reserves `"draining"`/`"archived"` as *examples of unknown newer states* — worth checking against what the fleet actually writes | **78% of evaluated rows fleet-wide** (census v3: 14,916/19,094; mc 3,044/10,217; platform 5,370); did not block any of the 97 keyed effects but bounds what either engine reasons about | **ga-f7v2ft.200** (P2, OPEN) |
| 18 | **Unknown-state throttled diagnostic** — the *stamp* is legacy's | `emitSessionUnknownStateDiagnostic` throttle marker: sweep deliberately does not stamp (comment `session_detector_sweep.go:612`: "The throttled diagnostic itself stays legacy-owned this wave — it stamps"); sweep only counts | **NEEDED** — same shape as .159: a legacy-owned diagnostic with no keyed owner. When the god function dies the escalating unknown-state diagnostic stops firing silently | `TestEmitSessionUnknownStateDiagnostic_ThrottlesAndEscalates` | **ga-f7v2ft.204 (P2, filed by this audit)** |
| 19 | **Unrevisioned rows** — `response.Revision == 0` | Every keyed candidate check declines: orphan-close `session_orphan_close_reconcile.go:38`, drain `session_drain_reconcile.go:37`, stall `session_stall_reconcile.go:76`, dup `session_dup_reconcile.go:38`, zombie `session_zombie_reconcile.go:63`, start `session_start_reconcile.go:1215,:1667` | DESIGNED-UNTIL-WE — a row the store handed no revision for cannot be fenced, so legacy owns it. Resolved by the same program as row 6: with the required store contract, every read carries a revision. (Signed-revision trap already fixed: `beads.RevisionKnown`, ga-f7v2ft.141) | — | ga-f7v2ft.164 |
| 20 | **`storeQueryPartial` cycles** | Sweep global guard suppresses affected families (fail-closed); legacy records Closed-without-closing (`session_reconciler.go:1987-1991,:2284-2288`) | EXISTS@site — the suppression IS the keyed behavior; legacy's phantom-close record DIES-AT-WE | Campaign class `store_query_partial_legacy_only` (bounded to partial-view cycles) | — |
| 21 | **Running-set unavailable** — whole-family fail-closed skip | `detector_running_set_unavailable` (D-ORPHAN family skip when the provider running-set read fails) | EXISTS@site — fail-closed, re-detected; proven live through the day-6 tmux-server outage | Campaign class `orphan_running_set_unavailable_fail_closed` (1 record, in-band proof) | — |
| 22 | **Provider liveness unknown** | `detector_provider_liveness_unknown` skip (`session_detector_sweep.go:206`) | EXISTS@site — fail-closed re-detection | — | — |
| 23 | **Template-not-in-detail-scope** (phantom class — observability, never ownership) | Per-session keyed records dropped when the row's template had no detail arm (`session_reconciler_trace_collector.go:717-776`); made the whole soak read "keyed did nothing" | Ownership was never affected; **FIXED for applied effects** — `cycle.recordKeyedEffect` puts `effect_applied=true` on the always-on tier (`4c06ee66a2`/`a29e5e3142`), plus `effect_owner=keyed` stamps for status-heal, reset stop, suspend stop, start commit. Refusals/yields remain detail-gated: yield visibility on an unarmed city is still zero (accepted residue, census v3 caveat) | Census v1: 59,447/59,486 records discarded on the unarmed fleet; v3: 97 applied effects visible fleet-wide unarmed | closed by the observability fix; residue noted in .161 |
| 24 | **Fleet-scan-skips-admitted-rows** (the inverse class: keyed-admitted rows are invisible to legacy) | Under `auto` the legacy fleet scan skips rows the keyed controller holds an admission for (`a0815625bb`: `fillAwakeNamedSessionWorkQueue` / `PublishWakeEvaluations`; `session_legacy_wake_standdown_test.go`) | EXISTS@site — admission implies ownership. The hazard is an admission that *soaks* (keyed neither acts nor releases): pre-fix that force-stopped drain rows (.179) and an attached session (`c0306558fe` re-pay, council B2). Guards: the awake-set projection shared to the keyed advance, `exactSessionUserAttached` consulted by cancel arm 3, and the `.173` bounded escalation | .161 council B2 correction; DETECTOR.md §3b D-DRAIN row as rewritten at `c0306558fe` | ga-f7v2ft.199 (live refusal-cycle specimen), .178 |
| 25 | **Orphan kept-open arms** — assigned work / suspend-deferred | `detector_orphan_assigned_work` (kept open), `detector_orphan_suspend_deferred` (`session_detector_sweep.go:176-177`); legacy kept-open arm `session_reconciler.go:2204` | EXISTS@site — designed non-action on both sides (A6-adjacent). Caveat: the assigned-work *input* on split stores scans the sessions binding as the "primary work store", so the blocker can be derived from the wrong ledger | ga-f7v2ft.163 finding | **ga-f7v2ft.163** (P2) |

## 4. Whole-mechanism gaps — legacy behaviors with NO keyed counterpart (the WE ledger)

| # | Class | Trigger | Keyed version | Evidence | Bead |
|---|---|---|---|---|---|
| 26 | **Stranded diagnostic marker ownership** | `stranded_event_emitted_at` stamped only by `emitSessionStrandedDiagnostic` in the god function's wake/sleep phase (+ `clearStrandedEventMarker` alive-tick clear, `ApplyOpenInfoPatch` carrier, `session.stranded` event); D-STRANDED *keys on* the marker but cannot own it | **NEEDED (tracked)** — keyed arm inherits the marker; at WE the stamp/clear/event must move or D-STRANDED loses its entry condition | WD.14 delta 3; D-STRANDED repair field-POSITIVE (census v3: 13 applied) so the dependency is live | **ga-f7v2ft.137** (P1) |
| 27 | **Sibling clean-close arm** — `poolFreeable && !hasAssignedWork` close with preserved `sleep_reason` | `session_reconciler.go:4408` (arm), from the asleep-identity/wake-sleep phase; no detector, does not yield | **NEEDED (tracked in .137)** — WD.14 delta 4: legacy-owned, unported. The asleep-dead-undesired *holder* shape is otherwise D-ORPHAN's (verdict on .161, 2026-08-23: "not a coverage hole"), but this specific clean-close spelling has no keyed owner | WD.14 delta 4; `session_stranded_reconcile.go:31` cites it as the sibling arm | **ga-f7v2ft.137** (explicitly in scope) |
| 28 | **Reset-stall alarm** — `events.SessionResetStalled` | Sole emitter `recordResetStallIfDue` (`session_reconciler.go:203-266`), called from the god function's row scan; fires for NOT-alive rows whose committed reset outlived startup timeout; zero mutation | **NEEDED (tracked)** — populations disjoint from D-STALL by construction (`detectStall` returns unless alive); at WE the "reset committed, session never came back" signal is silently lost unless ported onto .103 or the event retired from `events.KnownEventTypes` — recorded, not defaulted | Campaign: 8 records, class `reset_stall_alarm_no_detector_arm`; field specimen cycle-11e1730d6990ad8d | **ga-f7v2ft.159** (P2) |
| 29 | **Wake-failure accounting** — `checkStability` / `checkChurn` quarantine accrual | God-function forward pass; keyed side records failures via `commitStartFailure` but the *accrual* (quarantine/rate-limit accounting) is legacy's | DESIGNED-UNTIL-WE by architect ruling (.137 notes, 2026-08-10): moves wholesale with its own REDs at WE, or retires with the breaker redesign — decided at WE, not before | WD.10b deltas item 5 (deferred with stand-downs owed) | **ga-f7v2ft.137** (ruling recorded there) |
| 30 | **`not_agent_stamped` drain-ack service** | An acknowledgement with no agent provenance (older agent CLI, reconciler-authored marker) — "the only genuinely unprovable ack, and the only one the auto-mode legacy fallback may still serve" (`pool_allocation_controller.go:107-110`) | **NEEDED** — at WE the legacy server disappears; the ack must be terminally refused with escalation, healed, or grandfathered. Recorded decision required | Vocabulary comment is the contract | **ga-f7v2ft.206 (P2, filed by this audit)** |
| 31 | **Alive-runtime pending-create recovery** | Legacy defers pending-create recovery *only* for a row whose runtime is ALIVE (`session_reconciler.go:3117-3125`); `detectStaleCreate` excludes exactly those rows (`session_detector_sweep.go:1272-1274`) — the legacy-only record is structural | **NEEDED** — campaign class `live_runtime_recovery_excluded_from_sweep` (incomparable) blessed coexistence; at WE nobody recovers/defers the alive-runtime pending-create shape unless the exclusion is re-derived keyed-side or consciously retired | 9 campaign records (WE-SIGNOFF §3) | **ga-f7v2ft.205 (P2, filed by this audit)** |
| 32 | **Legacy quarantine skip (untraced)** | `session_reconciler.go:3702-3705` skips quarantined rows with no trace record | EXISTS@site for the successor: expired hold/quarantine heal at wake admission (WD.13, D-DUP/D-WAKE admission clear); the untraced legacy skip DIES-AT-WE | Campaign class `untraced_legacy_quarantine_skip` (detector-present/legacy-absent, expected) | — |
| 33 | **Legacy pending-interaction idle deferral** | Legacy defers an idle stop on a pending-interaction probe (probe-only signal) | EXISTS@site — the keyed deadline ladder has its own deferral arm (`detector_deadline_deferred`; hold/quarantine/work blockers in `DecideIdleTimeout`); the legacy probe arm's *provider-I/O spelling* dies at WE | Campaign class `legacy_pending_interaction_deferral` (D-DEADLINE expected divergence) | — |
| 34 | **Legacy rollback-budget deferral** | Legacy defers stale-create rollback beyond per-tick budget #6+ (`:1696` counter) | DIES-AT-WE — R6: budget retired, keyed rolls back by exact key (`session_stale_create_reconcile.go`) | Campaign class `legacy_defers_rollback_beyond_budget` | — |
| 35 | **Fleet-only no-wake verdict** (D-SLEEP) | Legacy drains on its end-of-tick fleet pass where the sweep's pre-tick snapshot predicts nothing | EXISTS@site — keyed sleep-drain (`session_sleep_reconcile.go`) + the published `awakeSetToWakeEvals` projection closed the fleet-cancel gap (`a0815625bb` + attached at `c0306558fe`); the residual snapshot-timing window DIES-AT-WE (only one view remains) | Campaign: 136/136 D-SLEEP records incomparable-by-design (`fleet_only_no_wake_left_to_legacy`), all a restart transient; WE-SIGNOFF §4.1: D-SLEEP has NO comparable campaign evidence — its parity claim is the WD-slice corpus | — |
| 36 | **Deadline min-floor rung missing** | Keyed deadline arm has no min-floor exemption rung: idle floor members enqueue-and-defer every sweep (legacy's `floorExempt` arm, §1 site 14) | **NEEDED (tracked)** — the keyed gap is filed; legacy's exemption survives until then | ga-f7v2ft.169 | **ga-f7v2ft.169** (P2) |
| 37 | **Provider-health respawn gate / circuit persistence** | Legacy respawn-gate `continue` (`:3755-3766`); breaker restore/persist | EXISTS@site — keyed gates at `session_start_reconcile.go:819-821,:1846-1858`; sweep hydration + handler-side persists (WD.11); zombie mark keyed (`session_zombie_reconcile.go`) | WD.11 deltas; `TestReconciler_CircuitOpenBlocksSpawn`; census: D-ZOMBIE no-demand (off/quiet) | — |
| 38 | **Legacy exit-classification sibling writer** | `checkRateLimitStability` writes the same terminal-error cluster on a keyed-owned row and does NOT yield | DESIGNED-UNTIL-WE — read-shared, CAS-loss-tolerant sibling writer (WD.11 delta 4 precedent); dies with the god function | Campaign D-ZOMBIE expected divergence | — |
| 39 | **BuildDeps / TopoOrder / ForwardPass / AwakeSet phase structure** | God-function phases (§1 rows 20, 21, 24, 25) | DIES-AT-WE (R3) — the sweep IS the forward pass reduced to read-only predicates; `ComputeAwakeSet` survives as a pure library | DETECTOR.md §1 | — |
| 40 | **`keyed_start_owner` seam arms + the whole yield-side vocabulary** | The 11 traced stand-down reasons legacy emits when it steps aside (`parityJoinYieldVocabulary`, `cmd/gc/cmd_perf_parity_join_table.go:862-920`) | These are legacy's *decision records under coexistence* — the instrument, not a gap. DIES-AT-WE with the god function and the D4-retained parity machinery | 77,648 campaign yield-joins | WE deletion ledger |

## 5. Census no-demand arms — keyed EXISTS, field-unevidenced

Fleet soak evidence tiers (ga-f7v2ft.161, censuses of 2026-08-24 → 2026-08-26):
**field-POSITIVE** — D-DRAIN (advance/cancel/complete/ack), D-ORPHAN
(close + close→drain hand-off), D-STRANDED repair: 97 applied effects, 59
rows, 23 driven terminal, 0 stuck. Everything below is the remainder.

| # | Arm | Keyed handler (EXISTS@site) | Why unevidenced | Evidence status | Bead |
|---|---|---|---|---|---|
| 41 | D-DEADLINE idle stop | `cmd/gc/session_deadline_reconcile.go` (idle leg; tracker `idle_tracker.go`) | **UNCONFIGURED fleet-wide** — no city.toml sets `idle_timeout`; arm disabled by config, not wedged | Census v3 D1: clean negative; correctness rests on specimen tests + fail-closed control | one-city config experiment recommended in .161 (win3's call) |
| 42 | D-DEADLINE max-age stop ("the arm with teeth" — credential rotation) | `session_deadline_reconcile.go:210-213` via `memoryMaxSessionAgeTracker.shouldRestart` (`max_session_age_tracker.go:132-136`: disabled at `maxAge <= 0`) | **UNCONFIGURED fleet-wide** — `grep max_session_age` over all six city.toml returns nothing | Census v3 D1: `TestExactDeadlineMaxAgeStopsAliveSessionOnIncompleteScan` + fail-closed control only; campaign D-DEADLINE 99.844% covered the *configured* campaign city | same |
| 43 | Suspend stop (user-hold; ga-bxa8r #2) | `session_start_reconcile.go:1984-2032` (suspended-session stop leg) | no-demand: no `gc session suspend` issued during soak | trigger-starved, not wedged (post-bxa8r) | — |
| 44 | Reset recycle (ga-bxa8r #3) | `session_start_reconcile.go` exact-ordinary-reset leg (`:2650-2668` region) | no-demand: no reset issued during soak | trigger-starved | — |
| 45 | Status-heal / active-alias heal (ga-bxa8r #6) | `city_runtime_session_start.go:333-339` (`recordExactSessionLifecycleStatusApplied`, always-on, `effect_owner=keyed` since the observability fix) | true-zero (ungated, so the zero is a real measurement) | census C3 | — |
| 46 | D-WAKE pool-fill materialize | `pool_allocation_controller.go:1261-1278` (ungated always-on) | true-zero — **this is entry #1's demand-side circularity** (win3's) | census C3 + v3 | ga-f7v2ft.161 (win3) |
| 47 | Start commit | `TraceSiteLifecycleStartCommit`, `session_start_reconcile.go` (shared start wave `executePlannedStartsTraced` → `commitStartResultTraced`, `session_lifecycle_parallel.go:2305/:2881`; owner-stamped since `a29e5e3142`) | true-zero — the other half of entry #1's pair | census v2 S1 / v3 | ga-f7v2ft.161 (win3) |
| 48 | D-DRIFT converge + defer | `session_drift_reconcile.go` (converge `:397+`, A6 deferral rungs keyed since WD.9) | no-demand: no config drift during soak windows | v59 journey debt open | ga-f7v2ft.134 (journey) |
| 49 | D-STALL recycle | `session_stall_reconcile.go` | off-by-default (`progress_stall_timeout` unset ⇒ gate 0) — the signed **Q3 test-only-parity** case | Q3 owner-signed 2026-08-12 | .169 (min-floor rung) |
| 50 | D-DUP retire + expired-timer heal | `session_dup_reconcile.go` | no-demand (no duplicate named rows in window) | campaign: none expected | — |
| 51 | D-ZOMBIE terminal mark | `session_zombie_reconcile.go` | no-demand (no running∧!alive rows in window) | `TestKeyedAppliedEffectPersistsOnAnUnarmedFleet` proves the record path | — |
| 52 | D-STALE-CREATE rollback | `session_stale_create_reconcile.go` | 39 detector-shadow records in v1 were **pre-routing** (preserve arm, `predicted_effect: none`); no rollback demand since | census C1; family-split class row 31 | — |
| 53 | D-STRANDED journey debt | handler field-POSITIVE (13 applied) but the v59 managed-Dolt journey is still owed | — | ga-f7v2ft.136 | **ga-f7v2ft.136** (P1) |

---

## Disposition counts

Over the 52 counted rows (row 1 is win3's active instance, excluded):

- **EXISTS@site**: 30 (rows 5, 7, 8, 10, 11, 15, 16, 20, 21, 22, 23, 24, 25, 32, 33, 35, 37, 41–53). Rows 7, 20, 32 also carry a legacy twin that dies at WE.
- **DESIGNED-UNTIL-WE** (coexistence lattice; retires with a named successor): 10 (rows 2, 3, 4, 6, 9, 12, 14, 19, 29, 38). The NEEDED halves of 6/19 ride ga-f7v2ft.164/.165, of 12 rides .176.
- **DIES-AT-WE** (pure deaths, no successor needed): 3 (rows 34, 39, 40)
- **NEEDED** (port or recorded decision required before WE): 9 — **already tracked**: .137 (rows 26, 27), .159 (row 28), .200 (row 17), .169 (row 36); **newly filed by this audit**: ga-f7v2ft.203 (row 13, P1), ga-f7v2ft.204 (row 18, P2), ga-f7v2ft.205 (row 31, P2), ga-f7v2ft.206 (row 30, P2)

**The WE walk**: at cutover, resolve rows 13, 18, 26, 27, 28, 29, 30, 31
(recorded decisions), confirm rows 2, 9, 12, 19, 34, 39, 40 die with the god
function, confirm D2 typed refusals replace rows 3, 4, 14, and confirm the
row-6/19 store contract is boot-enforced. Everything else is keyed-owned
already; the census tiers above say which arms carry field proof and which
rest on the test corpus.

*Audit: Fable, 2026-08-26, lane `rec/ga88-continue`. Entry #1 was win3's; their
poolDesired findings landed on ga-f7v2ft.161 the same day as the `6o`→`6s`
chain and are now ported — see its disposition section above.*
