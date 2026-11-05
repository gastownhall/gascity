**Verdict:** **PASS**

# Release gate: doctor cleared-hold routing

Bead: ga-nsvtj3j; build: ga-uu1fpt; review: ga-uc3efxs.
Reviewed source: `54d6c8dfe44429c30616dd16f19f4320ba5db064`.
Gate base: `54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae`; tested merge: `e082cc9d116ea6dedcf574d1507d62afbcad969c`.
Evidence directory: `/var/tmp/deploy-ga-nsvtj3j.e0bm17c7`.

| # | Verdict | Evidence |
|---|---|---|
| 1 | PASS | Review ga-uc3efxs records PASS with no HIGH findings. Reviewer test-only extension independently verified with reviewer-added-tests.sh. |
| 2 | PASS | Cleared holds preserve absent/existing routes; a live hold alongside cleared is still repaired in either label order. Both regression tests PASS by name. Only the doctor check and existing test file changed; waiver/deployer documentation untouched. |
| 3 | PASS | Five fresh remote phases and all 16 required native-floor steps passed; CI-required leaf accounting complete below. |
| 4 | PASS | Zero unresolved HIGH findings; reviewer noted only a nonblocking comment nit. |
| 5 | PASS | Clean source checkout; this record is the only deployer change. Publish verifies a clean index/worktree after its commit. |
| 6 | PASS | Clean merge materialized and verified against git merge-tree; whole-tree Bazel build/nogo and CLI smoke passed. |
| 7 | PASS | One theme: prevent released holds from overwriting bead routing metadata. No unrelated commits, new dependencies, CI changes, or internal-agent files. |

reviewer_tests_verified: 7b520db48645407b40678284bcb35c61fb021b84 -> 54d6c8dfe44429c30616dd16f19f4320ba5db064 files=cmd/gc/doctor_hold_label_routed_to_test.go
merge_state: clean (origin/main@54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae)
merge_validation: merge result built and vetted here; merge authority owns validation against subsequent main changes.
gate_base_kind: current-tip
gate_base: 54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae
fast_lane: PASS 2026-10-31T22:07:56.321816+00:00 merge_head=e082cc9d116ea6dedcf574d1507d62afbcad969c
criterion6: bazel build --config=remote-exec --noremote_local_fallback //...; rc=0; c6-result.log. CLI version and doctor --help smoke rc=0.
policy_lane: fresh-tree-run.sh -> gate-base.sh run -> isolated-test-run.sh --rbe -> make lint NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback"; shell guards check-release-dist-ignore/check-routed-test-rows/check-split-topology-rows/check-residency-boundary; //:go_format_test; rc=0. Lint/format executed locally; transport config alone supplies no endpoint in that fresh view.
drift_lane: separate fresh-tree-run.sh -> isolated-test-run.sh --rbe -> rbe-local-rc.sh -> make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec; git diff --exit-code; rc=0. Gazelle embed-expression warnings produced no drift.

FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.aeDdXR head=e082cc9d116e tree=6bb3dc5fc6a8 dir=/var/tmp/gc-fresh-tree.vc0KjI modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.aeDdXR,bazel-out
GATE_BASE_RECORD base=54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.vc0KjI
FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.aeDdXR head=e082cc9d116e tree=6bb3dc5fc6a8 dir=/var/tmp/gc-fresh-tree.RW0GP6 modified=0 untracked=0 ignored=6 ignored_names=.bazelrc.local,.github/workflows/scripts/__pycache__/,bazel-bin,bazel-gc-merge-validate.aeDdXR,bazel-out

test_cmd_scope: full-suite
test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.aeDdXR --base 54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae --bead-json /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/bead.json --owned /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/owned.tsv --run-dir /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence; detached c3-run completed rc=0.
Phases: unit make check; acceptance; integration-packages; full integration excluding only the two latency tests; latency tests alone under farm probe. Exact Bazel/Make command output is retained in each remote/<phase>.stdout and summarized below.
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_exec_mode: remote-exec (BEP-derived; native floor is additional local verification)
test_exec_bep: {"unit":{"results":388,"remote_executed":388,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":24,"remote_executed":24,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT"}
admission: {"class":"remote","floor_gib":25.0,"free_gib":54.85481262207031,"waited_seconds":0.001,"cap":2}
fallback_reason: none
farm_test_seconds: 11042.177
test_counts: PASS=135356 FAIL=0 SKIP=471 (top-level result lines across 529 logs; repeated executions are counted separately, not unique tests).
test_log_dir: /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/remote/logs
diff_tests_executed:
  TestHoldLabelRoutedToCheckClearedHoldDoesNotMaskLiveHold PASS (integration-packages-568812e4d525bdd1f974.log, unit-10cc7795c0ec199a5530.log)
  TestHoldLabelRoutedToCheckIgnoresClearedHold PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-5a9df316125cd0aeff2e.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
skip_inventory: remote/skip-inventory.json: 457 entries (386 other, 71 environment-absence). All environment-absence selections were rerun natively by package, including parent tests for subtests.
skip_justification: remaining non-owned skips cover unsupported provider contracts, platform/fixture guards, opt-in live providers, and absent PostgreSQL credentials. The diff changes only hold-label interpretation, not those preconditions; neither owned test skipped. Native top-up skips remain inventoried in floor logs.
owned_topup: no missing owned verdict; both tests PASS in unit and integration-packages.
floor_topup: complete=true; 16/16 steps rc=0, including pinned beadstest, Darwin compile, both runner self-tests and every environment-absence selection; remote/floor-topup.json and floor-logs/.
latency_leg: complete=true, attempt=1, probe_verdict=clear after 30s wait (load1=47.00, threshold=50); TestGcBdLatencyBudget PASS; TestGcSeatHookLatencyBudget PASS. Neither ran in the main integration leg.
heavy_mode: none
failure_attribution: n/a (zero test failures)
waiver_ref: none (no gascity waiver path)
ci_lane_run: n/a (no CI-config diff)
wrapper_mode: PASS-THROUGH, not isolated. Tests ran on the clean temporary merged tree with pinned test bd v1.3.2; remote tests used the farm, native floor scrubbed store selectors.
GATE_ICU74: verdict=OK provenance=recorded (host loader; no per-run library-path flags).

Native-floor load evidence: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=39.43 run_max_load=37.11 run_mean_load=32.14 run_readings=7 wait_first_load=35.56 wait_max_load=75.75 wait_mean_load=43.54 wait_readings=61 read_errors=0
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: true; run_start_load: 39.43; run_max_load: 37.11; run_mean_load: 32.14; run_readings: 7. Ordinary soft wait continued under its documented timeout policy.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.aeDdXR 54d9a6c6bdf2cb33a8f0b495c817d71d8dc8c8ae 54d6c8dfe44429c30616dd16f19f4320ba5db064 fork; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status; ci-required-closure.txt.
ci_required_accounting:
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-nsvtj3j.e0bm17c7/runner-policy.out python3 .github/workflows/scripts/runner_policy.py rc=0 /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/runner-policy.log
- changes | LOCAL-PASS | ci_required_triggers.py (closure invocation) rc=0 /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/ci-required-closure.txt
- credential-provider-windows | NOT-TRIGGERED | needs.changes.outputs.credential_provider == 'true'; filter credential_provider no match
- pack-gate | NOT-TRIGGERED | needs.changes.outputs.packs == 'true'; filter packs no match
windows_reach: none (windows_reach.sh rc=0, empty stdout).
deferred_to_pr_ci: none
not_covered: none

Full machine record: /var/tmp/deploy-ga-nsvtj3j.e0bm17c7/c3-run/evidence/execution.json. Compact fields above are the review record; raw logs and inventories remain on disk.
Initial C6 invocation rejected the test-only --config=ci on a build command before analysis; corrected invocation c6-run-2 passed. No failed test was retried into PASS.

Phase command mapping (BAZEL_FLAGS recorded above): unit `make check`; acceptance `bazel test $BAZEL_FLAGS --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests`; integration-packages `bazel test $BAZEL_FLAGS --config=integration //test:integration_packages`; integration-full `bazel test $BAZEL_FLAGS --config=integration --test_arg=-test.v --test_arg=-test.skip=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$ //test/integration:integration_test`; integration-latency uses the same target/config with that expression as --test_filter, --test_sharding_strategy=disabled, and --run_under=gate-farm-probe.sh (retained script digest and arguments in latency-leg.json).
