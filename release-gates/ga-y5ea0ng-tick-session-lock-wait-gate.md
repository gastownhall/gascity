# Tick session mutation lock wait release gate

**Verdict:** **PASS**

Evaluated 2026-10-16T13:56:04.110567+00:00. Deploy bead ga-y5ea0ng; build ga-8pzm5d; review ga-4bsp9xs.

- Reviewed deploy source: `1322ef78ac7c427bd46f13c7fa090a6b079d9a5a`.
- Pinned `origin/main` gate base: `4a1068a1da6d89890c7fa9ba130c73e0ca1eac42`.
- Tested materialized merge: `79f859da73718864860a12c750d087e71d1dd600`; tree `81fe06b42da4392248d1db247029b242b2feccfc`. The normal merge hook ran, compilation and nogo/vet passed, and the tree equals the conflict-free `git merge-tree` prediction. Main was fetched again before publication and still matched this pinned base.
- Deploy mode: remote; push remote: fork; isolated branch: `deploy/ga-y5ea0ng-gate`, cut exactly from the reviewed source, with only this gate record added.
- Stacked on PR #7970 (child 1, ga-4z6mbq3). Merge after that prerequisite lands. The rebase-preserved prerequisite and child 2 form one session mutation lock theme.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | ga-4bsp9xs records PASS on the resolved source above; no review carryover or unreviewed source substitution. |
| 2 | Acceptance | PASS | All twelve criteria inspected; deploy-stage PR text includes Refs #7170, stack/base, RED/GREEN and the pre-rollback/pre-commit write limits described below. Advisory complexity is explicitly disclosed. |
| 3 | Tests | PASS | Fresh full-suite CI-equivalent four-phase remote run, mandatory native floor, raw artifact verification and 47/47 owned names PASS; 115210 PASS / 0 FAIL / 369 SKIP (top-level result lines, including repeated jobs). |
| 4 | No open HIGH findings | PASS | Review has zero unresolved HIGH/blocking findings. Named limits and optional nits remain disclosed. |
| 5 | Final branch clean | PASS | Gate record is the only deploy-stage addition; normal pre-commit hook and final clean-status verification precede publication. Pre-existing unrelated scratch gate files are preserved outside the branch during publication. |
| 6 | Clean divergence and merged build | PASS | Conflict-free pinned merge above; Bazel build/nogo/vet of all targets succeeds. No self-rebase or source rewrite. |
| 7 | One feature theme | PASS | Tick-side session mutation lock waits, their shared session lock prerequisite, typed trace reasons and regression coverage; no independent feature or internal-doc contamination. |

## Acceptance evidence

One `tickSessionLockWait` constant bounds lock acquisition at 250 ms. The start gate follows the on-death/budget gates and precedes slots, capacity, runtime lease and breaker accounting; it applies to a nonblank ID and nonnil store. A held lock skips the candidate while healthy candidates start in the same wave; release permits retry. Prepare bounds the residual race, releases reservations, preserves markers and records a skipped outcome without a wake failure.

The three rollback callers were read in `cmd/gc/session_reconciler.go:2316`, `:3106` and `:3166`. Rate-limit paths that mutate continue before rollback, and start-in-flight paths defer. The closure at `:2134` performs existing idempotent runtime teardown/lease handling and a trace-only counter increment before the bounded session-lock acquisition. Busy rollback leaves the row and pending wait unchanged, including atomic-close and transaction fallback, and retries next tick.

Recovery was read at `cmd/gc/session_lifecycle_parallel.go:3510`: runtime lease acquisition (`:3526`) and preparation (`:3544`) precede the bounded commit (`:3615`). Preparation may persist an instance token or clear a stale resume hash. Busy commit returns `(false, nil)` without applying the commit; preparation residue reaches the snapshot next tick. A legacy row without an instance token may skip its incarnation check for this tick. A breaker restart counted before a residual prepare-time busy skip stays counted. Bounding acquisition does not bound hold time; the specified blocking operator/commit/shutdown and chat auto-suspend paths keep their behavior.

The reviewer independently rechecked RED: all five original lock-wait tests fail behaviorally before the change (10 s held-lock watchdog), with no build failure. GREEN control is 8 PASS / 0 FAIL / 0 SKIP, and 300 repetitions give 2400 PASS. Six mutants (zero/800 ms wait, marker clear, leaked probe, unrecognized atomic busy and canceled waits on busy) are killed. Fresh deploy coverage is independently listed below. Normal hooks are active. There is no config key, CLI, API, event-type or dashboard change. No issue/PR comment or runtime deployment is part of this release.

`make complexity-check` is advisory and remains red: baseline drift is tracked by ga-q7p6is; this diff also grows `executePlannedStartsTraced` 93→97, `reconcileSessionBeadsTracedWithNamedDemand` 608→609 and `recoverRunningPendingCreate` 19→21. This is disclosed, never recorded as green or attributed as identical to main. The CI complexity workflow is advisory. All required policy/lint lanes below passed.

## Test execution and policy

`test_cmd_scope: full-suite`; `heavy_mode: none`; `waiver_ref: none`; `failure_attribution: none`; `test_exec_mode: remote-exec`; `fallback_reason: None`.

Canonical source: TESTING.md and `.github/workflows/bazel.yml` (`docs/PROJECT_MANIFEST.md` does not exist). `gate-criterion3.sh` ran through `gate-detached-run.sh`, admission, load wrapper and `isolated-test-run.sh --rbe`. The wrapper reported PASS-THROUGH for this scratch path; no claim of wrapper-created isolation is made. Remote actions are hermetic, and the runner scrubs live store/session selectors from native commands. Pinned bd v1.3.1 was used; podman was configured with the pinned Dolt image and Ryuk opt-out.

Commands: remote Bazel `//...`; acceptance `//test/acceptance:acceptance_test`; integration packages `//test:integration_packages`; full integration `//test/integration:integration_test`. Every phase used `--nocache_test_results --noremote_local_fallback`; integration used its CI config with `GC_FAST_UNIT=0`, including the cmd/gc process journeys. Full integration had no smoke filter.

| Phase | Fresh remote attempts | Cached | Retried | Window/terminal predicate |
|---|---:|---:|---:|---|
| unit | 371 | 0 | 0 | PASS |
| acceptance | 24 | 0 | 0 | PASS |
| integration-packages | 81 | 0 | 0 | PASS |
| integration-full | 14 | 0 | 0 | PASS |

`farm_test_seconds: 6728.881`. All 490 attempts used strategy remote, attempt 1, neither cached flag, starts within each phase window and terminal verdicts. BEP evidence is retained in `c3-run/evidence/remote/*.evidence.json`; 980 test.xml/test.log artifacts were independently verified against recorded hashes and CAS lengths.

`admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 73.07149887084961, "waited_seconds": 0.001}`.
`rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}`.
`test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 81, "results": 81, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 371, "results": 371, "retried": 0, "window_ok": true}}`.

Policy/lint fast lane PASS before suite: pinned gofumpt/goimports formatting; changed-package Bazel nogo; four `make check` shell guards; all eight forced static policy targets. `make vet` builds/nogo-checks all 1357 targets. Each policy/format/drift lane ran in a separate fresh tree pinned by gate-base.sh.

`policy_lane: make lint-changed; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; Bazel go_format/goreleaser/CI-runner/analytics/core-boundary/go-mod/eventexport/native-surface targets — PASS`.
`drift_lane: make bazel-sync; git diff --exit-code; empty git status — PASS`.
`ci_lane_run: n/a (no CI-config change)`.
`owned_topup: []`.
`floor_topup: complete, 16/16 exit 0`: every environment-absence skip selected by package/parent, pinned-beads module check, Darwin compile and both runner self-tests. Systemd restart completed in 5.6 s against a 15 s budget. A cleanup diagnostic reported that the already-stopped supervisor was not running; the test and package exited 0.

Native soft load wait (ordinary remote floor, not a heavy defer):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=21.42 run_max_load=26.65 run_mean_load=23.97 run_readings=8 wait_first_load=21.88 wait_max_load=42.54 wait_mean_load=27.73 wait_readings=61 read_errors=0
```

`load_threshold: 15`; `load_waited_seconds: 1801`; `load_wait_timed_out: true`; `run_start_load: 21.42`; `run_max_load: 26.65`; `run_mean_load: 23.97`; `run_readings: 8`. The documented bounded soft wait permits proceeding after timeout; all floor checks passed. Remote phases were load-wait exempt.

## Skips and retained evidence

`skip_inventory`: {"environment-absence": 71, "other": 284} farm skip rows (355 total); native skip inventory/proof is retained separately (39 rows including subtests). None is diff-owned. Top-level combined count is 369, not a count of unique tests.

The 228 directly located farm skip sites are UNCHANGED. The remaining 67 rows resolve to five byte-identical helper files: close-notification/ready-parity/pinned-ID store conformance, runtime provider conformance, and the optional legacy-gc acceptance fixture. Native skip sites were independently checked unchanged at pinned base/merge. Reasons are optional replay/sim/legacy/provider fixtures, unavailable optional tools/services/Postgres, explicit fixture-controlled unsupported capabilities, unit-lane live-provider exclusions, and tmux-context requirements. The new session-lock tests use held in-memory locks, and no changed test or skip helper shares those conditions. Every environment-absence parent was retried by the required native floor; remaining non-owned skips are inventoried, not credited as PASS.

Evidence root: `/var/tmp/deploy-ga-y5ea0ng.jpmvva4v`. `test_log_dir: /var/tmp/deploy-ga-y5ea0ng.jpmvva4v/c3-run/evidence/remote/logs`; native raw logs: `c3-run/evidence/remote/floor-logs`. Preserve all logs. Record: `execution.json`, `artifact-verification.json`, `independent-verification.json`, `native-skip-proof.json`, `skip-site-proof.json`, `skip-supplemental-proof.json`, and `review-mutation-evidence.txt`.

## Result reader output

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-y5ea0ng.jpmvva4v/c3-run/evidence/remote/logs logs=506 logs_with_results=440
test_counts: PASS=115210 FAIL=0 SKIP=369
  (top-level result lines, column 0, summed over the 506 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestAsyncStartFinalCommitStaleCleanup PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-2ef13e1b6898c5544764.log)
  TestCommitAsyncStart_StaleResultStopsUnattributedBeadScopedPoolRuntime PASS (integration-packages-12697e2ea0d32c278115.log, unit-493754ea2d5b4d8858c8.log)
  TestExecutePlannedStarts_BusySessionLockSkipsAtTheGate PASS (integration-packages-b72a67a2f9444af705e3.log, unit-5a9df316125cd0aeff2e.log)
  TestExecutePlannedStarts_BusySessionStartsOnTheNextPassAfterRelease PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-10cc7795c0ec199a5530.log)
  TestExecutePlannedStarts_HolderReleasingWithinTheBoundStartsTheSessionInThePass PASS (integration-packages-9326181a736eb30b71ca.log, unit-4d20b3d28497f844aa39.log)
  TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareIsASkippedStart PASS (integration-packages-e5330b8c981927473156.log, unit-f679cd2551fcc64525aa.log)
  TestExecutePlannedStarts_LockTakenBetweenGateAndPrepareReleasesTheProbeAndKeepsTheMarker PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-2ef13e1b6898c5544764.log)
  TestPoolSessionCreate_TeardownFailureHoldsTheSlot PASS (integration-packages-cb110414c557c0928e82.log, unit-cd6930bb0d0da8646500.log)
  TestReconcileSessionBeads_BusySessionLockDefersPendingCreateRollback PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestReconcileSessionBeads_BusySessionLockOverAtomicCloseStoreLeavesRowAndWaitsUntouched PASS (integration-packages-e4245ddf6a37030111bc.log, unit-64014bc0bb3833bc198c.log)
  TestRecoverRunningPendingCreate_BusySessionLockIsAnUnappliedRecovery PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestRollbackPendingCreateClearingClaimIsNoopOnAlreadyClosedBead PASS (integration-packages-e5330b8c981927473156.log, unit-f679cd2551fcc64525aa.log)
  TestRollbackPendingCreateClearingClaimKeepsSessionNameWhenCloseFails PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestRollbackPendingCreateClearingClaimReturnsNilWhenTxFails PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestRollbackPendingCreateClearingClaimRunsRetiredCleanupWhenPostCloseFails PASS (integration-packages-9326181a736eb30b71ca.log, unit-4d20b3d28497f844aa39.log)
  TestRollbackPendingCreateClearingClaimUsesSingleTransactionForAllWrites PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-10cc7795c0ec199a5530.log)
  TestRollbackPendingCreateFallsBackToTheTxWhenTheCloseIsRefused PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestRollbackPendingCreateIsNoopOnAlreadyClosedBead PASS (integration-packages-b72a67a2f9444af705e3.log, unit-5a9df316125cd0aeff2e.log)
  TestRollbackPendingCreateLeavesAReopenedRowItsName PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-2ef13e1b6898c5544764.log)
  TestRollbackPendingCreateRefusesAStaleObservationBeforeWriting PASS (integration-packages-821a75e4a503601af052.log, unit-493754ea2d5b4d8858c8.log)
  TestRollbackPendingCreateRejectsStaleSnapshot PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-23e7d9fc763496dd8095.log)
  TestRollbackPendingCreateReleasesTheNameOnlyAfterTheClose PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestRollbackPendingCreateUnderRealConcurrentWritersNeverRestsClosedAwake PASS (integration-packages-074fc6c34a1fb2577127.log, unit-3d6d3036a2f5902472b5.log)
  TestRollbackPendingCreateUsesSingleTransactionForAllWrites PASS (integration-packages-830685c60d0c2dd88567.log, unit-cd6930bb0d0da8646500.log)
  TestStartCompletionRejectsRolledBackSnapshot PASS (integration-packages-12697e2ea0d32c278115.log, unit-2bde55fab377f50b4739.log)
  TestCommitStartedIfCurrentBoundsALosingFence PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentFencesAWriteAfterTheReread PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRaceAgainstAKill PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRedecidesPastAnUnrelatedWrite PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRefusesOverAnOutOfBandKill PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedIfCurrentRequireModeFailsClosed PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestCommitStartedRefusesAnOperatorSuspendDuringTheStart PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLeasedClosesRefuseALostLease PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestLeasedWritesRefuseALostLease PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyLeavesAReopenedRowItsName PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyReportsUnsupportedWithoutWriting PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestRollbackPendingCreateAtomicallyRetriesALostPostCloseFence PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStartAndRollbackHoldMutationLockAcrossReadAndWrite PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestStoreLockedMethodsReturnBusyWithoutWriting PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextAbandonedWaiterNeverRuns PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextBusyAtDeadline PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextDoneContextBusyWhenHeld PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextDoneContextStillAcquiresFreeLock PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextFIFO PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextReturnsFnError PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextStress PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestWithSessionMutationLockContextUncontended PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
GATE_TEST_EVIDENCE_RESULT ok: 47 diff-owned tests verified by name
```
