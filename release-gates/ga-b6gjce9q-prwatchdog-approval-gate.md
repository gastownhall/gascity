# Prwatchdog fork workflow approval release gate

**Verdict:** **PASS**

PASS authorizes opening the PR. Merge requires `CI / required` and the deferred native Windows job to succeed on the PR head; MPR owns that decision.

Deploy bead: ga-b6gjce9q. Build: ga-3b1nr5. Review: ga-7wwix41 (PASS).
Evaluated: 2026-11-16T14:35:35.461255+00:00.
DEPLOY_MODE: remote; push_remote: fork; base_ref: origin/main.
Reviewed source: `1b712ad2c089856a4b4fd304463ee5ca8539b586`.
Canonical tested merge: `13be2c055f8763c5d81ceb3a98b6580117b6590c`; tree: `6811f7c928c8af75c07b2b8edb0a29d97bf74a30`.
gate_base: `3180bbc4d55c33b565c6a371f33e4faa0011688c`.
gate_base_kind: current-tip.
merge_state: clean (origin/main@3180bbc4d55c33b565c6a371f33e4faa0011688c).
merge_validation: canonical merge result built, vetted and tested here; merge authority validates the current PR merge.
Evidence root: `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ`.

| # | Result | Evidence |
| --- | --- | --- |
| 1 | PASS | Review PASS binds the exact source. reviewer_tests_verified: 2c3d61407f832cdade65088d124ffd607fa431a8 -> 1b712ad2c089856a4b4fd304463ee5ca8539b586; only scripts/prwatchdog/cmd/watchdog/main_test.go, +31/-0, independently verified by reviewer-added-tests.sh. |
| 2 | PASS | AC1: held fork workflows name maintainer approval. AC2: unrelated absent checks retain the existing reason. AC3: action_required signal and controls are tested. Code inspection plus all12 owned tests below pass; pagination and lookup-error behavior are covered. |
| 3 | PASS | Full standard five-phase run, all fresh remote results, plus complete native floor and every ci-required leaf accounted below. No failure attribution or waiver. |
| 4 | PASS | Review has zero unresolved HIGH/blocker findings. Nonblocking follow-up remains ga-1dhakgx5. |
| 5 | PASS | Exact reviewed source checked out on isolated deploy branch; porcelain including untracked files was empty before adding this gate record. |
| 6 | PASS | C6 remote build //... and whole-tree make vet/nogo both rc0, 1496 targets. Canonical tree independently equals git merge-tree. build-rbe.log, vet-rbe.log. |
| 7 | PASS | Six source paths, one watchdog approval-diagnostic theme. Fresh ancestry scope guard passes for deploy/build/review IDs; no stacked PR or internal-doc contamination. |

## Test evidence

heavy_mode: none.
test_cmd_scope: full-suite.
test_exec_mode: remote-exec.
fallback_reason: none.
waiver_ref: none.
test_counts: 155070 PASS / 0 FAIL / 613 SKIP (top-level result lines per job, including repeats across jobs, over557 retained logs).
test_log_dir: `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/logs`.

The command was gate-criterion3.sh with --repo `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/merge`, --base `3180bbc4d55c33b565c6a371f33e4faa0011688c`, --bead-json `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/bead.json`, --owned `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/owned.tsv`, --run-dir `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence`, inherited GC_GATE_REMOTE_ENABLED=1 and remote floor25/cap2. Its phases were make -k check; Bazel acceptance including acceptance_solo_tests; integration_packages; full integration minus the latency list; and only that list in the dedicated probed latency lane. Exact phase flag records:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

| test_exec_bep phase | Fresh remote first-attempt results | Verdict |
| --- | --- | --- |
| unit | 415 | PASS |
| acceptance | 25 | PASS |
| integration-packages | 86 | PASS |
| integration-full | 14 | PASS |
| integration-latency | 1 | PASS |

All541 results have remote strategy, neither cache flag, attempt1, start inside the phase window and a terminal verdict. Cached locally/remotely=0; retries=0; other strategy=0. farm_test_seconds: 13510.838.
rbe_state: generated RC present; kill switch absent; certificate valid to 2027-01-09; worker-env tree and main both `sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72`.
admission: remote, floor25GiB, cap2, free67.319908GiB, waited0.001s; no local downgrade.
owned_topup: none; all12 owned tests passed in the full run.
floor_topup: complete, 16/16 steps rc0, including beadstest, environment-absence parents by package, Darwin fsys compile and both runner self-tests.
latency_leg: measured at attempt1; both named latency tests PASS. Farm probe waited50s, then clear at load1=48.02, threshold50 (32 CPUs).
skip_inventory: 599 classified entries (509 other, 90 environment-absence), retained in full execution.json. Unrelated platform, opt-in, capability and helper SKIPs remain inventoried; every environment-absence parent received the native top-up. None is owned by this diff or relabelled PASS.

Native floor load: load_threshold=15; load_waited_seconds=0; load_wait_timed_out=0; run_start_load=9.33; run_max_load=15.49; run_mean_load=12.86; run_readings=7. The farm run itself used remote admission without a host load wait; these floor measurements describe the local top-up. ICU74 precondition: GATE_ICU74 verdict=OK provenance=recorded.

```text
  TestEvaluate_AwaitingApprovalDoesNotRewriteOtherFailures PASS (unit-7da5fc5c40058205df45.log)
  TestEvaluate_CheckAbsentAwaitingApprovalNamesMaintainerApproval PASS (unit-7da5fc5c40058205df45.log)
  TestEvaluate_CheckAbsentWithoutPendingApprovalKeepsTestsNeverRan PASS (unit-7da5fc5c40058205df45.log)
  TestWatch_ApprovalLookupErrorIsReportedNotSwallowed PASS (unit-7da5fc5c40058205df45.log)
  TestWatch_CheckNeverRanChecksForPendingApprovalOnce PASS (unit-7da5fc5c40058205df45.log)
  TestWatch_CheckNeverRanWithoutPendingApprovalKeepsTestsNeverRan PASS (unit-7da5fc5c40058205df45.log)
  TestWatch_NoApprovalLookupUnlessCheckNeverRan PASS (unit-7da5fc5c40058205df45.log)
  TestFetchAwaitingApproval PASS (unit-c91b3883a6428ea958da.log)
  TestFetchAwaitingApproval_OnlyActionRequiredSuitesCount PASS (unit-c91b3883a6428ea958da.log)
  TestFetchAwaitingApproval_OtherErrorsAreReturned PASS (unit-c91b3883a6428ea958da.log)
  TestFetchAwaitingApproval_Paginates PASS (unit-c91b3883a6428ea958da.log)
  TestFetchAwaitingApproval_RateLimitIsTyped PASS (unit-c91b3883a6428ea958da.log)
GATE_TEST_EVIDENCE_RESULT ok: 12 diff-owned tests verified by name
```

Full execution.json retained: 6000 lines; /var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/criterion3-run/evidence/execution.json; SHA256 e0ca056faf1ffd78748e5d6e7904cfd2d9a590111aec7de9e9f50ddb796a57d2.
Full compact execution.json retained: 1 lines; /var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/execution.compact.json; SHA256 51a6a2c904c6d815a3fa63ecf14c94be20db53fece7cf432fcaed113bf68e170.
Full owned-test reader retained: 18 lines; /var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/final-by-name.txt; SHA256 dff6741fa530853303913134f3032a83714d8221d8ac338d0936ebce52c35d73.

## Fast lane

All lanes passed before runtime setup or the suite. Format: make fmt-check, pinned/resolved golangci-lint2.14.0, mismatch=false. Policy: C6 whole-tree nogo plus check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows and check-residency-boundary, rc0. Drift: CI's make bazel-sync then git diff --exit-code, rc0. Each lane ran in a distinct fresh view and contains its FRESH_TREE_RECORD/GATE_BASE_RECORD. Logs: format.log, policy.log, drift.log. known_red_precheck: proceed reason=nothing-known-red; stale tracker ga-1ptv4jey's guard passes.

The documented isolation wrapper was invoked; for this gascity tree it reports PASS-THROUGH on the disposable canonical scratch. Bazel test actions were verified as remote; native top-ups used the documented load-gated runner. No live runtime was deployed.

## Required CI jobs

ci_required_closure: ci_required_triggers.py `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/merge` `3180bbc4d55c33b565c6a371f33e4faa0011688c` `1b712ad2c089856a4b4fd304463ee5ca8539b586` fork; rc0; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs. Full account directory: `/var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/ci-required`. Runner policy resolves Windows to blacksmith-4vcpu-windows-2025.

| LEAF job | ci_required_accounting | Evidence within account directory |
| --- | --- | --- |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<account>/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; rc0; runner-policy.log |
| changes | LOCAL-PASS | ci_required_triggers.py, rc0; closure.txt |
| credential-provider-windows | NOT-TRIGGERED | credential_provider=false; closure.txt |
| pack-gate | NOT-TRIGGERED | packs=false; closure.txt |
| windows-lane | LOCAL-PASS | Exact four run steps from the merged workflow; rc0; windows-lane/units/gate.w1.{stdout,stderr}.log |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | blacksmith-4vcpu-windows-2025; own GOOS=windows test import closure, 238 directories, no changed input; windows-native-reach.json |

windows_reach: windows-lane-test=none, computed independently for its own GOOS=windows test import closure (not the credential-provider-only helper). Command and raw closure retained in windows-native-reach.json and windows-native-closure.txt.
windows-lane commands: GOOS=windows build of build.txt; GOOS=windows test compilation of test-build.txt; Windows nogo analyzer build/test compilation; GOOS=darwin build and test compilation of ./..., exactly as the workflow specifies. These are compile proofs; they do not claim native Windows execution. Job load: threshold15, wait0, timedout0, start10.74, max14.13, mean13.04, readings3; fresh-tree and load records retained.
deferred_to_pr_ci: windows-lane-test (non-linux-runner).
not_covered: none.
host_gaps: none.
ci_lane_run: n/a (no CI configuration change).

Full CI leaf accounting retained: 44 lines; /var/tmp/gate-ga-b6gjce9q-20261116.9tLa9khQ/ci-required/accounting.json; SHA256 8eae5f827693bcb00da024c32160937688b3170f222aac82e737d1de41caa99b.
