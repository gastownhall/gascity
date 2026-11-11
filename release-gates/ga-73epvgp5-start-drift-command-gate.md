# Startup drift command diagnostics release gate

**Verdict:** **PASS**

Bead: ga-73epvgp5; build: ga-acmcvf35; review: ga-2joeesoy.
Evaluated: 2026-11-07T13:30:00.442303+00:00.
deploy_mode: remote; base_ref: origin/main; push_remote: fork.
reviewed_source: e188a20afa545b99b9468f13b5c467399a153d41
reviewer_tests_verified: 96707e5215c8a2e578e0410c88b361c7dbb2acd5 -> e188a20afa545b99b9468f13b5c467399a153d41 files=test/integration/run_drift_command_test.go
gate_base_sha: 06372a1b5d93054ebfb77abfaf018ef3a0e9afbc
merge_state: clean (origin/main@06372a1b5d93054ebfb77abfaf018ef3a0e9afbc)
gate_base_kind: current-tip
gated_merge: 212e943d04c3fddf715af11d8a837de482966ab6; tree: d5af767f5d503eda6688b2e1a4e218a915b9ce1e
merge_validation: MPR checks the current head, clearance and required CI at merge time; moving main is not covered by this snapshot.
Manifest: docs/PROJECT_MANIFEST.md is absent; the supplied seven-criterion protocol, AGENTS.md, TESTING.md and current CI define this gate.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-2joeesoy has verdict pass for the pinned source; reviewer-added-tests.sh check re-derived its test-only addition. |
| 2 | Acceptance criteria met | PASS | Nonzero exits report elapsed time, deadline state and error text; only the systemd-managed startup uses a bounded 3-minute command wait; the build retains gc.fixes_tracker=ga-dhnv01xi. All six owned tests pass by name below. |
| 3 | Full-scope tests and CI accounting | PASS | Five fresh farm phases, complete native floor, all six owned tests verified, and four CI leaf jobs accounted for. No waiver or attribution. |
| 4 | No unresolved high-severity findings | PASS | Review style/security/spec findings have zero unresolved HIGH findings. |
| 5 | Clean feature tree | PASS | Source and merge scratch were clean; the deploy branch adds only this gate record. The normal commit hook and clean final status are checked before publication. |
| 6 | Merge state and build | PASS | The clean merged tree matches git merge-tree exactly. Normal pre-commit hook and make check-hooks exit 0; bazel build --config=remote-exec --noremote_local_fallback --keep_going //... exits 0, including nogo. |
| 7 | Single feature theme | PASS | Three test/integration files change startup-drift command diagnostics and its systemd command bound. No production file or BD_BIN feature change. |

The original 60-second deadline is a plausible cause, not a reproduced diagnosis. The existing 15-second NFR-2 restart assertion remains in force. This change alone does not clear ga-cgrwds2b: after landing, that deploy needs its own fresh complete gate.

policy_lane: fresh-tree-run.sh -- gate-base.sh run -- bash fast-policy.sh; exit 0. Includes shell guards, pinned format check, make lint/nogo and policy/format/docs Bazel targets.
drift_lane: separate fresh-tree-run.sh -- gate-base.sh run -- bash generated.sh; exit 0. make bazel-sync, git diff --exit-code and empty tracked/untracked status; OpenAPI sync and genclient tests exit 0.
format_lane: pinned golangci-lint 2.14.0, changed files against the recorded base; exit 0.
heavy_mode: none (HEAVY_MODE mode=none shard_required=- watch=-).
ci_lane_run: n/a (no CI-config change).
failure_attribution: none; policy_attribution: none; waiver_ref: none.

test_cmd_scope: full-suite
test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh start --run-dir /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3 --key d5af767f5d503eda6688b2e1a4e218a915b9ce1e -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-73epvgp5 DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/merge --base 06372a1b5d93054ebfb77abfaf018ef3a0e9afbc --bead-json /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/bead.json --owned /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/owned-tests.tsv --run-dir /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
Phase commands (with the phase BAZEL_FLAGS above): unit = make -k check (full bazel test //... plus shell guards); acceptance = bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages = bazel test $BAZEL_FLAGS --config=integration //test:integration_packages; integration-full = bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=<gate-latency-tests.txt expression> //test/integration:integration_test; integration-latency = bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_filter=<same expression> --test_sharding_strategy=disabled --run_under=<gate-farm-probe.sh, threshold50 hold600 poll10> //test/integration:integration_test. Every phase exits 0; exact phase commands and probe text are retained by the command scripts and phase logs.
Native floor commands: each package runs go test -tags integration -count=1 -v -run <computed environment-absence parent set> ./<package>; then make test-fsys-darwin-compile, bash scripts/test-push-gate-lock.sh and bash scripts/test-local-concurrency.sh. These are the protocol-mandated native floor, alongside the complete Bazel suite. Each exact argv and exit status is retained in floor-topup.json.
Native systemd result: restart 5.7s against 15s budget, test PASS in 53.12s; package exit 0. Post-PASS cleanup says supervisor was already stopped, as in reviewer observations.
All farm phases force --nocache_test_results and --noremote_local_fallback; predicates require fresh first-attempt remote execution and terminal verdicts inside the phase window.
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":403,"remote_executed":403,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
farm_test_seconds: 11643.157
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: None
admission: {"class":"remote","floor_gib":25.0,"free_gib":57.30650329589844,"waited_seconds":0.001,"cap":2}
owned_topup: []
floor_topup: complete=true; 16 steps, each exit 0; package-selected environment-absence top-ups, pinned beadstest, Darwin compile and both runner self-tests.
latency_leg: complete=true; clear farm probe; both listed budget tests PASS in their own integration-latency phase.

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/logs logs=545 logs_with_results=478
test_counts: PASS=149061 FAIL=0 SKIP=512
  (top-level result lines, column 0, summed over the 545 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+55 more)
diff_tests_executed:
  TestDriftSystemdCommandTimeout_FitsALoadedRestartButStaysBounded PASS (integration-full-5bb45aeaf433c2072f8e.log)
  TestRunDriftCommand_DeadlineKillIsReportedAsSuch PASS (integration-full-40a57ff329ade1bacee5.log)
  TestRunDriftCommand_FailureReportCarriesTheElapsedTime PASS (integration-full-82ff0c4322b44203e167.log)
  TestRunDriftCommand_OtherNonZeroExitIsNotCalledADeadline PASS (integration-full-0c1c194fca4897b865f3.log)
  TestRunDriftCommand_SuccessAddsNoDiagnostic PASS (integration-full-23490fe556f84649266a.log)
  TestStartDrift_SystemdManaged_RestartsToNewBuildID PASS (floor-12.log; also SKIP: TestStartDrift_SystemdManaged_RestartsToNewBuildID in integration-full-b2288f9e45f57dd8ece2.log (PASS in another job))
  skip_reason: TestStartDrift_SystemdManaged_RestartsToNewBuildID (integration-full-b2288f9e45f57dd8ece2.log): start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable
GATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name
```
test_log_dir: /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/logs
skip_justification: Owned systemd farm SKIP is repaired by native PASS. All non-owned farm skips have unchanged source sites; 38 other package test-binary closures cannot reach the changed integration helpers. The 13 non-owned integration roots have identical skip reasons at the untouched pinned base. Native top-up skips are inventoried separately; unchanged optional environments and intentionally skipped provider conformance subtests do not exercise the changed helpers.

Native load evidence:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=73.19 run_max_load=77.45 run_mean_load=73.90 run_readings=13 wait_first_load=52.43 wait_max_load=75.84 wait_mean_load=54.44 wait_readings=61 read_errors=0
load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 73.19
run_max_load: 77.45
run_mean_load: 73.90
run_readings: 13
wait_first_load: 52.43
wait_max_load: 75.84
wait_mean_load: 54.44
wait_readings: 61
read_errors: 0
```

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/merge 06372a1b5d93054ebfb77abfaf018ef3a0e9afbc e188a20afa545b99b9468f13b5c467399a153d41 fork; workflow_blob=e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status.
ci_required_accounting: runner-policy | LOCAL-PASS | runner-policy                            LEAF       linux                      EVERY-PR; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/ci-accounting/runner-policy.log
ci_required_accounting: changes | LOCAL-PASS | changes                                  LEAF       linux                      EVERY-PR; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/ci-accounting/closure.txt
ci_required_accounting: credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/ci-accounting/closure.txt
ci_required_accounting: pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/ci-accounting/closure.txt
runner-policy command: EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained output file> python3 .github/workflows/scripts/runner_policy.py; exit 0 in merged tree.
windows_reach: none
deferred_to_pr_ci: none
not_covered: none

Compact execution record:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":57.30650329589844,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":403,"remote_executed":403,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":11643.157,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":66.881,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=64.29 load5=73.06 load15=41.02 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=46.04 load5=68.33 load15=40.14 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 33.79 63.64 39.34 6/998 55128"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30724}},"log_dir":"/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":175999}}},"test_cmd":["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"],"test_counts":{"PASS":149061,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed:\n  TestDriftSystemdCommandTimeout_FitsALoadedRestartButStaysBounded PASS (integration-full-5bb45aeaf433c2072f8e.log)\n  TestRunDriftCommand_DeadlineKillIsReportedAsSuch PASS (integration-full-40a57ff329ade1bacee5.log)\n  TestRunDriftCommand_FailureReportCarriesTheElapsedTime PASS (integration-full-82ff0c4322b44203e167.log)\n  TestRunDriftCommand_OtherNonZeroExitIsNotCalledADeadline PASS (integration-full-0c1c194fca4897b865f3.log)\n  TestRunDriftCommand_SuccessAddsNoDiagnostic PASS (integration-full-23490fe556f84649266a.log)\n  TestStartDrift_SystemdManaged_RestartsToNewBuildID PASS (floor-12.log; also SKIP: TestStartDrift_SystemdManaged_RestartsToNewBuildID in integration-full-b2288f9e45f57dd8ece2.log (PASS in another job))\n  skip_reason: TestStartDrift_SystemdManaged_RestartsToNewBuildID (integration-full-b2288f9e45f57dd8ece2.log): start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable\nGATE_TEST_EVIDENCE_RESULT ok: 6 diff-owned tests verified by name\n"}
```

Full execution record retained: 5045 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/execution.json; SHA256 f2b9cf0bf98442bdfe9f8cd6ba9345dff4ef01502df717d38b29bcc1458d6faa.
Full farm phase and artifact hash audit retained: 107 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/phase-audits.json; SHA256 3054584bbfe3792ad1745ae53354d21bf7191f9057d0e760fd6436b3b7a93646.
Full farm skip inventory retained: 3986 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/farm-skip-inventory.json; SHA256 b841bfd3d49146c5db2d0a4c49be51b6305f0bade545c1444b1f19ebef031d45.
Full farm skip-site proof retained: 4478 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/farm-skip-site-mapping.json; SHA256 1be323093758f5680f39284f3dfaffbcada86ecaddbb825926034c88ba5b155c.
Full helper reachability proof retained: 593 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/skip-helper-reach-proof.json; SHA256 ca345d729d6c88e7b1995ba146e84fb8696d3b4ce1d2d21edbed5228426de37e.
Full base skip comparison proof retained: 95 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/base-skip-comparison-proof.json; SHA256 33849055b4258e9febae7f5709c29a53817eb8f1e02f680db1b16b5981f2b22b.
Full native floor steps and commands retained: 909 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c3/evidence/remote/floor-topup.json; SHA256 cc53ac5c121337dcdbf71e463c101ffb4eda6450b3ffcf9c2f3ec608bac2c394.
Full native skip inventory retained: 475 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/native-skip-inventory.json; SHA256 8faf13f7e222049c070d299c16f5407bc4f6c62d50cb4fa55592545308484604.
Full CI-required accounting retained: 46 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/ci-accounting/accounting.json; SHA256 1bef937238146f2d82913ebba1dffb3f4847fdca1dbf23d302cf2e6ad26346b6.
Full build log retained: 120 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/c6-build.log; SHA256 18e7c92db52485715e5d89e32307aab873e00e16d9588ec4c0cd21b8b168f17d.
Full policy log retained: 2791 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/policy.log; SHA256 70fd4de446f449888aa2154b1633b83e0ada7fa1157a3acd280377f4fa9338c1.
Full generated drift log retained: 146 lines; /var/tmp/gc-gate.ga-73epvgp5.5di89o_n/generated.log; SHA256 ec2297d635f4d2f5dd35cc49e564ac9b5d4d2a5c44953ed13466d2aadb10675a.
Evidence refs: refs/gate/ga-73epvgp5/{source,base,merge}; pack evidence commit: 0d2bfe07ae721bdd928b5b793c00dd5440b0217e
