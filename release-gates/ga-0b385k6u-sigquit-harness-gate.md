# SIGQUIT handling for the test-shard runner

**Verdict:** **PASS**

This gate authorizes PR-open. One unrelated latency failure is attributed under
criterion 3a; its failed verdict is retained. The 24-hour production observation
is deferred, explicitly not met and not waived.

- Deploy: `ga-0b385k6u`; source build `ga-5kse5n`; review `ga-b0mhhh5h`.
- Reviewed source: `c3e8f53fff9cfb39fccdea39b83921dd84d78b37`.
- Gate base: `33a23bb8edb2ebdd808eaf4bf2bb0a97282669d6` (`origin/main`, pinned at intake).
- Canonical merge: `3b7fe987a0bcd4dffeb05b00d4bd824da306db46`; tree `d3bc3afd919555556358f2739fa62659ca2b5e2c`.
- deploy_mode: remote; push_remote: fork; waiver_ref: none; heavy_mode: none.
- merge_state: clean (origin/main@33a23bb8edb2).
- gate_base_kind: current-tip; gate_base_sha: 33a23bb8edb2ebdd808eaf4bf2bb0a97282669d6.
- merge_validation: current-tip merge result built/vetted; merge authority verifies its eventual merge result and current CI. This record grants no merge authority.
- Evidence root (retained, .gc-no-reap): `/var/tmp/gc-gate-ga-0b385k6u-20261112.6KC2ZAyi`. Paths below are relative to this root.
- `docs/PROJECT_MANIFEST.md` is absent. Release criteria follow the deployer contract, TESTING.md, Makefile and the merged CI workflows.

| # | Result | Evidence |
|---|---|---|
| 1 Review PASS | PASS | Reviewer ga-b0mhhh5h, verdict pass on the exact reviewed source, read live. No carryover or reviewer-added test substitution. |
| 2 Acceptance | PASS | SIGQUIT handlers installed in both runner and supervised bash subshell; live handler and exit-131 behavior pinned by the owned test's two subtests. test-integration-shard has no separate trap/harness caller gap. Source item 4 is scoped to ga-n7c68q. Item 5 is DEFERRED to ga-706t0nlk by mayor mail gcg-7294977, NOT met/waived: real main landing plus 24h required. |
| 3 Tests | PASS (one attributed failure) | All five fresh remote phases, all four owned tests PASS, native floor 16/16 exit 0, fast lane and six CI leaf accounts below. Raw latency FAIL retained; no retry or waiver. |
| 4 Review findings | PASS | Live review records no unresolved HIGH findings; style/security/spec verdict pass. |
| 5 Clean branch | PASS | Canonical scratch clean; deploy branch contains reviewed source plus this gate record only. Publication verifies clean status after the normal pre-commit hook. |
| 6 Build/merge | PASS | Independently recomputed canonical merge tree and both parents. Whole-tree bazel build and make vet (alias of lint/nogo) successful BEP terminals. One initial DNS/BEP-upload exit 38 recovered once on the identical tree; both records retained, no test retry. |
| 7 One theme | PASS | Four source commits, three paths: scripts/harness_reap_test.go, scripts/lib/harness-reap.sh, scripts/test-go-test-shard. All implement/test the SIGQUIT runner disposition; no internal docs/config contamination. Ancestry scope verified with the source/review lineage IDs. |

## Tests and policy

test_cmd: service-detached gate-criterion3.sh --repo MERGE_SCRATCH --base GATE_BASE_SHA --bead-json criterion3-bead.json --owned owned-tests.tsv --run-dir c3-run/evidence; GC_GATE_REMOTE_ENABLED=1. Exact argv retained in c3-run/cmd.argv.
test_cmd_scope: full-suite. Unit //..., acceptance, all integration-tagged packages, full integration excluding registered latency tests, then only the registered latency tests in their dedicated lane. All phases force --nocache_test_results --noremote_local_fallback; exact commands/BEPs retained per phase.

| Phase | Remote shard results | Verdict |
|---|---:|---|
| unit | 411 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | FAIL: attributed below |

test_exec_mode: remote-exec (derived from BEP); test_exec_bep: 537/537 remote, zero cached-locally/cached-remotely/other-strategy/retried, all attempt 1, in-window, terminal. All 1074 CAS artifact digests independently reverified.
farm_test_seconds: 16015.313; rbe_state: enabled, kill switch absent, client cert valid to 2027-01-09, worker-env digest matches main (54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72).
fallback_reason: none; admission: remote, 25 GiB floor, cap 2, free 35.629 GiB, waited 0.001s. Native floor has local-heavy/load admission; CI windows-lane has local-heavy/40 GiB admission.
test_counts: farm 152756 PASS / 1 FAIL / 576 SKIP; native floor 24 PASS / 0 FAIL / 14 SKIP; combined 152780 PASS / 1 FAIL / 590 SKIP. Counts are top-level result lines across jobs, not unique tests; no source rerun.
test_log_dir: c3-run/evidence/remote/logs (553 retained farm/floor logs); native originals c3-run/evidence/remote/floor-logs. Read with gate-test-evidence.py; final-evidence-audit.json and farm_and_floor-by-name.txt retained.
diff_tests_executed:

- `TestGoTestShardReapsTestTreeWhenTheRunnerIsTerminated`: PASS (fresh unit phase, by name).
- `TestGoTestShardTerminationLeavesNothingHoldingTheCallersPipe`: PASS (fresh unit phase, by name).
- `TestGoTestShardTrapsQuitInsteadOfCoreDumping`: PASS (fresh unit phase, by name).
- `TestGoTestShardWatchdogKillsARunThatDefeatsGoTimeout`: PASS (fresh unit phase, by name).

policy_lane: PASS, fast-policy.bep.json plus verified C6 make vet/lint. Format check; shell guards including hook ownership/residency/routed rows; docsync, Go formatting, release/policy/native-boundary/analytics/CI suite coverage targets. Make check's full Bazel test tree and shell guards are covered.
drift_lane: PASS; fresh-tree-run.sh outermost for each phase, gate-base.sh pinned base. make bazel-sync followed by git diff --exit-code and empty git status including untracked files; schema/spec/client/dashboard/lockfile sync Bazel targets; OpenAPI breaking check. FAST_LANE_PASS markers and successful BEP terminals re-audited for all three phases before suite launch.
ci_lane_run: n/a (no CI-config diff).
skip_inventory: c3-run/evidence/remote/skip-inventory.json, 576 farm entries (90 environment-absence, 486 other). No scripts-owned/package SKIP. Every environment-absence package was selected by parent test for the native floor; all 16 steps exit 0. Remaining unrelated SKIPs cover explicit platform, fixture/provider, optional live-lane and pre-existing contract guards. Their reasons are retained; changed harness inputs cannot reach these skipped packages.
floor_topup: complete; c3-run/evidence/remote/floor-topup.json. Includes beadstest, Darwin compile, both runner self-tests and all environment-absence tests. Remaining 14 top-level SKIPs: optional replay/coverage fixtures, br, live herdr lane and inside-tmux keybindings. These are unrelated to the scripts change; no floor FAIL.
owned_topup: none needed, all four owned tests PASS in the fresh unit phase.
latency_leg: real clear-probe FAIL, never a deferral: prime fastest-of-7 1059ms > unchanged 1000ms. Bd, drain and mail budgets PASS. Probe clear, load1=18.95/load5=11.30/load15=15.01, 32 CPUs, threshold 50, waited 0s. Raw execution.json stays criterion3_valid=false/exit 1, SHA256 413bc77c17652019622734766f190d4c74bf9b4a190a0076a7bc2c3c6c6a8e2a.
failure_attribution: TestGcSeatHookLatencyBudget -> ga-2o854cfm; clause 1 UNCHANGED at gc_seat_hook_latency_test.go:183, no shared hunks; clause 3(a) MECHANISM proven by 195-directory integration/external-gc import closures and fixture subprocess inspection, neither changed harness script invoked; clause 4 disjoint package. Discovering-run tracker accepted under shared non-diff-owned-gate-failure gm-sf3238 with landed proof and clauses 1/4 clear. Tracker is not claimed to predate this run; attribution is not a waiver or a raw PASS. Evidence latency-mechanism-proof.json and latency-import-closure-reverified.txt, tracker comment read back.
load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: 1; native-floor threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=49.58 run_max_load=78.89 run_mean_load=64.50 run_readings=11 wait_first_load=19.99 wait_max_load=78.77 wait_mean_load=49.02 wait_readings=61 read_errors=0.
CI windows-lane load_waited_seconds: 1800; load_wait_timed_out: 1; LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=55.50 run_max_load=52.86 run_mean_load=50.65 run_readings=3 wait_first_load=82.61 wait_max_load=100.19 wait_mean_load=78.20 wait_readings=62 read_errors=0 prewait_s=1800.
GATE_ICU74: verdict=OK, recorded host-loader sonames checked; original lines/digests in icu74.txt.

## Required CI accounting

ci_required_closure: python3 packs/actual/deployer/scripts/ci_required_triggers.py MERGE_SCRATCH GATE_BASE_SHA DEPLOY_SHA fork; exit 0; ci.yml blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; "6 LEAF jobs need a status". origin/fork closures match. Aggregator check takes no status.

| Job | Status | Command/result/log |
|---|---|---|
| runner-policy | LOCAL-PASS | Exact runner_policy.py command, EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH=; exit 0; ci-required/runner-policy.out and runner-policy.log. Outputs Linux Blacksmith runners and blacksmith-4vcpu-windows-2025. |
| changes | LOCAL-PASS | ci_required_triggers.py, fork; exit 0; ci-required/closure-fork.txt. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: NON-LINUX, NOT-TRIGGERED, if needs.changes.outputs.credential_provider == 'true'. |
| pack-gate | NOT-TRIGGERED | Tool row: linux, NOT-TRIGGERED, if needs.changes.outputs.packs == 'true'. |
| windows-lane | LOCAL-PASS | Exact four run steps, including tools/nogo working directory, in ci-windows-lane.sh; exit 0; ci-windows-run/RESULT and units/gate.w1.{stdout,stderr}.log. No job/step env values are declared. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Windows PowerShell: go test -count=1 @(Get-Content .github/windows-lane/test.txt). Separate Windows test import closure computed for its own allowlist; 9 module directories, exit 0, no changed input reaches it. |

windows_reach: none, separately computed for windows-lane-test's credentialprovider/genshell test allowlist under GOOS=windows GOARCH=amd64, plain and integration tags, exit 0 (ci-required/windows-lane-test-reach.json). The credential-provider-only helper also exits 0/empty but is not reused for this different job.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.

Merge-ready requires CI / required green on the PR head and windows-lane-test success. The merge authority owns that check. Observation ga-706t0nlk must be armed from the actual main merge time plus 24h; baseline 103 bash cores at 2026-11-12T22:18Z is pre-deploy evidence only.
