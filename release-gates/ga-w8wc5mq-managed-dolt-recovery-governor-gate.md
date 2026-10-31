# Managed-Dolt recovery governor release gate

**Verdict:** **PASS**

Feature slug: `ga-w8wc5mq-managed-dolt-recovery-governor`. Deploy bead: `ga-w8wc5mq`. Build: `ga-6jzn19`. Review: `ga-hif97p4`.

Reviewed source: `739717cbc07548465f66006f43ac3dd162dc16af`. Pinned base: `origin/main@8ed726532c960f7da00153b22f208666dbde334d`.
Canonical tested merge: `cd20f77ebf47e1541a682c890839a3339b8abfa2`, tree `6e5199cc12d66871ba8d1ae8c074c6802b606102`.
Current main was fetched and still equals the pinned base before publication.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live review ga-hif97p4 records pass for the exact resolved source; no unresolved blocker or major finding. |
| 2 | Acceptance criteria met | PASS | All 13 owned governor tests pass by name, including the twelve design scenarios and bounded retry join. E1/E2/E3 share a flight, failure holds use the existing cooldown from completion, retry recovery takes the provider slot, and controller waits use the flight-start window. The architecture paragraph is present. The four reviewer sensitivity demonstrations and measured before/after counts are copied into the prepared PR, including the test-8 assertion limitation. ga-7id4lg remains OPEN/unlanded; its five-name guard requirement is carried to merge authority for a merge-time recheck. PR wording is Refs #7170. |
| 3 | CI-equivalent tests pass | PASS | Four full forced-execution remote phases: make check (368 attempts), acceptance (24), integration packages (78), full integration (14); 484 remote, uncached, first-attempt results with terminal PASS inside each phase window. Mandatory native floor: 16/16 steps exit 0. All 13 owned tests PASS with no owned FAIL, SKIP or MISSING. Raw BEP and 968 artifact hashes independently verified. |
| 4 | No high-severity findings open | PASS | Reviewer verdict has zero blockers/majors and only non-blocking follow-up ga-t0kn451; no requested change remains. |
| 5 | Clean source and tested branch | PASS | Reviewed source checkout and canonical test merge have clean tracked/untracked status. The only deployment addition is this gate record; the committed deploy branch must pass the final clean-status and source-presence guards before push. |
| 6 | Diverges cleanly from main | PASS | Canonical materialize_merge_tree merge is conflict-free, matches the predicted tree above, and builds/vets with nogo and active hooks. No self-rebase was used. The current fetched base remains the tested base. |
| 7 | Single feature theme | PASS | Five reviewed commits, six changed files: one per-city managed-Dolt entry governor with tests, BUILD wiring and its architecture paragraph. Ancestry guard passed with deploy/build IDs; no denied internal configuration paths or unrelated theme. |

## Criterion 3 evidence

- test_cmd: gate-criterion3.sh through gate-base.sh, admission, load-gate-run.sh and isolated-test-run.sh --rbe. The wrapper reports PASS-THROUGH -- NOT ISOLATED; each fast lane used a separate fresh view of the canonical merge.
- test_cmd_scope: full-suite
- test_exec_mode: remote-exec (derived from BEP)
- test_exec_bep: `/var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk/criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json`; independent predicates `/var/tmp/deploy-ga-w8wc5mq.gvpl96hk/farm-verification.json`.
- test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk/criterion3/evidence/remote/logs
- rbe_state: rc marker present; kill switch absent; certificate expires notAfter=Jan  9 20:13:22 2027 GMT; worker-env pins equal `sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0`.
- fallback_reason: none
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 88.19544219970703, "waited_seconds": 0.001}
- farm_test_seconds: 8058.424
- heavy_mode: none
- skip_inventory: `/var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk/criterion3/evidence/remote/skip-inventory.json`; {"environment-absence": 71, "other": 255} farm skip sites, with all environment-absence packages included in the native floor.
- owned_topup: [] (all owned names already passed)
- floor_topup: complete, 16/16 exits 0; `/var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk/criterion3/evidence/remote/floor-topup.json`.
- waiver_ref: none (no gascity waiver path)
- ci_lane_run: n/a; no CI-configuration change.
- failure_attribution: none for tests.
- load_threshold=15 load_waited_seconds=60 load_wait_timed_out=0 run_start_load=14.66 run_max_load=15.18 run_mean_load=14.35 run_readings=6

Residual SKIPs are unchanged opt-in live-provider/Postgres/replay fixtures, helper-process selectors, platform-specific branches and explicitly classified conformance cases. Their reasons are inventoried; all farm environment-absence packages were attempted natively, and no changed test or changed skip condition is skipped. The governor tests execute in both unit and integration-package output. Native-floor SKIPs remain inventoried and did not replace a required owned result.

The following block is copied directly from gate-test-evidence.py, rereading the retained logs without rerunning tests:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk/criterion3/evidence/remote/logs logs=500 logs_with_results=434
test_counts: PASS=112682 FAIL=0 SKIP=340
  (top-level result lines, column 0, summed over the 500 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestManagedDoltGovernorBoundsOneLogicalBdCall PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-f679cd2551fcc64525aa.log)
  TestManagedDoltGovernorControllerWaitWindow PASS (integration-packages-568812e4d525bdd1f974.log, unit-23e7d9fc763496dd8095.log)
  TestManagedDoltGovernorDedupesAndJoinsRetryPathRecover PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-cd6930bb0d0da8646500.log)
  TestManagedDoltGovernorHoldSurvivesLivePort PASS (integration-packages-074fc6c34a1fb2577127.log, unit-f93a0ca96ccc61d0cae9.log)
  TestManagedDoltGovernorHoldsAfterFailedFlight PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-abd97d2cb9b450225d57.log)
  TestManagedDoltGovernorJoinerCancelLeavesFlightRunning PASS (integration-packages-75af79934639abaae69c.log, unit-912ab643330b35bd0b6b.log)
  TestManagedDoltGovernorReentrantEntryRunsDirectly PASS (integration-packages-cb110414c557c0928e82.log, unit-4d20b3d28497f844aa39.log)
  TestManagedDoltGovernorRetryRecoverHonorsLegacyCooldown PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-5a9df316125cd0aeff2e.log)
  TestManagedDoltGovernorRetryRecoverJoinIsWindowed PASS (integration-packages-bc482d2ae768251ad47f.log, unit-2ef13e1b6898c5544764.log)
  TestManagedDoltGovernorRetryRecoverTakesProviderSlot PASS (integration-packages-509ef0a6cd81866833cc.log, unit-10cc7795c0ec199a5530.log)
  TestManagedDoltGovernorSharesOneFlightAcrossConcurrentEnvBuilds PASS (integration-packages-821a75e4a503601af052.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestManagedDoltGovernorShutdownEndedFlightSetsNoHold PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-088cc38d26d094729229.log)
  TestManagedDoltGovernorWindowExpiryIsNotContextError PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2bde55fab377f50b4739.log)
GATE_TEST_EVIDENCE_RESULT ok: 13 diff-owned tests verified by name
```

## Fast lanes and acceptance details

policy_lane: PASS — pinned format, shell policy guards, active hooks, whole-tree nogo/lint, and 11 policy Bazel results. drift_lane: PASS — make bazel-sync plus empty tracked/untracked diff and six generated-artifact checks, then OpenAPI compatibility. Every lane used a fresh view; all 36 retained fast artifacts and normalized logs were hash-verified.

policy_attribution: make complexity-check -> ga-q7p6is, the pre-existing stale baseline. Independent pristine-base and candidate invocations both exit 2 and print the identical 238 complaint lines. The four protected scores are unchanged: CityRuntime.run 65, shutdown 24, healthBeadsProviderContext 34, resolvedRuntimeCityDoltTargetContext 22. make complexity-diff against the pinned base exits 0 with no threshold changes. This uses criterion 3b's explicit baseline-policy mirror rule; no threshold or baseline was changed and no waiver was used. Proof and raw logs: `/var/tmp/deploy-ga-w8wc5mq.gvpl96hk/complexity/proof.json`.

TDD provenance resolved from git: RED `d7a83eb2a11b9673d45db10c35be376dd471b050`, GREEN `8e95b5eacf09fa9a61a9f0d8a4527b3a6ca9b861`, reviewed head `739717cbc07548465f66006f43ac3dd162dc16af`; RED is an ancestor of GREEN and GREEN of the reviewed head. The post-green context/join refinements are included in the reviewed source and in this gate's complete suite.

A2/A3 sensitivity demonstrations and operation-count measurements are reviewer evidence, copied verbatim into the PR's Maintainer notes from the deploy bead. Test 8's measured recover reduction is not enforced by its present separate assertions; ga-t0kn451 owns the non-blocking tightening. All 13 tests were independently executed here. A6's five names are governedManagedDoltHealth, governedManagedRecover, runGovernedManagedDoltEntry, runWindowedManagedDoltEntry, startGovernedManagedDoltFlight; if ga-7id4lg lands first, merge authority must require those names before merging this change. A7 uses Refs #7170.

All original evidence remains under `/var/tmp/deploy-ga-w8wc5mq.gvpl96hk` and `/var/tmp/gc-heavy-gate/runs/deploy-ga-w8wc5mq.gvpl96hk`. The detached driver finished 1/1 with zero failed units; its service is inactive/dead with MainPID 0 and Result success. No production binary deployment was performed. Merge belongs to MPR after exact-head clearance and a verified merge-request.
