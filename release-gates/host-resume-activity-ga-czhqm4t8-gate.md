# Release gate: Host-resume activity floor

**Verdict:** **PASS**

Bead ga-czhqm4t8; build ga-euj79c; review ga-cak0eic3.

- DEPLOY_MODE: remote; PUSH_REMOTE: fork; base_branch: main.
- reviewed_commit: 22683c5e72d93bb80c8b9b959364de66cfecedc7.
- gate_base_kind: current-tip; gate_base: 6bd609f5bfb41f43f444843b1220d0cde4473446.
- merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446).
- merge_head: de80fecd29ab86741d8927edad2e1f2e2938a54e; merge_tree: 73df3cfb1c38b31356442377900f272298286730.
- merge_validation: clean merged result built, vetted and tested here; MPR and required CI validate the final merge against its then-current base. No deployer rebase or merge.
- docs/PROJECT_MANIFEST.md is absent; gate commands come from TESTING.md, Makefile and actual workflows.

| # | Criterion | Evidence |
|---|---|---|
| 1 | PASS — Review | ga-cak0eic3 records verdict pass for the exact resolved source; no carryover or test-only SHA substitution. |
| 2 | PASS — Acceptance | All seven verification items inspected against source and fresh named tests: detector math/reload, 20-session idle/no-resume controls, provider routes, floor edge cases, both stall predicates/execution backstop, raw ordering readers and worker boundary. Generated config/schema tests pass. |
| 3 | PASS — Tests | Five fresh remote phases and required native floor complete; all 34 owned roots PASS in their own package with no failed/skipped descendant. Skips and required CI leaves accounted for below. |
| 4 | PASS — Findings | Zero unresolved HIGH review findings; advisory limits and follow-ups remain explicit. |
| 5 | PASS — Clean | Frozen scratch is clean with source/base/parents/tree re-derived. Publication requires only the gate-record addition and a clean isolated deploy branch after committing it. |
| 6 | PASS — Merge/build/vet | Clean merge at the recorded main; full Bazel build and whole-tree make vet/nogo have independently re-read SUCCESS BEPs. |
| 7 | PASS — Theme | One host-resume activity feature across detector, legacy decision plumbing, tests, requirement row and generated configuration documentation. No independent feature or internal prompt paths. |

## Test evidence

- test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/bead.json --owned /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/owned.tsv --run-dir /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence; exit 0.
- test_cmd_scope: full-suite.
- test_counts: {"FAIL": 0, "PASS": 150775, "SKIP": 515} (result observations; repeated jobs count separately).
- test_exec_mode: remote-exec.
- fallback_reason: none.
- waiver_ref: none.
- failure_attribution: none.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 79.94672775268555, "waited_seconds": 0.001}.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}.
- farm_test_seconds: 12470.27.
- heavy_mode: none, computed.
- test_log_dir: /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/logs.
- owned_topup: No missing owned test; all 34 roots passed in cmd/gc unit artifacts.
- floor_topup: 16 steps, all exit 0; beadstest, Darwin compile, runner self-tests and selected environment-absence packages.
- latency_leg: Dedicated integration-latency phase, after clear farm probe; full integration excludes these names.
- ci_lane_run: n/a: no CI-config changes.

| Phase | Test actions | Execution |
|---|---:|---|
| unit | 408 | PASS; remote attempt 1 inside phase window; no cache or fallback |
| acceptance | 25 | PASS; remote attempt 1 inside phase window; no cache or fallback |
| integration-packages | 86 | PASS; remote attempt 1 inside phase window; no cache or fallback |
| integration-full | 14 | PASS; remote attempt 1 inside phase window; no cache or fallback |
| integration-latency | 1 | PASS; remote attempt 1 inside phase window; no cache or fallback |

test_exec_bep: all five raw phase predicates independently regenerated; 1068 CAS log/XML hashes and 534 normalized logs verified.

Expanded canonical commands:

- test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
- test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

diff_tests_executed:

- cmd/gc / TestActivityFloor_Raise PASS; no failed/skipped descendant.
- cmd/gc / TestActivityFloor_StoredAsPlainWallInstant PASS; no failed/skipped descendant.
- cmd/gc / TestBeadReconcileTick_FloorsExecutionBackstopAfterHostResume PASS; no failed/skipped descendant.
- cmd/gc / TestBeadReconcileTick_FloorsIdleTimeoutAfterHostResume PASS; no failed/skipped descendant.
- cmd/gc / TestCityRuntimeActivityFloor_NilDetectorIsZeroFloor PASS; no failed/skipped descendant.
- cmd/gc / TestCityRuntimeControlDispatcherTick_DetectsResume PASS; no failed/skipped descendant.
- cmd/gc / TestCityRuntimeReload_KeepsResumeFloorAndDetectionCount PASS; no failed/skipped descendant.
- cmd/gc / TestCityRuntimeTick_DetectsResumeAndFloorsActivity PASS; no failed/skipped descendant.
- cmd/gc / TestCityRuntimeTick_FloorSurvivesLaterQuietTicks PASS; no failed/skipped descendant.
- cmd/gc / TestControlDispatcherTick_FloorsIdleTimeoutAfterHostResume PASS; no failed/skipped descendant.
- cmd/gc / TestExecutionBackstopResumeFloorHoldsNudgesAfterHostResume PASS; no failed/skipped descendant.
- cmd/gc / TestExecutionBackstopResumeFloorNudgesOnceGraceElapsesPastResume PASS; no failed/skipped descendant.
- cmd/gc / TestExecutionStalledDrainDoesNotStrandAMidDrainWake PASS; no failed/skipped descendant.
- cmd/gc / TestIdleTracker_ResumeFloorClaudeMuxSeatActivityRouteOnly PASS; no failed/skipped descendant.
- cmd/gc / TestIdleTracker_ResumeFloorClaudeSeatContentRouteUntouched PASS; no failed/skipped descendant.
- cmd/gc / TestIdleTracker_ResumeFloorMakesStaleActivityNotIdle PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_NoResumeControlStopsStaleSessionsFirstPass PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorClaimHolderStallRecyclesOncePastResume PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorClaimHolderStallSparesHolderBeforeThreshold PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorDoesNotCancelIdleRespawnAck PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorDoesNotCancelUnackedIdleRespawnDrain PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorDoesNotVetoIdleRespawnBegin PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorProgressStallClaimless PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorProgressStallRecyclesOncePastResume PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorSessionsStopOnceIdleTimeoutElapsesPastResume PASS; no failed/skipped descendant.
- cmd/gc / TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_FloorSnapshotIsStableAcrossLaterResume PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_LongSuspendFiresAndFloorsAtWallNow PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_NoPreviousReadingNeverFires PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_NormalTickDoesNotFire PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_SecondResumeAdvancesCounterAndRaisesFloor PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_ShortSuspendDoesNotFire PASS; no failed/skipped descendant.
- cmd/gc / TestResumeDetector_ThresholdIsStrictlyGreaterThanTwicePatrol PASS; no failed/skipped descendant.
- cmd/gc / TestSystemResumeClock_MonotonicAdvances PASS; no failed/skipped descendant.

Acceptance test mapping:

- 1_detector_math: TestResumeDetector_LongSuspendFiresAndFloorsAtWallNow, TestResumeDetector_NormalTickDoesNotFire, TestResumeDetector_ShortSuspendDoesNotFire, TestResumeDetector_ThresholdIsStrictlyGreaterThanTwicePatrol, TestResumeDetector_NoPreviousReadingNeverFires, TestResumeDetector_SecondResumeAdvancesCounterAndRaisesFloor, TestCityRuntimeReload_KeepsResumeFloorAndDetectionCount.
- 2_idle_and_no_resume_control: TestReconcileSessionBeads_ResumeFloorSparesQuietSessionsOnFirstPass, TestReconcileSessionBeads_ResumeFloorSessionsStopOnceIdleTimeoutElapsesPastResume, TestReconcileSessionBeads_NoResumeControlStopsStaleSessionsFirstPass.
- 3_provider_routes: TestIdleTracker_ResumeFloorClaudeSeatContentRouteUntouched, TestIdleTracker_ResumeFloorClaudeMuxSeatActivityRouteOnly.
- 4_floor_rows: TestActivityFloor_Raise, TestActivityFloor_StoredAsPlainWallInstant, TestResumeDetector_FloorSnapshotIsStableAcrossLaterResume.
- 5_stall_and_execution_families: TestReconcileSessionBeads_ResumeFloorProgressStallClaimless, TestReconcileSessionBeads_ResumeFloorProgressStallRecyclesOncePastResume, TestReconcileSessionBeads_ResumeFloorClaimHolderStallSparesHolderBeforeThreshold, TestReconcileSessionBeads_ResumeFloorClaimHolderStallRecyclesOncePastResume, TestExecutionBackstopResumeFloorHoldsNudgesAfterHostResume, TestExecutionBackstopResumeFloorNudgesOnceGraceElapsesPastResume, TestBeadReconcileTick_FloorsExecutionBackstopAfterHostResume.
- 6_order_only_readers: TestReconcileSessionBeads_ResumeFloorDoesNotCancelIdleRespawnAck, TestReconcileSessionBeads_ResumeFloorDoesNotVetoIdleRespawnBegin, TestReconcileSessionBeads_ResumeFloorDoesNotCancelUnackedIdleRespawnDrain.
- 7_worker_boundary: TestGCNonTestFilesStayOnWorkerBoundary.

## Fast lanes and runtime

- policy_lane: C6 full-tree make vet/nogo; run-pinned-lint.sh -- make fmt-check-changed; check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary, check-hooks; 13 policy Bazel targets. All pass before runtime setup/suite.
- drift_lane: make bazel-sync then git diff --exit-code and empty status including untracked files; seven generated-artifact checks; pinned-base OpenAPI breaking check. Three independent fresh views; none changed the suite scratch.
- runtime: ICU74 OK, disk floor PASS, heavy classification none. Wrapper PASS-THROUGH is recorded honestly; the temporary checkout is disposable, Bazel runs on the farm, native checks scrub live-store selectors and use pinned bd.
- known_red_precheck: tracked routed-row guard passes; stale tracker ga-1ptv4jey did not park the gate.

## Skip/source accounting

- skip_inventory: {"base": "6bd609f5bfb41f43f444843b1220d0cde4473446", "native_compared": 3, "native_events": 39, "remote_compared": 186, "remote_events": 501, "scope": "skip comparisons only; manual site and helper review and CI accounting remain before gate verdict", "source": "22683c5e72d93bb80c8b9b959364de66cfecedc7", "tree": "73df3cfb1c38b31356442377900f272298286730", "unchanged_remote_sites": 291, "unreachable_shared_hunks": 315, "verdict": "PASS"}.
- skip_justification: All 327 reported source-site checks are unchanged and their complete files are blob-identical to pinned main. Seven shared/helper-hunk entries are confined to cmd/gc; full tagged test-binary import closures select only that package for comparison. All 186 remote and three native reachable skip observations reproduce the same reasons at the untouched pinned base. Remaining skips are explicit lane/fixture opt-ins, OS/privilege/group constraints, unavailable archived or replay inputs, deliberate unsupported conformance rows and documented pending characterizations in unchanged code. The full integration variants exercise process lanes that unit mode deliberately skips. The 16-step native floor observes every classifier-selected environment absence, plus beadstest, Darwin compile and runner self-tests. Additional upstream-storage and localhost-SSH native observations both exit 0 but retain their original SKIP reasons; those are unchanged, non-owned files outside the changed helper closures, never test PASS claims. The single non-Go bridge skip is configured rules-ts non-applicability, with equal source/config blobs and an enabled dashboard counterpart passing. The detector and floor tests all pass; none of these skip guards was changed by the diff.
- No owned SKIP or FAIL was attributed or waived. Shared/helper-hunk packages and full import closures are retained; all required pinned-base comparisons match the original skip reasons.
- Manual audit evidence: /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/manual-skip-review.json.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 22683c5e72d93bb80c8b9b959364de66cfecedc7 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.

ci_required_accounting:

- changes | LOCAL-PASS | {"evidence": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/closure.stdout", "status": "LOCAL-PASS"}
- runner-policy | LOCAL-PASS | {"evidence": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/runner-policy.stdout", "status": "LOCAL-PASS"}
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/windows-native-reach.json", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
- credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/credential-windows-reach.stdout", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
- windows-lane | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/merge", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/run-ci-job.py", "windows-lane"], "rc": 0, "status": "LOCAL-PASS", "stderr": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/windows-lane.stderr", "stdout": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/windows-lane.stdout"}
- pack-gate | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/merge", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/run-ci-job.py", "pack-gate"], "rc": 0, "status": "LOCAL-PASS", "stderr": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/pack-gate.stderr", "stdout": "/var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/pack-gate.stdout"}

- windows_reach: {"credential": [], "native": []}.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner), credential-provider-windows (non-linux-runner).
- not_covered: none.
- All local jobs use their actual workflow commands and environments, run sequentially through fresh-tree/load/isolation wrappers. Deferrals remain test obligations on the exact PR head.
- This PASS authorizes opening the PR. MPR must verify CI / required and each deferred job before merging.

## Host load

Remote admission has no local-heavy load wait. Native checks, CI commands and native base comparisons retain threshold 15 / max-wait 1800 soft load waits. The following summaries retain threshold, waited_seconds, wait_timed_out, run_start_load, run_max_load, run_mean_load and run_readings:

- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.15 run_max_load=13.86 run_mean_load=13.30 run_readings=6 wait_first_load=12.15 wait_max_load=12.15 wait_mean_load=12.15 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/pack-gate.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.68 run_max_load=14.84 run_mean_load=14.42 run_readings=4 wait_first_load=14.68 wait_max_load=14.68 wait_mean_load=14.68 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.35 run_max_load=15.48 run_mean_load=14.37 run_readings=3 wait_first_load=13.35 wait_max_load=13.35 wait_mean_load=13.35 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/skip-base-evidence/native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.33 run_max_load=9.45 run_mean_load=9.18 run_readings=2 wait_first_load=8.33 wait_max_load=8.33 wait_mean_load=8.33 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/skip-base-evidence/owned-skip-integration-packages.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.65 run_max_load=9.34 run_mean_load=8.99 run_readings=3 wait_first_load=9.65 wait_max_load=9.65 wait_mean_load=9.65 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/skip-base-evidence/owned-skip-unit.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.91 run_max_load=11.66 run_mean_load=10.66 run_readings=6 wait_first_load=11.91 wait_max_load=11.91 wait_mean_load=11.91 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=34.66 run_max_load=40.43 run_mean_load=18.87 run_readings=46 wait_first_load=34.66 wait_max_load=34.66 wait_mean_load=34.66 wait_readings=1 read_errors=0
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/local-audit-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.01 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=9.01 wait_max_load=9.01 wait_mean_load=9.01 wait_readings=1 read_errors=0

## Retained evidence

- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/context.json; SHA256 49d35647e0a5f83984fb4440aa19fbe3c832649518a2d2cc4d148d38fcdc8cf4.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/resume-prerequisites-verified.json; SHA256 e5cffff8d725befaa105fa4cf95a56097556ce6d92e380f9957ce332298de83a.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/fast-verified.json; SHA256 49c0b991a42245d14a051ce0331b2f20fb72a7ab87ff33419943ee452b9e8203.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/acceptance-context-verified.json; SHA256 093deaec0d867510571211404db46e12a88061dc554915bbedd4428b4c6184cc.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/c3-run/evidence/execution.json; SHA256 3ea5b4d8228c7bc3474b976397dbe51b3417cc74363dd2aeab8c1c58aaded857.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/resume-c3-audit.json; SHA256 33966778e88d99f7170ae061a39b1997a2c4136710342d1478edfd58bcf964fd.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/owned-package-verified.json; SHA256 9761d9f770a6a4bebb3890e64bd94108336d1d83f04d41c4b48870b154ca484c.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/skip-proof-automated.json; SHA256 f807a3633a108072ecabbeed053dbf860573279b876ec4cdbf4f08f861ec2160.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/manual-skip-review.json; SHA256 96bd26ec2e033d1626a7ccc03548875aacd9c3adbbe0533ac59414ba3a3d2237.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/ci-required/accounting-verified.json; SHA256 7b95303cae9cc8f1c190aa42dfc4f5c20fc6d1da707738b4e7a5b1ff69e6352c.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/non-go-skip-reviewed.json; SHA256 d4b75f85e74f88668eaae1e51b91d1b435e8a0e8feeb0955266db49183fd41d6.
- /var/tmp/gc-gate-ga-czhqm4t8.sdzquhx5/supplemental-floor/verified.json; SHA256 e2611e7ba0deb32056a6cd2a81dd34ef7160ad6c844a084f5390d1e406357a6c.
