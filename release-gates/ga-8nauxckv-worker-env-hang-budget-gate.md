**Verdict:** **PASS**

# Worker-env test helper hang budget — release gate

Bead: `ga-8nauxckv`; review: `ga-aecp8090`; build: `ga-izzzy5t`; condition tracker: `ga-vkhfnj`.

Reviewed source: `377accf39e042a5e34e0a007282a8bdcaf013eae`. Tested merge: `6a2828bd82495b9c53821f4614ce2f551948401d`; tree: `a2cfa69a254a9e3fae94fbc38e222d61b593925f`.

gate_base: `11a3589ab9e5df40150028b9461a98c605ad3965`
merge_state: clean (`origin/main@11a3589ab9e5df40150028b9461a98c605ad3965`)
gate_base_kind: current-tip
merge_validation: exact merge result built and vetted here; the merge authority validates the eventual merge.
deploy_mode: remote; push_remote: fork; branch: `deploy/ga-8nauxckv-gate`.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Live closed review at the exact resolved source; no carryover or branch-tip substitution. |
| 2 | PASS | Three changed helper tests and all six existing worker-env grace scenarios PASS by name. All six scripts unit shards PASS (1189 top-level PASS, zero FAIL/SKIP). Grace assertions, retries and fixture isolation preserved. The stage-specific landing obligation remains with MPR as recorded below. |
| 3 | PASS | Canonical full suite complete and valid: 156666 PASS / 0 FAIL / 613 SKIP. Required-job accounting complete; skip evidence below. |
| 4 | PASS | No unresolved high findings. Minor evidence-comment and helper coverage follow-ups: ga-arr6cdob. |
| 5 | PASS | Tested tree clean; isolated publication carries reviewed source plus this gate record. Commit cleanliness checked before push. |
| 6 | PASS | `bazel build --config=remote-exec --keep_going --noremote_local_fallback //...`, then the same with `--output_groups=nogo_fix`, both exit 0 and BEP overallSuccess=true. |
| 7 | PASS | One scripts test-helper change: hang-budget sizing, named deadline errors, and regression coverage. Both commits cite the accepted build/condition lineage. |

## Acceptance and limits

AC1/AC2 characterization and sizing: retained raw log/XML hashes independently verified. Actual stalled testcase durations are 88.93s and 93.68s; the unrelated timing test itself is 0.460s. Its 105s wrapper includes t.Parallel waiting. The source comments retain that overestimated figure; the correction and re-derive-or-revert trigger are tracked in ga-arr6cdob. No root cause of the host stall has been proved.

The reviewed deadline is a hang detector under TESTING.md: 24 × ExecRaceTimeout = 240s, below the 300s scripts shard alarm. Every behavior assertion follows the child return. The reviewer disclosed treating creep evidence as inapplicable to this sizing; that judgment is preserved, with no waiver claimed. A stall longer than 240s can still exhaust it. This gate does not certify a cure for all host-load conditions.

AC3/AC4: grace publication/fetch behavior and all existing assertions/retries are preserved; BUILD sync, policy and normal checks pass. AC5: the reviewed fix SHA is pinned here; the verified PR will be recorded on this bead and build ga-izzzy5t. Main-landing evidence is pending MPR and must precede any resolved claim. The shared tracker ga-vkhfnj stays OPEN. AC6: ga-ik5xddw and ga-9uvbhln holds/edges remain untouched.

## Test evidence

test_cmd_scope: full-suite; test_exec_mode: remote-exec; heavy_mode: none; failure_attribution: none; waiver_ref: none.
test_log_dir: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/c3-run/evidence/remote/logs` (all per-job logs retained).

test_cmd: canonical gate-criterion3.sh at the exact merged tree. Five forced-uncached remote phases, then its computed native floor. The commands cover make -k check, acceptance and solo targets, integration_packages, full integration minus the registered latency names, and those latency names in their measured lane.

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
  TestRBEScriptHangBudgetClearsAHostStallButReportsBeforeTheShardAlarm PASS (unit-ee115c844954da53d34d.log)
  TestRunRBEScriptWithinKillsAWedgedScriptAndNamesTheBudget PASS (unit-271bc77ad6715453dfe1.log)
  TestRunRBEScriptWithinLetsASlowScriptFinish PASS (unit-db0feb4ad70da6248282.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name

All six TestWorkerEnvGrace* names independently PASS in the same full-scope output; complete by-name receipts retained. Native floor: 16 steps, all rc=0. ICU74 OK; rootless podman ping OK; pinned test bd v1.3.4. The wrapper reports pass-through for this rig; remote test actions are hermetic and native commands scrub live-store selectors, pin shell and Git config.

Compact execution record (large arrays retained by pointers):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":65.91707229614258,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15444.902,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":164.756,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=58.70 load5=56.15 load15=27.70 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=20s load1=48.11 load5=53.88 load15=27.56 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 57.42 55.29 30.21 46/2132 59189"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38616}},"log_dir":"/var/tmp/deploy-ga-8nauxckv.52srxh2t/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":509,"environment-absence":90},"top_level_counts_by_reason_class":{"other":251,"environment-absence":43},"tests":{"elided_array":{"items":599,"bytes":214880}}},"test_cmd":{"elided_array":{"items":5,"bytes":2059}},"test_counts":{"PASS":156666,"FAIL":0,"SKIP":613},"diff_tests_executed":"diff_tests_executed:\n  TestRBEScriptHangBudgetClearsAHostStallButReportsBeforeTheShardAlarm PASS (unit-ee115c844954da53d34d.log)\n  TestRunRBEScriptWithinKillsAWedgedScriptAndNamesTheBudget PASS (unit-271bc77ad6715453dfe1.log)\n  TestRunRBEScriptWithinLetsASlowScriptFinish PASS (unit-db0feb4ad70da6248282.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```

## Skip integrity

Top-level test counts exclude subtests; skip inventories include them. Remote inventory: 599 events, 316 unchanged sites. Native inventory: 72 events, 45 unchanged sites. Ownership is checked on the actual merged tree, whose owned-name set equals the reviewed source. The scripts shared-helper package has zero skips. Default/integration test-binary closure unions for other skipped packages exclude scripts. The only changed non-test file is its BUILD entry; no changed helper is imported into those binaries. OS, privilege, tool/opt-in and existing fixture skip predicates are unchanged.

One remote inventory reason omitted after === CONT is recovered verbatim from the adjacent original log and unchanged internal/dispatch/call_budget_test.go:537. The original inventory and logs are untouched; the audit records the recovery and raw-log hash. No test was rerun to account for skips.

## Fast lane and host load

policy_lane: fresh-tree-run.sh --from <tested merge> -- gate-base.sh run -- bash fast-policy.sh — shell guards plus 14 Bazel policy/format targets, rc=0.
drift_lane: separate fresh views for seven generated-artifact targets, OpenAPI compatibility, and make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec plus git diff --exit-code; every rc=0. Actual scripts and timestamps retained. All fast lanes completed before criterion-3 launch.
known_red_precheck: proceed (fix-carrying: build ga-izzzy5t stamped gc.fixes_tracker=ga-vkhfnj).

Native-floor load:
```text
load_threshold=15 load_waited_seconds=1801 load_wait_timed_out=1 run_start_load=70.47 run_max_load=69.43 run_mean_load=58.87 run_readings=7
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=70.47 run_max_load=69.43 run_mean_load=58.87 run_readings=7 wait_first_load=74.74 wait_max_load=85.47 wait_mean_load=61.75 wait_readings=61 read_errors=0
```

Own CI compile-job load:
```text
load_threshold=15 load_waited_seconds=90 load_wait_timed_out=0 run_start_load=14.27 run_max_load=15.55 run_mean_load=14.52 run_readings=3
LOAD_GATE_SUMMARY threshold=15 waited_seconds=90 wait_timed_out=0 run_start_load=14.27 run_max_load=15.55 run_mean_load=14.52 run_readings=3 wait_first_load=17.27 wait_max_load=17.27 wait_mean_load=15.84 wait_readings=4 read_errors=0
```

## Required-job accounting

ci_required_closure: python3 ci_required_triggers.py /var/tmp/gc-merge-ga-8nauxckv.ToYC5c 11a3589ab9e5df40150028b9461a98c605ad3965 377accf39e042a5e34e0a007282a8bdcaf013eae fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.

```text
diff 11a3589ab9e5df40150028b9461a98c605ad3965...377accf39e042a5e34e0a007282a8bdcaf013eae: 3 changed files
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
| runner-policy | LOCAL-PASS | {"command": "EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py", "log": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/runner-policy.log", "rc": 0} |
| changes | LOCAL-PASS | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/closure.txt"} |
| credential-provider-windows | NOT-TRIGGERED | {"evidence": "credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'"} |
| pack-gate | NOT-TRIGGERED | {"evidence": "pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'"} |
| windows-lane | LOCAL-PASS | {"command_file": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/windows-lane.sh", "rc": 0, "run_dir": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/windows-lane", "stderr": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/windows-lane-terminal.stderr", "stdout": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/windows-lane-terminal.stdout"} |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/windows-lane-test-reach.json", "reach": "none", "resolved_runs_on": "blacksmith-4vcpu-windows-2025"} |

windows_reach: windows-lane-test none, from its own GOOS=windows test-binary closure over the job allowlist; not the credential-only helper.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
ci_lane_run: n/a (no CI-config change in this diff).

Exact windows-lane commands (foreign test binaries compile without executing, as in that job):
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

This PASS authorizes PR opening. The exact-head CI / required and each deferred job must succeed before MPR merges.

## Retained evidence

Durable evidence: `/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj`; original run root: `/var/tmp/deploy-ga-8nauxckv.52srxh2t`. The exact merged tree is preserved in a verified Git bundle.

Full execution.json retained: 6000 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/c3-run/evidence/execution.json; SHA256 eff0a9e752cc062fb68a8c966d90849fdd243e0b494e62a803f967f97f36f151.

Full remote skip inventory retained: 4794 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/c3-run/evidence/remote/skip-inventory.json; SHA256 1fdb5f07215bd97faa75746c08eb8f4f0ffaec69833fd579a47f0ee77d548174.

Full native skip inventory retained: 578 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/native-skip-inventory.json; SHA256 a6678b7cf2d6a99dacf1b1b357784279052db20b7731ee49a05bc136da59772b.

Full remote skip-site proofs retained: 1898 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/remote-skip-site-results.json; SHA256 92b496bd6391a1d0b6e0542d96e5f8713aed4613830a2e4177d0e31977130076.

Full native skip-site proofs retained: 272 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/native-skip-site-results.json; SHA256 3cb44fb048a62597718ad397e3011157087efb4aaaafbf22db37136032c5c7a7.

Full floor steps retained: 1056 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/c3-run/evidence/remote/floor-topup.json; SHA256 8fbb18258b76879790ca14d5904e02260ff84e22a29b5fccf0db1e19ba52e30f.

Full CI accounting retained: 58 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/accounting.json; SHA256 abed83df94a703fd7fd33faf61f17e1b6b4633cd69c66afcb35b9f2ebbba5b6f.

Full Windows reach proof retained: 36 lines; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-8nauxckv/gate-20261118T171620Z-fr8pljsj/ci-required/windows-lane-test-reach.json; SHA256 88aa6a9fdee5ebb2e129542dc803deb6e20c8c78382383ce824975386504b99c.
