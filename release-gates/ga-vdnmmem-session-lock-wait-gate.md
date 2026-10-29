# Session mutation lock wait release gate — ga-vdnmmem

**Verdict:** **PASS**

## Coordinates and scope

- Reviewed source: `5bd312f069e056f08d906cb8c2b13e0fb2b6045f`; review ga-1abfkfk PASS, build ga-15ylcii. Every consumed SHA was resolved against Git.
- reviewer_tests_verified: `cc3bc5b6c4f6e0aff81cce23c044e6924be65a68` -> `5bd312f069e056f08d906cb8c2b13e0fb2b6045f` files=cmd/gc/session_start_lock_wait_test.go. The canonical helper returned reviewer-tests, rc0; the source is exactly one additive reviewer test commit above its reviewed parent.
- Provenance only: builder/ga-l4p6x27. Deploy branch deploy/ga-vdnmmem-gate is cut from that exact source; no builder branch is a push target.
- DEPLOY_MODE: remote; repository gastownhall/gascity; BASE_REF origin/main.
- merge_state: clean (origin/main@5b416c773dc10fbf7c0863fdf4281968cdde6be8).
- gate_base_kind: current-tip; gate_base_sha: `5b416c773dc10fbf7c0863fdf4281968cdde6be8`.
- Synthetic gated head: `308b2ec7056e0bb18b597361ba7e8cfa3856ba7b`; tree `add15d284ef03dd7bb3dbc252ddbc48fdc2ed498` equals the read-only predicted merge tree.
- merge_validation: full clean merged tree built, vetted and freshly tested. Normal merge hooks ran. No source rebase, reset or conflict resolution by the deployer; MPR owns merge-time freshness.
- Already-merged preflight: unpublished reviewer source was not found on GitHub (commit/pulls HTTP422, retained in source-prs.json.stderr); this is not recorded as a successful empty PR list. Source is not an ancestor of frozen main.
- Evidence root: `/var/tmp/deploy-ga-vdnmmem.91qd52ef`. Original logs, BEPs and XMLs are retained. No reviewer, previous publication or historical focused result substitutes for criterion3.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-1abfkfk reviewed the exact source, including its own additive gate-order test; canonical reviewer-added-tests check verified independently. No carryover shortcut. |
| 2 | Acceptance met | PASS | Independent static path audit, historical four-case RED/GREEN, and all 26 owned tests in the fresh complete gate; see acceptance map below. PR text explicitly includes before/after output, bounded/unbounded scope, earlier writes and Refs #7170. |
| 3 | Tests pass | PASS | Five fresh remote phases, native floor and SKIP reconciliation complete; all 26 owned tests PASS by name, no cache credit, failure or waiver. |
| 4 | No HIGH finding open | PASS | Review PASS; zero unresolved HIGH/blocker/major findings. Surviving M13 outcome-token mutation and untested standalone helper guards remain disclosed limits. |
| 5 | Final branch clean | PASS | Gated scratch clean after all checks, at the predicted tree. Publication commits only this record above the reviewed source and requires a clean resulting branch. |
| 6 | Merge state/build/vet | PASS | Clean frozen current-tip merge, complete bazel build //... and make vet/nogo PASS, normal hooks active. |
| 7 | One feature theme | PASS | Ten cmd/gc paths, +784/-76: bounded tick lock waits, preserved busy decisions and their tests. Four production files +160/-53. Same-theme ancestry check PASS with ga-15ylcii, ga-8pzm5d, ga-2kepv7u, ga-bmhjjhp; no stack. |

## Acceptance and review boundaries

The implementation bounds acquisition waits at 250 ms in the pre-start gate and preparation, pending-create rollback in the tick, and running pending-create recovery. It does not bound lock hold time or store IO. Gate placement precedes the limiter slot and endpoint probe; preparation rechecks with the same bound if the lock is taken after the gate. Busy candidates defer and healthy candidates proceed; release within the bound and next-tick success are tested. The race path releases its slot/probe, keeps its in-flight marker and records no failed wake; an already counted breaker restart remains counted.

The reviewer-added sync/async test observes both limiter and half-open probe while waiting; reviewer M16/M18 placement mutants fail it. No claim that tracker-slot or runtime-lease ordering alone, M13's exact outcome token, or blank-ID/nil-store/other-error helper guards are individually tested.

Planner write audit (static-acceptance-path-audit.json): early expired-create rollback precedes advisory healing, and a stability check that writes continues first. The foreign-runtime and later expired-create sites read the post-heal/post-zombie snapshot. A successful provider-terminal-error patch clears pending_create_claim, preventing a rollback call. Earlier advisory healing is outside rollback's transaction. The rollback helper may release a row-owned runtime under its runtime-name lease before the bounded mutation-lock acquisition; busy does not undo that cleanup.

Recovery obtains a runtime lease and builds preparation before its bounded commit. Preparation may persist an instance token or clear stale resume configuration. Busy returns unapplied without folding that residue into the current snapshot; next tick refreshes it, so a previously tokenless snapshot can stay stale for the remainder of the current tick.

Unbounded by design: commitStartResultTraced, restorePreWakeState, background rollback callers, session_async_start_drift's background context, and chat auto-suspend. No new exported API, configuration, command, persisted field or dependency. No live gc build/deploy performed. PR names main (origin/main), says Refs #7170 rather than closing the larger issue, and includes the independently executed historical excerpts.

Historical AC1: builder RED `cdaee997e788f9043e3968ecc75efdd6b716ff3d`, implementation `e87aa325b9c3e063768306818d76660abaf510b5`, implementation plus BUILD registration `9c1f46042977fcdbaf716c170c14d313ee887d52`, all resolved ancestors of source. Deployer independently executed the same four cases at RED and GREEN with identical test bytes; RED required ONLY the exact later one-line BUILD registration in its disposable view, with no production edit. All four assertion-FAIL at RED, all four PASS at GREEN, both first-attempt uncached remote executions. The assertion clocks are Go synctest time. Records, original log/XML hashes and exact excerpts: historical-red-green.json, historical-pair-excerpts.json and red-only-build-registration.patch. Expected historical FAILs are separate from criterion3 counts. The reviewer did not reproduce RED.

Deferred review requirements now independently satisfied: integration-packages actually executes TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync; TestTutorial01 and its tutorial subcases PASS. required-integration-evidence.json identifies the retained actual log lines. Reviewer evidence never supplies these passes.

## Build and fast lanes

C6 complete bazel build //... (1395 targets) and make vet/nogo PASS in c6-recovery-run, with normal merge hooks. Initial admission/setup attempts returned 98 before any test/build: first the default 40 GiB preflight lacked space, then an admitted 25 GiB remote run's nested wrapper retained 40 GiB. Forwarding the standard remote provisional 25 GiB to that wrapper allowed one fresh setup retry; no failed test was rerun, no native/downgrade floor was waived. Original refusal logs retained.

fast_lane: PASS before suite launch; each lane used fresh-tree-run.sh plus gate-base.sh run, so no generator wrote the suite scratch.

- policy_lane: pinned golangci-lint 2.14.0 fmt-check and full nogo/lint; check-hooks, four shell guards and ten explicit required Bazel policy targets PASS.
- drift_lane: make bazel-sync, git diff --exit-code and empty tracked/untracked status PASS.
- generated_lane: all seven required schema/spec/client/dashboard/lockfile checks PASS. Combined fast manifests retain 34 original log/XML artifacts, independently rehashed.
- The unchanged Openclaw lockfile check runs three Python subtests with one optional TypeScript-pin SKIP (no --rules-ts-typescript; this JavaScript bridge does not enable that macro option). Both required lockfile comparisons run. generated-python-skip-audit.json records original output, unchanged owner/macro/body and exact guard; this Python SKIP is separate from Go result-line counts.
- complexity: PASS under zero-growth form(ii), not a raw make complexity-check PASS. Both pinned base and merged tree return 2 with identical 118-row reports, byte-identical hash 59db1f060abb843c1df1280dee2195af62bc056e88bb8335e905ee101ef2a581; complexity-diff returns 0/no threshold changes; baseline unchanged. Older literal 94/625 caps are not claimed at a base already 96/630. complexity-verdict.json records comparisons.
- ci_lane_run: n/a — no workflow job, matrix, timeout or required-check list changed.
- heavy_mode: none; classify verified. No heavy-path exception or hand-picked full-suite substitution.

## Fresh criterion3

test_cmd_scope: full-suite.
test_exec_mode: remote-exec, derived from actual BEP; native checks are the canonical supplemental floor.
waiver_ref: none. failure_attribution: none.

Canonical gate-criterion3.sh ran under a persistent gate-detached-run.sh systemd service. Its five phase commands cover make check, complete acceptance (sharded and solo), integration packages, full integration minus the latency inventory, and only the latency inventory under the clear farm probe. Every phase forces --nocache_test_results and --noremote_local_fallback; no local/remote suite stitching. Canonical phase cases (latency regex and probe come from the retained inventory/runner):

```bash
make check
bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests
bazel test $BAZEL_FLAGS --config=integration //test:integration_packages
bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v "--test_arg=-test.skip=$latency_re" //test/integration:integration_test
bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v "--test_filter=$latency_re" --test_sharding_strategy=disabled "--run_under=$probe" //test/integration:integration_test
```

Exact per-phase flag expansions:

```text
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

| Phase | Result | BEP results | Remote executions | Cached local / remote / retry |
|---|---|---:|---:|---|
| unit | PASS | 375 | 375 | 0 / 0 / 0 |
| acceptance | PASS | 24 | 24 | 0 / 0 / 0 |
| integration-packages | PASS | 85 | 85 | 0 / 0 / 0 |
| integration-full | PASS | 14 | 14 | 0 / 0 / 0 |
| integration-latency | PASS | 1 | 1 | 0 / 0 / 0 |

All 499 terminal results are PASS, attempt 1, strategy remote, neither cache flag, start inside the phase window, other strategy 0, transport_failure false. The remote-integrity-audit independently rehashed all 998 original log/XML artifacts. Normalized logs preserve originals alongside them; no outcome rewriting.

test_counts: {"FAIL": 0, "PASS": 133740, "SKIP": 375}. These are retained Go result lines, not distinct tests or targets; the optional Python SKIP is disclosed above.
test_log_dir: `/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/logs`.
test_exec_bep: `/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote` (one original BEP and predicate report per phase).
farm_test_seconds: 12186.671 (sum of farm time, not wall time).
fallback_reason: none. owned_topup: [].

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/logs logs=515 logs_with_results=449
test_counts: PASS=133740 FAIL=0 SKIP=375
  (top-level result lines, column 0, summed over the 515 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestAsyncStartFinalCommitStaleCleanup PASS (integration-packages-509ef0a6cd81866833cc.log, unit-637c74c578a2b17fc654.log)
  TestCommitAsyncStart_StaleResultStopsUnattributedBeadScopedPoolRuntime PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-f679cd2551fcc64525aa.log)
  TestExecutePlannedStarts_BusySessionHoldsNoSlotOrProbeWhileItWaitsAtTheGate PASS (integration-packages-830685c60d0c2dd88567.log, unit-f93a0ca96ccc61d0cae9.log)
  TestExecutePlannedStarts_BusySessionLockSkipsAtTheGate PASS (integration-packages-509ef0a6cd81866833cc.log, unit-637c74c578a2b17fc654.log)
  TestExecutePlannedStarts_BusySessionStartsOnTheNextPassAfterRelease PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-423c6a0da1986b90e9a7.log)
  TestExecutePlannedStarts_HolderReleasingWithinTheBoundStartsTheSessionInThePass PASS (integration-packages-bc482d2ae768251ad47f.log, unit-752a0bf25b15b465517a.log)
  TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareIsASkippedStart PASS (integration-packages-568812e4d525bdd1f974.log, unit-1ab526405ace21879f85.log)
  TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareReleasesTheProbeAndKeepsTheMarker PASS (integration-packages-18745ddf7d62465654f5.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestPoolSessionCreate_TeardownFailureHoldsTheSlot PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-752a0bf25b15b465517a.log)
  TestReconcileSessionBeads_BusySessionLockDefersPendingCreateRollback PASS (integration-packages-75f7330b9b673df65e4f.log, unit-d9e667428f972d067bd3.log)
  TestReconcileSessionBeads_BusySessionLockOverAtomicCloseStoreLeavesRowAndWaitsUntouched PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-abd97d2cb9b450225d57.log)
  TestRecoverRunningPendingCreate_BusySessionLockIsAnUnappliedRecovery PASS (integration-packages-cb110414c557c0928e82.log, unit-fd986cf5bab8709b8d45.log)
  TestRollbackPendingCreateClearingClaimIsNoopOnAlreadyClosedBead PASS (integration-packages-12697e2ea0d32c278115.log, unit-10cc7795c0ec199a5530.log)
  TestRollbackPendingCreateClearingClaimKeepsSessionNameWhenCloseFails PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-23e7d9fc763496dd8095.log)
  TestRollbackPendingCreateClearingClaimReturnsNilWhenTxFails PASS (integration-packages-9326181a736eb30b71ca.log, unit-f679cd2551fcc64525aa.log)
  TestRollbackPendingCreateClearingClaimRunsRetiredCleanupWhenPostCloseFails PASS (integration-packages-e4245ddf6a37030111bc.log, unit-2bde55fab377f50b4739.log)
  TestRollbackPendingCreateClearingClaimUsesSingleTransactionForAllWrites PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-5a9df316125cd0aeff2e.log)
  TestRollbackPendingCreateFallsBackToTheTxWhenTheCloseIsRefused PASS (integration-packages-b72a67a2f9444af705e3.log, unit-912ab643330b35bd0b6b.log)
  TestRollbackPendingCreateIsNoopOnAlreadyClosedBead PASS (integration-packages-e5330b8c981927473156.log, unit-cd6930bb0d0da8646500.log)
  TestRollbackPendingCreateLeavesAReopenedRowItsName PASS (integration-packages-18745ddf7d62465654f5.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestRollbackPendingCreateRefusesAStaleObservationBeforeWriting PASS (integration-packages-830685c60d0c2dd88567.log, unit-f93a0ca96ccc61d0cae9.log)
  TestRollbackPendingCreateRejectsStaleSnapshot PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestRollbackPendingCreateReleasesTheNameOnlyAfterTheClose PASS (integration-packages-cb110414c557c0928e82.log, unit-fd986cf5bab8709b8d45.log)
  TestRollbackPendingCreateUnderRealConcurrentWritersNeverRestsClosedAwake PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-088cc38d26d094729229.log)
  TestRollbackPendingCreateUsesSingleTransactionForAllWrites PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-088cc38d26d094729229.log)
  TestStartCompletionRejectsRolledBackSnapshot PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-3d6d3036a2f5902472b5.log)
GATE_TEST_EVIDENCE_RESULT ok: 26 diff-owned tests verified by name
```

latency_leg: measured under a clear probe on the first attempt, unchanged thresholds/budgets; neither budget test ran in the main integration leg. Both separate-lane tests PASS; actual names/regex come from the canonical inventory. Original probe record:

```text
GATE_FARM_PROBE verdict=clear waited=0s load1=1.13 load5=4.76 load15=8.01 cpus=32 threshold=50 max_wait=600s
```

## Native floor and SKIPs

floor_topup: complete; all 16 steps exited 0, including beadstest, Darwin fsys compile, runner self-tests and every environment-absence selection by package/root. Native residual non-owned SKIPs remain inventoried; no owned SKIP/FAIL/MISSING.

| Package/check | Result | Selection |
|---|---|---|
| cmd/gc | PASS | 5 selected roots |
| examples/gastown | PASS | 2 selected roots |
| internal/beads | PASS | 2 selected roots |
| internal/beads/beadstest | PASS | 1 selected roots |
| internal/beads/contract | PASS | 1 selected roots |
| internal/beads/exec | PASS | 1 selected roots |
| internal/doctor | PASS | 3 selected roots |
| internal/mail/exec | PASS | 5 selected roots |
| internal/runtime/herdr | PASS | 6 selected roots |
| internal/runtime/runtimetest | PASS | 1 selected roots |
| internal/runtime/tmux | PASS | 4 selected roots |
| test/acceptance/helpers | PASS | 3 selected roots |
| test/integration | PASS | 1 selected roots |
| fsys-darwin-compile | PASS | 0 selected roots |
| push-gate-lock-selftest | PASS | 0 selected roots |
| local-concurrency-selftest | PASS | 0 selected roots |

Original native logs retained and hashed in native-log-hashes.json. Darwin evidence is fsys compilation, not execution of changed tests on macOS.

skip_inventory: {"counts_by_reason_class": {"environment-absence": 71, "other": 290}, "top_level_counts_by_reason_class": {"environment-absence": 42, "other": 218}}. Original per-test inventory: `/var/tmp/deploy-ga-vdnmmem.91qd52ef/c3-run/evidence/remote/skip-inventory.json`.
skip_justification: 400 farm/native inventory entries mapped to 271 actual unchanged source sites; unresolved 0. Explicit opt-in, platform, lane and optional tool/provider guards are outside this change. Canonical native rechecks cover environment absence; residual unrelated native SKIPs are not passes. skip-ownership-audit.json and skip-sites-proof.txt retain each original reason, source context and mechanical --site UNCHANGED proof. Shared hunks in the two changed test files are context plumbing and test-local helpers with callers confined to those files, no init/TestMain/new skip; no actual SKIP site points into either file. Parallel-log reason recovery, where necessary, cites the single literal original message, not an inferred substitute.

## Runtime, admission and load

Before C3, rootless Podman socket active and pinned deps.env Dolt image verified: 26231139a79b236ac21586766c88190d8278f111fe3a7537c856d87207fbd189. Wrapper supplied bd 1.3.2, pinned ref 9b65b9e19; host ICU74 loader/hash check PASS with no overlay or library-path alteration. Wrapper truthfully reports PASS-THROUGH / NOT ISOLATED for this owned throwaway scratch, with two unstripped ambient names GC_GATE_REMOTE_ENABLED and BEADS_ALLOW_UNREAPED_TESTCONTAINERS. Remote actions run in farm sandboxes; native commands use the canonical scrubbed environment. No claim of wrapper-created clean isolation.

admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 41.9681282043457, "waited_seconds": 0.001}.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.

Remote admission uses 25 GiB, cap 2 and no host-load/heavy wait. The native floor keeps its ordinary bounded soft load wait, threshold 15 on five-minute host load. Timeout proceeds by the standing rule, with no threshold override. Full original wait/run records (including run_start_load/run_max_load/run_mean_load/run_readings; wait_* is separate):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=17.81 run_max_load=104.99 run_mean_load=60.86 run_readings=111 wait_first_load=17.81 wait_max_load=17.81 wait_mean_load=17.81 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=78.25 run_max_load=77.12 run_mean_load=65.34 run_readings=8 wait_first_load=52.34 wait_max_load=105.09 wait_mean_load=75.26 wait_readings=61 read_errors=0
```

Named load fields (remote outer run, then native floor):

```json
[
  {
    "run_start_load": "17.81",
    "run_max_load": "104.99",
    "run_mean_load": "60.86",
    "run_readings": "111",
    "wait_first_load": "17.81",
    "wait_max_load": "17.81",
    "wait_mean_load": "17.81",
    "wait_readings": "1",
    "read_errors": "0",
    "load_threshold": "15",
    "load_waited_seconds": "0",
    "load_wait_timed_out": "0"
  },
  {
    "run_start_load": "78.25",
    "run_max_load": "77.12",
    "run_mean_load": "65.34",
    "run_readings": "8",
    "wait_first_load": "52.34",
    "wait_max_load": "105.09",
    "wait_mean_load": "75.26",
    "wait_readings": "61",
    "read_errors": "0",
    "load_threshold": "15",
    "load_waited_seconds": "1801",
    "load_wait_timed_out": "1"
  }
]
```

The gate record is the only deployer commit above source. Publish exact-head clearance only after verified PR creation, then route a peek-verified merge-request to mayor/MPR. The deployer never merges; work remains blocked until MPR lands it.
