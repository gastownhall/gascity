# Release gate: t3bridge transport and startup bounds (ga-24ebll6u)

**Verdict:** **PASS**

Evaluated 2026-11-23T15:48:16.676470+00:00. Gate PASS authorizes opening the PR; merge remains with MPR after current-head CI.

- reviewed_commit: c0e6c34c40a5ff6efdf816b3abca329e34a829b3
- build_bead: ga-yd8r0p; review_bead: ga-8lp73ecu
- gate_base: 97c0c9b3b11bfb0fa1c404c50cfd5caf597fc856
- gate_head (reviewed source): c0e6c34c40a5ff6efdf816b3abca329e34a829b3
- tested_merge: 237f6a526eebcf377d764313b06d8b3d13cacd49
- tested_tree: 67fea5740cbac66f9e76dee322fcaa960901bb3b
- merge_state: clean (origin/main@97c0c9b3b11bfb0fa1c404c50cfd5caf597fc856)
- gate_base_kind: current-tip
- deploy_mode: remote; base_branch: main; push_remote: fork
- merge_validation: MPR must verify current head, merge state, CI / required and native Windows CI; this gate does not authorize merging.
- waiver_ref: none (gascity has no waiver path)

| # | Criterion | Verdict and evidence |
|---|---|---|
| 1 | Review PASS | PASS. ga-8lp73ecu records PASS on the exact source; reviewer-added-tests.sh independently accepted one additive test-only commit from d4b77f7b6b415270ea97777e1963020043415811. |
| 2 | Acceptance criteria | PASS. Stalled writes have a five-second default bound; cache locking excludes HTTP I/O and protects both cache read and store; startup checks each required phase boundary, rolls back its created worktree and returns the context error; reuse and retry behavior are preserved. Nine owned tests and the race check passed. The PR text includes Refs #7170 and the unverified token-independence assumption. |
| 3 | Tests and required CI | PASS. Full uncached remote unit, acceptance, integration-package, full-integration and separate clear-probe latency phases; all native environment top-ups complete. Counts, ownership, skip proof and all six CI-job accounts below. |
| 4 | High-severity findings | PASS. Review has no blocker or major/high finding. Its minor token-independence assumption is carried into the PR. |
| 5 | Final branch clean | PASS. Assigned and tested worktrees were clean before the gate record; the deploy branch contains the reviewed commits plus only this committed evidence record. The publisher verifies a clean tree before push. |
| 6 | Merge state and build/vet | PASS. Raw clean merge of the pinned base and source; build and normal nogo/vet terminal BEPs independently successful. No rebase or hand edit. |
| 7 | Single feature theme | PASS. Four reviewed paths: t3bridge implementation, its new test file and BUILD entry, plus the test-server resource census. One transport/startup bound theme. |

## Acceptance verification

- FR-06 / AT-T1: stalled single and multiple WebSocket candidates return within the injected write bound; the typed timeout and stalled-candidate error text are checked.
- FR-07 / AT-T2: cached callers proceed while another server fetches a token; the eventual cache store waits for its lock.
- FR-08 / AT-T3 / AT-T4: entry and all later required boundaries stop canceled startup; the created worktree is removed, the error wraps the context cause, and reuse has no added interruption point.
- AT-T5: the existing t3bridge suite and the existing auth/reuse/transient tests remain green. Auxiliary race: 22 PASS, 0 FAIL, 0 SKIP; instrumentation verified with compile actions.
- Token issuance independence is INFERRED, not verified (GAP-2): concurrent cache misses each POST, with no single flight. No new config, provider interface, API field, dependency or retry classification.

## Test execution

- test_cmd: gate-criterion3.sh canonical full-scope chain: make -k check (bazel test //...), acceptance Tier A, integration_packages, full integration excluding the latency names, then only those latency names, followed by native floor checks.
- test_cmd_scope: full-suite
- test_exec_mode: remote-exec (BEP test results); required native floor is recorded separately. No farm/local suite stitching or local fallback.
- flags on every remote phase: --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --test_env=GO_TEST_WRAP_TESTV=1
- test_counts: 158970 PASS, 0 FAIL, 618 SKIP (top-level markers summed across jobs, not distinct tests)
- farm_test_seconds: 13163.405
- heavy_mode: none; heavy_units: n/a; coverage_check: n/a (ordinary full-suite gate)
- delta_gate: shadow, decision=full, reason=not-gate-pr; no narrowing applied
- rbe_state: worker-env pins match; kill switch absent; certificate valid through notAfter=Jan  9 20:13:22 2027 GMT
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 64.27796173095703, "waited_seconds": 0.001}
- test_log_dir: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/c3/evidence/remote/logs
- test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 15, "results": 15, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 87, "results": 87, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 426, "results": 426, "retried": 0, "window_ok": true}}
- fallback_reason: none (no downgrade)
- skip_inventory: complete in execution.json; remote and native floor inventories retained with printed reasons; source/closure and named pinned-base comparisons below.
- owned_topup: []; floor_topup: complete, 16/16 commands exit 0
- latency_leg: complete; both budget tests PASS on attempt 1; probe clear; neither ran in the full-integration leg.
- failure_attribution: none; policy_attribution: none; ci_lane_run: n/a (no CI-config change)

diff_tests_executed:

- TestIssueWebSocketTokenCacheHitDoesNotWaitForAnotherFetch PASS
- TestIssueWebSocketTokenStoreWaitsForTheAuthLock PASS
- TestRPCCallOnceBoundsAWriteToAPeerThatStopsReading PASS
- TestRPCCallOnceNamesTheStalledCandidateInItsJoinedError PASS
- TestStartChecksCtxBeforeCreatingAWorktree PASS
- TestStartDoesNotCheckCtxInsideTheReuseBranch PASS
- TestStartRollsBackItsWorktreeWhenCtxExpiresDuringCreate PASS
- TestStartStopsAtEachPhaseBoundaryOnceCtxEnds PASS
- TestStartStopsBeforeAnyRPCWhenCtxIsCancelled PASS

## Skip justification

All 618 top-level SKIP markers are outside the nine owned tests. The full per-subtest inventory and printed reasons are retained, rather than copied into this record. Every skip source site is unchanged at the tested merge (319 unique sites; no source-file differences from the pinned base). The one SHARED-HUNKS entry is the added t3bridge test helper file. Import closures were computed for all 51 package/tag combinations with no unknown closure. Every skipped binary importing t3bridge was measured by name at the pinned base: five remote groups (264 skipped events) plus one native cmd/gc group. Each comparison reports ALL PRE-EXISTING, with zero new, changed-reason, diff-owned, missing-reason or unjudged rows.

The remaining skips have unchanged bodies and reasons and no shared/helper hunk in their test-binary import closure. Their recorded reasons cover static capability/fixture gaps, platform-only cases, subprocess-only helpers, opt-in live services, absent historical binaries and environment prerequisites. Native floor top-ups ran for every environment-absence classification; remaining non-owned top-up skips are inventoried. None exercises the added write bound, token cache changes or startup phase checks.

- integration-packages //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=68 judged=68 pre-existing=68 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
- unit //internal/runtime:runtime_test: COMPARE-SUMMARY head_skips=2 judged=2 pre-existing=2 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
- unit //internal/runtime/t3bridge:t3bridge_test: COMPARE-SUMMARY head_skips=1 judged=1 pre-existing=1 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
- unit //cmd/gc:gc_sim_exec_test: COMPARE-SUMMARY head_skips=4 judged=4 pre-existing=4 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
- unit //cmd/gc:gc_test: COMPARE-SUMMARY head_skips=189 judged=189 pre-existing=189 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING
- native cmd/gc: COMPARE-SUMMARY head_skips=36 judged=36 pre-existing=36 new=0 reason-changed=0 diff-owned=0 no-reason=0 not-judged=0  => ALL PRE-EXISTING

## Fast lanes and policy

- policy_lane: PASS. Normal make lint (nogo/vet), format, shell guards and strict trusted-base storeclass baseline/growth all succeeded. make -k check in the canonical unit phase also succeeded.
- drift_lane: PASS. make bazel-sync followed by git diff --exit-code and empty git status in a fresh tree; generated schema/spec/docs/command-census targets passed in separate fresh views.
- complexity-diff: informational, exit 0; Start grows 40 to 48 from the required context checks. No acceptance criterion names complexity-check, no required CI lane runs it, and the baseline file is unchanged. No attribution of this growth as pre-existing.
- gate-icu74: OK, all three recorded libraries verified before the format/native lane.
- Hooks: make check-hooks passed; the active pre-commit hook validates the staged record.

load evidence (host, native/policy lanes; remote suite admission has no host-load wait):

- canonical native floor: load_threshold=15; load_waited_seconds=0; load_wait_timed_out=0; run_start_load=12.21; run_max_load=13.15; run_mean_load=12.57; run_readings=8.

- LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.21 run_max_load=17.84 run_mean_load=15.76 run_readings=4 wait_first_load=24.34 wait_max_load=24.34 wait_mean_load=18.93 wait_readings=14 read_errors=0; log: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/storeclass.stderr.log
- LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.69 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=12.69 wait_max_load=12.69 wait_mean_load=12.69 wait_readings=1 read_errors=0; log: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/runner-policy.stderr
- LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.21 run_max_load=17.06 run_mean_load=15.30 run_readings=4 wait_first_load=13.21 wait_max_load=13.21 wait_mean_load=13.21 wait_readings=1 read_errors=0; log: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/windows-lane.stderr
- LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.21 run_max_load=13.15 run_mean_load=12.57 run_readings=8 wait_first_load=12.21 wait_max_load=12.21 wait_mean_load=12.21 wait_readings=1 read_errors=0; log: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/c3/evidence/remote/floor-topup.stdout

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-ga-24ebll6u.dwtzbn8d 97c0c9b3b11bfb0fa1c404c50cfd5caf597fc856 c0e6c34c40a5ff6efdf816b3abca329e34a829b3 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status. The check aggregator rolls up its needs and takes no independent status.

| Job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | All job run commands and env executed verbatim in a fresh merge-tree view; exit 0; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/runner-policy.stdout |
| windows-lane | LOCAL-PASS | All job run commands and env executed verbatim in a fresh merge-tree view; exit 0; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/windows-lane.stdout |
| credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true' |
| pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| changes | LOCAL-PASS | Trigger/closure command exit 0; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/closure.stdout |
| windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; actual Windows allowlist test-binary closure has no changed path; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/windows-native-reach.json |

- windows_reach: none (native Windows allowlist own closure; credential-provider-windows path filter does not match).
- deferred_to_pr_ci: windows-lane-test (non-linux-runner)
- not_covered: none
- host_gaps: none
- Merge requires CI / required green on the actual PR head and the deferred native Windows job successful.

## Retained evidence

Full execution record retained: 6058 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/c3/evidence/execution.json; SHA256 0a5d9e1e72f4563ef4e7fbb0e3bf27362d27cbcf26fe13487378449eb84984ed.
Full independent remote audit retained: 94 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/remote-independent-audit.json; SHA256 b79d8199424c545094aedb47009fad1efe4c9a529485d12bd2e415cf824b418b.
Full canonical per-name results retained: 15 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/canonical-terminal-by-name.txt; SHA256 6fc47a3dc9f317db676ce4bc7690aad5fa0a71eaacc6a3e9b7558d6b2bf7e7ed.
Full skip source/ownership proof retained: 5426 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/skip-source-audit/evidence.json; SHA256 601636e58db8d8fe96d96e0adf5cf7834fa83d6123637b885acd3151625bfeb7.
Full skip import closures retained: 920 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/skip-import-closures.json; SHA256 46e738a41c6368a2714014e423cfdd1f0f4366d06287429e029df42e8decdccd.
Full base skip comparisons retained: 264 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/base-skip-evidence/report.json; SHA256 490e449ddb1cf84f693b75680a0d5928dbbb557df522ae1cfea5bdc2b50c7d28.
Full required-CI commands and accounting retained: 119 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/final-accounting.json; SHA256 c9f007282d7d40a45f49aeb1c6f2e407ed50a557c8cee6d9fc5aeaddb2abbd16.
Full acceptance map retained: 39 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/acceptance-map.json; SHA256 437c7f45c482ab38c45ec523207fd3e62b6a9e4615397ad723685f0dea25810d.
Full race proof retained: 20 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/race-verified.json; SHA256 e7bdf37a002b13be4b244248598f9966083aa48b4c8990fd1bf20faf2563b9b2.
Full strict storeclass proof retained: 59 lines; /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/storeclass-proof/verified.json; SHA256 d8a7394d3454a57b5f7654e640716ef8e4b05ed6c7e05f0d5cca7c44d76d988f.

Push routing: origin HEAD dry-run exited 1 (role branch non-fast-forward); fork selected by the deploy protocol. The fork trigger/closure command was independently rerun and produced byte-identical accounting scope; raw output: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/required-ci/closure-fork.stdout.

PROJECT_MANIFEST.md is absent in this checkout (no matching tracked manifest path); release criteria are the supplied seven-criterion protocol, with canonical commands resolved from TESTING.md, Makefile and the merged CI workflows.

Publication-time recheck independently re-read all 554 remote result actions and verified all 1108 retained artifact SHA256/CAS hashes: /var/tmp/gc-deploy-ga-24ebll6u.et71glx7/publication-remote-recheck.json; SHA256 76bff9980053a65a7a9cd437f580cfc29224ab8624ca6272a978425d3a1a1652.
