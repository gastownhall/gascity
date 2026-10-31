# Acceptance M1 port-collision release gate

**Verdict:** **PASS**

Policy PASS includes two attributed baseline integration failures, with the original failing results preserved. No waiver was used.

Deploy bead: ga-s6m0vo6. Build: ga-94toqk3. Independent review: ga-3xyl1kc (closed, PASS; no reviewer-added tests).
Reviewed source: `ed2bb0bf329834ca1bafd636c29b728d73deacc2`.
Pinned base: `origin/main@45e46d550f6f02531cf93b6fd7dbf21899d93c50`.
Tested synthetic merge: `6398044095588a5920c825defd149d0dd01531c6`; tree `6fb30e736d590c022ea9c3d32c84166802269f8c`.
Evidence root: `/var/tmp/deploy-ga-s6m0vo6.1XV0HOPw` (retained).

Main advanced while the gate ran. Publication-time origin/main is `b941bd330f411c115bb339c63465fa68b358dcf3`; a read-only merge-tree still succeeds (tree `439bff313e4fc9e1b5f4c39a5848f0be68b0c51b`). The complete gate evidence remains bound to the explicitly pinned base and tested merge above; no self-rebase or claim that a newer merge tree was tested.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Reviewed source | PASS — the review names the exact resolved source above; no carryover, reviewer-added patch, or branch-tip substitution. |
| 2 | Acceptance criteria | PASS — diagnostics precede rollback; status, arguments and isolated tool HOME survive; only proved fresh managed-local M1 collisions recover, once. Twelve owned tests pass by name, plus a real occupied-port M1 run and real pre-journal M5 top-up. Deferred tracker/old-gate conditions remain unchanged. |
| 3 | Full-scope tests and required lanes | PASS under criterion 3a — all four fresh farm phases completed. Two full-integration failures meet the four clauses and fix-carrying repeat exception below. All 16 local floor steps and the owned acceptance top-up completed; all twelve owned tests pass. Raw runner exit 1 and criterion3_valid=false remain intact. |
| 4 | Open high-severity findings | PASS — review has zero unresolved HIGH/blocker/major findings. |
| 5 | Clean source | PASS — reviewed content and merge scratch are clean; the gate record is the sole deployment addition. Final clean status is checked again after the gate-only commit, before publication. |
| 6 | Merge state, build and vet | PASS — evaluated first: clean synthetic merge against the pinned base. All-target build and nogo passed, with policy/format/drift checks in separate fresh views before the suite. |
| 7 | One feature theme | PASS — two reviewed commits, seven files under test/acceptance, +1004/-3: init diagnostics and the proved once-only recovery at the existing rig-add acceptance site. No production Go, role, provider, dependency pin or generic rollback change. |

merge_state: clean (origin/main@45e46d550f6f02531cf93b6fd7dbf21899d93c50)
gate_base_kind: current-tip
merge_validation: the recorded clean merge result was built and vetted here. Active GitHub main merge_queue (SQUASH, ALLGREEN, max_entries_to_build=4) requires Check, CodeQL x4, bazel test (side-by-side), and BUILD files in sync on the merge group; live rules read into main-rules.json.
deploy_mode: remote
base_ref: origin/main
heavy_mode: none
ci_lane_run: n/a (no CI-config diff)
waiver_ref: none (gascity has no waiver path)

Preflight found no PR associated with the reviewed source. Ancestry accepts only this deploy/build lineage; the builder branch is provenance and is never a push target. `make check-hooks` confirms active `.githooks`.

fast_lane: PASS before suite launch. `bazel build --config=remote-exec --noremote_local_fallback --keep_going //...`; `make lint` (nogo); format and policy targets `//:go_format_test //scripts/cipolicy:cipolicy_test //scripts/prwatchdog/... //scripts:scripts_test`; runner-policy/CI-suite-coverage Python checks; and make-check shell guards. `make bazel-sync` plus `git diff --exit-code` passed in a separate fresh view. Commands and output: `fast-check.sh`, `fast-result.txt`, `build.log`, `policy.log`, `drift.log` under the evidence root. TESTING.md and the Makefile supply the canonical commands; docs/PROJECT_MANIFEST.md is absent.

| Farm phase | Fresh target/result evidence |
|---|---|
| unit (`make check`, Bazel //...) | 287/287 targets PASS; 373/373 result attempts PASSED |
| acceptance (Tier A + repository SOLO targets) | 7/7 targets PASS; 24/24 result attempts PASSED |
| integration-packages | 31/31 targets PASS; 80/80 result attempts PASSED |
| integration-full (no smoke filter) | all 14 shards completed: 12 PASSED, 2 FAILED with the attributed signatures below |

test_cmd_scope: full-suite
test_cmd: `gate-criterion3.sh` through detached systemd service, admission and isolation. Each of the four phases uses --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1; integration-full additionally uses --config=integration --test_arg=-test.v on //test/integration:integration_test. Exact phase commands remain in their .stdout/BEP files.
test_exec_mode: remote-exec (BEP-derived for the complete farm suite; mandatory local floor and owned top-ups are additive)
test_exec_bep: 491/491 remote result attempts; 0 cached locally/remotely, 0 retries, every start inside its phase window and every verdict terminal. 489 PASSED / 2 FAILED. Independent reinspection and all 982 original artifact hashes are recorded in independent-phase-audit.json.
farm_test_seconds: 9431.769
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}
fallback_reason: none for the farm suite; no local suite or result stitching
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 65.2472915649414, "waited_seconds": 0.005}
test_counts: 123722 PASS / 2 FAIL (attributed) / 371 SKIP, top-level result lines summed over retained farm/floor/top-up logs, not unique names. All subtests remain available in the logs.
test_log_dir: /var/tmp/deploy-ga-s6m0vo6.1XV0HOPw/criterion3/evidence/remote/logs
skip_inventory: /var/tmp/deploy-ga-s6m0vo6.1XV0HOPw/criterion3/evidence/remote/skip-inventory.json — 356 original farm events (71 environment-absence, 285 other); environment-absence parents were selected by package for the floor. Residual native SKIPs are separately inventoried. The owned M5 gap is repaired by a real-binary top-up, never justified as a skip.
floor_topup: /var/tmp/deploy-ga-s6m0vo6.1XV0HOPw/criterion3/evidence/remote/floor-topup.json — complete, 16/16 steps exit 0, including beadstest, Darwin compile and both runner self-tests.
owned_topup: /var/tmp/deploy-ga-s6m0vo6.1XV0HOPw/native-acceptance — real pre-journal gc, M1 control and M5 PASS, zero native recovery lines, all eight M5 phases PASS, no owned FAIL/SKIP. The pre-journal binary hash is 96517962c480f35f62d5cfdcc121151c186e06b0bb154f88ae9a6ce334f1a646; source provenance remains in legacy-provenance.json. Pinned bd1.3.1 and Dolt2.2.0 are checked in the run output. The native run is the documented make test-acceptance-go top-up under threshold15 load gating and isolation, not a replacement for any farm phase.

Non-owned residual SKIPs are existing opt-in live-provider/environment checks, helper-process entries, declared capability gaps, topology selection, and tracked test-boundary exclusions. Their exact reasons remain in the inventory. In the changed helper package the three bd-absence skip sites are UNCHANGED with zero shared hunks; all three were selected for the native floor. The changed helper does not alter their skip conditions. M5 is owned and is handled by the real-binary execution above.

diff_tests_executed:

```text
  TestBeadsInitTopologyMatrix PASS (acceptance-00f787a1201c0c97f5dd.log, acceptance-2129301bc9bf3acdcdf7.log +3 more; also SKIP: TestBeadsInitTopologyMatrix/M5-legacy-gc-managed in acceptance-00f787a1201c0c97f5dd.log, acceptance-2129301bc9bf3acdcdf7.log, acceptance-f785906030efd99733e1.log +1 more (PASS in another job))
  TestBDInitWrapperBoundsOversizedLogsAndKeepsTheConflictLine PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperEmitsLogsBeforeControlReturns PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperFallsBackToTheWorkingDirectoryScope PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperKeepsToolHomeIsolation PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperLeavesSuccessAndOtherVerbsUntouched PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperPreservesStatusStreamsAndArgv PASS (unit-2af37a65322c095289c3.log)
  TestBDInitWrapperReportsUnavailableLogsAndKeepsTheOriginalError PASS (unit-2af37a65322c095289c3.log)
  TestParseBDInitDiagnostics PASS (unit-2af37a65322c095289c3.log)
  TestPortCollisionProof PASS (unit-2af37a65322c095289c3.log)
  TestRigAddRecoveryEligibility PASS (unit-2af37a65322c095289c3.log)
  TestRigAddRecoveryPolicy PASS (unit-2af37a65322c095289c3.log)
```

Acceptance evidence: `TestBDInitWrapper*` proves pre-rollback bounded logs and unchanged status/argv/HOME; `TestParseBDInitDiagnostics`, `TestPortCollisionProof`, `TestRigAddRecoveryEligibility` and `TestRigAddRecoveryPolicy` cover scope/proof parsing, endpoint/topology exclusions, missing or stale proof, retained/pre-existing stores and the second-failure boundary. Native M5 asserts the unjournaled initialization shape and runs all eight phases.

The independent occupied-port M1 run bound backend port38525 before Dolt, retained the exact own-server proof, logged one recovery, and PASSed in134.23s. `cases/squat/squat.log`, `m1-squat.bep.json` and `m1-squat-artifacts.json` retain that proof. The collector stopped after the PASS because it expected file URIs; its original error is retained. The completed test.log and test.xml were recovered directly from authenticated ByteStream and matched their BEP CAS hashes and sizes, with no test rerun. The subsequent, previously unrun control/M5 checks used the native runner after shared origin/main changed its worker pin; the original farm suite had matching pins at launch. Builder negative/control evidence was independently read: fixed-squat PASS/one recovery, base-squat FAIL, fixed-control PASS/zero recovery, fixed-repeat FAIL after one recovery. See builder-case-audit.json; none of those negative verdicts was retried or rewritten.

failure_attribution:

- `TestGCLiveContract_BeadsAndEvents` → `ga-od4rcxy`, designated unlanded fix `ga-ze0aym0`. Clause 1: untouched test/function and zero shared hunks (`site-audit.txt`). Clause 2: tracker opened and predates this run; new sighting appended. Clause 3(d): positive pristine-base reproduction at `b1a5e28d43f2122ff395282e6d0d86e6578a57b6`, log `/var/tmp/ga-12r0j8v-deploy.P7dxpp/base-repro/test.log`, SHA-256 `017eab47c0c160f3f5c66bcdf3ab758a175a332bab057adf3dfb4e8ce2799006`, read and rehashed during this gate. Clause 4: failing package `test/integration` has no diff overlap. The import closure does include `test/acceptance/helpers`; no unreachability or host-load proof is claimed.
- `TestGcSeatHookLatencyBudget` → `ga-bqsrp4o`, designated unlanded fix `ga-yu01v6x`. Clause 1: untouched test/function and zero shared hunks (`site-audit.txt`). Clause 2: tracker opened and predates this run; new sighting appended. Clause 3(d): positive pristine-base reproduction at `5b75e237fb70c593061c9957ae72fcc8fcf24071`, log `/var/tmp/seatlat-runs.lkjMkH/B1/logs/integration-full-f7217e413eb188f8ffa6.log`, SHA-256 `e5f46419117df43363a31b61b3aaa331d17b78bd91b0775faa3206f59c770245`, read and rehashed during this gate. Clause 4: failing package `test/integration` has no diff overlap. The import closure does include `test/acceptance/helpers`; no unreachability or host-load proof is claimed.

Fix-carrying repeat exception: this deploy's build_bead ga-94toqk3 is stamped gc.fixes_tracker=ga-86zysul. Each blocking condition has a designated gc.fixes_tracker-stamped fix that has not landed: ga-ze0aym0 (closed with blocked work record; source commit absent from pinned main) and ga-yu01v6x (open). Thus the existing repeat signatures may be attributed under all four clauses; no failed candidate test or farm suite was rerun. Active-step failed with HTTP422/query.assignee missing; seat-hook fastest prime1484/drain1052/mail907ms exceeded the1s budget. The baseline reproductions are historical proofs, explicitly not fresh base runs in this gate. The raw failure records remain unchanged.

Load measurements from the invoking host (not the remote farm):

```text
criterion3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=51.73 run_max_load=111.97 run_mean_load=87.16 run_readings=45 wait_first_load=51.73 wait_max_load=51.73 wait_mean_load=51.73 wait_readings=1 read_errors=0
supplement-queued: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1741 wait_timed_out=1 run_start_load=26.23 run_max_load=25.17 run_mean_load=22.89 run_readings=8 wait_first_load=60.75 wait_max_load=112.45 wait_mean_load=74.63 wait_readings=59 read_errors=0
floor-continuation: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=23.23 run_max_load=36.70 run_mean_load=31.04 run_readings=10 wait_first_load=30.84 wait_max_load=31.09 wait_mean_load=23.37 wait_readings=61 read_errors=0
native-acceptance: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=50.30 run_max_load=53.10 run_mean_load=51.15 run_readings=7 wait_first_load=22.67 wait_max_load=53.11 wait_mean_load=28.85 wait_readings=61 read_errors=0
```

Ordinary native waits are bounded soft waits; any timeout is recorded rather than waived. Farm execution intentionally uses no host load wait. Local runtime resolves ICU74; tooling and native temporary directories are confined to the owned test/evidence scope.

Deferred conditions verified live: ga-86zysul remains OPEN. ga-ilggxtc remains OPEN behind its blocks dependency on ga-86zysul. This deploy alone does not close the tracker, remove that dependency, retry the prior failed gate or change its evidence. Mayor/coordinator must verify the applicable reviewed fix on origin/main and accept its scope against that acceptance sighting; only then is a complete fresh gate eligible. Ordinary independent review is ga-3xyl1kc; this gate is the independent deploy verification.
