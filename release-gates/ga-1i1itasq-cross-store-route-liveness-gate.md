# Cross-store route-name liveness release gate

**Verdict:** **PASS**

Deploy bead: ga-1i1itasq; build bead: ga-jaj5wi; review: ga-dbgk3wvq.
Reviewed commit: b60b584100f81e102e49bdf4bd7f16d54829e412
Gate merge commit: 684a2d941bfc798ecf549304cdbde1c396b1340b; tree: 03dbacbc6ccd068a93f7a5f8c596319acee7c571
gate_base: 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c
merge_state: clean (origin/main@9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c)
gate_base_kind: current-tip
merge_validation: merge result built and vetted here
deploy_mode: remote; push_remote: fork; base_branch: main
deploy_ref_interlock: exit 0 (upstream lineage)

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — ga-dbgk3wvq records a reviewer PASS on the exact resolved commit. No carryover or reviewer-added commit. |
| 2 | Acceptance criteria met | PASS — four cases independently inspected and executed: city route protects rig work; rig route protects its own store; another rig is isolated; empty store reference refuses legacy matching. Each checks status and assignee. One new test function; existing regression owners remain unchanged. |
| 3 | Tests pass | PASS — full-suite, 159057 PASS / 0 FAIL / 618 SKIP (top-level result lines across lanes, not unique tests); 554 fresh remote shard results; all 16 native floor steps exit 0; owned parent and four subtests PASS. CI closure accounted below. |
| 4 | No open HIGH findings | PASS — reviewer style/security/spec findings contain no unresolved HIGH finding. |
| 5 | Final branch clean | PASS — verified before this record was written; only this record is added by deploy. Commit hook and final post-commit status are checked before push. |
| 6 | Merge state recorded; build/vet pass | PASS — clean merge at pinned base; bazel build --config=remote-exec --noremote_local_fallback --repo_env=DO_NOT_TRACK=1 //... exits 0, including nogo. |
| 7 | Single feature theme | PASS — reviewed range contains one commit and only cmd/gc/pool_session_name_test.go (+110). No production or CI-config change. |

## Suite evidence

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_counts: PASS=159057 FAIL=0 SKIP=618
waiver_ref: none
failure_attribution: none (no failures)
ci_lane_run: n/a (no CI-config change)
heavy_mode: none (classify exit 0)
owned_topup: none required
floor_topup: complete, 16/16 steps exit 0
latency_leg: measured, complete=true, both budget tests PASS under clear probe on first attempt
farm_test_seconds: 12876.531
test_log_dir: /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/logs
test_cmd: gate-detached-run.sh -> gate-criterion3.sh --repo /var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn --base 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c --bead-json /var/tmp/gc-gate.ga-1i1itasq.963yMj/bead.json --owned /var/tmp/gc-gate.ga-1i1itasq.963yMj/owned.tsv --run-dir /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence (GC_GATE_REMOTE_ENABLED=1; GC_GATE_DELTA=shadow; no narrowing applied)

Phase recipes: unit = make -k check (bazel test //... plus shell guards); acceptance = bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests; integration-packages = bazel test --config=integration //test:integration_packages; integration-full = bazel test --config=integration --test_arg=-test.v --test_arg=-test.skip=<the shipped latency regex> //test/integration:integration_test; integration-latency = that target with the shipped latency filter, sharding disabled, and the farm probe. The split is computed from gate-latency-tests.txt; no tests are omitted from both legs. Full flags actually used:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
  TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability PASS (integration-packages-e4245ddf6a37030111bc.log, unit-f93a0ca96ccc61d0cae9.log)
GATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name

```text
--- PASS: TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability/city_scoped_route_reaches_rig_work (0.00s)
--- PASS: TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability/rig_scoped_route_reaches_work_in_its_rig (0.00s)
--- PASS: TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability/rig_scoped_route_does_not_reach_another_rig (0.00s)
--- PASS: TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability/store_aware_empty_ref_does_not_use_legacy_matching (0.00s)
--- PASS: TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability (0.00s)
```

skip_justification: all skipped tests are outside the added function. Remote inventory has 604 events (514 other, 90 environment-absence); the native floor inventory has 72 events (34 other, 38 environment-absence). Every source location was checked: 322 remote and 45 native checks say UNCHANGED; no shared/helper hunks exist. Environment-absence parents were independently exercised by the required native floor; remaining skips are existing platform/fixture/provider-capability/legacy-binary/process-lane gates. Parallel log attribution for TestCrashInjectionCallBudgetMolecule/fail was checked manually at call_budget_test.go:537; its existing skip names ga-vnycm2.27. No changed test skipped. Detailed reasons and source-site proofs are retained by pointer below.

## Fast lane and isolation

policy_lane: make lint NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback --repo_env=DO_NOT_TRACK=1" -> PASS (exit 0), through gate-base.sh run. Nogo supplies lint/vet; no standalone golangci-lint binary is used.
format_lane: bazel test --config=remote-exec --noremote_local_fallback --repo_env=DO_NOT_TRACK=1 //:go_format_test -> PASS (exit 0).
drift_lane: BAZEL_SYNC_FLAGS="--config=remote-exec --repo_env=DO_NOT_TRACK=1" make bazel-sync; git diff --exit-code -> PASS (exit 0).
fast_lane: PASS 2026-11-24T05:16:02Z, before any suite launch.
known_red_precheck: proceed, check-routed-test-rows green; script reports tracker ga-1ptv4jey stale.
Initial setup build was interrupted before verdict because the scratch lacked remote definitions. The completed C6 remote build is the evidence; the interrupted build is not counted.
Each fresh view independently installed and verified remote definitions before its Bazel command.
```text
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn head=684a2d941bfc tree=03dbacbc6ccd dir=/var/tmp/gc-fresh-tree.o21aXV modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ga-1i1itasq.ltBvwn,bazel-out
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn head=684a2d941bfc tree=03dbacbc6ccd dir=/var/tmp/gc-fresh-tree.9qs9bI modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ga-1i1itasq.ltBvwn,bazel-out
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn head=684a2d941bfc tree=03dbacbc6ccd dir=/var/tmp/gc-fresh-tree.RKmlYW modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ga-1i1itasq.ltBvwn,bazel-out
```

## Required CI jobs

ci_required_closure: python3 packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn 9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c b60b584100f81e102e49bdf4bd7f16d54829e412 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit 0; /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/runner-policy.log. |
| changes | LOCAL-PASS | Closure tool exit 0; /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/closure.txt; packs, credential_provider, shared filters do not match. |
| windows-lane | LOCAL-PASS | Workflow's four own run commands, exit 0; /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/windows-lane.output; script retained at /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/windows-lane.sh. Windows allowlist build, Windows test compile, Windows analyzer build/test compile, Darwin all-package build/test compile. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Runner policy resolves runner_windows to blacksmith-4vcpu-windows-2025. Its three package patterns were checked with their own GOOS=windows go list -mod=readonly -test -deps closure (10 module package directories): no changed path reaches it. |
| credential-provider-windows | NOT-TRIGGERED | Closure tool row: needs.changes.outputs.credential_provider == true; filter did not match. Official windows_reach.sh also exits 0 with no paths. |
| pack-gate | NOT-TRIGGERED | Closure tool row: needs.changes.outputs.packs == true; filter did not match. |

windows_reach: none (windows-lane-test own closure); none (credential-provider-windows official reach tool)
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Merge readiness still requires CI / required green on the PR head and the deferred native Windows job successful; this gate authorizes opening the PR only.

Windows/macOS job commands (exact workflow commands, run in a fresh view after the suite):
```bash
#!/usr/bin/env bash
set -euo pipefail
printf 'CI_JOB_STEP Windows allowlist build\n'
GOOS=windows go build $(cat .github/windows-lane/build.txt)
printf 'CI_JOB_STEP Windows allowlist test compile\n'
GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)
printf 'CI_JOB_STEP Windows nogo analyzers\n'
(cd tools/nogo && GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...)
printf 'CI_JOB_STEP Darwin compile all packages and tests\n'
GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...
```

## Load evidence

/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3.output:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=26.35 run_max_load=26.87 run_mean_load=19.41 run_readings=41 wait_first_load=26.35 wait_max_load=26.35 wait_mean_load=26.35 wait_readings=1 read_errors=0
/var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/windows-lane.output:
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.ga-1i1itasq.ltBvwn head=684a2d941bfc tree=03dbacbc6ccd dir=/var/tmp/gc-fresh-tree.mSJmVw modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.ga-1i1itasq.ltBvwn,bazel-out
LOAD_GATE_SUMMARY threshold=15 waited_seconds=150 wait_timed_out=0 run_start_load=14.44 run_max_load=18.31 run_mean_load=16.91 run_readings=3 wait_first_load=18.18 wait_max_load=18.18 wait_mean_load=16.13 wait_readings=6 read_errors=0

Remote class admission is not a local-heavy load wait; the native CI job waited 150 seconds and started below threshold 15. The suite and job keep their own host telemetry.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote","gate_base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","gate_head":"b60b584100f81e102e49bdf4bd7f16d54829e412","admission":{"class":"remote","floor_gib":25.0,"free_gib":65.91338729858398,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"9285cfb3ed95f20bb4dea3ec085b4f36ff6b3a6c","head":"b60b584100f81e102e49bdf4bd7f16d54829e412","merge":"684a2d941bfc798ecf549304cdbde1c396b1340b","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"f7575c375450e499c6c6258ae39749003728e73af240986aad6d85d9acd82a53"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":12876.531,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":50.669,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=0.38 load5=3.41 load15=3.00 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 5.43 4.33 3.32 7/999 56113"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38934}},"log_dir":"/var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218522}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159057,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestReleaseOrphanedPoolAssignments_RouteNameLivenessHonorsStoreReachability PASS (integration-packages-e4245ddf6a37030111bc.log, unit-f93a0ca96ccc61d0cae9.log)\nGATE_TEST_EVIDENCE_RESULT ok: 1 diff-owned tests verified by name\n"}
```

## Retained evidence

Full full execution.json retained: 6058 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/execution.json; SHA256 4629706b1f712c13e6bb6cc87bf782963db38de9e19c0622457055f5b0b1678b.
Full remote skip inventory retained: 4834 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/skip-inventory.json; SHA256 e5e0f04f62d112312248840b5c537f8a3bd572052961d670ba0b5c355c223cf4.
Full native skip inventory retained: 578 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/floor-skip-inventory.json; SHA256 624156a22d09383884507669c3493f38cf587b9d6ae44960f2434c7accafd57c.
Full remote skip source proofs retained: 369 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/skip-site-proofs.txt; SHA256 6942df92b7fced5a7c1fb48999f85ffcf2eff2f960e614752b76177a87a33809.
Full shared-helper skip source proofs retained: 77 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/skip-shared-site-proofs.txt; SHA256 f48ebd403fa0fd6a6e48015f96e3581ec8b4c1ea2da6a16ba80f140d099cf332.
Full native skip source proofs retained: 107 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/floor-skip-site-proofs.txt; SHA256 e1356bb79c4ffef72c306d1262be9e00684ed1001ecd7b9148242e00fc597567.
Full floor step commands and results retained: 1056 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/floor-topup.json; SHA256 170d8d681b28c3501d6604ecebc5b1d75a765725001a75efb0a3566876c95d34.
Full CI closure retained: 17 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/closure.txt; SHA256 61cd89147a49645ac0deaf24da0a8ef8ee300c02950bd8d1e93321cb54ce8f24.
Full native Windows closure proof retained: 16 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/accounting/windows-native-reach.json; SHA256 f9aea55052620d06b81452405b65e7d66ebccbd2d140553dc548d211f2d2030b.

Explicit load fields:
suite: load_threshold=15, load_waited_seconds=0, load_wait_timed_out=0, run_start_load=26.35, run_max_load=26.87, run_mean_load=19.41, run_readings=41
windows-lane: load_threshold=15, load_waited_seconds=150, load_wait_timed_out=0, run_start_load=14.44, run_max_load=18.31, run_mean_load=16.91, run_readings=3
Full acceptance-artifacts.json retained: 702 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/acceptance-artifacts.json; SHA256 f13b1dc85a0e097a41743633c63311cb82360f5c7294ad68e9f724c8403210b7.
Full integration-full-artifacts.json retained: 422 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-full-artifacts.json; SHA256 ade39a3151421fe10bb76b9ce7e4c604b1f504c1e9b1c75afaa2e2ce0d40bd13.
Full integration-latency-artifacts.json retained: 30 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-latency-artifacts.json; SHA256 7361645b75eb613b6e377b3a2e27e89d5fe6afbda042811b50cb0b4bc5112f1c.
Full integration-packages-artifacts.json retained: 2438 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/integration-packages-artifacts.json; SHA256 746222980aae483289b942af4430caa696b89d5d0c081b8ce00c88a47fe7e7b7.
Full unit-artifacts.json retained: 11930 lines; /var/tmp/gc-gate.ga-1i1itasq.963yMj/criterion3/evidence/remote/unit-artifacts.json; SHA256 684bf2a145f81f449eecb7d81d41ac8bb7f3101484bfa225e33b7210e1e6f2d0.
