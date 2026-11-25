# Drained named holder routed-demand regression gate

**Verdict:** **PASS**

Date: 2026-11-24. Deploy bead: ga-aq1wf7tc; build: ga-lr64hm; review: ga-17zyjzi8.
Reviewed source: `f3476c96756d8f55a204d98b82dfb465709161e0`. Branch provenance: `builder/ga-lr64hm`.
Gate base: `f0ff37e1caa178692aacf72e90bd730b7e82ab1f`; validated merge: `c262f19c184feed0204a4ab0e8a51c448d0205d8`.
Evidence root: `/var/tmp/gc-gate-ga-aq1wf7tc-20261124.wjd5mqaC` (retained, protected by `.gc-no-reap`).

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-17zyjzi8 records `verdict: pass` for the resolved reviewed source; no carryover or reviewer-added commits. |
| 2 | Acceptance | PASS — both persisted drained shapes wake the canonical named holder exactly once for routed, unassigned demand; the asleep control and six named neighbor tests pass in unit and integration runs. Existing routed-demand exemption remains present; only the new test and BUILD registration change. |
| 3 | Tests | PASS — full suite and native floor complete, zero failures; required-job accounting below. |
| 4 | Review findings | PASS — reviewer style/security findings are none, no unresolved HIGH findings. |
| 5 | Clean branch | PASS — reviewed source is clean; this gate record is the sole deployment addition and is committed before publication; final status is verified clean. |
| 6 | Merge/build/vet | PASS — clean scratch merge; `bazel build //...` and `make lint` exit 0 (`c6-build.log`, `c6-vet.log`; detached C6 completed). |
| 7 | Single theme | PASS — one test-only session reconciliation regression: 224 test lines plus its single BUILD entry. |

merge_state: clean (origin/main@f0ff37e1caa178692aacf72e90bd730b7e82ab1f)
gate_base_kind: current-tip
merge_validation: merged result built and tested at the recorded base; MPR owns the merge-time check if the base moves.

## Test evidence

test_cmd: `gate-criterion3.sh` on the recorded merge, five `gate-rbe-phase.sh` phases: `make check` (`bazel test //...` plus shell guards), acceptance, integration-packages, full integration excluding latency tests, then the dedicated latency leg. Exact argv, phase commands and BEPs are in `c3-run/cmd.argv` and `c3-run/evidence/execution.json` under the evidence root.
test_cmd_scope: full-suite
test_counts: 159166 PASS, 0 FAIL, 618 SKIP (top-level lines summed across jobs; tests run in multiple jobs count per job).
diff_tests_executed: `TestDrainedNamedHolder_RoutedDemandWakesHolder_GaQbbnr9` PASS, including both drained subtests; `TestAsleepNamedHolder_RoutedDemandWakesHolder_Control_GaQbbnr9` PASS. Both pass in unit and integration-packages. `diff-owned-tests.py --explain` confirms the two owned functions.
Named acceptance neighbors: routed-demand wake, on-demand named singleton wake, asleep singleton regression, and all three canonical-alias holder tests PASS in both lanes (retained logs, verified by `gate-test-evidence.py`).
test_log_dir: `/var/tmp/gc-gate-ga-aq1wf7tc-20261124.wjd5mqaC/c3-run/evidence/remote/logs`
test_exec_mode: remote-exec (BEP-derived; local floor recorded separately).
test_exec_bep: five phase JSON/BEP pairs under `c3-run/evidence/remote`; 554 results executed remotely, zero cached locally/remotely, zero other strategies, zero retries, every phase window valid.
rbe_state: `c3-run/evidence/rbe-state.json`: enabled, certificate valid, tree and origin/main worker-env pins agree.
fallback_reason: none
admission: remote class, floor 25 GiB, 51.53 GiB observed free, cap 2; local floor uses host load admission.
farm_test_seconds: 13222.071
latency_leg: measured, first attempt, probe clear (load1 43.97, threshold 50 on 32 CPUs); both budget tests PASS; neither ran in the main integration leg.
floor_topup: all 16 native steps exit 0; complete=true in `remote/floor-topup.json` (environment-absence package checks, beadstest, Darwin compile and runner self-tests).
owned_topup: no repair needed; owned tests already PASS remotely.
skip_inventory: `remote/skip-inventory.json` retains 604 farm skip entries (514 other, 90 environment-absence); final union has 618 top-level SKIPs including native floor.
skip_justification: no owned test skips. Existing lane selection, optional provider capabilities, platform requirements and opt-in/live infrastructure account for the unrelated skips; native floor checks every environment-absence package. Residual native skips include optional live herdr, unavailable PostgreSQL, optional br, and unsupported split-reference cases. The added helpers are private to the new passing tests and cannot change these skip conditions.
failure_attribution: none (zero FAIL)
waiver_ref: none (no gascity waiver path)
heavy_mode: none
ci_lane_run: n/a (no CI configuration change)
delta_gate: shadow/full, reason not-gate-pr; no narrowing applied.

## Fast lane and load

policy_lane: PASS — six Bazel format/policy/guard targets plus `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary`; `fast-policy.log`, exit 0.
drift_lane: PASS — `make bazel-sync` then `git diff --exit-code`; `fast-drift.log`, exit 0.
Each fast lane used its own `fresh-tree-run.sh` checkout at the recorded merge; FRESH_TREE_RECORD shows modified=0, untracked=0. Neither wrote the suite tree.
load_threshold: 15
load_waited_seconds: 150
load_wait_timed_out: 0
run_start_load: 14.75
run_max_load: 16.26
run_mean_load: 15.25
run_readings: 7
These load fields are the native floor's LOAD_GATE_SUMMARY. Remote driver monitoring separately observed start 24.09, max 25.82, mean 18.53, 44 readings; remote execution has no host-load admission. Cross-compile admission waited 0s, start 14.74, max 17.28, mean 16.44, 4 readings.

## Required CI accounting

ci_required_closure: `python3 packs/actual/deployer/scripts/ci_required_triggers.py <merge-scratch> f0ff37e1caa178692aacf72e90bd730b7e82ab1f f3476c96756d8f55a204d98b82dfb465709161e0 fork`, exit 0; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. Full output: `ci-required/closure.stdout`.

| LEAF job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | Workflow script with EVENT_NAME=pull_request, PR_AUTHOR=quad341, FORCE_BLACKSMITH empty; exit 0; `ci-required/runner-policy.*`. Linux and Windows runner outputs resolved explicitly. |
| changes | LOCAL-PASS | Closure tool exit 0; packs, credential_provider, shared all no match. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: `if: needs.changes.outputs.credential_provider == 'true'`, NOT-TRIGGERED. |
| pack-gate | NOT-TRIGGERED | Tool row: `if: needs.changes.outputs.packs == 'true'`, NOT-TRIGGERED. |
| windows-lane | LOCAL-PASS | All four workflow steps reproduced verbatim by `ci-required/windows-lane.sh`: Windows allowlist build/test compile, Windows nogo build/test compile, whole-tree Darwin build/test compile. Detached, fresh checkout, load-gated, isolation wrapper; exit 0 in `ci-required/windows-lane-output.txt`. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Native Windows runner required. This job's own GOOS=windows test dependency closure computed from `.github/windows-lane/test.txt`, exit 0, neither changed path reachable; `ci-required/windows-reach.json`. |

windows_reach: none for windows-lane-test (its own closure, not the credential-provider helper).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

Gate PASS authorizes opening the PR. Merge requires CI / required green on the exact head and successful native Windows tests. Gate source, branch ancestry, record size, clean status and PR head are checked again before publication/clearance.
