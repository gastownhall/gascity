# Live-contract active-step assignee release gate

**Verdict:** **PASS**

bead: ga-q422865
build_bead: ga-ze0aym0
review_bead: ga-0dzqy1p
reviewed_commit: f39fa5525a3a947a8e5f28548e3087cf73fcf2e1
gate_base_sha: 913ba5adb176dde0fe6b9a04e949a1b6f0036a4d
gate_base_kind: current-tip
merge_head: 4f21ce6c42e99566655d593c7fab3f9c28b994bc
merge_tree: 159a5c5a9e9aceffc002de85614f47b1668e081f
merge_state: clean (origin/main@913ba5adb176dde0fe6b9a04e949a1b6f0036a4d)
deploy_mode: remote
evaluated_at: 2026-10-24T20:59:19.357746+00:00
evidence_dir: /var/tmp/deploy-ga-q422865-reread.r7k8rapg

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed ga-0dzqy1p style/security/spec PASS at the resolved reviewed source; no reviewer-added tests or carryover substitution. |
| 2 | Acceptance criteria met | PASS | The active-step route remains in discovered GET probes; fixture session identity goes through url.Values.Set/Encode. New special-character regression PASS; real TestGCLiveContract_BeadsAndEvents and its active-step subtest PASS. Existing missing/blank-assignee and client-refusal validation PASS by name. No production API contract changed. |
| 3 | Full-scope tests and required lanes | PASS | All five canonical phases plus the mandatory native floor completed. The original unrelated unit failure is retained and attributed under the four clauses below; no candidate test retry and no waiver. All diff-owned tests PASS. |
| 4 | No unresolved HIGH review findings | PASS | Recorded style/security/spec review has none. |
| 5 | Branch clean | PASS | Starting assigned checkout and final frozen merge scratch are clean; final branch contains the reviewed source plus this gate record only. Git commit runs normal hooks. |
| 6 | Merge state and build/vet | PASS | Exact frozen clean merge tree verified; all-target remote Bazel build/nogo, vet and active Git hooks PASS before the independent fast views. |
| 7 | Single feature theme | PASS | Two source commits, one existing integration test file, +33 lines: the active-step fixture query remedy and its regression. Accepted ancestry ids are this deploy, ga-ze0aym0 and ga-od4rcxy only. |

## Full-scope execution

test_cmd: canonical gate-criterion3.sh, followed only by the unrun canonical phases through gate-rbe-run.py after proof of the unrelated unit failure; original unit output is immutable
test_cmd_scope: full-suite
test_counts: 126131 PASS, 2 FAIL, 370 SKIP (top-level terminal lines; repeated jobs count separately; parent/subtest FAIL lines describe one attributed condition)
remote_test_counts: 126110 PASS, 2 FAIL, 356 SKIP
native_floor_counts: 21 PASS, 0 FAIL, 14 SKIP, plus 3 non-Go compile/runner checks exit 0
actual_remote_results: 497
test_exec_mode: remote-exec (BEP-derived; mandatory native floor recorded separately)
test_exec_bep: /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/unit.bep.json; /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/acceptance.bep.json; /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/integration-packages.bep.json; /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/integration-full.bep.json; /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/integration-latency.bep.json; each phase's evidence.json records remote strategy, no cached flags, attempt 1, start within phase window and terminal verdict
farm_test_seconds: 9928.786
heavy_mode: none
waiver_ref: none
ci_lane_run: n/a (no CI config change)
owned_topup: [] (owned test already PASS in the integration-full output)
floor_topup: /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/floor-topup.json; complete=true, 16/16 steps exit 0, including beadstest, Darwin compile and both runner self-tests
latency_leg: /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/latency-leg.json; complete=true, both latency tests PASS under the farm probe
skip_inventory: /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/skip-inventory.json; 356 remote rows (71 environment-absence, 285 other); native residual SKIPs retained in floor logs
skip_justification: no diff-owned SKIP. Every environment-absence row was selected for the canonical native package top-up. Remaining rows are unchanged platform, opt-in, tool/fixture or documented conformance exclusions; all original reasons are inventoried. No skipped site is in the changed gc_live_contract_test.go; the changed helper is called only by that file's read-probe path, whose regression and real sweep PASS. No changed non-test input reaches another test package. Native residual non-owned SKIPs remain recorded, not counted as PASS.
test_log_dir: /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/logs (513 retained per-job logs, 497 remote plus 16 native/check logs); original floor copies also remain in /var/tmp/deploy-ga-q422865-reread.r7k8rapg/c3/evidence/remote/floor-logs
diff_tests_executed: TestLiveContractReadProbes_ActiveStepCarriesAssignee PASS in integration-full-8e315f95edb09e5631cb.log
required_live_sweep: TestGCLiveContract_BeadsAndEvents and GET_/v0/city/{cityName}/work/active-step PASS in integration-full-65412ec6e874f54aa912.log
existing_validation: TestWorkActiveStepStatuses/no_assignee_is_rejected, /a_blank_assignee_is_400 and TestActiveWorkStepClientRefusesNoAssignee PASS; assignee-validation-by-name.json retains the exact log names

| Canonical phase | Fresh remote results | Farm test seconds | Verdict |
|---|---|---|---|
| unit | 373 | 2824.94 | original FAILED verdict retained; attributed below |
| acceptance | 24 | 1245.06 | PASS |
| integration-packages | 85 | 3360.427 | PASS |
| integration-full | 14 | 2408.429 | PASS |
| integration-latency | 1 | 89.93 | PASS |

The complete command vector and flags are retained in each phase's command/stdout/BEP artifacts. All phases use --nocache_test_results and --noremote_local_fallback. The original unit run covered all //... targets. Only its unfinished successors were continued, on the identical frozen commit/tree and canonical runner; no failed test or phase was rerun. continuation-result.json records exit_code=0, original_unit_unchanged=true, unit_rerun=false. All 994 raw remote artifacts were independently SHA256-verified.

Original execution.json remains criterion3_valid=false because it honestly describes the stopped original unit invocation. This gate's PASS depends on the documented independent attribution and the completed continuation, never on rewriting that failed verdict into a green one.

rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
admission_initial: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 44.072715759277344, "waited_seconds": 0.001}
admission_continuation: {"class": "remote", "floor_gib": 25.0, "free_gib": 55.048614501953125, "waited_seconds": 0.001, "cap": 2}
fallback_reason: none; no local replacement suite was stitched to remote results

## Failure attribution: ga-byumbdq

failure_attribution: TestExternalReadsResumeServesNoPreSuspensionScaleCheck/straddle=false (and its parent FAIL line) -> ga-byumbdq; proof (a), mechanism

1. Not diff-owned: cmd/gc/allocator_scalecheck_lane_test.go:438 is UNCHANGED; the parent body is untouched and shared_hunks=0. Candidate changes only test/integration/gc_live_contract_test.go.
2. The open gate-tracker ga-byumbdq was read live and names this exact initial fixture failure, before suspend/resume: “before suspending: ok not trusted.” It was filed in this discovering round; gm-sf3238's timing exception applies because clauses 1/4 and a decisive proof (a) were established before filing. No host-load cause or deadline-cause is asserted.
3. Fresh import closure of the failing default-tag cmd/gc test binary: 178 module package directories, no changed non-test input. That binary cannot load the integration-tagged fixture. This is a landed mechanism proof, not a passing serial retry. Full proof: unit-failure-proof.json and retained closure/site evidence.
4. No package/path overlap with the changed integration fixture; no census bump, new target/file, sleeps, process/listener or parallelism. The added regression performs synchronous URL/map construction.

The original FAILED unit BEP, stdout, artifact hashes and both FAIL lines remain intact. No waiver_ref, selective retry or failure erasure was used. This is this condition's first discovering round, not a repeat signature.

## Fast lanes, isolation and load

policy_lane: PASS; pinned lint/format, all-target nogo/vet, shell guards and all required policy tests in a separate fresh view
drift_lane: PASS; CI's make bazel-sync followed by git diff --exit-code in another fresh view; generated schema/spec/client/dashboard/lockfile checks in their own fresh view
runtime_setup: ICU74 check PASS; podman socket active; cached pinned Dolt image matched deps.env

```text
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-q422865-reread.r7k8rapg/merge head=4f21ce6c42e9 tree=159a5c5a9e9a dir=/var/tmp/gc-fresh-tree.5RjxEe modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=913ba5adb176dde0fe6b9a04e949a1b6f0036a4d knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.5RjxEe
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-q422865-reread.r7k8rapg/merge head=4f21ce6c42e9 tree=159a5c5a9e9a dir=/var/tmp/gc-fresh-tree.nnXtQ5 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-q422865-reread.r7k8rapg/merge head=4f21ce6c42e9 tree=159a5c5a9e9a dir=/var/tmp/gc-fresh-tree.J3cnpy modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=104.58 run_max_load=125.48 run_mean_load=119.93 run_readings=12 wait_first_load=106.66 wait_max_load=117.95 wait_mean_load=107.60 wait_readings=61 read_errors=0
```

The native floor's bounded local wait timed out after 1802 seconds, then proceeded under the documented soft-wait rule; all native steps returned 0. Remote admission used no host-heavy slot/load wait. Farm latency probe and both latency results are retained in latency-leg.json.

Finalization waited for the optional footprint sampler's filesystem read; it exited naturally, the continuation returned 0, and all retained unit hashes still matched. No process was killed, no test was rerun and no verdict was bypassed. Raw evidence and the original failed record remain preserved.
