# Native acceptance process-floor release gate

**Verdict:** **PASS**

Deploy: ga-5nau0tb6. Build: ga-14bdnm9x. Review: ga-75jtigxo.
Reviewed source: `97b23c4e2f5adb1ecabaeefe9a86f1b9e0bea0b5`.
Tested merge: `5277f4e1fd08156914ac82d1d7f52f00e5c63a18`; retained at `refs/release-gates/ga-5nau0tb6/reread-GWHJSc/tested-merge`.
gate_base: `944ef25f3b2b6f23a9d4b835ec6ec1979a397005`. gate_base_kind: current-tip.
merge_state: clean (origin/main@944ef25f3b2b).
merge_validation: merge result built and vetted here.
Evidence root: `/var/tmp/gc-ga-5nau0tb6-reread.GWHJSc`. Original held-run evidence is retained separately; this record covers the fresh run.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Reviewer PASS binds to the exact source; reviewer-added-tests check accepts the two append-only test files. |
| 2 | Acceptance criteria | PASS | Startup branch matches gc bd; samples alternate; persistent inversions fail the required lane; real over-budget readings return immediately. All owned tests and the edited row pass. |
| 3 | Tests and CI accounting | PASS | Full-suite runner valid, exit 0; five fresh farm phases, all 16 native steps, skip attribution and six leaf accounts below. |
| 4 | High-severity findings | PASS | Zero unresolved HIGH findings; reviewer-added tests close F1/F2. Remaining findings are minor/info. |
| 5 | Clean source | PASS | Source checkout clean before writing the record; publication uses a record-only commit and a clean-tree push check. |
| 6 | Merge state and build/vet | PASS | Clean merged tree; bazel build --config=remote-exec --noremote_local_fallback //... exit 0, including nogo. |
| 7 | Single feature theme | PASS | One acceptance process-floor measurement change and its regression coverage. |

reviewer_tests_verified: 167aad1925ca97769576827c2d20da2e3dfcd1c9 -> 97b23c4e2f5adb1ecabaeefe9a86f1b9e0bea0b5 files=test/acceptance/helpers/bd_time_test.go,test/processfloor/processfloor_test.go.
fast_lane: PASS; format, make lint, all four make-check shell guards, and make bazel-sync plus git diff --exit-code, each in its own fresh view.
policy_lane: make lint with remote-exec/no-fallback flags; PASS.
drift_lane: make bazel-sync then git diff --exit-code; PASS.
heavy_mode: none (computed). ci_lane_run: n/a (no CI-config diff). waiver_ref: none.

test_cmd: gate-criterion3.sh; unit make -k check, acceptance, integration-packages, full integration minus the registered latency tests, the registered monitored latency leg, and required native floor. Exact phase commands/BEPs are in criterion3/evidence/execution.json.
test_cmd_scope: full-suite.
test_exec_mode: remote-exec. fallback_reason: None.
test_counts: PASS=159219 FAIL=0 SKIP=618 (top-level result lines summed over jobs, not unique tests).
test_log_dir: `/var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/criterion3/evidence/remote/logs`.
farm_test_seconds: 14601.269.
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 50.546485900878906, "waited_seconds": 0.001}.
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}.

test_exec_bep (every result has other_strategy=0):

| Phase | Results | Remote | Cached remote/local | Retried | Window valid |
|---|---:|---:|---|---:|---|
| unit | 427 | 427 | 0/0 | 0 | True |
| acceptance | 25 | 25 | 0/0 | 0 | True |
| integration-packages | 87 | 87 | 0/0 | 0 | True |
| integration-full | 15 | 15 | 0/0 | 0 | True |
| integration-latency | 1 | 1 | 0/0 | 0 | True |

owned_topup: none needed. floor_topup: 16/16 steps exit 0.
latency_leg: measured/PASS on attempt 1; clear probe after 10 s; window 11 readings, max_load1=43.91, over_threshold=0, threshold=50; both registered tests PASS and absent from the main integration leg.
local_native_load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=17.87 run_max_load=18.64 run_mean_load=18.07 run_readings=7 wait_first_load=87.28 wait_max_load=87.28 wait_mean_load=51.15 wait_readings=61 read_errors=0`.
load_evidence (native floor): {"load_threshold": 15.0, "load_wait_timed_out": true, "load_waited_seconds": 1800, "run_max_load": 18.64, "run_mean_load": 18.07, "run_readings": 7, "run_start_load": 17.87}.
The outer remote-driver host telemetry is a separate scope (wait 0, start 23.67, max 92.88, mean 53.29, 113 readings); latency uses the worker window above.

diff_tests_executed:

```text
diff_tests_executed:
  TestAcceptanceProcessFloorTakesGCBDsDiscoveryBranch PASS (integration-packages-9326181a736eb30b71ca.log, unit-1ab526405ace21879f85.log)
  TestMeasureMarginalOverFloorInterleavesTheTwo PASS (unit-2af37a65322c095289c3.log)
  TestMeasureMarginalOverFloorKeepsTheBestSampleOfEach PASS (unit-2af37a65322c095289c3.log)
  TestMeasureMarginalOverFloorNeverRemeasuresAnOverBudgetReading PASS (unit-2af37a65322c095289c3.log)
  TestMeasureMarginalOverFloorReadsThroughALoadBurst PASS (unit-2af37a65322c095289c3.log)
  TestMeasureMarginalOverFloorRemeasuresAnInvertedReading PASS (unit-2af37a65322c095289c3.log)
  TestMeasureMarginalOverFloorReportsAPersistentInversion PASS (unit-2af37a65322c095289c3.log)
  TestArgsHandsEachCallerItsOwnArray PASS (unit-3a0b42835aa366c358ac.log)
  TestArgsReturnsAFreshSlice PASS (unit-3a0b42835aa366c358ac.log)
  TestBeadsProxiedDefaultNativeLane PASS (acceptance-747f813113e1da321d1d.log)
GATE_TEST_EVIDENCE_RESULT ok: 10 diff-owned tests verified by name
TestBeadsProxiedDefaultNativeLane/bd-list-gc-side-budget PASS (official reader scan/judge API; acceptance log verified).
```

The edited row measured gc-side 84 ms, process floor 50 ms, marginal 33 ms (rounded separately), best of five, against the unchanged 100 ms budget. TestTutorial01 and all 35 subtests PASS in the integration-packages process lane.

skip_inventory / skip_justification: 676 records including subtests map to 320 unchanged source sites. Fast-tier exclusions, platform/tool absence, optional provider capability and explicit fixture-topology exclusions are unrelated to this diff. Nine pristine-base remote groups and one exact native base command reproduce every skip reason in shared/helper reach; unresolved=0, unknown import roots=0, diff-owned skips=0. Full reasons and site proofs: skip-audit-summary.json, farm-skip-site-index.json, farm-skip-manual-resolutions.json, native-skip-resolved.json, all-skip-site-proofs.json, skip-baseline-evidence, native-baseline-evidence under the evidence root.
failure_attribution: none required (this fresh run has no FAIL). delta_gate: shadow/full; no prior PR record; no narrowing.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-5nau0tb6-reread.qmsTGy 944ef25f3b2b6f23a9d4b835ec6ec1979a397005 97b23c4e2f5adb1ecabaeefe9a86f1b9e0bea0b5 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
ci_required_accounting:

| Leaf job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-runner-policy.output python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-runner-policy.log |
| changes | LOCAL-PASS | ci_required_triggers.py; exit 0; /var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-closure.txt |
| credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | bash /var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-windows-lane.sh; exit 0; /var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-windows.output |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; reach none; /var/tmp/gc-ga-5nau0tb6-reread.GWHJSc/ci-windows-native-reach.json |

windows_reach: none for the native Windows test allowlist (own GOOS=windows test-binary closure, exit 0).
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none. host_gaps: none.
Opening the PR is cleared. Merge readiness requires CI / required and the deferred native Windows job to succeed on its exact head; MPR owns that check and the merge.
