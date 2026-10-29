# Certified-stop process-reap wait release gate

**Verdict:** **PASS**

bead: ga-10nkls9
build_bead: ga-ft7tn96
review_bead: ga-lmgbfge
reviewed_commit: 67785b63cb57eec93bcfe44bcce0024a38d78779
gate_base_sha: 8279266a5707684e5558563b12e33c95f9d75e07
gate_base_kind: current-tip
merge_head: 817158f5c640c0879693570cf511d4140920462b
merge_tree: 1daf6fa4e81f05583ebf8a62314b92b6c5613274
merge_state: clean (origin/main@8279266a5707684e5558563b12e33c95f9d75e07)
deploy_mode: remote
evaluated_at: 2026-10-24T22:09:18.183790+00:00
evidence_dir: /var/tmp/deploy-ga-10nkls9.o8j0t6bv

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed ga-lmgbfge records style/security/spec PASS on the resolved reviewed source. No reviewer-added tests or carryover substitution. |
| 2 | Acceptance criteria met | PASS | Only adapter_test.go changes, +7/-3. The existing stubborn pane is snapshotted with PID/start identity before certified stop; the existing identity-bound helper waits with testutil.ExecRaceTimeout. The real named-tmux boundary test PASSes in the fresh tagged suite. Source inspection confirms a surviving identity still reaches the helper's fatal deadline; review's independently recorded no-KILL mutant fails that assertion. No production code or cleanup contract changed. |
| 3 | Full-scope tests and required lanes | PASS | All five canonical remote phases and all16 mandatory native checks completed. The original unrelated active-step query failure is retained and attributed under all four clauses below, without candidate retry or waiver. The diff-owned test PASSes by name. |
| 4 | No unresolved HIGH review findings | PASS | Style/security/spec notes have none. |
| 5 | Branch clean | PASS | Assigned checkout and frozen merge scratch verified clean. Final isolated deploy contains the reviewed source plus this gate record only; normal commit hooks run. |
| 6 | Merge state and build/vet | PASS | Clean merge on frozen base above. Full remote Bazel build/nogo //..., tagged tmux test compilation, make vet and active Git hooks passed before the fast lanes. Main may advance; merge authority validates the eventual merged tree. |
| 7 | Single feature theme | PASS | One source commit and one existing integration test file, solely the certified-stop reap-race remedy. Accepted ancestry: this deploy, ga-ft7tn96 and ga-xylwy8v. |

## Full-scope execution

test_cmd: canonical gate-criterion3.sh; after proof of the unrelated integration failure, only the never-run latency leg and complete native floor continued through canonical gate-rbe-run.py
test_cmd_scope: full-suite
test_counts: 126228 PASS, 1 FAIL, 370 SKIP (top-level terminal lines; repeated jobs count separately)
remote_test_counts: 126207 PASS, 1 FAIL, 356 SKIP
native_floor_counts: 21 PASS, 0 FAIL, 14 SKIP, plus3 non-Go compile/runner checks exit0
actual_remote_results: 497
test_exec_mode: remote-exec (BEP-derived; mandatory native floor recorded separately)
test_exec_bep: /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json; each evidence.json verifies strategy remote, neither cached flag, attempt1, a start inside the phase window and a terminal verdict for every result
farm_test_seconds: 11107.247
heavy_mode: none
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
owned_topup: [] (the changed test already PASSed in integration-packages)
floor_topup: /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/floor-topup.json; complete=true, 16/16 steps exit0, including beadstest, Darwin compile and both runner self-tests
latency_leg: /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/latency-leg.json; complete=true, measured under clear farm probe, both named timing tests PASS on attempt1
skip_inventory: /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/skip-inventory.json; 356 remote rows, 71 environment-absence and285 other; native residual14 SKIPs retained in floor logs
skip_justification: no diff-owned SKIP. The canonical floor selected every environment-absence SKIP, selecting parents for subtests. Residual exclusions are unchanged platform, opt-in, tool/fixture or documented conformance cases with their printed reasons preserved. Ownership explain shows both modified hunks lie solely in the PASSing boundary test and shared_hunks=0, so other unchanged skips in adapter_test.go do not reach them. No production input or shared helper changed. Native residual SKIPs remain recorded, never credited as PASS.
test_log_dir: /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/logs (513 retained logs: 497remote plus16native/check); original floor copies in /var/tmp/deploy-ga-10nkls9.o8j0t6bv/c3/evidence/remote/floor-logs
diff_tests_executed: TestProviderStopUnattendedSessionRealNamedTmuxBoundary PASS in integration-packages-15c091b746137e5c2921.log

| Canonical phase | Fresh remote results | Farm test seconds | Verdict |
|---|---|---|---|
| unit | 373 | 3339.338 | PASS |
| acceptance | 24 | 1174.584 | PASS |
| integration-packages | 85 | 3530.363 | PASS |
| integration-full | 14 | 2985.105 | original FAILED retained; attributed below |
| integration-latency | 1 | 77.857 | PASS |

All phases force --nocache_test_results and --noremote_local_fallback. Full command vectors, stdout and BEPs remain in each phase's artifacts. All994 raw remote artifacts were independently SHA256-verified. continuation-result.json records original_evidence_unchanged=true and candidate_test_rerun=false. Every hash of the original four phases and execution.json still matches retained-phases-sha256.json.

Original execution.json remains exit_code=1 and criterion3_valid=false, honestly recording the original stopped chain. PASS here depends on documented attribution plus completion of the unrun successors, never rewriting that failure. No farm/local replacement suites were stitched.

rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
admission_initial: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 50.93678283691406, "waited_seconds": 0.001}
admission_continuation: {"class": "remote", "floor_gib": 25.0, "free_gib": 53.692962646484375, "waited_seconds": 0.002, "cap": 2}
fallback_reason: none

## Failure attribution: ga-od4rcxy

failure_attribution: TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step (and its parent) -> ga-od4rcxy; proof(a), mechanism

1. Not diff-owned: test/integration/gc_live_contract_test.go:1512 UNCHANGED, own body untouched and shared_hunks=0. Only internal/runtime/tmux/adapter_test.go changes.
2. The condition tracker was opened and read, predates this run (created2026-10-19), and names the same422 response from a missing required assignee. The sighting comment was read-verified before continuation.
3. The failing integration test binary's integration-tagged import closure was freshly computed at the frozen merge:63 module package directories, no changed non-test input. It cannot compile or execute the separate tmux test file. This is a landed structural proof, not an inconclusive passing retry. integration-failure-proof.json and raw closure/site evidence retain the proof.
4. No package/path overlap, new target/file/census/parallelism or added fixture. The existing assertion becomes an identity-bound wait.

At attribution time, this was fix-carrying: own build ga-ft7tn96 is stamped gc.fixes_tracker=ga-xylwy8v, and blocking fix ga-ze0aym0 was CLOSED/blocked and unlanded. Both all-status live records are retained with that proof. Its active-step remedy subsequently landed as PR#8276 at22:03:30Z while this gate's remaining native checks settled. The tracker is now closed with landed evidence; that does not change the immutable result on this earlier frozen base. A new sighting after that fix would be a new condition. No claim that the remedy remains unlanded is made.

## Fast lanes, isolation and load

policy_lane: PASS; pinned integration-tagged changed-package lint, fmt-check, full nogo/vet, shell guards and required policy targets in an independent fresh view
drift_lane: PASS; CI's make bazel-sync then git diff --exit-code in another fresh view; generated schema/spec/client/dashboard/lockfile checks in their own fresh view
runtime_setup: ICU74 recorded libraries verified; podman.socket active; cached Dolt image matches deps.env

```text
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-10nkls9.o8j0t6bv/merge head=817158f5c640 tree=1daf6fa4e81f dir=/var/tmp/gc-fresh-tree.krBHLe modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=8279266a5707684e5558563b12e33c95f9d75e07 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.krBHLe
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-10nkls9.o8j0t6bv/merge head=817158f5c640 tree=1daf6fa4e81f dir=/var/tmp/gc-fresh-tree.hMoFH5 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-10nkls9.o8j0t6bv/merge head=817158f5c640 tree=1daf6fa4e81f dir=/var/tmp/gc-fresh-tree.ig9zQk modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=68.38 run_max_load=74.53 run_mean_load=66.25 run_readings=11 wait_first_load=131.64 wait_max_load=132.51 wait_mean_load=78.79 wait_readings=61 read_errors=0
```

The native floor's bounded local wait timed out after1801s, then proceeded under the documented soft-wait rule; all16 steps returned0. Remote admission used no host-heavy slot/load wait. The latency probe was clear. No failed candidate test was rerun and no process was killed.
