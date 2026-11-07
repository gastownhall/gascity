# Release gate: dirty-table init recovery (ga-9nnvbd6b)

**Verdict:** **PASS**

date: 2026-11-07T23:15:28.316867+00:00
deploy_mode: remote
reviewed_source_sha: 677f370b7c140b3a59a9b437631f3838a3c5e32d
gate_base_sha: 7e2431b2bd27636333755f4f4d90773c5d1cddac
gate_base_kind: current-tip
merge_head_sha: d3ff00c4e6e9f185dea3d6ef962b9ad9e7cf5b53
merge_tree_sha: ab2a9cec551af0fe724ada7651e751e73e228878
merge_state: clean (origin/main@7e2431b2bd27636333755f4f4d90773c5d1cddac)
merge_validation: The reviewed source was mechanically merged onto the pinned base and that exact tree was built and tested. The merge authority must enforce current-head CI and merge-time validation.
evidence_root: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9nnvbd6b/gate-20261107-6lrn0_51

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review ga-asbgld0s closed PASS on the resolved source above. Reviewer-added tests independently verified: cb3d5e9c34209299f7131a321c31f4247c9f2d0c -> 677f370b7c140b3a59a9b437631f3838a3c5e32d; files=cmd/gc/beads_provider_dirty_tables_test.go. raw/review-provenance.json and raw/reviewer-added-tests-check.log. |
| 2 | Acceptance criteria met | PASS | Both sibling init paths invoke bounded dirty-table recovery, retain commit errors, and accept an already-initialized retry. Six named tests cover these cases. The seventh runs live Dolt with a clean default database and a different dirty target database, proving the existing count query is database-qualified. raw/static-criteria.json maps each contract clause to its test. |
| 3 | Tests pass | PASS | Full five-phase fresh remote suite, mandatory native floor, all seven owned tests by name, policy/generation lanes and required CI accounting pass. Details below. |
| 4 | No unresolved HIGH findings | PASS | Review PASS; unresolved HIGH count=0. A stale test-helper comment was a nonblocking nit. |
| 5 | Final branch clean | PASS | Canonical merge scratch and assigned worktree clean after checks. The deploy branch carries only the exact reviewed source plus this gate record; publication verifies clean status after its record commit. |
| 6 | Merge state and build/vet | PASS | Clean two-parent mechanical merge matches predicted tree. bazel build --config=remote-exec --noremote_local_fallback --keep_going //... exits 0 (1456 targets, nogo/vet included); normal pre-commit hook and make check-hooks pass. raw/c6-build.log, raw/c6.bep.json, raw/c6-commit.log, raw/check-hooks.log. |
| 7 | Single feature theme | PASS | Two cmd/gc files: dirty-table recovery in scoped Kubernetes and default-rig initialization, with regression and live-Dolt tests. |

## Runtime evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-detached-run.sh start --run-dir /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/c3 --key ab2a9cec551af0fe724ada7651e751e73e228878 -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-9nnvbd6b DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-ga-9nnvbd6b._lfhq_rx --base 7e2431b2bd27636333755f4f4d90773c5d1cddac --bead-json /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/bead.json --owned /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/owned-tests.tsv --run-dir /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/c3/evidence
Phase commands: make -k check; Bazel acceptance; integration-packages; full integration excluding the configured latency tests; the separate latency-only phase. Exact flags, commands, windows and BEPs are retained under raw/c3/evidence/remote.
test_cmd_scope: full-suite
heavy_mode: none
test_exec_mode: remote-exec
fallback_reason: none
test_counts: {"FAIL": 0, "PASS": 149135, "SKIP": 513}
Counts are top-level Go result lines summed across retained job logs, including repeated execution across configurations; subtest skips are also listed in the inventories.
farm_test_seconds: 12522.681
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-9nnvbd6b/gate-20261107-6lrn0_51/raw/c3/evidence/remote/logs
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 87.98625564575195, "waited_seconds": 0.001}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 403, "results": 403, "retried": 0, "window_ok": true}}
All five phases force --nocache_test_results and --noremote_local_fallback. Every test result uses remote strategy, neither cached flag, attempt 1, a phase-window start and a terminal verdict. Artifact SHA256 values match every retained manifest. No farm/local fallback suites were stitched.

| Phase | Fresh remote actions | Verified artifacts |
|---|---:|---:|
| unit | 403 | 806 |
| acceptance | 25 | 50 |
| integration-packages | 86 | 172 |
| integration-full | 14 | 28 |
| integration-latency | 1 | 2 |

latency_leg: {"attempts_allowed": 2, "complete": true, "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS", "phase": "integration-latency", "rc": 0, "tests": ["TestGcBdLatencyBudget", "TestGcSeatHookLatencyBudget"], "verdict": "measured"}; complete probe and attempt evidence: raw/c3/evidence/remote/latency-leg.json.
owned_topup: []
floor_topup: complete=true; 16 steps, including every environment-absence skip selected by package/root, beadstest, Darwin compile and runner self-tests. Exact commands, exits and logs: raw/c3/evidence/remote/floor-topup.json.

diff_tests_executed:
  TestInitBeadsForDirWithExecutorK8sRecoversFromDirtyTables PASS (integration-packages-12697e2ea0d32c278115.log, unit-637c74c578a2b17fc654.log)
  TestInitBeadsForDirWithExecutorK8sSurfacesCommitFailure PASS (integration-packages-9326181a736eb30b71ca.log, unit-423c6a0da1986b90e9a7.log)
  TestInitBeadsForDirWithExecutorK8sTreatsAlreadyInitializedRecoveryAsSuccess PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-752a0bf25b15b465517a.log)
  TestInitDefaultRigBdStoreRecoversFromDirtyTables PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-1ab526405ace21879f85.log)
  TestInitDefaultRigBdStoreSurfacesCommitFailure PASS (integration-packages-e4245ddf6a37030111bc.log, unit-d9e667428f972d067bd3.log)
  TestInitDefaultRigBdStoreTreatsAlreadyInitializedRecoveryAsSuccess PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestManagedDoltDirtyTableCountIsDatabaseQualified PASS (integration-packages-821a75e4a503601af052.log; also SKIP: TestManagedDoltDirtyTableCountIsDatabaseQualified in unit-fd986cf5bab8709b8d45.log (PASS in another job))
  skip_reason: TestManagedDoltDirtyTableCountIsDatabaseQualified (unit-fd986cf5bab8709b8d45.log): beads_provider_dirty_tables_test.go:619: requires a real Dolt server; run make test-cmd-gc-process for full coverage
GATE_TEST_EVIDENCE_RESULT ok: 7 diff-owned tests verified by name

waiver_ref: none (gascity has no waiver path)
failure_attribution: none; no runtime test failed

## Skip integrity

skip_inventory: {"environment-absence": 72, "other": 427}; full printed reasons: raw/c3/evidence/remote/skip-inventory.json and raw/native-skip-audit.json.
skip_justification: Every non-owned skip has a source site outside the changed hunks and an import-closure proof. The changed shared-helper input is cmd/gc/beads_provider_dirty_tables_test.go (SHARED-HUNKS=1); its package is cmd/gc. 186 farm skip events in reachable binaries have targeted same-command pinned-BASE comparisons with identical reasons. Unreached skip source blobs are unchanged. One empty parsed reason was recovered only from adjacent raw CONT/reason/SKIP lines (raw/skip-reason-recovery.json); the original inventory remains intact. The owned live-Dolt unit-lane skip is cured by a real integration PASS, never excused. Native reachable skips have their own same-command BASE proof; other native skip sites have no changed shared-helper package in reach. Reasons and per-site proofs: raw/farm-skip-site-mapping.json, raw/farm-skip-sites-explained.txt, raw/farm-skip-source-blobs.json, raw/base-skip-comparison-proof.json and raw/native-skip-audit.json.
Farm BASE comparison: [{"compare_exit": 1, "non_owned_pre_existing": 155, "non_owned_skips": 155, "owned_rows_disposition": "Unit-lane owned SKIP cured by a real same-C3 integration PASS; original nonzero comparison report retained", "owned_rows_never_claimed_pre_existing": ["TestManagedDoltDirtyTableCountIsDatabaseQualified"], "phase": "unit", "summary": "COMPARE-SUMMARY head_skips=156 judged=156 pre-existing=155 new=0 reason-changed=0 diff-owned=1 no-reason=0 not-judged=0  => NOT ALL PRE-EXISTING: only pre-existing clears the rule, the rest are the diff's"}, {"compare_exit": 0, "non_owned_pre_existing": 31, "non_owned_skips": 31, "owned_rows_disposition": "none", "owned_rows_never_claimed_pre_existing": [], "phase": "integration-packages", "summary": "COMPARE-SUMMARY head_skips=31 judged=31 pre-existing=31 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING"}]; commands, selected test names and raw reports: raw/base-skip-comparison-proof.json.
Native BASE comparison: raw/native-base-skip-proof.json

## Fast lane and CI accounting

policy_lane: PASS; shell guards, eleven Bazel policy/lint/format targets and full make -k check. Exact commands: raw/fast-ci.sh and raw/fast-ci-lane.sh; raw/fast-ci-*.log and *.bep.json retain verdicts.
drift_lane: PASS before runtime setup; seven generated-artifact targets, OpenAPI compatibility, make bazel-sync plus git diff --exit-code and empty git status --porcelain --untracked-files=all in separate fresh trees. The canonical merge scratch stayed unchanged.
ci_lane_run: n/a (no CI configuration diff)
ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-ga-9nnvbd6b._lfhq_rx 7e2431b2bd27636333755f4f4d90773c5d1cddac 677f370b7c140b3a59a9b437631f3838a3c5e32d fork; workflow_blob=e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs.
ci_required_accounting:
- runner-policy | LOCAL-PASS | runner-policy                            LEAF       linux                      EVERY-PR | /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/ci-accounting/runner-policy.log
- changes | LOCAL-PASS | changes                                  LEAF       linux                      EVERY-PR | /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/ci-accounting/closure.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' | /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/ci-accounting/closure.txt
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' | /var/tmp/gc-gate.ga-9nnvbd6b.6lrn0_51/ci-accounting/closure.txt

windows_reach: none
deferred_to_pr_ci: none
not_covered: none
The changes closure and runner-policy commands each exit 0. Runner policy uses EVENT_NAME=pull_request, PR_AUTHOR=quad341 and FORCE_BLACKSMITH empty. Aggregators take no leaf status. This PASS authorizes PR-open; current-head required CI must still pass before merge.

## Host conditions and retained evidence

The isolation wrapper reported PASS-THROUGH for the temporary scratch checkout (not on its isolate list), so it ran in place with disclosed ambient environment. Its pinned bd and disk-floor checks passed. ICU check passed. Remote phases use remote admission at 25 GiB/cap 2 with no host-load wait; native floor retains the local soft load wait under the remote admission; native BASE proof separately enforces the 40 GiB floor and local-heavy admission/load wait.

Native floor:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=79.72 run_max_load=103.78 run_mean_load=96.07 run_readings=14 wait_first_load=121.14 wait_max_load=125.61 wait_mean_load=97.77 wait_readings=61 read_errors=0
```

Native BASE:
```text
GATE_ADMISSION prewait class=local-heavy result=timed_out waited_s=1801 readings=61
GATE_ADMISSION {"class": "local-heavy", "floor_gib": 40.0, "free_gib": 58.70283889770508, "waited_seconds": 0.001, "cap": 3, "prewait_s": 1801.528}
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=78.73 run_max_load=86.63 run_mean_load=83.78 run_readings=5 wait_first_load=122.74 wait_max_load=125.70 wait_mean_load=87.93 wait_readings=62 read_errors=0 prewait_s=1801
```

The durable evidence inventory SHA256-verifies the retained copies. Detailed logs, BEP, skip inventories and comparisons remain outside the PR diff; this file is the compact gate record.
