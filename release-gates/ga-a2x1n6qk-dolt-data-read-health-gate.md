# Release gate: report unreadable ignored Dolt tables

**Verdict:** **PASS**

Bead: ga-a2x1n6qk. Review: ga-0kg0g815 (PASS at the exact source; no carryover, reviewer-added commit, or waiver). Build lineage: ga-rmrnl1, ga-bff0pxcv, ga-inhcbxv, ga-4ymxxmkn.
Evaluated independently on 2026-11-21 UTC. `docs/PROJECT_MANIFEST.md` is absent at the pinned base; the deployed release-gate protocol, repository TESTING.md, CI workflow and bead acceptance criteria supply the checks.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Current full review ga-0kg0g815 reread live; its notes unchanged, PASS explicitly names the verified source below. |
| 2. Acceptance criteria | PASS | All 33 owned tests PASS by name; code and complete test bodies inspected. Actual cold corruption detected through the shipped helper and real shell gate on both CLI and direct-driver paths; clean controls and live read-only probe healthy. Details below. |
| 3. Tests and CI accounting | PASS | Complete canonical suite: five fresh remote phases, 16 native checks, 0 FAIL,33/33 owned tests PASS. Six required CI leaves accounted below; native Windows deferred to PR CI. |
| 4. Review findings | PASS | Unresolved HIGH/blocker/major findings: 0. Accepted P3 follow-ups remain ga-c50y6lll, ga-c2aqpkx and ga-3ipmuxh0; independently opened. |
| 5. Clean final branch | PASS | Reviewed source and validation scratch clean; this record is the sole added artifact. Empty worktree status is a mandatory pre-push check. |
| 6. Merge state and build/vet | PASS | Clean synthetic merge at pinned origin/main; complete Bazel build and explicit nogo lint both exit 0. |
| 7. Single feature theme | PASS | Nine files +1085/-5 implement ignored-table data reads, health reporting, recovery suppression and bd store-open verification in one managed-health path. No internal-agent docs, unrelated theme or CI-config change. |

## Exact source and merge state

- deploy_mode: remote; push_remote: fork; target repository: gastownhall/gascity; base_branch: main.
- reviewed_commit: 055ec16635a961ab8e8c0834d3d5f7695f250ee4
- gate_head: 055ec16635a961ab8e8c0834d3d5f7695f250ee4
- gate_base: e6e47235027b758b9a86b0ddb99fb5d3a69c59e4
- merge_state: clean (origin/main@e6e47235027b; files:none)
- gate_base_kind: current-tip (pin captured when criterion6 materialized the merge).
- synthetic_merge: 14e89a9dafcda1cbb5fdbd7d943980bbb308b532; tree: 64c921e11120cef68be2cd9c68d93cd0f73a6fc1 (identical to reviewed source tree).
- merge_validation: `engdocs/contributors/release-gate-criteria-conventions.md` and the repository merge-time guarantee; merge authority checks the eventual PR head and CI. No rebase or conflict resolution performed.
- waiver_ref: none. No failure attribution or escape used.

Full C6 and static-lane evidence: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/c6-audit.json and /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/fast-independent-audit.json. Hooks were disabled only for scratch materialization; source build/vet used the unmodified merge tree.

## Acceptance proof and limits

The probe discovers all nine ignored-table name/pattern families and performs unaggregated `SELECT * ... LIMIT 1` reads, one CLI batch per database or iterated driver rows, inside a 20-second total budget. Absent tables are healthy; chunk/root/checksum/server-form EOF errors retain their text; other errors warn. The helper reports the data error in-band, and beads-health preserves it without entering recover. Both shell branches also verify bd store-open with `bd ping`.

The original build's mayor amendment of 2026-10-30 governs the real-server check. Independent Dolt 2.6.0 experiment: four private copies of a real ignored `wisps` table with 20,000 incompressible rows, private HOME/config and loopback servers, stats disabled in startup config. Damaged copies had 5,598,192 journal bytes overwritten in place after startup, preserving the first 4096 and last 65536 bytes. Catalog queries and `COUNT(*)=20000` still succeeded, and `database_ready=true`; leaf reads failed.

- CLI damaged: ``gascity: error on line 1 for query SELECT * FROM `gascity`.`wisps` LIMIT 1: Error 1105 (HY000): checksum error``; real shell gate exit 1.
- Direct-driver damaged: `gascity: Error 1105 (HY000): checksum error`; real shell gate exit 1.
- Both clean controls: no data-read finding, shell gate exit 0. Independent SQL read reproduced the checksum error in each damaged case.
- Live store: read-only helper invocation without database/write-probe flags, query_ready=true, no data-read finding, exit 0. Preserved damaged fixture never accessed; no live data changed.
- Every private server stopped and fixture removed; captured PIDs independently absent. Raw logs, exact scripts/configs and hashes retained in /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/c2-real-startupfix-evidence/ and /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/c2-independent-audit.json.

Three experimental setup stops occurred before any probe or corruption (interactive password default, TLS default, then offline stats persistence). Each was corrected in a new retained run; no failing source test was rerun into PASS. Only the final controlled experiment is credited. The old impossible damaged fixture was not used.

Scope limits: the probe reads the first leaf, and warm caches can hide on-disk loss until turnover. Real checksum detection is proven on both paths; actual server-form EOF remains unit-pinned. The scheduled dolt-health order is the separately tracked accepted descoping ga-oupiyi8. The P3 follow-ups above remain nonblocking.

## Fast lanes, before runtime and suite

Format, policy plus shell guards, generated-file drift (`make bazel-sync` plus clean diff and seven drift targets), OpenAPI drift, and complexity zero-growth all passed in independent fresh views. BUILD files remained in sync. No API/OpenAPI/dashboard implementation changed, so dashboard preview is not required.

Complexity scoring is the documented form(ii): base and merged `make complexity-check` both exit 2, no merged row worsens its corresponding base row, `COMPLEXITY_BASE_REF` diff reports no threshold changes, baseline file untouched. This is the prescribed zero-growth criterion, not an attributed red lane. Full rows: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/complexity-zero-growth.json.

## Complete test evidence

- test_cmd: `/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.7OVgsJ --base e6e47235027b758b9a86b0ddb99fb5d3a69c59e4 --bead-json /var/tmp/gc-gate-ga-a2x1n6qk-8mww4bqs/bead-criterion3.json --owned /var/tmp/gc-gate-ga-a2x1n6qk-8mww4bqs/owned.tsv --run-dir /var/tmp/gc-gate-ga-a2x1n6qk-8mww4bqs/c3-run/evidence`; terminal exit 0, criterion3_valid:true.
- delta_gate: mode=shadow, decision=full, reason=not-gate-pr, applied=false; delta_check:n/a (full scope).
- test_cmd_scope: full-suite; heavy_mode:none; delta shadow chose full (`not-gate-pr`), applied:false.
- test_counts: PASS=158013 FAIL=0 SKIP=617 (official reader's top-level result lines across 565 logs; duplicates across jobs counted per job).
- diff_tests_executed: 33 PASS, 0 FAIL, 0 SKIP; explicit names below. Mac-only owned tests: 0; no narrowed run substituted for the suite.
- test_exec_mode: remote-exec
- fallback_reason: none
- test_exec_bep: phase table below; every evidence report has no errors.
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/c3-run/evidence/remote/logs
- farm_test_seconds: 13831.756 (sum across actions).
- admission: {"class":"remote","floor_gib":25.0,"free_gib":63.81386184692383,"waited_seconds":0.0,"cap":2}
- rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- skip_inventory: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/skip-independent-audit.json (603 remote plus72 native SKIP events, nested included;675 total,317 source sites).
- owned_topup: none; all owned tests found in the full-suite output.
- floor_topup:16 checks, all exit 0; exact commands and logs in /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/c3-run/evidence/remote/floor-topup.json.
- latency_leg: measured, first attempt, both registered tests PASS. Farm probe waited 40s, then load1=43.00 below threshold 50 (load5=100.26 recorded); no latency test ran before clear.
- native observed load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.12 run_max_load=13.20 run_mean_load=11.71 run_readings=7 wait_first_load=10.12 wait_max_load=10.12 wait_mean_load=10.12 wait_readings=1 read_errors=0`.
- runtime: ICU74 check OK with retained soname/hash provenance; podman socket active and ping OK, pinned Dolt 2.6.0 image present, recent testcontainer sweep and disk floors verified before launch.

| Phase | Result actions | Fresh remote | Cached remote / local / retried | Starts inside phase window |
| --- | ---: | ---: | --- | --- |
| unit | 422 | 422 | 0 / 0 / 0 | true |
| acceptance | 25 | 25 | 0 / 0 / 0 | true |
| integration-packages | 87 | 87 | 0 / 0 / 0 | true |
| integration-full | 14 | 14 | 0 / 0 / 0 | true |
| integration-latency | 1 | 1 | 0 / 0 / 0 | true |

All 549 actions PASSED at attempt 1 with remote strategy, no local fallback or cached test result. All 1098 raw log/XML artifacts matched their declared digest and length. Independent audits: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/five-phase-raw-audit.json and /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/suite-independent-audit.json. Native floor and runner self-tests all exit 0. No partial run or reviewer result was credited.

### Every owned test

- TestHealthBeadsProviderReturnsDataReadFailureWithoutRecover — PASS
- TestHealthDataReadGateDiesOnDataReadError — PASS
- TestHealthDataReadGatePassesSilentlyWhenClean — PASS
- TestHealthDataReadGateWarnsButPassesOnInconclusiveRead — PASS
- TestHealthOpDiesOnDataReadErrorAndCarriesTheProviderMarker — PASS
- TestHealthOpFailsWhenBdPingFailsOnTheHelperBranch — PASS
- TestHealthOpFailsWhenBdPingFailsOnTheShellFallbackBranch — PASS
- TestHealthOpPassesSilentlyOnTheShellFallbackWhenNothingFails — PASS
- TestHealthOpPassesSilentlyWhenTheHelperReportIsClean — PASS
- TestHealthStoreOpenGatePassesWhenBdPingSucceeds — PASS
- TestHealthStoreOpenGateRunsBdPingAndFailsWithItsOutput — PASS
- TestHealthStoreOpenGateSkipsCityWithoutBeadsDir — PASS
- TestLoadHealthCheckFromGCParsesDataReadFields — PASS
- TestManagedDoltDataReadProbeAbsentTableIsHealthy — PASS
- TestManagedDoltDataReadProbeChunkErrorBeatsEarlierInconclusiveError — PASS
- TestManagedDoltDataReadProbeConnectionLevelEOFStaysInconclusive — PASS
- TestManagedDoltDataReadProbeDiscoveryChunkErrorIsFailure — PASS
- TestManagedDoltDataReadProbeDiscoveryFailureIsWarning — PASS
- TestManagedDoltDataReadProbeHealthyWhenReadsSucceed — PASS
- TestManagedDoltDataReadProbeNoIgnoredTablesIsHealthy — PASS
- TestManagedDoltDataReadProbeOtherErrorsAreWarningsNotFailures — PASS
- TestManagedDoltDataReadProbePropagatesChunkErrorText — PASS
- TestManagedDoltDataReadProbeReportsOnlyErrorLinesBounded — PASS
- TestManagedDoltDataReadProbeRootHashMissingIsNotHealthy — PASS
- TestManagedDoltDataReadProbeServerChunkReadErrorsAreFailures — PASS
- TestManagedDoltDataReadProbeSkipsSystemDatabases — PASS
- TestManagedDoltDataReadProbeUnclassifiedFailureReportsErrorLinesNotRows — PASS
- TestManagedDoltDataReadProbeUnrelatedDoesNotExistIsInconclusive — PASS
- TestManagedDoltHealthCheckFieldsEmitDataReadFindingsOnlyWhenPresent — PASS
- TestManagedDoltHealthCheckReportsDataReadErrorWithoutFailingTheCheck — PASS
- TestManagedDoltHealthCheckReportsDataReadWarning — PASS
- TestManagedDoltIgnoredTableDiscoverySQLCoversEveryIgnoredPattern — PASS
- TestManagedDoltLeafReadStatementsForForceALeafFetch — PASS

### Skip justification

No owned SKIP or unresolved site. Every one of 675 SKIP events has its actual printed reason, unchanged skip site and supporting evidence in the inventory. The official reader's 617 top-level skips and the 675 all-depth inventory intentionally use different counting scopes.

Shared hunks explicitly considered: cmd/gc/dolt_data_read_probe_test.go, cmd/gc/dolt_sql_health_database_test.go, cmd/gc/gc_beads_bd_health_data_read_test.go, cmd/gc/gc_beads_bd_health_database_test.go. All are in cmd/gc. Independent pinned-BASE comparisons used the same target/phase or exact native command: unit193, integration68, native36; all 297 matched by test and reason. Other test binaries cannot import the changed main package; their skip-source blobs are identical at base and merge. For binaries embedding examples/bd, inspected guards concern unchanged host/tool/fixture/capability/config conditions before this health operation; the changed health-script body cannot decide them. Every environment-absence native follow-up completed, with remaining non-owned fixture/platform skips retained rather than erased.

COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a00, head rbe-a06)

Raw BASE comparisons, including the first reader call that accidentally named two PASS-only parents and all pre-execution setup refusals, remain in /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/base-skip-comparisons/. No source tests were rerun for those reader/setup corrections. Full source witnesses and import closures: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/skip-source-witnesses.txt and /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/skip-reach-proof.json.

## Required CI job accounting

ci_required_closure: ["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gc-merge-validate.7OVgsJ", "e6e47235027b758b9a86b0ddb99fb5d3a69c59e4", "055ec16635a961ab8e8c0834d3d5f7695f250ee4", "fork"]; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; `6 LEAF jobs need a status; the rest are rolled up through their needs.`

ci_required_accounting:
- runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained output> python3 .github/workflows/scripts/runner_policy.py; exit 0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/ci-required-v3/runner-policy-output.txt
- changes | LOCAL-PASS | ci_required_triggers.py /var/tmp/gc-merge-validate.7OVgsJ e6e47235027b758b9a86b0ddb99fb5d3a69c59e4 055ec16635a961ab8e8c0834d3d5f7695f250ee4 fork; exit 0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/ci-required-v3/closure.txt
- credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane | LOCAL-PASS | /var/tmp/gc-gate-ga-a2x1n6qk-8mww4bqs/ci-windows-lane.sh (all four literal job commands; see ci-workflow-run-steps.json); exit 0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/ci-required-v3/windows-lane-output.txt
- windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR; windows_reach:none

windows_reach: credential-provider-windows:none; windows-lane-test:none. Native Windows reach was independently computed for that job's own allowlist (credentialprovider, genshell, processgroup), ordinary and integration-tag import closures; no reused credential-only proxy. `runner_windows` resolves to blacksmith-4vcpu-windows-2025; Linux cross-build job uses blacksmith-16vcpu-ubuntu-2404. No Windows vet top-up is required for empty reach. Full job environment/commands, fresh-tree records, terminal statuses and load observations retained in /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/ci-independent-audit.json and /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q/evidence/ci-required-v3/.

deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

This PASS authorizes PR-open only. Merge requires `CI / required` green on the PR head and successful native Windows CI. The merge-request pins the exact pushed head; a different head requires a new gate.

## Retention

Hash-verified archive: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-a2x1n6qk/gate-20261121-iuewr57q (initial snapshot 2682 files, 198624125 bytes; manifest SHA256 6f45b02eaf0b1f4be1284f09124d6bde25517009d345684c64124c0daf8fe137). Raw logs are retained. Synthetic merge bundle SHA256 e90ef4d59f384ebb42a3c5f8c5c3c99ec10089d5f2ea180ae26f3b1ab36a470a; bundle prerequisite is the pinned base. The archive receives publication/cleanup receipts after PR-open without altering original measurement files.
