# Dispatch local gate budget — ga-5lad67jw

**Verdict:** **PASS**

Reviewed source: `34ee209f8c883ab5c17ea24ee0b38ca370d57769`; build `ga-m2wt6mb`; review `ga-088o6z2v`.
DEPLOY_MODE: remote; push remote: fork; base ref: origin/main.
Gated merge: `35bc93b59e740ec21b2018a35bc0060712820a6d`; tree: `5a0829c66a67e8597358a6c0aa37bcc4b88d55b8`.
merge_state: clean (origin/main@189612d9ab766ae1b56cee37b0231bb163f72f61)
gate_base_kind: current-tip
merge_validation: MPR owns merging; GitHub's merge queue tests the merge group. A changed PR head requires new deploy clearance.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-088o6z2v records verdict pass for the resolved reviewed source; no carryover or reviewer-added commit. |
| 2 | Acceptance criteria | PASS — AC1–5 checked below against the one-file diff, local execution and retained review evidence; merge-time obligations remain explicitly assigned. |
| 3 | Tests and required jobs | PASS — fresh five-phase full suite, 16/16 native floor steps exit 0, fast lanes and complete six-leaf CI accounting below. Native Windows tests are deferred to PR CI. |
| 4 | No unresolved HIGH findings | PASS — review security_findings: none; style_findings: none blocking. Remaining wording/positional-balance advisories are informational. |
| 5 | Final branch clean | PASS — source worktree and canonical merge clean before adding this record; this dedicated record is the only addition; a clean post-commit status is required before push. |
| 6 | Merge state and build/vet | PASS — clean pinned merge; full `bazel build //...` and `make vet` (nogo), each exit 0, in c6-run-output.log; CRITERION6_PASS. |
| 7 | Single feature theme | PASS — one source commit changes only internal/dispatch/BUILD.bazel (+15/-0): dispatch target partitioning and its explanation. |

## Acceptance evidence

AC1–2: unfiltered local `bazel test --config=fork-cache --config=ci --nocache_test_results --test_env=GO_TEST_WRAP_TESTV=1 --test_output=all //internal/dispatch:dispatch_test`, once per tree, through fresh-tree-run, local-heavy admission, load-gate-run (threshold 15/max-wait 1800) and isolated-test-run. Baseline `189612d9ab766ae1b56cee37b0231bb163f72f61`: one first-attempt linux-sandbox result, 83.784 s. Gated merge: five first-attempt linux-sandbox results, 34.461/48.676/54.873/56.382/70.261 s. Both: 1032 PASS/0 FAIL/1 identical SKIP; exactly the same 1033 names/statuses; 580 PAUSE=CONT pairs. All twelve retained log/XML hashes independently verified. Scope: affected-package acceptance; never a substitute for criterion 3.

The baseline and changed run had different natural host loads (mean 14.74 versus 61.33); these observations show completion under the unchanged 300 s action budget, not a speedup ratio or p95. Builder-only synthetic contention evidence: baseline TIMEOUT 300.6 s, five shards PASS at 141.1 s slowest; quiet-ish baseline 87.0 s, five shards 31.2 s. Those are single cells, independently reviewed but not reproduced by this deployer. No synthetic load was generated here.

AC3: no Go source/test/assertion, skip, retry, timeout, census or resource-budget change. The only executable BUILD addition is shard_count=5. The existing dispatch SKIP is TestCrashInjectionCallBudgetMolecule/fail, call_budget_test.go:537, reason “ga-vnycm2.27: interrupted abort-scope converges with different skip metadata”; site UNCHANGED and base comparison ALL PRE-EXISTING.

AC4: existing target execution is the oracle; no shard-count pin test. Policy, generated/BUILD sync, hooks, independent review and this deploy gate pass. MPR's independent merge-time pass is required before landing.

AC5: build ga-m2wt6mb retains gc.fixes_tracker=ga-pxiuas5. Tracker ga-pxiuas5 remains OPEN until this repair is confirmed on main by work-record evidence. Held gates ga-vqt4gk3 (PR #7305) and ga-0taedav (PR #8506) were not rerun here; after all prerequisites land, each needs its own fresh complete gate. Neither build/review close nor this PR-open PASS clears them.

## Test commands and evidence

policy_lane: PASS — fresh view through gate-base.sh run: run-pinned-lint.sh -- make fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=189612d9ab766ae1b56cee37b0231bb163f72f61; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; policy Bazel targets listed in retained fast-lane.sh; all exit 0. Whole-tree nogo: C6 make vet exit 0.
drift_lane: PASS — separate fresh view: make bazel-sync; git diff --exit-code and empty git status --porcelain --untracked-files=all; generated artifact targets; separate make openapi-breaking-check OPENAPI_BREAKING_BASE=189612d9ab766ae1b56cee37b0231bb163f72f61; all exit 0. Full commands/log: /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/fast-lane.sh and fast-run-output.log.
fast_lane: PASS; heavy_mode: none; GLT pin/resolved: 2.14.0; mismatch: false.
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/merge --base 189612d9ab766ae1b56cee37b0231bb163f72f61 --bead-json /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/bead.json --owned /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/owned.tsv --run-dir /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence; GC_GATE_REMOTE_ENABLED=1; all five phases exit 0.
Common Bazel flags: --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=<phase>.bep.json; NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec.

- unit: make -k check (bazel test //... plus shell guards), exit 0.
- acceptance: bazel test <common flags> --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests, exit 0.
- integration-packages: bazel test <common flags> --config=integration //test:integration_packages, exit 0.
- integration-full: bazel test <common flags> --config=integration --test_arg=-test.v --test_arg=-test.skip=<latency list regex> //test/integration:integration_test, exit 0.
- integration-latency: bazel test <common flags> --config=integration --test_arg=-test.v --test_filter=<latency list regex> --test_sharding_strategy=disabled --run_under=<gate-farm-probe.sh> //test/integration:integration_test, exit 0. Regex derives solely from gate-latency-tests.txt.

test_cmd_scope: full-suite
test_counts: PASS=154290 FAIL=0 SKIP=604 (runner's per-log result-line count, not unique test identities)
diff_tests_executed: none (no diff-owned tests; mechanical inventory empty, no Go hunks)
waiver_ref: none (no Gas City waiver path)
test_log_dir: /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/remote/logs
skip_justification: all Go skip sites are unchanged by this BUILD-only diff. Remote inventory: 590 occurrences, 90 environment-absence and 500 other; every selected environment-absence parent ran in the native floor. Remaining non-owned skips cover unavailable herdr/br/Postgres/live-tmux fixtures, replay/coverage inputs, intentional simulated-unavailable providers, topology shapes, platform/helper-only paths and existing tracked exclusions. Full names/reasons remain in the retained inventories and floor logs. Dispatch's one skip was additionally reproduced at pinned base with identical reason; no diff-owned test is skipped, failed or missing.
ci_lane_run: n/a (no CI job/matrix/timeout/required-check configuration change; target sharding measured locally and in the full gate)

load_threshold: 15 (local floor); load_waited_seconds: 1561; load_wait_timed_out: 0; run_start_load: 14.32; run_max_load: 13.80; run_mean_load: 12.98; run_readings: 6. Raw telemetry values retained verbatim.

Local-floor load (remote phases do not wait on host load): LOAD_GATE_SUMMARY threshold=15 waited_seconds=1561 wait_timed_out=0 run_start_load=14.32 run_max_load=13.80 run_mean_load=12.98 run_readings=6 wait_first_load=79.06 wait_max_load=92.87 wait_mean_load=56.46 wait_readings=53 read_errors=0
Local acceptance baseline: LOAD_GATE_SUMMARY threshold=15 waited_seconds=210 wait_timed_out=0 run_start_load=14.59 run_max_load=15.08 run_mean_load=14.74 run_readings=3 wait_first_load=20.79 wait_max_load=20.79 wait_mean_load=16.94 wait_readings=9 read_errors=0 prewait_s=210
Local acceptance head: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=58.70 run_max_load=61.50 run_mean_load=61.33 run_readings=3 wait_first_load=24.78 wait_max_load=77.19 wait_mean_load=53.34 wait_readings=62 read_errors=0 prewait_s=1800
CI cross-compile load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.33 run_max_load=11.84 run_mean_load=10.66 run_readings=3 wait_first_load=9.33 wait_max_load=9.33 wait_mean_load=9.33 wait_readings=2 read_errors=0 prewait_s=0

Execution record (compact; full step arrays, skip inventories and raw logs retained below):

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":52.85309600830078,"waited_seconds":0.002,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"test_exec_bep":{"unit":{"results":415,"remote_executed":415,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":13036.239,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":56.295,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=17.75 load5=8.35 load15=3.21 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 16.64 8.98 3.59 12/1249 53757"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":39570}},"log_dir":"/var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":500,"environment-absence":90},"top_level_counts_by_reason_class":{"other":242,"environment-absence":43},"tests":{"elided_array":{"items":590,"bytes":216555}}},"test_cmd":{"elided_array":{"items":5,"bytes":2104}},"test_counts":{"PASS":154290,"FAIL":0,"SKIP":604},"diff_tests_executed":"diff_tests_executed: none (no diff-owned tests)\nGATE_TEST_EVIDENCE_RESULT ok: the diff owns no test\n"}
```

## Required CI closure

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/merge 189612d9ab766ae1b56cee37b0231bb163f72f61 34ee209f8c883ab5c17ea24ee0b38ca370d57769 fork; merged workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status. check is an AGGREGATOR with its Bazel coverage supplied by the full five-phase gate; no leaf is omitted.
ci_required_accounting:

- changes | LOCAL-PASS | closure tool above, exit 0; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/closure.txt.
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py, exit 0 in the pinned merge; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/runner-policy.log.
- credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/merge -- /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py --class local-heavy --record /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/windows-admission.json --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --class local-heavy --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-windows-lane.sh; exit 0; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/windows-lane-output.log.
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | native runner blacksmith-4vcpu-windows-2025; its own Windows unit+integration import closures for ./internal/credentialprovider and ./internal/genshell/... contain no changed inputs; closures retained under ci-required/.

windows-lane executed the workflow's commands verbatim, exit 0 each: GOOS=windows go build $(cat .github/windows-lane/build.txt); GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt); in tools/nogo, GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...; GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./.... These are compile proofs, not native Windows execution.
windows_reach: windows-lane-test=none; credential-provider-windows=NOT-TRIGGERED.
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

This PASS authorizes PR-open. Merge requires CI / required green on the exact cleared head, including success of windows-lane-test; MPR owns the merge. The tracker closes only after confirmed main landing.

## Retained evidence

Full execution retained: 5927 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/execution.json; SHA256 804ef9e76f490526155c63820535b987be72d32e4c07033b8a99d15980eb1c3f.

Full floor steps retained: 1056 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/remote/floor-topup.json; SHA256 7dedb1473b8b327264fb7f1f9612c278553450a262371b023fdb21207cae69fd.

Full remote skips retained: 4722 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/criterion3-run/evidence/remote/skip-inventory.json; SHA256 1e8150780e67b96425a6efdf284574a644692d78f81a5a93dd1bc215086c32c0.

Full CI accounting retained: 114 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/ci-required/accounting.json; SHA256 bbae52cfe83889cfe3661440b556f5916cda75580b4f59e060111fdd161e90c0.

Full local dispatch audit retained: 29 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/c2-independent-audit.json; SHA256 4444f46b542326ff7b9cabef45e3baeefbcbb7e81a1c5c2c3a1626f9d75029ac.

Full dispatch skip proof retained: 1 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/c2-skip-ownership.txt; SHA256 e47d9fa175eea52b228a13d9c643dbe70b29357dac2b3dd8128506cb8fa667ad.

Full dispatch skip comparison retained: 2 lines; /var/tmp/gc-gate-ga-5lad67jw-5gpny30l/c2-skip-comparison.txt; SHA256 d33006d7cad3bd8f2d1dbebcb97e78cbddcc37b74388627fe875eee979ab18f6.
