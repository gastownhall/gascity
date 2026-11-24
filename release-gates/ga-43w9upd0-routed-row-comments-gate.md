**Verdict:** **PASS**

# Routed-row comment classification — release gate

Bead: `ga-43w9upd0`; review: `ga-baer3dx1`; build: `ga-miihvqf`. Non-blocking follow-up: `ga-7k26cyhl`.

Reviewed source: `db2f06952f0d6517ec39f22b5d6004aea6c90b90`. Tested merge: `08cbabf234f3f273ac75295a208e6cde20bdbabd`; tree: `318e04e77f6a21c572ac4833943d43a66eabf482`.
gate_base: `3764de45ea174753b4a401d98f6ab86740a151f7`
merge_state: clean (`origin/main@3764de45ea174753b4a401d98f6ab86740a151f7`)
gate_base_kind: current-tip
merge_validation: the exact merge result built and vetted; the merge authority validates the eventual merge.
deploy_mode: remote; push_remote: fork; isolated branch: `deploy/ga-43w9upd0-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Closed reviewer PASS at the exact resolved source; no carryover or branch-tip substitution. |
| 2 | PASS | All 56 routed-row names PASS: 8 test functions, 20 new subtests, and 28 original negative controls. Validator tests preserved; real-corpus guard exits 0. |
| 3 | PASS | Canonical full-suite evidence: 156884 PASS / 0 FAIL / 613 SKIP. Required-job accounting and unchanged-skip proofs complete. |
| 4 | PASS | No unresolved HIGH review findings. Minor follow-ups remain open in ga-7k26cyhl; no waiver claimed. |
| 5 | PASS | Tested tree and source checkout clean. Publication adds only this gate record; post-commit cleanliness is checked before push. |
| 6 | PASS | Whole-tree Bazel build and nogo output-group build both exit 0, BEP overallSuccess=true. |
| 7 | PASS | One theme: distinguish real routed-row case names from comment text. Two files, +135/-5; no manifest, census, BUILD or dependency changes. |

## Acceptance evidence

The checker strips Go line and block comments before matching case-name fields, preserving string, raw-string and rune literals. The production-script change is required by the accepted contract despite the older test-only wording.

- AC1: `TestCheckRoutedTestRowsIgnoresUnrelatedMarkerText` PASS with all 10 value/argument/comment subtests.
- AC2: `TestCheckRoutedTestRowsRecognizesCaseNameSpacing` PASS with all 8 listed/unlisted spacing cases.
- AC3: `TestCheckRoutedTestRowsRejectsCommentOnlyListedMatrix` PASS with both comment-only denominator cases, asserting UNDER-COVERED 0/6.
- AC4: `TestCheckRoutedTestRowsPassesOnThisRepo`, the listed-matrix control, and all 28 original negative subtests PASS. The census reads the full-suite logs; no narrower test run supplies this evidence.

Test file: 98 added lines, 0 removed. Blob 3565e78897b8690c5c0911ce1e8ec3d66179c7b9 equals validator db97464c35a26006c40c0fa1cfaef01a76e2841c. RED 0c8d2a693ad16b0c6ac25f1bab82583ffdab8fe2 patch-id d205afc00665264f8b2d2b38408bb8fb01854f89 equals validator artifact a5eb27934b027b75018d5ee15a2fb0d861e51fc6. GREEN changes only scripts/check-routed-test-rows.sh. No acceptance assertion was weakened.

Limits: this is a lexical POSIX-awk pass. The reviewer identified six surviving lexer-internal mutations and a here-string write-failure case that can fail open on a large unlisted fixture, while the measured real corpus fails closed. These are minor/non-blocking follow-ups in ga-7k26cyhl. Review measured guard cost near 2.9 s versus 1.1 s before. BSD awk and mawk were not separately verified. This gate does not claim those follow-ups are fixed.

## Full-suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
heavy_mode: none
failure_attribution: none
waiver_ref: none
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/c3-run/evidence/remote/logs` (all per-job logs retained).

Actual invocation:
```text
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-43w9upd0 DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-43w9upd0.9SG731 --base 3764de45ea174753b4a401d98f6ab86740a151f7 --bead-json /var/tmp/deploy-ga-43w9upd0.ovdvlmxf/bead-c3.json --owned /var/tmp/deploy-ga-43w9upd0.ovdvlmxf/owned.tsv --run-dir /var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence
```

The canonical chain runs five forced-uncached remote phases and its computed native floor. Exact phase flags:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
  TestCheckRoutedTestRowsIgnoresUnrelatedMarkerText PASS (unit-81d8a9af827e251f6b12.log)
  TestCheckRoutedTestRowsRecognizesCaseNameSpacing PASS (unit-9f907e0fea7ca680f21c.log)
  TestCheckRoutedTestRowsRejectsCommentOnlyListedMatrix PASS (unit-b0c13e82fd961ff4f637.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name

Native floor: 16 steps, all exit 0. ICU 74 check OK with recorded provenance; rootless podman ping OK. The changed fixture tests invoke the real Bash checker and need no container runtime. Pinned test bd and wrapper pass-through details are retained in the raw logs.

Compact execution record; full large arrays retained by pointer:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":67.14171600341797,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":19180.91,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":125.0,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=16.77 load5=25.77 load15=31.63 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.17 28.64 32.14 23/1479 56928"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-43w9upd0.ovdvlmxf/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214878}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":156884,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestCheckRoutedTestRowsIgnoresUnrelatedMarkerText PASS (unit-81d8a9af827e251f6b12.log)\n  TestCheckRoutedTestRowsRecognizesCaseNameSpacing PASS (unit-9f907e0fea7ca680f21c.log)\n  TestCheckRoutedTestRowsRejectsCommentOnlyListedMatrix PASS (unit-b0c13e82fd961ff4f637.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```

## Skip integrity

skip_justification: no diff-owned SKIP; every remote/native skip maps an unchanged site and recorded reason.

Remote inventory: 599 events at 316 unchanged source sites. Native: 72 events at 45 unchanged sites. Top-level counts exclude subtests. Ownership is recomputed at the actual merged head and matches the reviewed source.

The fmt import is a SHARED-HUNKS entry in `scripts/check_routed_test_rows_test.go`. There are no skipped tests in scripts. Default/integration test-binary closure unions for other skipped packages exclude scripts; the import and changed policy script are outside those package-input closures. Existing OS, privilege, opt-in and fixture predicates are unchanged. Any reason recovery is recorded with its raw-log hash and source site; original inventories/logs are untouched. No test was repeated to account for skips.

## Fast lane and host load

policy_lane: Bash syntax and ShellCheck for the changed guard; shell guards and 14 Bazel policy/format targets, all exit 0, through gate-base.sh run in a fresh view.
drift_lane: seven generated-artifact targets, OpenAPI compatibility, and make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec plus git diff --exit-code; independent fresh views, all exit 0. Fast lane completed before suite setup/launch.

known_red_precheck:
```text
known-red-precheck: target=check-routed-test-rows verdict=green merge=0 base=- (it passes on the merge tree)
known-red-precheck: stale: tracker=ga-1ptv4jey `make check-routed-test-rows` passes on this merge tree; the tracker may be ready to close
known-red-precheck: proceed reason=nothing-known-red (check-routed-test-rows=green)
```

Native-floor load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=11.47 run_max_load=11.92 run_mean_load=11.51 run_readings=6
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.47 run_max_load=11.92 run_mean_load=11.51 run_readings=6 wait_first_load=11.47 wait_max_load=11.47 wait_mean_load=11.47 wait_readings=1 read_errors=0
```

Own CI compile-job load:
```text
load_threshold=15 load_waited_seconds=0 load_wait_timed_out=0 run_start_load=11.03 run_max_load=12.40 run_mean_load=11.88 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.03 run_max_load=12.40 run_mean_load=11.88 run_readings=3 wait_first_load=11.03 wait_max_load=11.03 wait_mean_load=11.03 wait_readings=1 read_errors=0
```

## Required-job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-ga-43w9upd0.9SG731", "3764de45ea174753b4a401d98f6ab86740a151f7", "db2f06952f0d6517ec39f22b5d6004aea6c90b90", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff 3764de45ea174753b4a401d98f6ab86740a151f7...db2f06952f0d6517ec39f22b5d6004aea6c90b90: 2 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:

| Job | Status | Command / evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/closure.txt"} |
| credential-provider-windows | NOT-TRIGGERED | {"evidence": "credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"} |
| pack-gate | NOT-TRIGGERED | {"evidence": "pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'"} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/windows-lane.sh", "rc": 0, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/windows-lane-terminal.stdout"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/windows-lane-test-reach.json", "reach": "none", "resolved_runs_on": "blacksmith-4vcpu-windows-2025"} |

windows_reach: windows-lane-test none, computed from its own GOOS=windows test-binary closure over the job allowlist.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI job, matrix, timeout or required-check-list change).

Exact CI compile-job commands:
```bash
#!/usr/bin/env bash
set -euo pipefail
# Build the Windows allowlist
GOOS=windows go build $(cat .github/windows-lane/build.txt)
# Compile the Windows allowlist's tests
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
# Build the nogo analyzers and their tests for Windows
(
cd tools/nogo
GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...
)
# Build every package and its tests for macOS
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

This PASS authorizes PR publication. Exact-head CI / required and each deferred job must succeed before MPR merges.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6`; original run root: `/var/tmp/deploy-ga-43w9upd0.ovdvlmxf`. The exact merged tree is retained in a verified Git bundle.

Full execution.json retained: 5999 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/c3-run/evidence/execution.json; SHA256 a2648443ba70c6c7ee76753e8f89f397af308f814079b1e23c65a47c9303a564.

Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/c3-run/evidence/remote/skip-inventory.json; SHA256 7869aaafbae071715eb410db32e3bd178e8793915ffc40ecf6757e97ecd8a764.

Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/native-skip-inventory.json; SHA256 e5e742f0afe34fb2f73994d6c4a87bd23024f2ea007f07098c969832910425a8.

Full remote skip-site proofs retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/remote-skip-site-results.json; SHA256 6d674c7454e3d451effde0318bc5013164631e84f66d6eb561728cff1425794e.

Full native skip-site proofs retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.

Full floor steps retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/c3-run/evidence/remote/floor-topup.json; SHA256 cdf78122becd311c9ddfba186a2317cedce30b4a0195bd5748571eda719dc9e5.

Full CI accounting retained: 58 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/accounting.json; SHA256 132492cc8a704064d0eaf764e95e53d463c58d15edbc6b25615362be33b26507.

Full Windows reach retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/ci-required/windows-lane-test-reach.json; SHA256 1750a300b43fc9f0043ebdacd7782c88a1cca894de0a306f82e48bcb090e0b3e.

Full validator preservation proof retained: 24 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/acceptance-static.json; SHA256 4af0bf8b7e7b2c82b9dc903533a5ff6a60c42a91def7b69513448949d6d982c7.

Full 56-name acceptance census retained: 348 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/acceptance-results.json; SHA256 04421c95ab916e362c6a6fd8a12e1956baa8ea722bb8f0cf85ab4263a35dd846.

Full skip import closure proof retained: 573 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-43w9upd0/gate-20261118T193519Z-4e02d3a6/skip-import-closures.json; SHA256 f67986f32ad04b0165e171ecb054a2020e151f1ee63c2ff3d95c821c85cddfb3.
