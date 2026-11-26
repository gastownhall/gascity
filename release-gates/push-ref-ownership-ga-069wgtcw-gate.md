# Release gate: pre-push ownership follows pushed branches

**Verdict:** **PASS**

Evaluated: 2026-11-24T01:28:51.702802+00:00
Deploy bead: ga-069wgtcw; build: ga-d11es38m; original contract: ga-bugldjz0; review: ga-b2du98oe.
Reviewed commit: f53ed685493374a5fd39e57f2aec41a66bf3b7f4
Deploy mode: remote; push remote: fork; base ref: origin/main.
Gate rules: supplied release-gate-criteria protocol and current TESTING.md/CI; docs/PROJECT_MANIFEST.md is absent.

gate_base: 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c
gate_head: f53ed685493374a5fd39e57f2aec41a66bf3b7f4
gate_base_kind: current-tip
merge_state: clean (origin/main@9285cfb3ed95)
merge_validation: raw merge result built and vetted here; MPR validates the current merge result before landing.
Merge scratch: /var/tmp/gc-merge-validate.SBXXyg (temporary; evidence retained separately).
Evidence root: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review ga-b2du98oe is closed PASS. Reviewer-added test integrity independently verified below. |
| 2 | Acceptance criteria | PASS | Six added shell regressions PASS by name; reviewer-added hook test and all three subtests PASS. Covers each pushed branch, stale/unreadable bead rejection, and checkout fallback only when stdin names no branch. |
| 3 | Tests and required lanes | PASS | Full uncached remote suite, 16 native floor jobs, policy/format/drift checks, and six-leaf CI accounting below. Native Windows tests deferred to PR CI. |
| 4 | No unresolved HIGH findings | PASS | Review records no blocker or major. Minor HEAD-spelling coverage limitation is tracked separately as ga-8m6w6c0i. |
| 5 | Final branch clean | PASS | Feature content is exactly the reviewed commit; the only deploy addition is this gate record, committed before push. Final working-tree check accompanies the push. |
| 6 | Merge state and build/vet | PASS | Clean raw merge onto the pinned base. bazel build --config=remote-exec --noremote_local_fallback //cmd/gc //cmd/gc-osc7501 and make lint (nogo/vet) both exit 0. Log: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c6.result.txt. |
| 7 | Single feature theme | PASS | Four implementation/test files address one pre-push ownership-resolution behavior. No independent feature or internal configuration changes. |

reviewer_tests_verified: 4b1fd336536a818d217e7c6e1e703932a9293483 -> f53ed685493374a5fd39e57f2aec41a66bf3b7f4 files=scripts/githooks_pre_push_stdin_test.go
fast_lane: PASS before criterion 3 launch.
policy_lane: fresh-tree-run.sh -> gate-base.sh run -> isolated-test-run.sh --rbe -> make lint NOGO_BUILD_FLAGS='--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback'; exit 0.
format_lane: fresh-tree-run.sh -> isolated-test-run.sh --rbe -> bazel test --config=remote-exec --noremote_local_fallback //:go_format_test; exit 0.
drift_lane: own fresh view; make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback', then git diff --exit-code; exit 0, no drift.
Remote definitions were provisioned with rbe-local-rc.sh in each fresh view. Full lane logs: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/fast.result.txt.
known_red_precheck: proceed reason=nothing-known-red; check-routed-test-rows green (tracker ga-1ptv4jey stale).

```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.SBXXyg head=66369f3fbdd5 tree=26d8ed6b5bcf dir=/var/tmp/gc-fresh-tree.C7xLm5 modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.SBXXyg,bazel-out
GATE_BASE_RECORD base=9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.C7xLm5
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.SBXXyg head=66369f3fbdd5 tree=26d8ed6b5bcf dir=/var/tmp/gc-fresh-tree.EJCpT0 modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.SBXXyg,bazel-out
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.SBXXyg head=66369f3fbdd5 tree=26d8ed6b5bcf dir=/var/tmp/gc-fresh-tree.Tx9LHi modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.SBXXyg,bazel-out
```

## Test evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
tests_green: true
criterion3_valid: true
test_counts: PASS=159051 FAIL=0 SKIP=618 (top-level result lines summed across jobs, not distinct test functions).
test_log_dir: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/logs
heavy_mode: none
failure_attribution: none
waiver_ref: none
ci_lane_run: n/a (no CI-config change).
fallback_reason: none
farm_test_seconds: 14436.488
owned_topup: none needed; owned Go regression PASS in the full unit phase.
floor_topup: complete, 16/16 commands exit 0; environment-absence selections rerun locally, plus beadstest, Darwin compile, push-lock and concurrency self-tests.
skip_inventory: {"counts_by_reason_class": {"environment-absence": 90, "other": 514}, "top_level_counts_by_reason_class": {"environment-absence": 43, "other": 256}}; complete named reasons retained at the pointer below.
skip_justification: existing fast-unit, platform/provider, manual and version-gated cases; every environment-absence selection reran in the native floor. Residual non-owned skips are inventoried. No scripts-package skip and no diff-owned skipped test. No file outside the hook and scripts test subsystem changed.

The detached gate-criterion3.sh command ran all five phases with the following recorded flags (each phase exit 0); unit used make -k check, including all shell guards. Build/nogo may use cache; test verdicts below did not.

```text
bazel test --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/unit.bep.json --keep_going //...
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

| Phase | Target results | Remote shard results | Cached | Retries | Window valid |
|---|---|---|---|---|---|
| unit | 309/309 PASS | 426/426 | 0 | 0 | yes |
| acceptance | 8/8 PASS | 25/25 | 0 | 0 | yes |
| integration-packages | 38/38 PASS | 87/87 | 0 | 0 | yes |
| integration-full | 1/1 (15 shards) PASS | 15/15 | 0 | 0 | yes |
| integration-latency | 1/1 PASS | 1/1 | 0 | 0 | yes |

diff_tests_executed:

- TestPrePushGuardFallsBackToCheckedOutBranchOnlyWhenStdinNamesNoBranch PASS, including HEAD spelling, tag push, and named-branch override subtests.
- TestPushOwnershipGuard PASS in the full unit run; direct shell run additionally exposes every added shell test name:

```text
ok   resolve/pushed-ref-wins-over-head-branch (rc=0, asked about ga-tioxl6 only)
ok   resolve/pushed-ref-stale-bead-blocks-despite-live-head-bead (rc!=0, names ga-stale02)
ok   resolve/pushed-ref-unreadable-bead-blocks (rc!=0, names ga-tioxl6)
ok   resolve/pushed-refs-each-checked-any-stale-blocks (stale bead blocks in either position)
ok   hook/allows-push-of-live-branch-from-checkout-on-stale-branch
ok   hook/blocks-push-of-stale-branch-from-checkout-on-live-branch (rejected, remote untouched)
pass=86 fail=0
```

Supplemental shell command: own fresh view -> load-gate-run.sh -> isolated-test-run.sh -> hermetic env -i -> bash scripts/test-push-ownership-guard.sh; exit 0; /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/shell.result.txt.
latency_leg: complete, measured, first attempt, probe clear; both tests from gate-latency-tests.txt PASS; no verdict for them in the full-integration main leg.

## CI-required accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-validate.SBXXyg 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c f53ed685493374a5fd39e57f2aec41a66bf3b7f4 fork; exit 0.
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612
6 LEAF jobs need a status; the rest are rolled up through their needs.

| LEAF job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Closure tool found no packs, credential-provider, or shared-path match; /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/ci-accounting/closure.txt. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained file> python3 .github/workflows/scripts/runner_policy.py; exit 0; runner-policy.out/log. Linux runner expressions resolve to Blacksmith Ubuntu. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: needs.changes.outputs.credential_provider == 'true'; no match. |
| pack-gate | NOT-TRIGGERED | Tool row: needs.changes.outputs.packs == 'true'; no match. |
| windows-lane | LOCAL-PASS | Exact four CI steps: Windows allowlist build/test compile; tools/nogo Windows build/test compile; all packages Darwin build/test compile; exit 0. Commands: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/windows-lane.sh; log: /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/windows.result.txt. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native job runs on Blacksmith Windows 2025. GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps over .github/windows-lane/test.txt: no changed path in its main-module closure; query exit 0. Closure and reach retained under ci-accounting/. |

windows_reach: none (native windows-lane-test closure); none (credential-provider closure, job not triggered).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

The PASS authorizes PR-open. Merge requires CI / required green on the current PR head and windows-lane-test success there; MPR owns that check and the merge.

## Load and retained evidence

```text
c3.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=18.34 run_max_load=22.54 run_mean_load=10.33 run_readings=39 wait_first_load=18.34 wait_max_load=18.34 wait_mean_load=18.34 wait_readings=1 read_errors=0
shell.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.03 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=8.03 wait_max_load=8.03 wait_mean_load=8.03 wait_readings=1 read_errors=0
windows.result.txt: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.53 run_max_load=11.07 run_mean_load=10.11 run_readings=3 wait_first_load=7.53 wait_max_load=7.53 wait_mean_load=7.53 wait_readings=1 read_errors=0
```

load_threshold: 15
load_waited_seconds/load_wait_timed_out/run_start_load/run_max_load/run_mean_load/run_readings: recorded per command in the LOAD_GATE_SUMMARY lines above; NA means the command completed before a periodic sample, not a zero load. Remote admission does not wait for host load; the native floor and supplemental jobs do.

Full criterion-3 execution retained: 6058 lines; /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/execution.json; SHA256 d102bbc67b4fd3b70d1683148e0d0450596c3bc861495545b08f2fb4ffd796ba.
Full skip inventory retained: 4834 lines; /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/skip-inventory.json; SHA256 1bbbbd7ced0b2b67e03be3c26d45344eb2378b72c5523c0a0b61b23e04701992.
Full native floor steps retained: 1056 lines; /var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/floor-topup.json; SHA256 40ad28a48641dc2ac12334ff26b90bca1a1e4321ffb7ac6414161c1a8d1cdaf8.


Compact criterion-3 execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote","gate_base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","gate_head":"f53ed685493374a5fd39e57f2aec41a66bf3b7f4","admission":{"class":"remote","floor_gib":25.0,"free_gib":61.242042541503906,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","head":"f53ed685493374a5fd39e57f2aec41a66bf3b7f4","merge":"66369f3fbdd5a8fb8878d8cdf54bf2fa3f93b09f","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"aabc51e136e1d7f439832701876aa3c4b6c52af2ccd15ba063b1e004c868cb0e"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14436.488,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":88.086,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=20.62 load5=14.49 load15=12.15 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 22.81 15.72 12.66 20/2011 58094"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38298}},"log_dir":"/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":214898}}},"test_cmd":["test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec","test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-deploy-ga-069wgtcw.3uJTxK/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec"],"test_counts":{"PASS":159051,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestPrePushGuardFallsBackToCheckedOutBranchOnlyWhenStdinNamesNoBranch PASS (unit-db0feb4ad70da6248282.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```
