**Verdict:** **PASS**

## Staged self-handoff release gate

- Evaluated: 2026-11-01T15:21:38+00:00
- Deploy bead: ga-txa0crm; build: ga-gylcvg; review PASS: ga-bkm7jf9.
- Reviewed source: `021a34ce16b85d906dd41e1b358b9caf8756bb34`; provenance branch: `builder/ga-951bwb9`.
- Isolated publication branch: `deploy/ga-txa0crm-gate`; push remote: `fork` (quad341/gascity).
- Deploy mode: remote; base reference: origin/main.
- Materialized gate commit: `2f32f8501bc09e189d107d460735e7701053df74`; its tracked tree equals the reviewed source exactly.
- Scope: 17 feature commits; 63 changed files, +5896/-1025, including required schema/client/dashboard regeneration.

## Criteria

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer ga-bkm7jf9 explicitly records verdict pass on the exact reviewed source; no carryover or reviewer-test substitution. |
| 2 | PASS | All six acceptance criteria mapped below; 31 diff-owned tests passed by name in the full suite. |
| 3 | PASS | Five fresh remote phases, 502/502 remote results, no cached test results and no retries; mandatory native floor 16/16; CI dependency accounting complete. Windows execution remains deferred to PR CI. |
| 4 | PASS | Review records no unresolved HIGH/blocker/major finding. Carried minor observations remain non-gating. |
| 5 | PASS | Clean code tree at the reviewed source; this release record is the only publication addition. Publication also requires a clean post-commit status. |
| 6 | PASS | Full Bazel build/nogo succeeded on the branch at its own merge base. Conflicts with current main are recorded below and deferred to merge authority. |
| 7 | PASS | Every feature commit belongs to staged self-handoff lifecycle, mail visibility, store routing, or its required generated artifacts. No independent feature or .claude path. |

## Acceptance criteria

| Requirement | Independent evidence |
|---|---|
| Success, timeout, controller loss, cancellation, retry and crash recovery | TestLiveSuccessorReleasesStagedHandoffExactlyOnce, TestStagedHandoffTimeoutNeverReleasesToOriginatingIncarnation, TestControllerLossLeavesSelfHandoffProvisional, TestFailedSelfHandoffRemainsProvisionalAndAuditable, TestRetryAfterFailedRestartSupersedesProvisionalBrief, TestRestageSupersedesWrittenOffBriefEvenAfterTokenChange: PASS. |
| Durable before restart, hidden from origin, released once to successor | TestRestartableSelfHandoffStagesBeforeRestartRequest, TestOriginatingIncarnationCannotReleaseStagedHandoff, TestStagedHandoffIsInvisibleAcrossEveryMailReadSurface and successor release test: PASS. Source persists the stage before recording restart and compares nonempty successor instance token with the staged token. |
| CLI and typed events distinguish staging from delivery | TestSelfHandoffRestartNotRecordedWording and TestHandoffLifecycleEventsAreKnownAndTyped: PASS. Source prints held/staged wording before release. |
| Messaging/session store routing and terminal cleanup | TestRestageRoutesEachClassToItsOwnStore, TestHandoffStagedArmLandsInTheBindingOnAMigratedCity, TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore, TestReconcileReleasesStagedHandoffAcrossDistinctStores and restage/retry tests: PASS. Reads and writes use each class handle. |
| Existing auto, target and named-session behavior; help generation | TestNonRestartingHandoffModesRemainOrdinaryMail, TestExistingOrdinaryAutoHandoffRemainsVisible, prime compatibility tests and pinned named-session test: PASS. Long help is unchanged; generated-doc/schema/client checks passed. |
| Focused regression coverage, full baseline, vet and generated docs | 31/31 diff-owned tests, complete unit/acceptance/integration tiers, full build/nogo, format, docsync, dashboard and BUILD drift gates passed. |

## Merge state and build

merge_state: conflicts (origin/main@d5261665dcd8; files: cmd/gc/BUILD.bazel,cmd/gc/city_runtime.go,internal/api/dashboardspa/dist/assets/Activity-C6VoEr9I.js,internal/api/dashboardspa/dist/assets/Activity-CjJDk1h2.js,internal/api/dashboardspa/dist/assets/Activity-a2oSI7J3.js,internal/api/dashboardspa/dist/assets/AgentDetail-BNQrFreZ.js,internal/api/dashboardspa/dist/assets/AgentDetail-GtGtGpCJ.js,internal/api/dashboardspa/dist/assets/AgentDetail-pTsX__We.js,internal/api/dashboardspa/dist/assets/Agents-BBjNK0nd.js,internal/api/dashboardspa/dist/assets/Agents-BM0wDIAS.js,internal/api/dashboardspa/dist/assets/Agents-pzfC80dD.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-BB0aAl1i.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-CKE2a7qT.js,internal/api/dashboardspa/dist/assets/BeadDetailModal-CSnbXhX4.js,internal/api/dashboardspa/dist/assets/Beads-BOwAegEX.js,internal/api/dashboardspa/dist/assets/Beads-C8lD6Os3.js,internal/api/dashboardspa/dist/assets/Beads-CDjLxVBI.js,internal/api/dashboardspa/dist/assets/CockpitHome-BGmaEfik.js,internal/api/dashboardspa/dist/assets/CockpitHome-DCg-8_L6.js,internal/api/dashboardspa/dist/assets/CockpitHome-rZGmlLq_.js,internal/api/dashboardspa/dist/assets/Field-CsF4YZ0W.js,internal/api/dashboardspa/dist/assets/Field-YyVcnxwA.js,internal/api/dashboardspa/dist/assets/Field-qTwKMhf1.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-B4My9rOc.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-CS9RL4Vd.js,internal/api/dashboardspa/dist/assets/FormulaRunDetail-Cksl0MHj.js,internal/api/dashboardspa/dist/assets/Health-CnEGnNZp.js,internal/api/dashboardspa/dist/assets/Health-DAoOeL-j.js,internal/api/dashboardspa/dist/assets/Health-DxBVGfqr.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-BG4tXlMS.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-BjxYANlQ.js,internal/api/dashboardspa/dist/assets/LiveSessionPeek-BkElusvQ.js,internal/api/dashboardspa/dist/assets/Mail-CdUMUq1u.js,internal/api/dashboardspa/dist/assets/Mail-DM5wvhoe.js,internal/api/dashboardspa/dist/assets/Mail-DVgPX7g7.js,internal/api/dashboardspa/dist/assets/PageHeader-B-Q7pm7-.js,internal/api/dashboardspa/dist/assets/PageHeader-DIk0ZIbK.js,internal/api/dashboardspa/dist/assets/PageHeader-DotJty2s.js,internal/api/dashboardspa/dist/assets/Runs-CjBxgcLX.js,internal/api/dashboardspa/dist/assets/Runs-NIFvZXph.js,internal/api/dashboardspa/dist/assets/Runs-vImDlMn6.js,internal/api/dashboardspa/dist/assets/SseIndicator-Bmzqsept.js,internal/api/dashboardspa/dist/assets/SseIndicator-C4V6AFY8.js,internal/api/dashboardspa/dist/assets/SseIndicator-rso4PFd-.js,internal/api/dashboardspa/dist/assets/StageLadder--K5T1jFR.js,internal/api/dashboardspa/dist/assets/StageLadder-6fJCEX0I.js,internal/api/dashboardspa/dist/assets/StageLadder-B6-gNT2X.js,internal/api/dashboardspa/dist/assets/Table-3DqvPotv.js,internal/api/dashboardspa/dist/assets/Table-AwdnALb9.js,internal/api/dashboardspa/dist/assets/Table-UYKCcimD.js,internal/api/dashboardspa/dist/assets/agentReads-CU8WcFJH.js,internal/api/dashboardspa/dist/assets/agentReads-NSZr1uT9.js,internal/api/dashboardspa/dist/assets/agentReads-mkqekMo4.js,internal/api/dashboardspa/dist/assets/constants-Btp9m7_9.js,internal/api/dashboardspa/dist/assets/constants-D6sbHlqV.js,internal/api/dashboardspa/dist/assets/constants-T6_aNRGg.js,internal/api/dashboardspa/dist/assets/index-B9icpNT8.js,internal/api/dashboardspa/dist/assets/index-BH-boD4M.js,internal/api/dashboardspa/dist/assets/index-DIU6rBbJ.js,internal/api/dashboardspa/dist/assets/projectOf-AtE1m32c.js,internal/api/dashboardspa/dist/assets/projectOf-CzfmiyyB.js,internal/api/dashboardspa/dist/assets/projectOf-LK127tGW.js,internal/api/dashboardspa/dist/assets/useListFilters-BPZ52K7a.js,internal/api/dashboardspa/dist/assets/useListFilters-CoQu1rW_.js,internal/api/dashboardspa/dist/assets/useListFilters-DTQnCPO8.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-CdCVJTLX.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-DUK05hxI.js,internal/api/dashboardspa/dist/assets/useVisibleRefresh-DoYoox9Z.js,internal/api/dashboardspa/dist/index.html,internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client/index.ts,internal/api/dashboardspa/web/shared/src/generated/gc-supervisor-client/types.gen.ts)
gate_base: fe971e2f2ae3a69916a82bab97f0d2dbef3d51e7
gate_base_kind: own-base
merge_validation: deferred to merge time — gastownhall/gascity GitHub merge queue (verified via rules/branches/main); mayor must route current conflicts for rebase-only refresh before MPR can merge. GC_MPR_REGENERATE=0 remains configured. See deployer merge-time-guarantee.md Gap 1.
criterion-6-merge-validated: PASS — bazel build --config=remote-exec --noremote_local_fallback --keep_going //...; 1411 targets; 22254 actions; 81.199s; nogo lint/vet included.
build_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/build.output.log; sha256=1d23fb596f5e74cd3c756cb8ab4b4fb47f48a6982da34f83f509122eaad9b817; bytes=16476; lines=98
merge_rules_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/merge-rules.json; sha256=985196a749df182414b7afbeb662ff7bd9ca4eb6c1a509b2e372d1ee981f3bfb; bytes=816; lines=1

## Fast lane

fast_lane: PASS before the suite launch; same committed merge tree throughout.
policy_lane: PASS — fresh-tree-run.sh -> gate-base.sh run -> isolated-test-run.sh --rbe -> run-pinned-lint.sh -> make fmt-check; make lint; make-check shell guards; make check-hooks. GLT_RECORD pin=2.14.0 resolved=2.14.0 mismatch=false.
policy_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/policy.output.log; sha256=3ff76b3c050ea745e108a8fa68eb54780c3b95fd3e910b901c168c211d15e6ef; bytes=16721; lines=102
drift_lane: PASS — separate fresh-tree-run.sh view; make bazel-sync then git diff --exit-code (CI command and drift condition).
drift_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/drift.output.log; sha256=b9874572684fe805d2a1113e420c694f2cac5334dd13a26ec89d2bff4e78d3d2; bytes=7150; lines=55
failure_attribution: none; policy_attribution: none; waiver_ref: none.

## Full-suite evidence

```text
launcher: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-validate.2bda1O --base fe971e2f2ae3a69916a82bab97f0d2dbef3d51e7 --bead-json /var/tmp/gc-gate-ga-txa0crm.U0jMXI/bead.json --owned /var/tmp/gc-gate-ga-txa0crm.U0jMXI/owned.tsv --run-dir /var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence
heavy_mode: none
coverage_check: full-suite via the five mandatory phases and native floor
local_floor: 16/16 steps, all exit 0; environment-absence roots selected by package, plus beadstest, Darwin compile, runner self-tests
owned_topup: none (all 31 already verified)
test_exec_mode: remote-exec
fallback_reason: none
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
admission: {"class":"remote","floor_gib":25.0,"free_gib":45.23769760131836,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 10032.535
latency_leg: measured; both budget tests PASS; attempt=1; probe=clear; farm wait=20s; threshold=50; test load1=34.83 on 32 CPUs
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: 0
run_start_load: 5.18
run_max_load: 6.62
run_mean_load: 6.07
run_readings: 6
```

Native floor load readings above are local; remote phases have no host load wait. Latency uses its separate worker probe. ICU74: OK, recorded host libraries (icu74.txt). Dolt/tmux preflight passed; this repository has no testcontainers dependency or container-backed test suite.

Actual phase flags (verbatim from execution.json):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Phase commands: unit runs make check (bazel test //... plus shell guards); acceptance runs --config=acceptance over acceptance_test and acceptance_solo_tests; integration-packages runs --config=integration //test:integration_packages; integration-full runs --config=integration --test_arg=-test.v --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ //test/integration:integration_test; integration-latency runs only that regex, unsharded, with the gate-farm-probe.sh run-under. The retained phase stdout and BEP name every actual test action.

| Phase | Results | Remote executed | Cached | Retries | Window valid |
|---|---:|---:|---:|---:|---|
| unit | 378 | 378 | 0 | 0 | True |
| acceptance | 24 | 24 | 0 | 0 | True |
| integration-packages | 85 | 85 | 0 | 0 | True |
| integration-full | 14 | 14 | 0 | 0 | True |
| integration-latency | 1 | 1 | 0 | 0 | True |

```text
BEP predicate: all 502 results strategy=remote, uncached locally/remotely, attempt 1, started within phase window, terminal PASSED verdict.
criterion3_valid: true
exit_code: 0
waiver_ref: none
```

## Test results and skipped-test proof

```text
test_cmd_scope: full-suite
test_counts: PASS=134343 FAIL=0 SKIP=377
```

Counts are top-level result lines across 518 retained logs, including repeated execution of the same name across tiers; they are not a count of distinct tests. No diff-owned test or subtest failed or remained skipped.

```text
diff_tests_executed:
  TestControllerLossLeavesSelfHandoffProvisional PASS (integration-packages-074fc6c34a1fb2577127.log, unit-64014bc0bb3833bc198c.log)
  TestCreateHandoffMailRoutesThroughProviderSeam PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-637c74c578a2b17fc654.log)
  TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession PASS (integration-packages-cb110414c557c0928e82.log, unit-fd986cf5bab8709b8d45.log)
  TestDoPrimeWithHook_DeliveredStartupPromptKeepsStepReminder PASS (integration-packages-9326181a736eb30b71ca.log, unit-1ab526405ace21879f85.log)
  TestDoPrimeWithHook_JSONModeDoesNotArchiveAutoHandoff PASS (integration-packages-e4245ddf6a37030111bc.log, unit-fd986cf5bab8709b8d45.log)
  TestDoPrimeWithHook_SessionStartDedupsAutoHandoffAndKeepsOrdinaryMailOpen PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-d9e667428f972d067bd3.log)
  TestFailedSelfHandoffRemainsProvisionalAndAuditable PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-2ef13e1b6898c5544764.log)
  TestHandoffStagedArmLandsInTheBindingOnAMigratedCity PASS (integration-packages-830685c60d0c2dd88567.log, unit-f93a0ca96ccc61d0cae9.log)
  TestLiveSuccessorReleasesStagedHandoffExactlyOnce PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestNonRestartingHandoffModesRemainOrdinaryMail PASS (integration-packages-75af79934639abaae69c.log, unit-493754ea2d5b4d8858c8.log)
  TestOriginatingIncarnationCannotReleaseStagedHandoff PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-3d6d3036a2f5902472b5.log)
  TestReconcileReleasesStagedHandoffAcrossDistinctStores PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-088cc38d26d094729229.log)
  TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore PASS (integration-packages-b72a67a2f9444af705e3.log, unit-912ab643330b35bd0b6b.log)
  TestRestageAfterStagedTimeoutRestartsReleaseLifecycle PASS (integration-packages-568812e4d525bdd1f974.log, unit-1ab526405ace21879f85.log)
  TestRestageDefersToControllerWhenEarlierBriefIsReleasable PASS (integration-packages-75f7330b9b673df65e4f.log, unit-d9e667428f972d067bd3.log)
  TestRestageIgnoresDanglingStagedPointer PASS (integration-packages-18745ddf7d62465654f5.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestRestageLeavesResolvedPriorBriefAlone PASS (integration-packages-bc482d2ae768251ad47f.log, unit-752a0bf25b15b465517a.log)
  TestRestageRoutesEachClassToItsOwnStore PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-abd97d2cb9b450225d57.log)
  TestRestageSupersedesPendingStagedBrief PASS (integration-packages-509ef0a6cd81866833cc.log, unit-637c74c578a2b17fc654.log)
  TestRestageSupersedesWrittenOffBriefEvenAfterTokenChange PASS (integration-packages-cb110414c557c0928e82.log, unit-fd986cf5bab8709b8d45.log)
  TestRestartableSelfHandoffStagesBeforeRestartRequest PASS (integration-packages-821a75e4a503601af052.log, unit-4d20b3d28497f844aa39.log)
  TestRetryAfterFailedRestartSupersedesProvisionalBrief PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-423c6a0da1986b90e9a7.log)
  TestSelfHandoffRestartNotRecordedWording PASS (integration-packages-e5330b8c981927473156.log, unit-cd6930bb0d0da8646500.log)
  TestSessionReconcilePhaseTraceUsesDistinctSites PASS (integration-packages-75af79934639abaae69c.log, unit-493754ea2d5b4d8858c8.log)
  TestSessionStartAutoHandoffUsesProvidedStoreWithoutOpeningCity PASS (integration-packages-cb110414c557c0928e82.log, unit-2bde55fab377f50b4739.log)
  TestStagedHandoffTimeoutNeverReleasesToOriginatingIncarnation PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-27b23e708b477fce4dad.log)
  TestWorkerSiteHandoffRecordsTheRestart PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestHandoffLifecycleEventsAreKnownAndTyped PASS (unit-c5e75abf11f6f150560a.log)
  TestBulkArchiveLeavesAStagedHandoffOpen PASS (unit-b061b554055944af2e43.log)
  TestExistingOrdinaryAutoHandoffRemainsVisible PASS (unit-b061b554055944af2e43.log)
  TestStagedHandoffIsInvisibleAcrossEveryMailReadSurface PASS (unit-b061b554055944af2e43.log)
GATE_TEST_EVIDENCE_RESULT ok: 31 diff-owned tests verified by name
```

skip_justification: non-owned fast-unit/process exclusions, OS-specific paths, optional fixtures/providers/legacy binaries, and existing placeholders are inventoried with their printed reasons. All environment-absence groups received the mandatory native floor (13 package groups plus three compile/runner checks), every step exit 0. Remaining top-up skips are inventoried and none is diff-owned. Live pack-registry checks were additionally executed successfully by the pack CI job.

Shared-hunk check: cmd/gc/session_reconciler_test.go changes the reconciler test helper; the three new staged_handoff_*_test.go files add helpers used by the new tests. Every command-package skip was independently rerun at the pinned base with the same unit/integration environment: unit 144/144 and integration 28/28 skip records have the same full test name and reason. No new, changed-reason, diff-owned or reasonless skip. The new helpers serve the passing new tests. internal/events/handoff_payloads_test.go and internal/mail/beadmail/staged_handoff_test.go are new and all their tests pass, with no package skips. The three internal/session shared-hunk files (fields_test.go, info_apply_patch_test.go, info_codec_test.go) have no skip site or package skip; their affected field/codec tests pass. These _test.go helpers are not compiled into other packages' test binaries. No production test-helper hunk was found.

The only three skip sites in touched candidate files were checked with --site: cmd/gc/cmd_prime_test.go:1269 and :1336, cmd/gc/worker_site_test.go:1819 — all UNCHANGED, line_in_hunk=no, func_touched=no, shared_hunks_in_file=0.

skip_unit_comparison: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/skip-compare-unit.txt; sha256=2f873d5a75d25e2c6c33532c4419ce3de4a77d31f12eca8fd43f4f09bb21d9bf; bytes=36320; lines=145
skip_integration_comparison: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/skip-compare-integration.txt; sha256=ab29ad6415312bc99bf32008bfe4d9e30ad1b13b0ea65f7b627dc0277cd91ff8; bytes=6132; lines=29
skip_sites: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/skip-sites.txt; sha256=1645d533521ad7d7e36c4fcc43fb60be726ce78d77a542d3ecdfa2c4d6689f4a; bytes=585; lines=3
shared_hunk_explanation: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/diff-tests-explain.txt; sha256=28aa39e58b221ad797b9cb5334464981f4a769bb3621f6a30576f0fc9fe9a357; bytes=63394; lines=533
skip_inventory: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/skip-inventory.json; sha256=775dcda42629700f3fd9de5337dd714dd8558f0fd408ba4635df0c920d523b17; bytes=150019; lines=2916
floor_topup: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/floor-topup.json; sha256=479fefa1d306222f3a467da8d0ea4166450c1e3128fbb8b46f0f7057e967d2ce; bytes=39610; lines=901
by_name: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/by-name.txt; sha256=3d21766e1f80fc6019b2c54e494df7377c9a5db8f32b44a74aa42ba1bd7f61d8; bytes=4807; lines=37

## CI required-job accounting

ci_required_closure: python3 packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.2bda1O fe971e2f2ae3a69916a82bab97f0d2dbef3d51e7 021a34ce16b85d906dd41e1b358b9caf8756bb34 fork; workflow blob=e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status; closure tool exit 0.

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | ci_required_triggers.py uses the merged workflow and repository matcher; shared filter MATCH on internal/events; suite_mode full; ci/closure.txt. |
| runner-policy | LOCAL-PASS | Exact python3 .github/workflows/scripts/runner_policy.py, EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH=, GITHUB_OUTPUT retained; exit 0; ci/runner-policy.log. |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | Resolved runner_windows=blacksmith-4vcpu-windows-2025. windows_reach: none, exit 0, empty stdout; no Windows closure input or module file changed, so extra Windows vet not required. |
| pack-gate | LOCAL-PASS | Exact two workflow run steps, including their env/retry loops: scripts/update-bundled-gastown-pack --check, then GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live. Both first attempts exit 0; source pins match 0.1.12; command and acceptance live registry canaries pass. Fresh view, load gated, isolated, detached; ci/pack-gate.log. |

deferred_to_pr_ci: credential-provider-windows (non-linux-runner)
not_covered: none
windows_reach: none (exit 0)
ci_lane_run: n/a (no CI-config change)
CI PASS here authorizes opening the PR. Merge requires CI / required and the deferred Windows job to be green on the PR head, plus repository merge-queue checks.

CI network precondition: timeout 30 git ls-remote --exit-code https://github.com/gastownhall/gascity-packs.git HEAD through isolated-test-run.sh, exit 0. No host gap was deferred.
ci_closure_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/ci/closure.txt; sha256=a53e5829fc71cde1b3b79a51a7c30b21e1be9e7004ff7966725f05f1498d14ff; bytes=1136; lines=15
ci_runner_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/ci/runner-policy.log; sha256=0e3c0660f779115678f4205f689489c973da0bc33e8ee99122bdeb8262b217e2; bytes=2234; lines=18
ci_pack_evidence: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/ci/pack-gate.log; sha256=0221a9d3e64eb7cb415a072d5598b720520473996a420982321eade4cec834a2; bytes=6021; lines=19

## Dashboard, run disclosure and retained evidence

dashboard_check: PASS in the active pre-commit hook; the five-phase unit run also freshly executed dashboard targets. dashboard_smoke: the same hook completed make dashboard-check dashboard-smoke during scratch merge creation (npm preview served successfully by the target's curl guard); no preview process remains in this scratch. Hook-created npm/dist directories were removed before the gate build; tracked tree remained identical to the reviewed source.

run_disclosure: Initial criterion-6 launch refused before compilation because scratch RBE definitions were absent (exit 70); definitions were provisioned by rbe-local-rc.sh and the build then passed. One additional skip diagnostic accidentally ran on the feature scratch and passed; retained as base-unit-wrong-cwd.log and excluded from base proof. Corrected diagnostics ran in the pinned-base checkout with a SHA guard. Go test SYN bytes were removed only in derived baseline text logs, matching gate-rbe-evidence.py normalization; raw logs remain retained. No failed test was retried or discarded. Full-suite results are from one complete farm run, with no local fallback or stitching.

execution_compact: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/execution-compact.json; sha256=8131b5879f59e578881b060b2f97926a5eb0d7da596353d9bcaaed60c0973158; bytes=4463; lines=118
execution_raw: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/execution.json; sha256=f6434399888b3f0d67251a1b83b3697c2541b0fd4378935d558444d1fe91e666; bytes=207453; lines=3957
latency_raw: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/suite/evidence/remote/latency-leg.json; sha256=5fb46a0eaa665e88abfc87bf28ee869fb8942ae909b101938f0111c43d532ca2; bytes=1812; lines=56
icu74: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/icu74.txt; sha256=dc1ab2e8ccf94bcf9a0c827a085e548134cafef5ec8378acc25ae4c948c21196; bytes=616; lines=4
raw_artifact_manifest: /var/tmp/gc-gate-ga-txa0crm.U0jMXI/artifact-manifest.tsv; sha256=fff69c8c189c7467e63916d2e4a0c623b2b47737e3f7ad1a83b7be04493e1357; bytes=305318; lines=1752

All raw logs/manifests remain outside this committed record, retained under /var/tmp/gc-gate-ga-txa0crm.U0jMXI. No merge was performed; merge authority is MPR via mayor.
