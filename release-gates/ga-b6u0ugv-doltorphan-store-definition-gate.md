# Dolt store-definition release gate — ga-b6u0ugv

**Verdict:** **FAIL**

Reviewed source: fcb8a7600d4bb22cc52469c316cc643f2c5112be
Frozen gate base: 2dec01c71a3dd5cdd0ca494ea7c72f4806d6c770
Canonical merge commit: a236e670b95b380ba2fc17135ec92d7807d7671a
Canonical merge tree: 47ca4af3331a7cbafa6e0e12e31de603b3e2cd0f
gate_base: 2dec01c71a3dd5cdd0ca494ea7c72f4806d6c770
DEPLOY_MODE: remote; BASE_REF: origin/main
merge_state: clean (origin/main@2dec01c71a3dd5cdd0ca494ea7c72f4806d6c770)
gate_base_kind: current-tip
merge_validation: merge result built and vetted here; no rebase

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS | PASS | ga-dzmgn7b records verdict pass on the git-resolved reviewed source. |
| 2 | Acceptance | FAIL | AC9's independently required focused native real-Dolt test failed at line72 before SIGKILL. AC13 PR publication is not reached. The amended AC12 complexity condition passed. |
| 3 | Full CI-equivalent tests | FAIL | Four forced-fresh remote phases passed, but the same diff-owned test FAILed natively. The required native floor had not started; its wait was intentionally canceled after this decisive failure. No full criterion3 PASS is claimed. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer records zero HIGH/blocker findings; minor follow-ups are nonblocking. |
| 5 | Clean candidate | PASS | Canonical committed merge is clean. Only this local gate record is committed to the isolated audit branch; no feature source is changed. |
| 6 | Merge state and merged build/vet | PASS | Exact merge-tree equality verified. Bazel build //... and full-tree make vet/nogo pass; real pre-commit hooks ran during materialization. |
| 7 | Single feature theme | PASS | Three source files: one store-definition rule, its unit regressions and real-Dolt canary. |

## Decisive native failure

Command (inside the required isolation wrapper, after load-gate-run.sh):

```text
PATH=/usr/local/bin:<pinned test PATH> go test -tags integration -count=1 -v -run '^TestSweep_ReapsRealDoltDataDirAfterSIGKILL$' ./examples/gastown
```

The command scrubbed GC_, BD_, BEADS_ and DOLT_ selectors and disabled interactive credentials. `/usr/local/bin/dolt version` reported 2.3.3. The wrapper's disk check passed at 77.5GiB free / 40GiB floor. Supplemental scope: focused; counts: 0 PASS, 1 FAIL, 0 SKIP.

```text
    dolt_orphan_sweep_integration_test.go:72: data dir vanished from a sweep pass that should have skipped it: stat /var/tmp/gotmp/TestSweep_ReapsRealDoltDataDirAfterSIGKILL724540542/001/dolt-data: no such file or directory
--- FAIL: TestSweep_ReapsRealDoltDataDirAfterSIGKILL (15.37s)
```

The failure precedes the explicit SIGKILL and the diff's added second-sweep assertions. Reaching line72 means the earlier checks for zero Removed and Skipped==1 did not fail. The root cause and removal actor are not established. No causal blame is assigned to the implementation.

Ownership is still decisive: this diff modifies the test's own function body. diff-owned-tests.py --site reports func_touched=+92,18, shared_hunks_in_file=0, => TOUCHED even though line72 itself is outside the added hunk. Clause1 forbids attributing this FAIL away. A successful remote execution does not erase a later native FAIL. No rerun, attribution PASS or waiver was attempted; gascity has no waiver path.

Load evidence:

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=18.18 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=44.25 wait_max_load=49.46 wait_mean_load=32.55 wait_readings=61 read_errors=0
load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 18.18
run_max_load: NA
run_mean_load: NA
run_readings: 0
```

The ordinary bounded wait timed out and ran the command as configured. This wait result is not itself a test failure. NA denotes a command shorter than the sampling interval.

## Retained successful evidence

Already-merged preflight: no associated PR. Deploy-ref interlock and ancestry guard PASS with ga-b6u0ugv and source build ga-47vvd8. The reviewed source was never rebased, reset, amended or pushed.

fast_lane: PASS 2026-10-17T02:32:44Z
policy_lane: PASS — format, full-tree nogo/vet, changed-package nogo, required shell guards and all nine static policy/format targets
drift_lane: PASS — make bazel-sync; git diff --exit-code; clean untracked check
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none

PM's base-relative-no-new-regression-v1 amendment and mayor's hold release were honored. `make complexity-diff` against this SAME gate base, with verified gocyclo v0.6.0 and CCN20, exited0 and printed no threshold changes. Analyzer, baseline and exclusions were unchanged. The previous absolute-baseline HOLD remains historical.

Independent RED/GREEN proof: the candidate tests with the prior sweep implementation give 34 PASS /13 FAIL /0 SKIP result lines (subtests included); restoring the candidate gives 47 PASS /0 FAIL /0 SKIP. These focused results do not replace full-suite evidence.

| Remote phase | Verdict | Fresh remote results | Cached results |
|---|---|---:|---:|
| unit | PASS | 371 | 0 |
| acceptance | PASS | 24 | 0 |
| integration-packages | PASS | 81 | 0 |
| integration-full | PASS | 14 | 0 |

Every phase BEP has strategy remote, no cached flag, attempt1, a start in its phase window, terminal verdicts and zero predicate errors. Full integration uses no smoke filter. Farm test seconds (sum): 7467.515.

Remote Go log counts after all four phases: 116282 PASS /0 FAIL /355 SKIP top-level result lines across 490 retained logs. All eleven owned tests PASS by name remotely, including the subsequently failing native real-Dolt test. Remote SKIPs:71 environment-absence,284 other; none belongs to this diff or cites a changed file. The required native floor was waiting on host load and was stopped before its commands ran. Its completion is not claimed.

test_cmd: gate-criterion3.sh (make check; acceptance; integration-packages; full integration; required native floor)
test_cmd_scope: full-suite
test_exec_mode: remote-exec for the four completed farm phases; native floor incomplete
test_exec_bep: phase evidence JSONs under /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/
test_log_dir: /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/logs
rbe_state: /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/rbe-state.json
admission: /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/admission.json
fallback_reason: none
owned_topup: not needed remotely; no rerun of the native FAIL
floor_topup: not run — wait canceled after decisive independent failure
skip_inventory: /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/skip-inventory.json
failure_attribution: none (clause1 diff-owned)

## Disposition and evidence

Technical FAIL for builder diagnosis/rework under the mandatory handoff. No push, PR, clearance, merge request or production deployment. No test is retried into PASS. The normal post-SIGKILL assertions and added second-sweep assertions were not reached in the failed native execution.

Evidence root: /var/tmp/deploy-ga-b6u0ugv.6HKsbPSm
Failure: focused-real-dolt.log; failure-site.txt; failure-verification.json
Remote phases: c3-run/evidence/remote/*stdout, *evidence.json, *bep.json and logs/
Fast lane: build-nogo.log, vet.log, complexity.log, complexity-tool.txt, complexity-tool.sha256, format.log, policy.log, drift.log
Acceptance: red.log, green.log, red-green-proof.json

The suite was stopped only through its recorded gate-detached-run service. Live readback: gc-heavy-c3-run-1792204393-467160.service inactive/dead/MainPID0; acceptance service failed and its test process exited; captured Dolt PID3488274 is absent. The watcher completed and its self-notification gm-wisp-hkgthgr was recipient/subject peek-verified. No live suite remains. Raw logs are retained, not deleted.

Actual completed farm commands:

```text
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-b6u0ugv.6HKsbPSm/c3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Admission:

```json
{"class":"remote","floor_gib":25.0,"free_gib":51.757659912109375,"waited_seconds":0.001,"cap":2}
```

RBE state:

```json
{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0","worker_env_origin_main":"sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}
```
