# Release gate: delivered-but-unobserved nudge handling (`ga-ob71a1`)

- Deploy bead: `ga-ob71a1`
- Source build bead: `ga-civwyz`
- Review bead: `ga-yy08yh`
- Reviewed source: `fea720f9bd4ed2426005cf4cc90a3e36a0cc7d76`
- Source branch (provenance only): `builder/ga-civwyz`
- Base: `origin/main@c60a563ea3645b507cc3b97041731ed483e1fc59`
- Merge base: `8d0f7db075c8ff70d0813d2fa41a548d448e2b6c`
- Deploy mode: remote
- Evaluated: 2026-09-13
- Overall: **FAIL — criterion 3**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-yy08yh` records `verdict: pass` for the exact reviewed SHA, with no carryover. |
| 2 | Acceptance criteria met | **PASS** | Independent source inspection confirmed the drained-composer classifier, delivered-but-unobserved sentinel, three caller classifications, startup retry suppression, and unchanged fence-mismatch dead-letter path. All 10 diff-owned test groups passed in explicit runs, including the still-drafted composer case and the two real-tmux lag tests. |
| 3 | Tests pass | **FAIL** | The documented full-suite command ran all 40 jobs: 39 PASS, 1 FAIL, 0 unexecuted jobs. `integration-packages-runtime-tmux-1-of-3` failed `TestSecretEnvIsAbsentFromProcCmdline` because `session_env_file_integration_test.go:93` found a staged directory after session creation: `[gc-tmux-session-2278533761]`. The test file is not diff-owned, but the candidate changes production code in the same package, adds `internal/runtime/tmux/nudge_confirm_lag_integration_test.go`, and raises the package's resource census. The mandatory same-package clause-4 guard therefore fails; no mayor/operator waiver covers this failure. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed all Python policy tests and Go policy packages. `go vet ./...`, `go build ./...`, `git diff --check`, changed-Go formatting, and `make check-hooks` also passed. |
| 4 | No high-severity review findings open | **PASS** | `ga-yy08yh` records no blockers, majors, minors, security findings, or unresolved high-severity findings. |
| 5 | Final branch is clean | **PASS** | The detached exact-SHA evaluation worktree was clean before this gate record was written; `origin/builder/ga-civwyz` resolves to the reviewed SHA. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main fea720f9bd4ed2426005cf4cc90a3e36a0cc7d76` exited 0 and produced merge tree `23867d3207b59f0da259b5fdbb81263a6daae933`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The 14-file delta has one theme: classify delivered-but-unobserved tmux nudge submission as delivery, prevent duplicate queueing/retry, and pin that behavior in its test manifests and resource ledger. |

## Criterion 3 evidence

- `test_cmd`: `LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel`, invoked through `packs/actual/all/scripts/isolated-test-run.sh`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 40 jobs; 39 PASS, 1 FAIL, 0 unexecuted jobs. The non-verbose shard runner does not emit an aggregate test-level PASS/SKIP count.
- Full log: `/var/tmp/ga-ob71a1-full-suite.log`
- Shard logs: `/var/tmp/gc-local-tests.FdoHUo`
- Failing shard: `/var/tmp/gc-local-tests.FdoHUo/integration-packages-runtime-tmux-1-of-3.log`
- `diff_tests_executed`: 10/10 groups PASS, 0 FAIL, 0 SKIP in explicit verbose runs:
  - `TestDeliverSessionNudgeWithWorkerAcksDeliveredUnobservedInsteadOfFailing` PASS
  - `TestSendMailNotifyWithWorkerAcksDeliveredUnobservedInsteadOfDuplicating` PASS
  - `TestTryDeliverQueuedNudgesByPollerAcksDeliveredUnobservedInsteadOfRetrying` PASS
  - `TestDoStartSessionReturnsNudgeDeliveryError/delivered-but-unobserved_submit_is_not_fatal` PASS
  - `TestSendStartupNudgeWithRetry_DeliveredButUnobservedNeverRetried` PASS
  - `TestPaneShowsDrainedComposer` PASS, 13/13 subtests
  - `TestNudgeConfirmBusyRenderLag` PASS, 2/2 subtests
  - `TestNudgeConfirmBudgetThreshold` PASS, 6/6 subtests
  - `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory` PASS
  - `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce` PASS
- Supplemental logs: `/var/tmp/ga-ob71a1-diff-tests.log`, `/var/tmp/ga-ob71a1-pane-test.log`, and `/var/tmp/ga-ob71a1-optin-tmux.log`
- The two `GC_TMUX_INTEGRATION=1` tests ran under the procedure authorized by the mayor in `ga-82s3eo` on 2026-09-13 at 19:48Z. This authorization establishes their executed PASS evidence; it does not waive the separate full-suite failure.
- `waiver_ref`: none for `TestSecretEnvIsAbsentFromProcCmdline`
- `ci_lane_run`: n/a — the diff does not modify CI configuration.
- Condition tracker: `ga-tvfw4r`, created from this first sighting and intentionally not routed as work.
- `failure_attribution`: not attributed. Clause 2 cannot establish a pre-existing condition because `ga-tvfw4r` was created by this run. Independently, clause 4 fails: `same_package=yes`, no landed cross-PR/coverage/base-ref proof, and `added_test_load=yes` because the diff adds a test file in `internal/runtime/tmux` and updates `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and `TESTING.md`.

## Disposition

Criterion 3 is FAIL under the mandatory same-package/added-test-load rule. No isolated deploy branch, push, pull request, deploy-clearance status, or merge action may be created from this gate. Return the bead to the builder for a fresh reviewed handoff after the cleanup failure is fixed or the merge authority records an independent waiver for this exact failure.
