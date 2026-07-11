# W-test-fixture — live PROGRESS log (this wave)

Append-only status for the final wave. Pairs with `W-TESTFIXTURE-HANDOFF.md` (the plan)
and `test-double-migration-plan.md` (the categorization). Branch
`refactor/store-domain-objects`, LOCAL/UNPUSHED (`git push` only).

## Landed so far (each verified; batch 1 red-team = APPROVE, 0 blockers)
| tip | what | codec sites |
|---|---|---|
| `c77230e8b` | **Phase 0** — `internal/session/sessiontest/` (`Store`/`Info`/`SeedBead`/`InfoFromMeta`) + `reconcilerTestEnv.sessionInfo`/`createSessionInfo`; byte-identity pins | foundation |
| `b0dac0708` | **Pilot** — `session_reconciler_drift_defer_test.go` | 15 → 0 |
| `47a395447` | **Batch 1** — `session_reconciler_test.go` (24→0, incl. 3 struct-literal), `session_reconciler_drift_resume_test.go`+`session_reconciler_trace_integration_test.go` (7→0) | 31 → 0 |
| `d5f76a6f1` | plan-doc SeedBead signature fix (red-team nit) | — |
| `86bc8a587` | **Batch 2** — `session_lifecycle_parallel_test.go` (89→0: 53 SeedBead-on-local + 36 struct-literal), `session_reconcile_test.go` (48→0 via `seedSessionInfo`), `session_wake_test.go` (29→1: `wakeInfo` + 10 SeedBead; 1 adapter deferred) | 166 → 1 |
| `b3bc66e05` | Batch-2 red-team **APPROVE-with-nits (0 blockers)**; nit fixed (`wakeInfo`→`seedSessionInfo` delegate) | — |
| `716ef1826` | **Batch 3** — build_desired_state (24→0), telemetry (17→0)+compute_awake_bridge (14→0), model_phase0_rare_state (14→0)+lifecycle_chaos (11→0). Red-team **APPROVE-with-nits (0 blockers)**; nit fixed (divergence comment). | 80 → 0 |
| in flight | **Batch 4** (base `939c9affe`) — cmd_session(11)+assigned_work(8)+fork_launch(5)+pool_replacement(2) [w4-cmd DONE `ccca708fa`, 25/26, 1 deferred `sessionInfosFromBeads`]; tail cluster of 8 small files (20) [w4-tail running] | ~46 sites |

## KEYSTONE FINDING (adjusts the plan's categorization)
`MemStore.Create` unconditionally rewrites **ID→gc-N, Status→open, CreatedAt→now**
(`internal/beads/memstore.go:76`), and `session.CreateSessionInfo` inherits that. So
verbatim fixture fidelity exists ONLY at construction via `beads.NewMemStoreFrom`, NOT
via Create. Consequences the downstream waves MUST honor:
- `sessiontest.SeedBead(t, b)` seeds VERBATIM (throwaway `NewMemStoreFrom` + front-door
  Get) — that is the only store-double route that preserves a pinned id / Status=closed /
  custom labels / pinned CreatedAt.
- `sessiontest.Info(t, s, spec)` (store-create) yields a STORE-ASSIGNED id — only use it
  when the test reads the returned `Info.ID`, never when it asserts a specific id.
- `CreateSpec.AgentName` drives the `agent:<name>` LABEL only; `Info.AgentName` comes from
  `metadata["agent_name"]`.

## THE DECISION TREE (proven + red-team-approved in batch 1 — use for every site)
For each `session.InfoFromPersistedBead(<bead>)`, by how `<bead>` is obtained:
1. **Cracking a captured LOCAL bead** `InfoFromPersistedBead(localBead)` where the bead is a
   VALID session bead (non-empty ID + session Type/label) → `sessiontest.SeedBead(t, localBead)`.
   **This is the SAFER DEFAULT (batch-2 insight):** `SeedBead(t,X)` is UNCONDITIONALLY ==
   `InfoFromPersistedBead(X)` (verbatim seed of the captured snapshot), whereas
   `sessionFrontDoor(store).Get(id)` re-reads CURRENT store state — which diverges if the test
   later mutates the store on purpose (stale-snapshot tests, e.g. `IgnoresStaleSessionSnapshot`).
   Use `sessionFrontDoor(store).Get(id)` / `env.sessionInfo(id)` ONLY when the intent is to read
   the CURRENT persisted state (operand was itself a fresh `store.Get(id)`, reconciler-env lockstep).
2. **NON-session-shaped bead that is conceptually a session** (from `makeBead` → Type="" no label,
   or `store.Create` default → Type="task"): the front door NARROWS it away, so stamp the type and
   verbatim-seed. **REUSE the existing helpers — do NOT redefine (package-main duplicate = compile
   error):** `seedSessionInfo(b beads.Bead) session.Info` (in `session_reconcile_test.go`, `t`-less,
   panics) or `wakeInfo(t, b)` (in `session_wake_test.go`). Both stamp `Type=session` then
   verbatim-seed + front-door Get. Delta vs raw codec = ONLY `Info.Type`; behavior-identical because
   NO consumer reads `Info.Type` (the sole cmd/gc-source read is `resolveOpenQualifiedAliasBasename`,
   a store-lister, unreachable from these consumers). Red-team-verified in batch 2.
2b. **Standalone VALID session bead** never stored → `sessiontest.SeedBead(t, bead)`.
3. **Standalone DEGRADED / non-session / deliberately-divergent** (empty ID, no session
   type/label, pinned CreatedAt a Create would stamp, stale/`ID:"missing"` shapes) → the
   front door rejects/normalizes it, so build the `session.Info{...}` STRUCT LITERAL the
   consumer reads (map metadata→Info 1:1 per `info_store.go`; set exactly what the
   consuming fn reads → outcome-identical). This was needed for the degraded `pendingCreate`
   classifier fixtures in `session_reconciler_test.go`.
4. **Stale-local-crack** (raw code cracked an IN-MEMORY bead that a full reconcile pass
   mutated in the store but NOT in the local var) → `sessiontest.SeedBead(t, localBead)`
   (verbatim, so it reads the stale local shape the test intends, not the store's newer one).
   Batch 1 hit exactly one (`RecordsResetStallDiagnostic`).
- Mock-store sites (write-tracking / error-injection, e.g. in `session_reconcile*`): a naive
  memstore swap changes how writes are asserted — read Info through the SAME store the raw
  code read, or use SeedBead (throwaway store, doesn't perturb the mock).

## CROSS-FILE BLOCKERS (a raw-bead adapter shared by callers in ≥2 test files) — need a coordinated pass
These project ANY bead shape (session AND task) and are called from multiple files, so no
single-file agent can zero them (front door narrows task beads; signature is locked by other
callers). Handle in a dedicated pass that owns ALL callers together (retire the adapter or split
per-shape), THEN they stop blocking the unexport:
- `infoLookupFromBeadLookup` (`*b`) — 1 site left in `session_wake_test.go:~843`; also called from
  `session_sleep_test.go` + `session_reconciler_trace_integration_test.go`. Doc says "drain tests
  still carry raw beads."
- `wakeReasonsForBead` / `healStateInfo` bridge helpers — called from `session_reconcile_test.go`
  (converted) + `session_sleep_test.go` + `session_reconcile_ratelimit_test.go` (NOT yet converted).
- `sessionInfosFromBeads(bs []beads.Bead) []session.Info` (`assigned_work_scope_test.go:23`, `t`-less)
  — the batch codec, called from **8+ files** (`pool_desired_state_test.go`, `build_desired_state_test.go`,
  `session_reconciler_test.go`, `cmd_sling_test.go`, `session_circuit_breaker_test.go`,
  `pool_desired_state_wake_test.go`, `session_model_phase0_demand_spec_test.go`, `assigned_work_scope_test.go`).
  Projects any bead shape incl. deliberately-narrowed task beads → a Type-stamp would defeat other
  callers' narrowing tests. Coordinated pass must own all callers (e.g. thread a projector or convert
  callers to pass Infos). ← this is the LAST InfoFromPersistedBead site in several of those files.

## In flight — Batch 2 (3 solo Opus agents, base d5f76a6f1, worktrees /data/projects/gascity-sdo-w2-*)
- `session_lifecycle_parallel_test.go` (89) — bare-memstore + standalone
- `session_reconcile_test.go` (48) — mixed; watch mock-store sites
- `session_wake_test.go` (29) — bare-memstore + `makeWakeBead`
Loop per batch: manual worktree off HEAD → Opus impl → `sdo-review.js` Fable red-team
(`Workflow`) → `git merge --no-ff` → verify (0 codec calls, gofmt/vet, scoped tests, census green).

## Remaining after batch 2 (~cmd/gc files; counts = InfoFromPersistedBead sites)
- **Twin/equiv oracles (nuanced — Info-side→front-door OR keep struct/raw):**
  `session_classifier_info_equiv_test.go` (28, `makeBead` corpus), `session_r3_info_equiv_test.go` (15),
  `session_wpool_twins_test.go` (7), `session_wtick_twins_test.go` (5, keep TrimSpace raw/struct),
  `session_w3_split_equiv_test.go` (4), `session_w4_split_equiv_test.go` (3),
  `session_drainack_info_equiv_test.go` (1, degraded→keep), `nudge_target_info_equiv_test.go` (2),
  `session_prepare_twin_coherence_test.go` (1)
- **Mediums (mostly store-read/standalone):** `build_desired_state_test.go` (24),
  `telemetry_lifecycle_metrics_test.go` (17), `session_model_phase0_rare_state_spec_test.go` (14),
  `compute_awake_bridge_test.go` (14), `session_sleep_test.go` (13), `session_lifecycle_chaos_test.go` (11),
  `cmd_session_test.go` (11), `session_reconcile_ratelimit_test.go` (11, MOCK store),
  `assigned_work_scope_test.go` (8), `session_reconciler_drift_defer`=done,
  `session_reconciler_fork_launch_test.go` (5, SeedBead-flagged),
  `session_reconciler_pool_replacement_test.go` (2, SeedBead-flagged),
  `session_model_phase0_rare_state_spec`, `compute_awake_bridge`, `session_template_overrides_test.go` (4),
  `session_lifecycle_start_deadline_test.go` (4), `session_model_phase2_pin_spec_test.go` (3), + the ~20 one/two-site tail.
- **internal/api:** `session_response_wire_test.go` (1) → `Store.GetPersistedResponse(id)` (drops BOTH InfoFromPersistedBead + PersistedResponseFromBead).
- **internal/worker:** `session_record_equiv_test.go` (2) — the CROSS-PACKAGE unexport blocker → `Store.Get(id)`.
- **internal/session:** ~64 in-package oracle sites → mechanical lowercase rename (Phase 10, WITH the codec).

## Phase 10 (LAST, gated)
Repo-wide grep gate: `git grep -n 'session.InfoFromPersistedBead\|sessionpkg.InfoFromPersistedBead\|PersistedResponseFromBead' -- '*.go'`
must return ONLY internal/session. Then lowercase `InfoFromPersistedBead`→`infoFromPersistedBead`
(+ the def in `info_store.go`, delete the exported name), and flip the census in
`typedclass_edge_guard_test.go` to a hard zero-pin + add `infoFromPersistedBead(` as an
interior-zero needle. Full sharded suite + census green. Then STOP (do not push unless asked).
