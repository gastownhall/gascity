# Hook matcher release gate

**Verdict:** **PASS**

Bead: ga-0waf3181. Build: ga-lwtazy. Review: ga-bbaayqj5 (closed PASS).
UTC: 2026-11-25T06:15:12.094151+00:00
deploy_mode: remote; base_ref: origin/main; push_remote: fork.
reviewed_commit: 577b8f1f442466bd148789977028ab81a39fbf2b
gate_base_sha: af2429b01c66823df7950aac09a3a480c56a45b2
gate_merge_commit: 59cc5807fb59c6d7035494d16f2b64e8af919758
gate_base_kind: current-tip
merge_state: clean (origin/main@af2429b01c66)
merge_validation: merge authority must validate the eventual merge result; deploy clearance authorizes PR opening, never merge.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS. Review approves the source with reviewer-added tests; reviewer-added-tests.sh check independently returned reviewer-tests files=internal/overlay/lint_test.go. |
| 2 | Acceptance | PASS. PermissionDenied warning, comma split, message event/index and partially wrapped/non-array category cases pass. Six required/extra mutations are killed by their intended tests. Pinned golangci-lint 2.14.0: 0 issues; nogo lint/vet pass. |
| 3 | Tests | PASS. Full five-phase suite plus required native floor, owned tests by name, skip source/reach audit and all six CI leaf accounts below. No waiver or failure attribution. |
| 4 | High findings | PASS. No unresolved HIGH findings in the review; production change is one static event map entry. |
| 5 | Clean branch | PASS. Source checkout clean before the record; publication commits this sole bookkeeping addition and verifies clean status. |
| 6 | Merge/build | PASS. Clean merged tree built/vetted with bazel build --config=remote-exec --noremote_local_fallback //... (22683 actions). No source rebase. |
| 7 | One theme | PASS. Only internal/overlay/lint.go and lint_test.go change: hook matcher validation/coverage. Ancestry accepts only ga-0waf3181 and ga-lwtazy; no stack and no internal-doc contamination. |

## Review and acceptance

reviewer_tests_verified: 306ce9277c30c69d9b2652b7493645fb1fc474ae -> 577b8f1f442466bd148789977028ab81a39fbf2b files=internal/overlay/lint_test.go
PD, M18, M24, M27, M28 and MC each caused the expected named test failure while 165 or 166 other result lines passed. PD uses its original raw log after removing Go output framing; it was not rerun. Mutations ran in separate fresh views and never changed the merge scratch.
Optional decisions remain as recorded by the builder: no version-dependent handler-if recommendation; existing JavaScript-only regex WARN handling already covers the stated RE2 examples.
acceptance_lint: run-pinned-lint.sh -- golangci-lint run ./internal/overlay/...; rc 0; pin=resolved=2.14.0, mismatch=false.

## Test evidence

policy_lane: git diff --check; make lint with explicit remote-exec/no-fallback NOGO_BUILD_FLAGS; bazel test //:go_format_test; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; all rc 0. Each lane ran in its own fresh view; gate-base.sh pinned the base.
drift_lane: make bazel-sync with explicit remote-exec/no-fallback BAZEL_SYNC_FLAGS, then git diff --exit-code; rc 0, empty diff.
ci_lane_run: n/a (no CI-config change).
heavy_mode: none; delta_gate: shadow/full, reason=not-gate-pr (no narrowing).
waiver_ref: none; failure_attribution: none; policy_attribution: none.
commands: gate-criterion3.sh -> make check / bazel test //...; acceptance; integration-packages; full integration without latency tests; isolated latency phase. Every remote result is uncached, strategy remote, attempt 1 and inside its phase window. Native floor ran 16 steps, all rc 0.
```text
test_counts: PASS=159207 FAIL=0 SKIP=618
diff_tests_executed:
  TestFindBareHookEntries_NonArrayCategoryDoesNotStopTheScan PASS (unit-a54202c8767a4f2cca9b.log)
  TestFindBareHookEntries_RequiresNeitherKey PASS (unit-a54202c8767a4f2cca9b.log)
  TestFindInvalidHookMatchers_Classification PASS (unit-a54202c8767a4f2cca9b.log)
  TestFindInvalidHookMatchers_OrdersFindingsByCategoryThenIndex PASS (unit-a54202c8767a4f2cca9b.log)
GATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name
```
test_log_dir: /var/tmp/gc-ga-0waf3181-gate.cMsULH/criterion3/evidence/remote/logs
skip_justification: 618 top-level SKIP result lines (676 total including subtests); 319 unique source sites all read UNCHANGED, line_in_hunk=no. No overlay test SKIPs. Reasons include unit/process lane guards, platform restrictions, optional provider capabilities and absent host tools. Every environment-absence farm skip received a native top-up; remaining non-owned skips are inventoried. All 42 skip packages' test dependency closures contain no overlay test variant, so the sole SHARED-HUNKS entry (lint_test.go imports fmt/slices) cannot reach them; no HELPER-HUNKS changed. None of the skip-site source files references FindInvalidHookMatchers, hookMatcherProblem or toolMatcherEvents. The only production hunk adds a static PermissionDenied map key used by lint diagnostics, not any recorded skip condition. Thirty FencedInput subtest records forward through unchanged fencedStart at runtime/runtimetest/conformance.go:509. No base reproduction is required by the shared-hunk rule.

```text
criterion3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=28.23 run_max_load=28.48 run_mean_load=18.84 run_readings=51 wait_first_load=28.23 wait_max_load=28.23 wait_mean_load=28.23 wait_readings=1 read_errors=0
ci-required/windows-lane: LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 run_start_load=14.93 run_max_load=17.20 run_mean_load=16.75 run_readings=4 wait_first_load=15.47 wait_max_load=15.47 wait_mean_load=15.25 wait_readings=3 read_errors=0
acceptance-pinned-lint: LOAD_GATE_SUMMARY threshold=15 waited_seconds=120 wait_timed_out=0 run_start_load=14.42 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=16.42 wait_max_load=16.42 wait_mean_load=15.58 wait_readings=5 read_errors=0
```

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.25hr53 af2429b01c66823df7950aac09a3a480c56a45b2 577b8f1f442466bd148789977028ab81a39fbf2b fork; rc 0.
workflow_blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; check is an aggregator.
Runner expressions resolved from runner_policy.py: Ubuntu Blacksmith runners for Linux; runner_windows=blacksmith-4vcpu-windows-2025.

| Job | Status | Evidence under ci-required/ |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc 0; runner-policy.out and runner-policy.log |
| changes | LOCAL-PASS | ci_required_triggers.py (merged workflow and repository path matcher); rc 0; closure.txt |
| credential-provider-windows | NOT-TRIGGERED | closure.txt: if needs.changes.outputs.credential_provider == 'true'; no matching paths |
| pack-gate | NOT-TRIGGERED | closure.txt: if needs.changes.outputs.packs == 'true'; no matching paths |
| windows-lane | LOCAL-PASS | Exact four run steps from merged ci.yml, with tools/nogo working directory honored; ci-required/windows-lane.sh; detached fresh-tree/load/isolation wrappers; rc 0; windows-lane/units/gate.w1.stdout.log and .stderr.log |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner_windows=blacksmith-4vcpu-windows-2025; Windows test closure from .github/windows-lane/test.txt has no changed file; windows-native-reach.txt; rc 0 |

windows_reach: none (credentialprovider, genshell, processgroup native Windows test closures). Import closure computed with GOOS=windows GOARCH=amd64 go list -mod=readonly -test -deps over .github/windows-lane/test.txt; rc 0.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Before merge: CI / required must be green at the PR head and windows-lane-test must succeed. This PASS alone is no merge authorization.

## Retained records

Gate root: /var/tmp/gc-ga-0waf3181-gate.cMsULH (protected from reaping; raw logs retained).
Full execution retained: 6059 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/criterion3/evidence/execution.json; SHA256 fa3778943b484475245b406f5f9bb99293290939c93287e1390eb4af3ca664b2.
Full skip-source audit retained: 9091 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/skip-audit.json; SHA256 0c8da44bf66b286f1eb51a11fd89415d7839b3058f062bb13c2f7e18ab757267.
Full skip import reach retained: 2144 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/skip-reach.txt; SHA256 2bdad353d923fdd03c4358bf21e9758845eeaad257c3382e224de7bb72535068.
Full CI closure retained: 17 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/ci-required/closure.txt; SHA256 942f065f6b818328138ea1a3784a94edf0c4863ec210afa8ccfd05ebf994a3c0.
Full CI accounting retained: 39 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/ci-required/accounting.json; SHA256 5a1bb0ac95e337147d76555b27553c90b6d711816cd901038feef83a5cc9fbf1.
Full mutation evidence retained: 127 lines; /var/tmp/gc-ga-0waf3181-gate.cMsULH/mutation-evidence/summary.json; SHA256 ac4f01af9a51fdaa301694ec3aea778384f60579fd61495af03b9953cb85228e.

Compact execution record (large arrays retained in the files above):
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-ga-0waf3181-gate.cMsULH/criterion3/evidence/remote","gate_base":"af2429b01c66823df7950aac09a3a480c56a45b2","gate_head":"577b8f1f442466bd148789977028ab81a39fbf2b","admission":{"class":"remote","floor_gib":25.0,"free_gib":42.51313018798828,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"af2429b01c66823df7950aac09a3a480c56a45b2","head":"577b8f1f442466bd148789977028ab81a39fbf2b","merge":"59cc5807fb59c6d7035494d16f2b64e8af919758","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"cb1c46417b85729fe14658d5827cee459d24006629bad3b2b3f702aaefffa0be"},"test_exec_bep":{"unit":{"results":426,"remote_executed":426,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":14504.659,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":122.786,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=81.84 load5=109.62 load15=68.13 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=40s load1=46.04 load5=96.85 load15=65.57 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 32.00 87.78 63.70 7/1066 56261"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38935}},"log_dir":"/var/tmp/gc-ga-0waf3181-gate.cMsULH/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218624}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159207,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestFindBareHookEntries_NonArrayCategoryDoesNotStopTheScan PASS (unit-a54202c8767a4f2cca9b.log)\n  TestFindBareHookEntries_RequiresNeitherKey PASS (unit-a54202c8767a4f2cca9b.log)\n  TestFindInvalidHookMatchers_Classification PASS (unit-a54202c8767a4f2cca9b.log)\n  TestFindInvalidHookMatchers_OrdersFindingsByCategoryThenIndex PASS (unit-a54202c8767a4f2cca9b.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```
