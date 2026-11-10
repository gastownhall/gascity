# Contract-radar Go toolchain release gate

**Verdict:** **PASS**

Deploy: ga-kp3y040; build: ga-4ajtkml; review: ga-28tiqup. Evaluated 2026-11-01T01:59:19.246922+00:00.

- deploy_mode: remote; push_remote: fork; base_ref: origin/main
- reviewed_commit: 52fcc7de5c5b4805778191b66359568b4d5d569b
- gate_base_sha: 7472f2967c2f80481146693c2a69fc61451d52e6
- merge_head: b465d925c121b038e9a8fcff78a9751139e56ef1; merge_tree: 6321776022f661e6b60271b4c96eb033cb949b8c
- merge_state: clean (origin/main@7472f2967c2f80481146693c2a69fc61451d52e6)
- gate_base_kind: current-tip
- merge_validation: the recorded merge result was built, vetted and tested. MPR owns the merge decision and any later merge-time change.
- PR: https://github.com/gastownhall/gascity/pull/8647 (opened as a guarded draft for criterion 3c at the exact reviewed commit)

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-28tiqup records a fresh PASS for the resolved reviewed commit. No carryover or reviewer-added source commit. |
| 2 | Acceptance | PASS — the diff leaves ci.yml and its execution-hash pin unchanged, applies GOTOOLCHAIN=auto only to the bd-main build step in advisory-probes.yml, and adds its structural regression test. The PR's actual advisory run selected the radar and passed the bd build, PATH verification and both CLI contract steps. |
| 3 | Tests | PASS — all five fresh remote phases, all 16 native-floor steps, the clear-probe latency lane, the changed test by name, the changed workflow's real run, and all four CI dependency accounts passed. 136661 PASS / 0 FAIL / 472 SKIP top-level result lines. |
| 4 | Review findings | PASS — zero unresolved HIGH findings. The review's comment about extending the structural test to job/workflow-level env is nonblocking; this diff has only the step-level override. |
| 5 | Clean source | PASS — the deploy branch and merge scratch were clean at audit. This record is the sole deployer edit. |
| 6 | Merge state and build | PASS — clean merge against the frozen base; Bazel build including nogo/vet and CLI version/doctor-help smoke all exited 0. |
| 7 | Single feature | PASS — one toolchain-compatibility change: four workflow lines and one 30-line test. Both source commits cite ga-4ajtkml; the ancestry guard accepted only that build and this deploy bead. |

fast_lane: PASS 2026-11-01T01:16:43.282475+00:00, before the suite, on merge_head b465d925c121b038e9a8fcff78a9751139e56ef1.
policy_lane: make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; bazel test --config=remote-exec --noremote_local_fallback //:go_format_test — exit 0.
drift_lane: make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code — exit 0, no drift.
Each lane used fresh-tree-run.sh; policy used gate-base.sh run. Their FRESH_TREE_RECORD and GATE_BASE_RECORD lines are retained in /var/tmp/deploy-ga-kp3y040.81qo_r55/fast-run-2-result.log. The initial policy view lacked the RBE definitions and was stopped during compilation, before a test verdict; its logs remain in fast-run. The corrected fresh view provisioned the existing RBE definitions and completed. No test failure was retried into PASS.
criterion6_command: bazel build --config=remote-exec --noremote_local_fallback //...; bazel run with the same flags //cmd/gc -- version; bazel run with the same flags //cmd/gc -- doctor --help — exit 0. Evidence: /var/tmp/deploy-ga-kp3y040.81qo_r55/c6-run-result.log.

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.c9kjZ4 --base 7472f2967c2f80481146693c2a69fc61451d52e6 --bead-json /var/tmp/deploy-ga-kp3y040.81qo_r55/bead.json --owned /var/tmp/deploy-ga-kp3y040.81qo_r55/owned.tsv --run-dir /var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence, detached through gate-detached-run.sh and the admission/load/isolation wrappers — exit 0.
test_cmd_scope: full-suite
test_counts: PASS=136661 FAIL=0 SKIP=472 (top-level lines across 530 retained logs; repeated jobs count separately).
test_log_dir: /var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/logs
heavy_mode: none (heavy-composite-gate.sh classify exit 0)
waiver_ref: none
failure_attribution: none

| Farm phase | Fresh BEP results | Exit |
|---|---|---|
| unit | 389 | 0 |
| acceptance | 24 | 0 |
| integration-packages | 86 | 0 |
| integration-full | 14 | 0 |
| integration-latency | 1 | 0 |

All 514 results used remote strategy, attempt 1, no cached flags, a start inside the phase window and a terminal PASS. The independent audit rechecked every BEP and all 1,028 retained log/XML hashes. Actual commands and flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:

```text
diff_tests_executed:
  TestContractRadarBuildsBDWithItsOwnToolchain PASS (unit-b0c13e82fd961ff4f637.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name
```

skip_justification: existing optional/live fixtures, platform/privilege cases, unsupported provider capabilities and helper-only tests explain the inventoried skips. No scripts-package test skipped. The diff changes one test body and one workflow step, with no shared helper, production Go or skip condition changed. Every farm environment-absence skip was selected for the native floor by package and parent test; all native steps exited 0. Remaining native skips (including subtests) retain their printed reasons in the separate inventory and are not diff-owned.

ci_lane_run: draft-PR-triggered PASS — https://github.com/gastownhall/gascity/actions/runs/41262978856, workflow advisory-probes.yml, event pull_request, head 52fcc7de5c5b4805778191b66359568b4d5d569b. The radar job was selected and completed, rather than skipped.

- PASS — Build bd from beads main HEAD
- PASS — Verify bd on PATH
- PASS — bd CLI contract (main HEAD)
- PASS — bd CLI contract HOME isolation (main HEAD)

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.c9kjZ4 7472f2967c2f80481146693c2a69fc61451d52e6 52fcc7de5c5b4805778191b66359568b4d5d569b fork — exit 0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status.

| CI leaf | Status | Verified job |
|---|---|---|
| runner-policy | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/41262978836/job/124719848862 |
| changes | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/41262978836/job/124719900316 |
| credential-provider-windows | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/41262978836/job/124719944197 |
| pack-gate | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/41262978836/job/124719944127 |

ci_required_accounting: all four LEAF jobs above are completed/success at the exact reviewed head; check and ci-required are aggregators.
windows_reach: none (windows_reach.sh exit 0, stdout empty).
deferred_to_pr_ci: none
not_covered: none
host_gaps: none
The gate authorizes publication. CI / required must be green on the final PR head before merge.

The latency registry tests ran only in their own lane, with a clear farm probe, and both passed on attempt 1. The main integration leg recorded no verdict for them.

Native-floor load telemetry:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=420 wait_timed_out=0 run_start_load=14.32 run_max_load=19.45 run_mean_load=18.35 run_readings=7 wait_first_load=17.57 wait_max_load=17.87 wait_mean_load=16.48 wait_readings=15 read_errors=0
```

load_threshold: 15
load_waited_seconds: 420
load_wait_timed_out: 0
run_start_load: 14.32
run_max_load: 19.45
run_mean_load: 18.35
run_readings: 7

Host ICU prerequisite:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

Compact execution record (test_exec_mode, test_exec_bep, rbe_state, fallback_reason, admission, farm_test_seconds, skip_inventory, owned_topup, floor_topup, latency_leg):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":40.73893737792969,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":389,"remote_executed":389,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":9062.314,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":96.315,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=20.68 load5=15.98 load15=8.54 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 16.81 15.48 8.61 11/1002 52425"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30622}},"log_dir":"/var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":387,"environment-absence":71},"top_level_counts_by_reason_class":{"other":223,"environment-absence":42},"tests":{"elided_array":{"items":458,"bytes":163769}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":136661,"FAIL":0,"SKIP":472},"diff_tests_executed":"diff_tests_executed:\n  TestContractRadarBuildsBDWithItsOwnToolchain PASS (unit-b0c13e82fd961ff4f637.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

Full execution record retained: 4716 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/execution.json; SHA256 73c1745885d70e188634092072b37bb59321f127d9ffbc8608b3ef4c872a60f3.

Full farm skip inventory retained: 3666 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/skip-inventory.json; SHA256 2935a50776f018eed60acffda6ccccdaa1a1891a1a072c5aedd85e84ee681e63.

Full all skip reasons and native inventory retained: 3983 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/skip-explanation-inventory.json; SHA256 47a04c0033c4324ed508e784447f0a8129b15ab7b0d011d14fe1ac3ed0b08f9f.

Full native floor commands/results retained: 901 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/c3-run/evidence/remote/floor-topup.json; SHA256 5104e728f0272a912ae030c4f30bce1986cea124230f55e8232d106dcdac14a9.

Full independent BEP audit retained: 86 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/farm-independent-audit.json; SHA256 a34280e51b5886cb066606044a8cba84184d60ed855420074f7338e5f5b356eb.

Full retained artifact hashes retained: 5142 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/retained-log-sha256.json; SHA256 288bd05bb48e5e0652c4ff7b4bd309195980ceae505c1846d1a55ccd7c19105c.

Full CI closure retained: 15 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/ci-required/closure.log; SHA256 d85617a1a5952c4b5e9592e0c6ccea758aee2b25a42c08caf0c0d335077f0e9e.

Full CI accounting retained: 39 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/ci-required/accounting.json; SHA256 dbd52b47bacb7b89e04633d792613a0af397de3b54d287dee155e0ac1eb33242.

Full advisory run retained: 1 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/advisory-run.json; SHA256 d9d6ade4351368d59e3eb996e0e208a730acd875cdd3d19f2423db9be9037b79.

Full advisory job/step results retained: 1 lines; /var/tmp/deploy-ga-kp3y040.81qo_r55/advisory-jobs.json; SHA256 1c1a81dcef36248f2e04762d857ab53f2c623a2913b8885027cc21be62e3edb0.

