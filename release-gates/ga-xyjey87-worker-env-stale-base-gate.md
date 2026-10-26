# Worker environment stale-base release gate

**Verdict:** **FAIL**

Disposition: HOLD. The complete replacement run stopped on an unrelated fixture cleanup failure; required remaining coverage is absent. No candidate implementation rejection, clearance or merge-request.

Deploy bead: ga-xyjey87. Reviewed source: fee292391d2fac3fc88ab3384815bd757aef530d.
Review: ga-d2tzyvx. Build lineage: ga-s9mdfm / ga-6kdidhx.
Isolated branch: deploy/ga-xyjey87-gate. Draft PR: https://github.com/gastownhall/gascity/pull/8366.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Independent PASS names the exact reviewed source. reviewer-added-tests.sh again verifies 3cab89576431ebfd85795ef8ce76b5981c13172a -> fee292391d2fac3fc88ab3384815bd757aef530d, files=scripts/rbe_worker_env_drift_test.go, build bead ga-s9mdfm. |
| 2 | Acceptance criteria met | PASS | All three changed tests pass by name; stale remote, stale non-remote, fresh base, moved pin, unreadable base and missing-file behavior independently checked. Real first PR-event workflow passes all four changed lanes. T2 actual red/green output is in the PR body, with no Closes line. |
| 3 | Full CI-equivalent gate | FAIL | Unit and acceptance pass. Integration-packages:35/36 targets pass, one fixture cleanup failure. The runner stops before full integration, latency and mandatory native floor; criterion3_valid=false. Mechanism attribution of the unrelated failure cannot supply the absent coverage. |
| 4 | No unresolved HIGH finding | PASS | Reviewer has zero blocking HIGH findings; advisories a-f are explicitly non-blocking. |
| 5 | Branch clean | PASS | No implementation changes. Only this local FAIL record changes; commit and final clean status verified separately. This record is not pushed. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge matches git merge-tree output; gc build and full-tree nogo/vet pass. |
| 7 | Single feature theme | PASS | One CI worker-env stale-base behavior, with tests, docs and resource ledger. |

merge_state: clean (origin/main@37bd09351ac80604606366872b4efeed63b624b8)
gate_base: 37bd09351ac80604606366872b4efeed63b624b8
gate_base_kind: current-tip
merge_validation: canonical merge result built and vetted here; merge authority owns merge-time freshness.
scratch_merge_tree: 5985aaf2f44f7abded7765b7701b4dff1096369c
deploy_mode: remote
heavy_mode: none
waiver_ref: none

Build: bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc -> PASS.
Vet/policy: make vet and make lint with full-tree nogo, --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback; four shell guards -> PASS.
Format: make fmt-check under run-pinned-lint.sh, pin/resolved2.14.0 mismatch=false -> PASS.
Drift: make bazel-sync then git diff --exit-code -> PASS. Each fast lane uses a separate fresh-tree-run.sh view; canonical scratch is unchanged.
policy_attribution: literal make complexity-check rc2 -> ga-q7p6is. Pristine same-base and candidate have 74 byte-identical finding rows, SHA256 2ac4f0a851ec91ed7aae01aa2dcf2f7c69be041f395fc1625349e8e338f43642.
Positive same-run BASE proof d under criterion3b mirror; no offending/scanned production path changed. Pinned gocyclo v0.6.0; make complexity-diff against that exact base returns0/no threshold changes. No baseline edit or waiver.

ci_lane_run: PASS, FIRST pull_request run https://github.com/gastownhall/gascity/actions/runs/40396599218, attempt1 on the exact reviewed source.
All four matrix lanes pass worker-env preflight, Set up Bazel and their actual test step; entire workflow SUCCESS. CI-resolved base1036b2cb20c15f0a6532fbd04fbb7d63ae42e2b3 resolves locally. Raw preflight has matching checkout/default worker pins, pin-moved=false measure=false.
Actual T2 before/after: named expected-red run at f6f2c3ed58e48eb91a0233bbb0023c9458949cc1 reports step error <nil>, want failure true, pin-moved=true/measure=true and shell exit0; direct named run at fee292391d2fac3fc88ab3384815bd757aef530d passes parent and seven cases. Supplemental historical source receipts are separate from this replacement C3.

test_cmd_scope: full-suite (canonical chain launched, incomplete after phase3 FAIL)
test_cmd:
    test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-regate.r7stjN/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-regate.r7stjN/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-regate.r7stjN/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_counts: PASS=126629 FAIL=1 SKIP=344 (top-level Go result lines summed across482 retained logs; repeated tests count per job)
diff_tests_executed:
  TestBazelLaneWorkerEnvStepStaleBase PASS (unit-81d8a9af827e251f6b12.log)
  TestBazelMultiLaneWorkerEnvPreflight PASS (unit-271bc77ad6715453dfe1.log)
  TestRBEWorkerEnvDriftPreflight PASS (unit-81d8a9af827e251f6b12.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
farm_test_seconds: 7814.199
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: None
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 40.64474105834961, "waited_seconds": 0.001}
skip_inventory:344 observations (274 other,70 environment-absence), raw reasons retained in skip-inventory-observed.json. Mandatory environment-absence topups were NOT RUN; these skips cannot establish complete coverage. No changed test skips.
owned_topup: NOT RUN (three changed tests already PASS in the unit phase; chain stopped first).
floor_topup: NOT RUN.
latency_leg: NOT RUN.
integration_full: NOT RUN.
test_log_dir: /var/tmp/gc-ga-xyjey87-regate.r7stjN/criterion3/evidence/remote/logs

Failure attribution and hold:

TestReaperOwnerProtectionOnRealBd -> ga-p6ihcl1. //examples/gastown:gastown_test shard3of5, first attempt,9.59s. Test-body assertions report no failure; t.TempDir RemoveAll reports directory not empty at the fixture001 root. Specific writer/root cause is unestablished.
Clause1: not diff-owned; no examples/gastown path changed. Clause3: proof(a), computed full integration-tag module import closure reaches none of the changed production package internal/testpolicy/resourcecensus. Invoked maintenance scripts are unchanged; changed worker-env helper/workflow is not invoked here. Clause4: no package/path overlap.
Clause2: exact-name query and all open tracker bodies searched; no matching established condition. New condition record ga-p6ihcl1 is supported by landed mechanism proof(a) under the discovering-run exception. It is not claimed as prior-run proof. Similar cleanup error text in other fixture trackers is insufficient to claim one root cause.
This attributes the observed failure outside the reviewed diff. It does not change the actual runner exit1/criterion3_valid=false or fill the missing phases. Canonical gate-rbe-run.py stops on that phase verdict; no supported attribution/continuation path was used or invented.
Named exit: mayor determines the documented canonical-runner disposition or routes a demonstrated condition fix. A valid complete gate remains required. No failed-test retry into green, manually assembled continuation, result stitching, waiver or builder rework requested.

load_threshold:15 (native floor not entered)
load_waited_seconds: not applicable (remote class has no host-load wait; native floor not entered)
load_wait_timed_out: not applicable
run_start_load: not applicable (no native run; farm worker load not inferred from host)
run_max_load: not applicable
run_mean_load: not applicable
run_readings:0
Host load snapshot at disposition, not farm execution load: 32.81 56.71 71.91 44/4938 687254

Round history: earlier city-located run remains FAIL in local commit9820416eb9fe807bca8ad5aadd7ea392c1fa608b, with raw evidence retained. Mayor disposition gm-wisp-s6n73lv closed caller-placement tracker ga-8arookd and authorized this complete replacement run, all evidence/logs/go-tmp under /var/tmp outside every city. The corrected placement is verified by Bash ancestor probe exit1/empty/no stderr, but the native assertions were never reached in this replacement run. No old result counts toward this run.
Replacement evidence: /var/tmp/gc-ga-xyjey87-regate.r7stjN
Retained farm artifacts: {"bytes": 67528739, "files": 964, "full_suite": false, "phases": ["unit", "acceptance", "integration-packages"], "sha256_verified": true}
All retained log/XML digests independently verified. Failed service completed; owned scratch/cache cleanup is recorded separately. No push, force-push, source rewrite, clearance, merge-request or merge from this failed round.
