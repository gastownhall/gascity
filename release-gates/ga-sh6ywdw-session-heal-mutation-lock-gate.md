# Session heal mutation lock release gate

**Verdict:** **PASS**

- Deploy bead: ga-sh6ywdw; build ga-rxg8786; review ga-54leg4z.
- Reviewed source: 827726521d8da2de2da5a7fcb3a51d7876fd1c08.
- gate_base: 3bd6c5e46fce77c921a4af48b2e57e7955072efb
- gate_base_kind: current-tip
- merge_state: clean (origin/main@3bd6c5e46fce77c921a4af48b2e57e7955072efb)
- merge_validation: the clean merge result was built and vetted, then independently tested here.
- Canonical scratch merge: 12fc3407f9f4f0074d64bc3626e875140a166f04; tree 89b5d5f1495066f9167daa4cc5fbabe707ec09d1.
- Evidence root: /var/tmp/gc-gate.ga-sh6ywdw.9SniWt
- Scope: only internal/session; three owned tests; no CI configuration changes.
- Deployment authority: PR and exact-head clearance only; merge belongs to MPR.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-54leg4z records verdict pass at the resolved reviewed source; no carryover or reviewer-added-test substitution. |
| 2 | Acceptance criteria met | PASS | Both read/write boundaries hold the mutation lock; a concurrent suspend survives; a busy row skips without waiting. All three regressions fail by assertion with the two production files restored to the pinned base and pass in the canonical suite. Each also passes 20 race runs, zero race reports. The caller-held-lock variant is unchanged. Doc comments, SESSION-RECON-021, AGENTS.md, and BUILD registration are present. The optional 60-run live-contract probe was not required or run. |
| 3 | Full-scope tests and policy | PASS | Unit287/287, acceptance7/7, integration-packages31/31; all14 full-integration results are fresh remote, with one attributed failure under criterion3a. The separate latency leg and all16 native-floor steps passed. All three owned tests pass by name; no waiver. Raw runner failure is preserved and the separate attributed completion proves the unrun remainder completed. |
| 4 | No unresolved HIGH finding | PASS | Reviewer security findings none; spec PASS. Four style advisories are minor/nit and nonblocking. |
| 5 | Final branch clean | PASS | Only this gate file is added to the exact reviewed source; pre-existing untracked gate records are retained outside the deploy checkout. The publishing step verifies clean status after the active pre-commit hook. |
| 6 | Merge recorded; build/vet | PASS | Clean merge onto the pinned base; bazel build --config=remote-exec --noremote_local_fallback --keep_going //... built1385targets with nogo, elapsed307.972s. |
| 7 | Single feature theme | PASS | Advisory session heals share the existing in-process mutation lock with operator commands. All six source paths belong to internal/session. |

## Execution evidence

- test_cmd_scope: full-suite
- test_cmd: gate-criterion3.sh on the immutable scratch; standard forced unit make check, acceptance, integration-packages, integration-full excluding the latency registry, and integration-latency. After the attributed phase-four failure, the untouched standard latency_leg and floor_topup functions completed the unrun remainder on the same pinned scratch. No failed candidate test was rerun.
- Every remote test phase used --config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 and its own BEP; exact command lines remain in each phase .stdout.
- test_exec_mode: remote-exec, derived from all five BEPs.
- test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 80, "results": 80, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
- farm_test_seconds: 10474.869
- test_counts: 125018 PASS, 1 attributed FAIL, 370 SKIP (top-level results per retained job).
- diff_tests_executed: TestApplyPatchIfLifecycleUnchangedHoldsMutationLockAcrossReadAndWrite PASS; TestApplyPatchIfLifecycleUnchangedKeepsAConcurrentOperatorSuspend PASS; TestApplyPatchIfLifecycleUnchangedSkipsWhileAnOperatorHoldsTheRow PASS. By-name evidence: /var/tmp/gc-gate.ga-sh6ywdw.9SniWt/by-name-final.txt.
- test_log_dir: /var/tmp/gc-gate.ga-sh6ywdw.9SniWt/c3/evidence/remote/logs; original artifacts and digests retained under bazel-artifacts.
- Evidence audit: 492 fresh remote results, 984 digest-verified raw artifacts, 492 normalized logs checked byte-for-byte against raw logs with the documented control-marker removal.
- rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 55.124908447265625, "waited_seconds": 0.001}; continuation admission separately retained in continuation-admission.json.
- fallback_reason: none.
- skip_inventory: /var/tmp/gc-gate.ga-sh6ywdw.9SniWt/c3/evidence/remote/skip-inventory.json; classification counts {"environment-absence": 71, "other": 285}.
- skip_justification: all owned tests ran and passed. Remaining skips are untouched unit/tag, platform, opt-in, or unavailable-environment gates; the complete native floor exercised every environment-absence selection by parent/package. The new test file contains no skip; unrelated skip sites are outside its diff. Non-owned native skips remain inventoried.
- owned_topup: none needed; all owned tests were found in the canonical full-suite output.
- floor_topup: 16/16 steps exit0; /var/tmp/gc-gate.ga-sh6ywdw.9SniWt/c3/evidence/remote/floor-topup.json (beadstest, Darwin compile, runner self-tests, and environment-absence package top-ups).
- latency_leg: complete=true, both registry budgets PASS under a clear probe; /var/tmp/gc-gate.ga-sh6ywdw.9SniWt/c3/evidence/remote/latency-leg.json.
- load_threshold: 15
- load_waited_seconds: 1802
- load_wait_timed_out: 1
- run_start_load: 63.53
- run_max_load: 59.77
- run_mean_load: 50.91
- run_readings: 8
- Remote phases used remote admission without the local-heavy slot or host-load wait; the required native floor kept the normal soft local load wait above.
- heavy_mode: none.
- waiver_ref: none; gascity has no waiver path.
- ci_lane_run: n/a (no CI configuration changed).

## Failure attribution

failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy.

The untouched probe fails at gc_live_contract_test.go:1509 with HTTP422, required query.assignee missing, want200. Clause1: official --site UNCHANGED, shared_hunks_in_file=0, and not one of the three owned tests. Clause2: the open condition tracker predates this run; the read-back-verified sighting is comment3752cb5b-f7b1-5ae6-9eb4-e69526a98ec5. Clause3(b): the retained full deploy gate for our merged PR8189 has the exact same file/line and error; that diff changes only test/conversationlane/x2_test.go and shares no path with this source. PR8189 checks were also read; the failure comparison is its retained full deploy-gate log, not an assertion that PR CI failed. Clause4: no failing-package overlap.

Repeat-signature exception: this deploy's build ga-rxg8786 is stamped gc.fixes_tracker=ga-zt0ulg3; the blocking condition's designated fix ga-ze0aym0 is stamped gc.fixes_tracker=ga-od4rcxy and is unlanded. Its resolved source f39fa5525a3a947a8e5f28548e3087cf73fcf2e1 is not in the pinned base, the fixture fix is absent there, and its work outcome is blocked. The tracker remains open until actual landing.

Raw execution.json remains exit_code1/criterion3_valid=false with its digest retained in raw-execution.sha256. The original four phase records and their raw failure were not edited. Separate attributed-completion.json reports completion of only the previously unrun latency/native floor; failure-attribution.json records the proof. This satisfies criterion3a without a waiver or failed-test retry.

## Fast lane

Full build/nogo, fmt-check (pin2.12.0, resolved2.12.0, mismatch=false), full nogo policy, active .githooks, four shell guards, and workflow Python policy checks passed. make bazel-sync then git diff --exit-code and an empty untracked-inclusive status passed. Format, policy and drift each used an independent fresh-tree-run view with the same scratch commit; the suite's scratch was untouched. GATE_BASE_RECORD/FRESH_TREE_RECORD/GLT_RECORD lines and full output are retained in fast/units/gate.w1.*.log.

## Supplemental regression proof

red.log/red-proof.json: restoring only chat.go/store.go to the pinned base makes each owned regression fail at its intended assertion (unlocked heal; awake instead of suspended; busy row writes). probes/units/gate.w1.stdout.log/race-proof.json: each owned test PASS20/FAIL0/SKIP0 under the race configuration, no DATA RACE. Nogo was independently verified in the normal build/policy/full suite; the separate race configuration uses --norun_validations to avoid the recorded race/nogo analyzer tool clash.
