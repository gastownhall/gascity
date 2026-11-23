# Heartbeat hold for the execution backstop — release gate

**Verdict:** **PASS**

Gate: `ga-02g0dpo`; build: `ga-n4r7hh.1`; review: `ga-1tw2v8v`.
Reviewed source: `df139d02d191b1f25a44ad852ba89b60731dfbd6`. Gate evaluated 2026-11-09T18:12:53.137402+00:00.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Live review ga-1tw2v8v records verdict: pass for the exact reviewed source; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS | Future heartbeat prevents nudge, pacing writes and drain; absent/empty/expired heartbeat preserves escalation; lapse and multi-claim hold preserve the prior pacing state. Four owned tests PASS in both unit and integration-package logs. The adapter guidance is updated in engdocs/architecture/health-patrol.md. |
| 3 | Full test evidence | PASS | Five uncached remote phases, 497 target/shard results PASSED; mandatory native floor 16/16 jobs rc=0. Retained top-level Go result lines: 126878 PASS, 0 FAIL, 372 SKIP; 4/4 owned tests PASS by name. SKIP accounting and CI closure below. |
| 4 | No open HIGH findings | PASS | Review style/security findings: none; uncovered and descoped acceptance: none. |
| 5 | Clean branch | PASS | Reviewed source and the gated scratch are clean. The isolated deploy branch carries the reviewed commits plus this gate record; publication requires a clean committed worktree. |
| 6 | Build/vet and merge state | PASS | Full bazel build //... and make vet/nogo exit 0, terminal SUCCESS BEPs. Conflict recorded below; own-base tree is byte-identical to the reviewed source tree. |
| 7 | One feature theme | PASS | Three reviewed files implement and document heartbeat suppression of the execution-stalled backstop. Stack-aware ancestry scope check accepts only this deploy and build ga-n4r7hh.1; no stack or denied paths. |

## Tree and merge validation

deploy_mode: remote; push_remote: fork; base_ref: origin/main.
merge_state: conflicts (origin/main@abf276cd43e4fda3bf27ef478fdb2ae84ebb6729; files: cmd/gc/execution_backstop_test.go).
gate_base_kind: own-base; gate_base_sha: 5ee67e887d873d60ba5ed7d7271ed006ca2e7806.
gate_head: 9b5e5853cbc1985919cc052debdfd53f3548bf7d; gated_tree: 6d858d37411b64da726256fdd25e633ac90641e0.
merge_validation: deferred to merge time — gastownhall/gascity main has an ALLGREEN SQUASH merge queue (live rules API verified in this run). MPR pins clearance to the exact PR head. The existing test-file conflict requires a builder rebase-only handoff through mayor unless a sweep bead already exists; see deployer docs/merge-time-guarantee.md, Gap 1. No rebase performed here.

## Test execution

test_cmd_scope: full-suite.
test_exec_mode: remote-exec (BEP-derived; the mandatory native floor is reported separately).
test_counts: 126878 PASS, 0 FAIL, 372 SKIP. These are top-level result lines summed over 513 retained logs, not unique test names. Baseline SKIP proof runs are excluded.
test_log_dir: /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/logs; floor_log_dir: /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/floor-logs.
waiver_ref: none; failure_attribution: none; ci_lane_run: n/a (no CI-config change).
heavy_mode: none (classify rc=0); fallback_reason: none; owned_topup: none (all owned tests already PASS).
farm_test_seconds: 9744.776.
floor_topup: complete=true; 12 environment-absence packages / 34 selected parent tests, plus mandatory beadstest, Darwin compile, push-gate-lock self-test and local-concurrency self-test; 16/16 exit 0.
latency_leg: measured PASS under a clear farm probe; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget both PASS; neither ran in bulk integration. Probe waited 30s, load1=45.94 at admission, threshold=50.
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; run_start_load: 68.44; run_max_load: 86.64; run_mean_load: 74.50; run_readings: 12 (native floor soft wait). Remote admission has no local load wait: threshold15, waited_seconds0, wait_timed_out0, run_start_load38.79, run_max_loadNA, run_mean_loadNA, run_readings0.
Runtime: ICU74 verdict=OK (recorded sonames), rootless podman socket active, matching cached images, pinned bd v1.3.2. Isolation wrapper used; remote runner and native floor scrub store/session selectors. The wrapper reports PASS-THROUGH for the private scratch path; no clean-environment claim is based on the wrapper alone.

Commands (each phase exit 0; all use --nocache_test_results --noremote_local_fallback):

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

policy_lane: PASS — run-pinned-lint.sh -- make fmt-check-changed; make lint with nogo; release-dist-ignore, routed-test-rows, split-topology-rows, residency-boundary and hook checks; 13 policy/format/docsync Bazel targets.
drift_lane: PASS — make bazel-sync then git diff --exit-code and empty git status; 7 generated client/schema/dashboard/lockfile checks; OpenAPI-breaking target PASS. Each lane ran in its own recorded fresh-tree view before runtime setup and the suite.

diff_tests_executed:

- `TestExecutionBackstopHeartbeatHoldKeepsThePacingState`: PASS in unit and integration-packages.
- `TestExecutionBackstopHonorsAHeartbeatHold`: PASS in unit and integration-packages.
- `TestExecutionBackstopIgnoresAHeartbeatThatIsNotInForce`: PASS in unit and integration-packages.
- `TestExecutionBackstopMultiClaimHoldKeepsThePacingState`: PASS in unit and integration-packages.

## SKIP evidence

skip_inventory: 358 farm rows (287 other, 71 environment-absence); native floor 39 SKIP rows (29 environment-absence, 10 other), all non-owned. These inventories include subtests, so their counts differ from top-level test_counts.
skip_justification: unchanged platform/provider capability guards, intentionally unavailable provider-error fixtures, opt-in replay/coverage/live journeys, missing optional br/PostgreSQL fixtures, and tmux bind-key tests outside tmux. The local floor ran every farm environment-absence selection; remaining non-owned floor SKIPs are inventoried rather than counted as PASS. Seven native live-herdr SKIPs take the unchanged unit-lane opt-in guard instead of the farm missing-herdr guard. No heartbeat test was skipped.
All 269 farm-reported sites are UNCHANGED, 0 unresolved. Shared helper hunks are holdUntil and holdThenLapse; their callers are only the four owned tests. Named pinned-base comparisons for cmd/gc: default 141 + integration 27 pre-existing SKIPs, 0 new, 0 reason-changed, 0 diff-owned. Native remaining reasons and sites were inspected separately.

## Required CI job accounting

ci_required_closure: python3 $GC_CITY_ROOT/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-02g0dpo.p5obgnvv/merge 5ee67e887d873d60ba5ed7d7271ed006ca2e7806 df139d02d191b1f25a44ad852ba89b60731dfbd6 fork; workflow blob 654b52ade60492ba2cc64e3ef1a0a0d2afe88e18; 4 LEAF jobs need a status.
ci_required_accounting:

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-02g0dpo.p5obgnvv/runner-policy.outputs python3 .github/workflows/scripts/runner_policy.py; rc=0; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/ci-run/units/gate.w1.stdout.log and stderr log. Fresh matching tree; load threshold15, waited90s, no timeout, start14.45. |
| changes | LOCAL-PASS | Post-suite ci_required_triggers.py rc=0; exact tool output retained below. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED if: needs.changes.outputs.credential_provider == 'true'. |
| pack-gate | NOT-TRIGGERED | Tool row: linux NOT-TRIGGERED if: needs.changes.outputs.packs == 'true'. |

check is an AGGREGATOR, not a leaf lane. windows_reach: n/a (job NOT-TRIGGERED); deferred_to_pr_ci: none; not_covered: none.
PASS authorizes PR-open. Merge still requires all applicable CI checks on the current head and conflict resolution through merge authority.

## Compact execution record

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote","admission":{"class":"remote","floor_gib":25.0,"free_gib":68.51789093017578,"waited_seconds":0.0,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98","worker_env_origin_main":"sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"},"test_exec_bep":{"unit":{"results":373,"remote_executed":373,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":85,"remote_executed":85,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":9744.776,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":117.413,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=65.81 load5=59.35 load15=27.90 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=30s load1=45.94 load5=55.12 load15=27.49 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 43.45 53.00 28.51 67/2256 53869"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":30622}},"log_dir":"/var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":287,"environment-absence":71},"top_level_counts_by_reason_class":{"other":215,"environment-absence":42},"tests":{"elided_array":{"items":358,"bytes":127999}}},"test_cmd":{"elided_array":{"items":5,"bytes":2054}},"test_counts":{"PASS":126878,"FAIL":0,"SKIP":372},"diff_tests_executed":"diff_tests_executed:\n  TestExecutionBackstopHeartbeatHoldKeepsThePacingState PASS (integration-packages-e5330b8c981927473156.log, unit-637c74c578a2b17fc654.log)\n  TestExecutionBackstopHonorsAHeartbeatHold PASS (integration-packages-b72a67a2f9444af705e3.log, unit-3d6d3036a2f5902472b5.log)\n  TestExecutionBackstopIgnoresAHeartbeatThatIsNotInForce PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-27b23e708b477fce4dad.log)\n  TestExecutionBackstopMultiClaimHoldKeepsThePacingState PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-423c6a0da1986b90e9a7.log)\nGATE_TEST_EVIDENCE_RESULT ok: 4 diff-owned tests verified by name\n"}
```

## Retained evidence

Full execution.json retained: 3917 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/execution.json; SHA256 eec981423f4ec2c64423ae48b859aa72d02cd612bcfdab0382d3b4876336d7b3.

Full independent phase/artifact audit retained: 70 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/resume-remote-audit.json; SHA256 3a47bcc0c3b217888e2ad449e2dbd375979ecb2f406ac55da24f0a92348aecc9.

Full fast-lane records retained: 111 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/fast-verified.json; SHA256 2bd6d420678bc5bf9206a20ecaa866ede5fc93df0041fa8d3791260c65161dcc.

Full criterion6 build/vet audit retained: 49 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c6-verified.json; SHA256 f5e206bcf8b1fd31e56bdf08d15bc18cf78cdc6e677c2b96d72e8702e2f15d3f.

Full farm SKIP inventory retained: 2866 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/skip-inventory.json; SHA256 180f45fd1f375ba7f07b5d2244ec06425a5da885268fbc351924b246f3665cc9.

Full native SKIP inventory retained: 314 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/native-skip-inventory.json; SHA256 59c8a520bb1bdbfaf7f246b8adcc208b5de5e928cfa52f82b272686b291318d0.

Full SKIP site proofs retained: 273 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/skip-sites.txt; SHA256 523d9b4b2679909489ba357ca2b8e433996739ad134ee94f47ab1f2da9c910f9.

Full shared-hunk SKIP baseline proof retained: 34 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/skip-base-verified.json; SHA256 3156f3dfe0791892760eec8689429f3f5358cca028b6ca5d9364966ddf105679.

Full floor steps retained: 901 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/c3-run/evidence/remote/floor-topup.json; SHA256 42f8062cda233a41de95bd90de18900da0ae5376330821aa38ce5b7d8c1a6949.

Full owned results and counts retained: 10 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/final-by-name.txt; SHA256 8c7a301ef66543e7626050b2d7c3e7e290a4ea8decffd47077e263d8e5452b40.

Full CI closure retained: 17 lines; /var/tmp/deploy-ga-02g0dpo.p5obgnvv/ci-closure-final.txt; SHA256 df68df3a267555df2130a71fab9b212081401f596df58a591f1d793dfa92778e.

