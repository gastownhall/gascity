# Release gate: tmux nudge with an empty pane PID

**Verdict:** **PASS**

- Deploy bead: ga-ahh4ifs; build: ga-njc7gg6; review: ga-djpcyj6.
- Reviewed source: `df98bdec6bf211f2e02c0d2bfd2d68f4a4db89a2` (resolved commit object).
- Gate base: `16777d7880f3bc7734bf49e76fc88d158ecbefaa`; gate_base_kind: current-tip.
- Canonical scratch: `/var/tmp/gc-merge-validate.EDTcBAlm`; HEAD `fd7a86797f6b1784fa2475c47ee54d75ac0d36e8`; tree `cad26a3bd951875031b4f22e00f32a541c1b572c`.
- merge_state: clean (origin/main@16777d7880f3bc7734bf49e76fc88d158ecbefaa); merge_validation: the materialized merge result was built, vetted and tested. Merge authority must settle freshness at merge time.
- Evidence root: `/var/tmp/deploy-ga-ahh4ifs.aZtx2w4R`. Raw test logs and BEP artifacts are retained.
- Deploy mode: remote; push remote: fork; intended branch: deploy/ga-ahh4ifs-gate, cut from the reviewed source after PASS.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review ga-djpcyj6 is closed with verdict pass and the exact resolved source. No carryover or reviewer-added commit substitution. |
| 2 | Acceptance criteria met | PASS | Source inspection and five owned tests cover empty PID rows alone, last and first; malformed rows, marked-agent fixtures and real dead-pane nudge. Native tmux 3.8 witness passed both named checks. |
| 3 | Tests pass | PASS | Five fresh remote phases plus all 16 native floor steps passed. 133967 PASS / 0 FAIL / 377 SKIP top-level result lines; all five owned tests PASS by name. Required CI leaf jobs accounted below. |
| 4 | No high-severity review findings open | PASS | Reviewer security, style and specification findings contain no blocker, major or unresolved HIGH. |
| 5 | Final branch clean | PASS | Canonical scratch and source checkout have empty porcelain at final assessment. This record is the sole deployment bookkeeping delta; publication requires its commit and empty porcelain before push. |
| 6 | Merge state, build and vet | PASS | c6/RESULT complete, 1/1 units, failed0; bazel build --config=remote-exec --noremote_local_fallback //... rc0, including full nogo/vet. c6-output.log. |
| 7 | Single feature theme | PASS | Five files, +146/-11: agentInputTarget, its unit/integration fixtures and runtime-tmux test-manifest entries. No independent feature or internal configuration paths. |

## Acceptance and scope

The row format ends with runtimeInventoryEnd; SplitN(...,4) requires that exact terminator. Empty pane_pid is preserved across executor trimming; truncated/extra-field/wrong-terminator rows remain rejected. Existing marked-agent selection stays intact. The new fake expands the actual format and trims like the real executor. The new real-tmux test resolves a retained dead agent pane and nudges it successfully. No changes to realExecutor trimming, parsePaneProcessState, provider contracts, configuration or dependencies.

Native witness: PASS on tmux 3.8. TestProvider_NudgeResolvesDeadPane PASS (3.96s) and TestTmuxConformance/SharedSession/Nudge_RunningSession PASS (1.32s). Four PASS result lines including parents, 0 FAIL, 0 SKIP. local-tmux/RESULT complete, failed0. local-tmux-proof.bep.json: one PASSED result, strategy linux-sandbox (local), neither cache flag. Command: local-tmux-proof.sh, --config=integration --config=ci --nocache_test_results, sharding disabled, named filter and verbose output. This focused AC witness supplements the complete suite.

## Criterion 3 evidence

- test_cmd: prescribed gate-criterion3.sh on the canonical scratch through remote admission and isolated-test-run.sh --rbe; commands and flags are recorded in c3/evidence/execution.json and each phase stdout. Unit: make check / bazel test //...; acceptance: --config=acceptance //test/acceptance:acceptance_test; integration-packages: --config=integration //test:integration_packages; full integration: //test/integration:integration_test with the prescribed latency split; integration-latency runs only the latency inventory.
- All five phases force --nocache_test_results and --noremote_local_fallback. test_cmd_scope: full-suite.
- test_exec_mode: remote-exec (suite, derived from BEP); test_exec_bep: c3/evidence/remote/<phase>.bep.json and <phase>.evidence.json. Native floor and native tmux witness are separately identified local supplements.
- BEP: 499/499 results remotely executed with terminal verdicts, attempt1, within phase windows; cached_remotely=0, cached_locally=0, other_strategy=0, retried=0, no predicate errors. Phase counts: unit375, acceptance24, integration-packages85, integration-full14, integration-latency1.
- farm_test_seconds: 9503.663; fallback_reason: none; rbe_state: kill switch absent, rc marker present, matching canonical/base worker pin, certificate valid through 2027-01-09 (c3/evidence/rbe-state.json).
- admission: remote, floor25GiB, free38.050437927246094GiB, cap2, waited0.001s; c3/evidence/remote/admission.json.
- test_log_dir: `/var/tmp/deploy-ga-ahh4ifs.aZtx2w4R/c3/evidence/remote/logs`; 515 logs, 449 with test result lines. Counts include repeated tests per job, exclude indented subtests, and are not unique-test totals. final-by-name.txt was independently regenerated with gate-test-evidence.py.
- owned_topup: none required; every owned test ran and passed in the full-suite output.
- floor_topup: complete=true, all16 steps rc0; c3/evidence/remote/floor-topup.json and floor-logs. Includes every environment-absence SKIP selection, pinned beadstest, Darwin compile and both runner self-tests. Non-owned remaining SKIPs are inventoried.
- latency_leg: complete=true, measured under clear farm probe, attempt1; both budget tests PASS; neither ran in the main full-integration leg. c3/evidence/remote/latency-leg.json.
- waiver_ref: none; failure_attribution: none in the canonical criterion-3 run. Publication-hook failures are attributed separately below.

| Diff-owned test | Result |
|---|---|
| TestAgentInputTargetPrefersMarkerAndFailsClosed | PASS |
| TestAgentInputTargetRejectsMalformedRows | PASS |
| TestAgentInputTargetResolvesDeadAgentPaneWithEmptyPID | PASS |
| TestAgentInputTarget_WrappedPane | PASS |
| TestProvider_NudgeResolvesDeadPane | PASS |

### Skip inventory and load

skip_inventory: c3/evidence/remote/skip-inventory.json plus floor-topup.json. Farm inventory contains 363 individual skip records (71 environment-absence, 292 other); final top-level reader count is 377 after native floor. All owned tests have PASS, no FAIL and no unresolved subtest SKIP.

skip_justification: remaining skips are unchanged opt-in live-service, destructive-cleanup, helper-subprocess, platform, unsupported-provider or explicit known-quarantine conditions. The process-capable integration lane covers the unit lane's intentional fast skips. Native floor repeated every environment-absence selection; remaining non-owned skips retain their actual reason, rather than being counted as executed. The diff changes no skip condition. skip-site-audit.json records 267 UNCHANGED sites against reviewed source; three files introduced on the frozen base are UNCHANGED against the canonical merge in skip-site-audit-resolved.json. That supplement also resolves shared runtime/bead conformance locations and the interleaved dispatch skip at call_budget_test.go:536 (ga-vnycm2.27). The only shared test hunk adds the fake used solely by the new owned test; it cannot reach any skip site. Unchanged tmux opt-in measurement, cleanup, keybinding-server and Darwin guards do not replace the owned regression evidence.

Runtime: recorded ICU74 probe OK, host podman.socket active, pinned bd v1.3.2 / ref9b65b9e19. Isolation wrapper reports PASS-THROUGH for the canonical scratch, not an isolated clone or a clean ambient environment.

Native floor load (5-minute average): load_threshold15; load_waited_seconds1801; load_wait_timed_out1; run_start_load63.55; run_max_load96.09; run_mean_load82.69; run_readings14; wait_first_load46.99; wait_max_load63.55; wait_mean_load39.02; wait_readings61; read_errors0. The ordinary soft wait timed out and proceeded as configured; this is not a failure attribution.

### Fast lane and CI accounting

policy_lane: PASS, pinned formatting plus make lint (full nogo/vet), check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary and check-hooks. fast-policy.log; FAST_POLICY rc0. make check's shell guards plus unit Bazel targets cover the repository-wide quality gate.

drift_lane: PASS, make bazel-sync BAZEL_SYNC_FLAGS=--config=remote-exec followed by git diff --exit-code in a fresh view; fast-drift-output.log, GATE_RUN_EXIT rc0. Both lanes ran in fresh views before the suite. Early setup/admission attempts ran no candidate tests and are retained as setup history, not test evidence.

heavy_mode: none; heavy-classify.txt. ci_lane_run: n/a (no CI job/matrix/timeout/required-check-list changes).

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.EDTcBAlm 16777d7880f3bc7734bf49e76fc88d158ecbefaa df98bdec6bf211f2e02c0d2bfd2d68f4a4db89a2 fork, rc0; merged workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs. ci-required/closure.txt. Runner expressions resolve to Blacksmith Linux runners, or the explicitly Windows path-gated runner.

| ci_required_accounting job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | Closure tool rc0; suite_mode filtered, no packs/shared/credential-provider path match; ci-required/closure.txt. |
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<evidence>/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py, rc0 in canonical scratch; runner-policy.log and .out. |
| credential-provider-windows | NOT-TRIGGERED | Tool row: needs.changes.outputs.credential_provider == 'true', no match. |
| pack-gate | NOT-TRIGGERED | Tool row: needs.changes.outputs.packs == 'true', no match. |

Check is an aggregator and takes no leaf status. deferred_to_pr_ci: none. not_covered: none. windows_reach: n/a (job not triggered). The Bazel-required test and sync lanes are covered above. Gate PASS authorizes PR-open only; required PR checks must be green at merge time.

## Publication and completion

All gate criteria PASS. Cut the isolated deploy branch from the resolved reviewed source, then add only this gate record through normal hooks. Publication verifies the pushed head, publishes exact-head success clearance and routes a verified mayor merge-request. PR URL, artifact head and verified mail id are recorded on ga-ahh4ifs after those actions occur. The designated tracker ga-2ygnccm stays open until the fix lands on main. The deployer does not merge.

Native witness load: threshold15; waited1801s; timed_out1; run_start_load44.82; run_max_load44.60; run_mean_load43.66; run_readings3; wait_first_load63.55; wait_max_load120.78; wait_mean_load79.78; wait_readings62; read_errors0; prewait_s1801. Local-heavy admission floor40GiB, free57.59788131713867GiB, cap3; local-tmux-admission.json. The bounded soft wait completed before the local command.

Artifact integrity: all998 retained raw farm log/XML files match their recorded SHA256 (retained-artifact-audit.json). Ancestry scope recheck PASS after fetching origin/main; accepted ids ga-ahh4ifs and ga-njc7gg6. The gate remains pinned to the base above; merge-time checks are required as main advances.

## Publication-hook evidence

The normal pre-commit hook passed for the gate record. The subsequent normal pre-push hook on `9b1f2c4e162f04e1c1578b64887571633f4e7858` ran `bazel test //... --config=fork-cache --keep_going`: 283/288 targets passed and five targets failed. This result remains a failed full-suite hook run. The focused coverage runs below establish attribution; they do not replace that run or the canonical full gate.

All 12 original failed log/XML artifacts remain retained and hash-verified in `prepush-failed-artifacts.json`. The non-diff-owned-gate-failure protocol was read before attribution. Every failing test is UNCHANGED according to the official ownership/site evidence, with no shared hunk in its file (`prepush-attribution/ownership-explain.txt` and `other-sites.txt`). All cited trackers were opened and sightings were read back.

| failure_attribution | Tracker | Clause 3 proof | Clause 4 |
|---|---|---|---|
| TestWriteTokenFileKeepsGroup: supplementary-group chown EINVAL | ga-bwvl64t | (a) Complete 47-package test-binary import closure excludes all changed inputs; mechanism.json and tokenrender-import-closure.txt. | No package overlap. |
| productmetrics: both shards hit the 5m package alarm; active tests TestPurgeSpoolWithinBudgetKeepsCumulativeEventCapAcrossPasses and TestUploaderPersistsAttemptAndClosedSettlementDiagnostics/server_failure | ga-46vdxg | (a) Complete 8-package test-binary import closure excludes all changed inputs; productmetrics-mechanism.json and productmetrics-import-closure.txt. | No package overlap. |
| TestStopManagedCityPreservingSessionsWaitsForRuntimeShutdownOnTimeout: state stopping instead of stopped | ga-elb781 | (c) Exact named test executed under coverage; all 50 instrumented lines in the sole changed production function, (*Tmux).agentInputTarget, had zero hits. | No package overlap. |
| TestSimCoverageGate: target reached its 60s deadline | ga-3kc9awk | (c) Exact named test executed through gc_test with all 32 original census inputs copied and hash-verified; all 50 instrumented changed-function lines had zero hits. | No package overlap. |
| TestDockerSessionProtocol/failed_start_removes_created_container: start process signal:killed instead of exit 1 | ga-hq0j256 | (c) Exact named subtest executed under coverage; all 50 instrumented changed-function lines had zero hits. | same_package=yes (scripts manifest), proof=c, added_test_load=no: no census/policy increase, new target or new scripts test file. |

The first four trackers predate this run. Open-tracker search and the closed-tracker lookup (72 closed trackers, NONE for the Docker parent/subtest) found no Docker kill-signature match. ga-d5l7kb was opened and tracks a different cancellation assertion. ga-hq0j256 was created only after coverage proof (c) landed, satisfying the discovering-run tracker rule (gm-sf3238); it is an unrouted gate-tracker. No repeat blocking run occurred. Tracker sightings/proof updates are retained under `prepush-attribution/`, `cli-proof-comment-verified.json`, `sim-proof-comment-verified.json` and `docker-killed-tracker-verified.json`.

Coverage evidence: `prepush-coverage-proof.json`, per-target coverage/log/XML in `prepush-coverage-artifacts/`, and fresh BEP in `prepush-coverage.bep.json`. Docker and shutdown tests executed PASS, with zero changed-function hits. The simulation variant executed PASS but emitted an empty coverage file, which was rejected as attribution evidence. Its replacement direct invocation is recorded in `sim-direct-coverage.sh`, `sim-direct-coverage-proof.json`, `sim-direct-coverage.bep.json` and `sim-direct-coverage-artifacts/`; TestSimCoverageGate executed PASS with a nonempty per-target coverage report and zero changed-function hits. The original simulation timeout log contains no PASS or teardown message; its cause is not asserted to be teardown.

Native attribution load: original coverage run threshold15, waited840s, timed_out0, run_start14.23, max37.17, mean25.37, readings9; wait_first59.48, max59.48, mean28.19, readings30, errors0. Admission floor40GiB, free41.616756439208984GiB, cap3. Direct simulation proof threshold15, waited450s, timed_out0, run_start14.85, run_max/run_mean NA (zero samples); wait_first28.19, max28.19, mean19.57, readings17, errors0. Admission floor40GiB, free44.892391204833984GiB, cap3. Both ran through the isolation wrapper and detached service, at the unchanged pre-push head.

All four attribution clauses pass for every hook failure. The protocol authorizes `git push --no-verify` for the specific resulting head after this record is committed; claim, isolated target, reviewed ancestry and remote-head equality are checked explicitly. This is attribution, with no waiver. Source code and tests remain exactly the reviewed source; the additional commit changes only this record.
