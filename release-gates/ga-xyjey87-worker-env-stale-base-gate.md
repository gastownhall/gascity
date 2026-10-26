# Worker environment stale-base release gate

**Verdict:** **FAIL**

Disposition: HOLD on native runner environment; no candidate implementation rejection.
Deploy bead: ga-xyjey87. Source: fee292391d2fac3fc88ab3384815bd757aef530d. Review: ga-d2tzyvx.
Build lineage: ga-s9mdfm / ga-6kdidhx. Isolated branch: deploy/ga-xyjey87-gate.
Draft PR: https://github.com/gastownhall/gascity/pull/8366 (remote head remains exact reviewed source).

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Independent PASS binds to the exact reviewed source. reviewer-added-tests.sh independently verifies 3cab89576431ebfd85795ef8ce76b5981c13172a -> fee292391d2fac3fc88ab3384815bd757aef530d, files=scripts/rbe_worker_env_drift_test.go, build bead ga-s9mdfm. |
| 2 | Acceptance criteria met | PASS | Three changed regression tests pass by name; T2 parent and all seven scenarios pass in the full unit run and a direct forced remote replay at the exact source SHA. Real first PR-event lane passes. Bash syntax, legacy skip, unreadable-base/absent-file handling, wiring, docs and resource-census checks verified. |
| 3 | Full CI-equivalent gate | FAIL | All five fresh farm phases pass, but mandatory native-floor push-gate-lock self-test returns1 (53 assertions pass, two fail), and the final concurrency self-test never runs. floor complete=false; criterion3_valid=false. No retry, waiver, clearance or merge-request. |
| 4 | No unresolved HIGH review finding | PASS | Reviewer has zero blocking HIGH findings; advisories a-f remain non-blocking. |
| 5 | Branch clean | PASS | No implementation changes. Only this local FAIL record is added; commit/status verified separately. The FAIL record is not pushed. |
| 6 | Merge state and build/vet | PASS | Canonical merge tree matches git merge-tree output. gc builds; full-tree nogo/vet passes. |
| 7 | Single feature theme | PASS | One CI worker-env stale-base behavior plus tests, docs and resource ledger. |

merge_state: clean (origin/main@e2671dc4dfeeb7bc5ff99a2d55ced65431911aee)
gate_base: e2671dc4dfeeb7bc5ff99a2d55ced65431911aee
gate_base_kind: current-tip
merge_validation: canonical merge result built and vetted here.
scratch_merge_tree: c970f5db2a46e620a18104c22cd756c83c4326b0
deploy_mode: remote
heavy_mode: none
waiver_ref: none

Build: bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc -> PASS.
Vet/policy: full-tree nogo (make vet/make lint) with --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback; four make-check shell guards -> PASS.
Format: make fmt-check via run-pinned-lint.sh, pin/resolved 2.14.0, mismatch=false -> PASS.
Drift: make bazel-sync then git diff --exit-code -> PASS. Fast lanes use separate fresh-tree-run.sh views, leaving the canonical merge unchanged.
policy_attribution: literal make complexity-check rc2 -> ga-q7p6is; pristine same-base and candidate have74byte-identical finding rows, SHA256 c46e67c285c0378673ae8708f9951730c82580c2abb6fdd95132932c6745bc20.
Positive same-run BASE proof, no scanned-path overlap; pinned gocyclo v0.6.0. make complexity-diff against the same base returns0 / no threshold changes. No baseline edit or waiver.

ci_lane_run: PASS, FIRST pull_request run https://github.com/gastownhall/gascity/actions/runs/40396599218, attempt1, exact reviewed source.
All four lanes pass worker-env preflight, Set up Bazel and their actual test step; entire workflow completes SUCCESS. Raw preflight uses fresh base1036b2cb20c15f0a6532fbd04fbb7d63ae42e2b3, with matching checkout/default worker pins, pin-moved=false measure=false. This satisfies the evidence deferred at review; it does not excuse the native-floor failure.
T2 before/after output is in the PR body: actual named expected-red test at f6f2c3ed58e48eb91a0233bbb0023c9458949cc1, actual named green test directly at fee292391d2fac3fc88ab3384815bd757aef530d, plus the real extracted-step outputs. No Closes line.

test_cmd_scope: full-suite (canonical chain launched; mandatory native floor incomplete)
test_cmd:
    test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
native_cmd: gate-floor-topup.py, exact commands and outcomes retained in floor-topup.json.
test_counts: farm Go top-level lines 126771 PASS / 0 FAIL / 358 SKIP; native Go top-level lines21 PASS / 0 FAIL /14 SKIP; mandatory shell self-test53 PASS /2 FAIL, exit1. These are separate observations, not an overall green count.
diff_tests_executed: TestBazelLaneWorkerEnvStepStaleBase PASS; TestBazelMultiLaneWorkerEnvPreflight PASS; TestRBEWorkerEnvDriftPreflight PASS (resolved in full fresh unit logs).
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
farm_test_seconds: 12435.32
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: None
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 47.77246856689453, "waited_seconds": 0.001}
skip_inventory: farm287 other /71 environment-absence observations; full inventory at /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/skip-inventory.json.
skip_justification: non-owned platform/tag/live-environment observations. Required Go package topups all complete rc0, with their remaining14 non-owned skip reasons retained. No owned test skips. Overall floor remains invalid; no PASS inferred from skip counts.
owned_topup: not run (owned tests already pass; native floor fails first).
floor_topup: complete=false;13 Go package commands and Darwin compile rc0, push-gate-lock-selftest rc1, local-concurrency-selftest NOT RUN.
latency_leg: complete=true, measured under probe clear, one attempt; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS.
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/logs
native_test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23/criterion3/evidence/remote/floor-logs

Native failure and disposition:

Tracker: ga-8arookd. No source rework requested from builder.
The caller kept durable evidence inside the live city. gate-floor-topup.py sets TMPDIR=<run-dir>/go-tmp, so shell no-city fixture paths inherit the real city ancestor. The tested library correctly discovers that ancestor, defeating the test premise.
Failing assertions: slots_dir.falls_back_to_repo_relative (scripts/test-push-gate-lock.sh:312) and slots_dir.linked_worktree_uses_common_git_dir (:331). Both resolve the real city gate-slots directory rather than the fixture .git/gate-slots.
failure_attribution: deterministic caller/fixture environment outside the reviewed diff. Both shell test and sourced library are blob-identical at pinned base and reviewed source. Positive BASE reproduction of BOTH exact failing path-resolution conditions is retained in native-base-proof.stdout; it does not acquire live city slots. No altered source is in this shell execution closure.
This tracker was created from this run; it is not cited as pre-existing proof. The positive BASE reproduction establishes the diagnosis. Attribution does not repair the canonical floor: actual FAIL and unexecuted last step remain.
Named exit: mayor determines a documented environment correction and fresh-gate disposition (outside-city temp/evidence placement or a designated isolation fix). No retry into green, stitching a last-step pass, waiver, or deploy clearance from this failed run. A complete fresh gate is required after the correction.

load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 60.33
run_max_load: 62.13
run_mean_load: 60.56
run_readings: 13
Native load receipt: threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=60.33 run_max_load=62.13 run_mean_load=60.56 run_readings=13 wait_first_load=26.96 wait_max_load=74.44 wait_mean_load=47.74 wait_readings=61 read_errors=0

Evidence: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-xyjey87/deploy-gate-20261026-a9rycg23
Farm raw log/XML artifacts:994 files /67365822 bytes; all recorded SHA256 hashes independently verified. Native logs and supplemental exact-source T2 output are retained separately. All services are checked/cleaned before stand-down.
