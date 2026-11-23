# Release gate: settled plain-shell agent-running fixture

**Verdict:** **PASS**

- Deploy bead: ga-6rth3lzq; build bead: ga-atixjw; review: ga-8ahs4by4 (closed PASS).
- Reviewed source: `c459ecc25033e5e216c8fac4a34bd33e3b2a847a` (resolved with git).
- Gate base: `b01560689f8ab55a7f1e6d1cd59ac9a13713d24e`.
- Tested merge commit: `b97c4d2a3901e54aeda3716df51afdd063be3415`.
- Gate time: 2026-11-08T15:26:58.631410+00:00.
- Deploy mode: remote; base ref: origin/main; push remote: fork.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review ga-8ahs4by4 names the resolved reviewed source; no carryover or reviewer-added-test substitution. |
| 2 | Acceptance criteria met | PASS | Only TestIsAgentRunning adopts the existing newPlainShellSession helper. No helper duplication, debounce change or WaitForShellReady deletion. Built from an ancestor of main without a stack. Independent empty-HOME run: 50 parent PASS, 350 subtest PASS, 0 FAIL/SKIP. Package coverage is included in the full integration-package phase. |
| 3 | Tests pass | PASS | Five forced remote phases plus all 16 native floor steps completed; 149215 PASS, 0 FAIL, 512 SKIP top-level result lines. The owned test and all seven subtests PASS by name. No waiver or failure attribution. CI accounting below is complete. |
| 4 | No open high-severity findings | PASS | Review style/security/spec findings contain no unresolved HIGH finding. |
| 5 | Clean branch | PASS | Reviewed/tested source trees are clean; the release record is the only deploy addition. Final clean status is checked again after its commit. |
| 6 | Merge state and build/vet | PASS | Clean merge onto the pinned main tip; both binaries built and full nogo/vet passed. Normal commit hooks ran. The tested tree was independently verified as exactly the base plus reviewed source. |
| 7 | Single feature theme | PASS | One function's fixture in internal/runtime/tmux/tmux_test.go; 5 additions, 12 deletions. No independent theme or internal instruction-file contamination. |

merge_state: clean (origin/main@b01560689f8a)
merge_validation: merge result built and vetted here; subsequent merge remains the merge authority's work
gate_base_kind: current-tip
gate_base: b01560689f8ab55a7f1e6d1cd59ac9a13713d24e

policy_lane: PASS — make lint plus check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows and check-residency-boundary; pinned by gate-base.sh in a fresh view.
format_lane: PASS — bazel test --config=remote-exec --noremote_local_fallback //:go_format_test in a fresh view.
drift_lane: PASS — make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback', then git diff --exit-code and empty git status --porcelain --untracked-files=all in a fresh view.
fast_lane: PASS before criterion 3 launch.
ci_lane_run: n/a (no CI-config change).

```text
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/merge head=b97c4d2a3901 tree=8eaee313a04c dir=/var/tmp/gc-fresh-tree.KHL26G modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-merge,bazel-out
GATE_BASE_RECORD base=b01560689f8ab55a7f1e6d1cd59ac9a13713d24e knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.KHL26G
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/merge head=b97c4d2a3901 tree=8eaee313a04c dir=/var/tmp/gc-fresh-tree.uaqXDj modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-merge,bazel-out
GATE_BASE_RECORD base=b01560689f8ab55a7f1e6d1cd59ac9a13713d24e knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.uaqXDj
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/merge head=b97c4d2a3901 tree=8eaee313a04c dir=/var/tmp/gc-fresh-tree.NeDFPf modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-merge,bazel-out
GATE_BASE_RECORD base=b01560689f8ab55a7f1e6d1cd59ac9a13713d24e knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.NeDFPf
FAST_LANE_PASS
```

## Test evidence

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/merge --base b01560689f8ab55a7f1e6d1cd59ac9a13713d24e --bead-json /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/bead.json --owned /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/owned.tsv --run-dir /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence; GC_GATE_REMOTE_ENABLED=1, GC_REMOTE_GATE_FLOOR_GIB=25, GC_REMOTE_GATE_CAP=2, ISOLATED_TEST_BEAD=ga-6rth3lzq, launched through gate-detached-run.sh.

The unit command was make -k check; acceptance includes acceptance_test and acceptance_solo_tests; integration-packages is //test:integration_packages. Full integration excludes only the registry's latency tests; the separate latency phase runs those tests under the clear-probe hold. Actual phase flags:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

```json
{"test_exec_mode":"remote-exec","test_cmd_scope":"full-suite","test_counts":{"PASS":149215,"FAIL":0,"SKIP":512},"fallback_reason":null,"farm_test_seconds":12259.089,"admission":{"class":"remote","floor_gib":25.0,"free_gib":73.62559127807617,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"diff_tests_executed":"TestIsAgentRunning PASS; all 7 subtests PASS","waiver_ref":"none","heavy_mode":"none","floor_topup":"complete: 16/16 commands exit 0","owned_topup":[],"latency_leg":"measured under clear probe; both budget tests PASS","skip_inventory":{"farm":"/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/skip-inventory.json","native":"/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/native-skip-inventory.json"},"test_log_dir":"/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/logs","load_scope":"local floor top-up","load_threshold":"15","load_waited_seconds":"1380","load_wait_timed_out":"0","run_start_load":"14.41","run_max_load":"16.53","run_mean_load":"15.71","run_readings":"7"}
```

test_counts count top-level result lines across retained jobs, not distinct test names. Subtests are verified separately for the owned test.

skip_justification: every skip is non-owned. The diff changes only TestIsAgentRunning's body, with no SHARED-HUNKS or HELPER-HUNKS. All six skip sites in the touched file are UNCHANGED with line_in_hunk=no and func_touched=no; no base skip comparison is required by the unchanged-site rule. Other skips occur in untouched tests/files and cannot execute this test's fixture body. The inventories retain each test's printed reason. All environment-absence rows received native floor runs; remaining non-owned native SKIPs are inventoried. No owned test or subtest skipped or failed.

LOAD_GATE_SUMMARY threshold=15 waited_seconds=1380 wait_timed_out=0 run_start_load=14.41 run_max_load=16.53 run_mean_load=15.71 run_readings=7 wait_first_load=62.37 wait_max_load=64.01 wait_mean_load=37.18 wait_readings=47 read_errors=0

Independent C2 check: isolated-test-run.sh --rbe -- bazel test --config=remote-exec --noremote_local_fallback --nocache_test_results --config=integration //internal/runtime/tmux:tmux_test --test_sharding_strategy=disabled --test_filter=^TestIsAgentRunning$ --test_arg=-test.count=50 --test_env=HOME=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/empty-home --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all --experimental_ui_max_stdouterr_bytes=-1; exit 0, 50 parent PASS and 350 subtest PASS, no FAIL/SKIP.

test_bd: GASCITY_TEST_BD v1.3.4, reports bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c), pin ref_check=match:7ef2fd05e; wrapper-prepared test bd, fleet PATH bd unchanged.
Test wrapper mode: PASS-THROUGH (not an isolated clone); the gate ran in its exact temporary merge worktree. Native floor scrubs store selectors. This is not a clean-environment claim.

## Required CI job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/merge b01560689f8ab55a7f1e6d1cd59ac9a13713d24e c459ecc25033e5e216c8fac4a34bd33e3b2a847a fork; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status.
ci_required_accounting:
- changes | LOCAL-PASS | closure tool exit 0; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/ci-required/closure.txt.
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/ci-required/runner-policy.log.
- credential-provider-windows | NOT-TRIGGERED | tool row: needs.changes.outputs.credential_provider == 'true'; no match.
- pack-gate | NOT-TRIGGERED | tool row: needs.changes.outputs.packs == 'true'; no match.
windows_reach: none (windows_reach.sh exit 0 with empty output).
deferred_to_pr_ci: none.
not_covered: none.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":73.62559127807617,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":407,"remote_executed":407,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12259.089,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":85.344,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=69.76 load5=80.78 load15=41.58 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=46.17 load5=73.88 load15=40.52 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.82 67.46 39.60 6/1013 55470"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30988}},"log_dir":"/var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":426,"environment-absence":72},"top_level_counts_by_reason_class":{"other":233,"environment-absence":43},"tests":{"elided_array":{"items":498,"bytes":177493}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":149215,"FAIL":0,"SKIP":512},"diff_tests_executed":"diff_tests_executed:\n  TestIsAgentRunning PASS (integration-packages-5a078b24388ef3c79748.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

## Retained evidence

Full full execution.json retained: 5045 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/execution.json; SHA256 bc2636f9008afb350536a9ed5e5ed256e647cb087bb7b764bc5b3971a1de3dff.

Full farm skip inventory retained: 3986 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/skip-inventory.json; SHA256 e05698fa5ceec4ce908dae974a7e7f656de45408da9f254dc247c254dfee4c44.

Full native skip inventory retained: 314 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/native-skip-inventory.json; SHA256 ee40e1eb8762da08c33c943e12aa5adf3c72bf7911563e63c664927bfbb8732f.

Full skip-site proofs retained: 32 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/tmux-skip-site-proofs.json; SHA256 486309814a5030080d30af3d6946b285419769dc52447d61e3b70dc3d969d171.

Full diff ownership proof retained: 120 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/diff-owned-explain.txt; SHA256 e2fd6fc42312a605b652a72cdf3bd74e2bafa9778ccfb27353d9c3cb48661bd8.

Full floor steps retained: 909 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/floor-topup.json; SHA256 dfe2398038aa065d76183b5ad1c03c88356a147d9a8ffc84f4358cb3281bcf82.

Full original BEP command lines retained: 5162 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/bep-command-lines.json; SHA256 521899a0c35c3bab68b63ff987d9c4e6eb7d6efabc38042f42a7b5427255833e.

Full criterion 6 log retained: 179 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c6-evidence.log; SHA256 3436efb10f3ed79482de4001abad39da1131b002730892ea493b68dd17dc9701.

Full fast lane log retained: 220 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/fast-evidence.log; SHA256 9f03d8290f43eac9ec233c7e497d0fc97ee10b4e58236bdb6131cde1d8016ec1.

Full stability log retained: 1238 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/stability-evidence.log; SHA256 037ee2d3601fc7b3ba010d7bd2990653d58530d3da09c90023f97e5988e5d0e0.

Full unit artifact SHA256 manifest retained: 11398 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/unit-artifacts.json; SHA256 45fad62b3a9ebb4aeae7175194cb668996ea9778f8699ada93a365dbf5822e88.

Full acceptance artifact SHA256 manifest retained: 702 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/acceptance-artifacts.json; SHA256 afe23d28c7a3b0e1edd074f184c9c5a6a2bb48b9182873795afaa98566a4ef5f.

Full integration-packages artifact SHA256 manifest retained: 2410 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-packages-artifacts.json; SHA256 188840d2797e89dfb8d6101f88339d6226e64db9cb2580f00ac1c872eb8a673d.

Full integration-full artifact SHA256 manifest retained: 394 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-full-artifacts.json; SHA256 0edb61d4191afd59addc678e30e8014e7b2390b63160cfda9e3ddd9a8df38a98.

Full integration-latency artifact SHA256 manifest retained: 30 lines; /var/tmp/deploy-ga-6rth3lzq.nl4fwjer/c3-run/evidence/remote/integration-latency-artifacts.json; SHA256 044599c082cf7f2926989d4ae2f7e189b6c97b8c85ae290f1f780ef2244f38ad.

## Local runtime check

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```
