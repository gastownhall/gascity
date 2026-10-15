# Hook readiness veto release gate — ga-0ur46m3

**Verdict:** **PASS**

Reviewed source: `a71b252dc7814c7ababc724b686663604089aad3`.
Pinned main: `30b8ac715e84c80759bbc09b7f733eaf5f4e2aed`.
Validated merge: `96cbd2e0a93a587add3876938578af88664b63f5`; tree `2ca6f790ae5bb42acbdabdace6b9a789fcb3eb21`.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS — ga-gy1omkg round 2 records PASS for the exact resolved reviewed source. |
| 2 | Acceptance | PASS for pre-merge checks: discovery and claim share the veto; three ready-blocking edge types, owner-store batching/memoization, bounded fail-open reads, resumes and passed-formula-step exception are exercised. Two deployed-binary checks remain open below. |
| 3 | Tests | PASS — all four full-scope remote phases, mandatory native floor, and all 22 owned tests passed. Final result lines: {'PASS': 112681, 'FAIL': 0, 'SKIP': 340}. |
| 4 | Review findings | PASS — zero unresolved HIGH findings; round-1 F1–F3 fixed and round-2 approved. |
| 5 | Clean branch | PASS — isolated branch cut at the exact reviewed source; clean porcelain status checked before writing this record. This record is the only added file and is committed before publication. |
| 6 | Merge with main | PASS — canonical materialized merge matches predicted tree; actual pre-commit hook, build and vet completed successfully. No self-rebase required. |
| 7 | Single theme | PASS — five cmd/gc files implement and test hook readiness agreement. |

## Fast lane

Policy/lint: PASS. Pinned formatter, nogo, shell policy guards and 11 policy Bazel targets passed. Generated drift: PASS. make bazel-sync followed by git diff --exit-code and an empty full porcelain status; six generated-artifact targets passed. OpenAPI breaking check passed against the pinned base. Each lane ran in its own fresh view through gate-base.sh run. Logs and BEPs: /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/fast.

## Test evidence

test_cmd_scope: full-suite
heavy_mode: none
waiver_ref: none
failure_attribution: none
test_exec_mode: remote-exec
test_log_dir: /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/criterion3/evidence/remote/logs

Remote phase BEPs independently show 484 remote executions, zero cached results, zero retries, all attempts inside their phase windows, and zero evidence errors. Farm result lines: 112660 PASS, 0 FAIL, 326 SKIP. All 22 owned tests passed by name; TestTutorial01 also passed in integration-packages. The mandatory native floor completed successfully; final combined result-line counts are recorded below. No owned top-up was needed.

The isolation wrapper reported `PASS-THROUGH -- NOT ISOLATED` for the scratch path. Remote tests ran in Bazel's remote environment; native-floor commands scrub live store/session selectors. No isolated-clone claim is made.

## Scope and provenance

Ancestry guard accepts exactly ga-0ur46m3 plus confirmed same-feature siblings ga-id9fqz and ga-55g0ci. Source branch builder/ga-gr1qi7 is provenance only. No API, controller, config or wire change. Manifest docs/PROJECT_MANIFEST.md is absent; the supplied release protocol, TESTING.md, Makefile and CI define this gate.

## Open post-merge acceptance

- Mayor/operator: after merge and binary deployment, repeat the live readiness census in both directions, including a passed formula step carrying gc.work_outcome=blocked; check the motivating ga-o04bfr.1.2 row. A live binary deployment requires the city deploy-point rule and verifying GC_REAPER_SESSION_PURGE_AGE.
- After PR #6469 lands, compare hook filtering against pool-resume readiness.
- Mayor: repair stale follow-up gating edges for ga-5ya83x, ga-74bv8w and ga-isgdxm; notify the architect after landing. Non-blocking mutation follow-up ga-pl4s6y2 remains separate.

## Final execution evidence

test_cmd: gate-criterion3.sh, with make check; acceptance plus solo targets; all integration packages; full test/integration without a smoke filter; mandatory native floor.

test_counts: {'PASS': 112681, 'FAIL': 0, 'SKIP': 340} (top-level result lines, summed per job; subtests excluded).

test_exec_bep: /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json. Each phase predicate requires remote strategy, no cached flag, attempt 1, phase-window start and a terminal verdict.

| Phase | Targets passing | Remote executions | Cached / retried |
|---|---|---|---|
| unit | 283 | 368 | 0 / 0 |
| acceptance | 7 | 24 | 0 / 0 |
| integration-packages | 30 | 78 | 0 / 0 |
| integration-full | 1 | 14 | 0 / 0 |

rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}.

fallback_reason: null.

admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 91.85984420776367, "waited_seconds": 0.001}.

farm_test_seconds: 6859.364.

owned_topup: [].

floor_topup: complete=true; /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/criterion3/evidence/remote/floor-topup.json records every command and exit code. Required scope includes every environment-absence SKIP by owning package, internal/beads/beadstest, Darwin compile and both runner self-tests.

skip_inventory: /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/criterion3/evidence/remote/skip-inventory.json; farm reason classes {'other': 255, 'environment-absence': 71}. Non-owned skips are lane guards, platform exclusions, absent optional providers/services/tools or explicit unsupported capability cases. The native floor rechecks all environment-absence rows and retains its remaining skips. No owned test skipped; none of the skip sites or helpers is changed by this cmd/gc-only hook diff.

Native skip inventory: /var/tmp/deploy-ga-0ur46m3.cc1bz2we/native-skip-inventory.json records all 36 native SKIP rows including subtests. Its 14 top-level skips are three replay/coverage/herdr opt-ins in cmd/gc, six live herdr opt-ins, four tmux binding cases requiring an enclosing tmux session, and one unavailable br adapter. Remaining subtest skips cover optional PostgreSQL and intentionally unavailable provider-conformance environments. All are outside the changed test bodies and retain their printed reasons.

Load observations (remote admission bypasses the local wait; the native floor keeps threshold 15 and the bounded 1800-second soft wait):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=32.04 run_max_load=58.56 run_mean_load=44.09 run_readings=10 wait_first_load=86.01 wait_max_load=86.01 wait_mean_load=45.38 wait_readings=61 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=91.69 run_max_load=98.30 run_mean_load=56.50 run_readings=94 wait_first_load=91.69 wait_max_load=91.69 wait_mean_load=91.69 wait_readings=1 read_errors=0
```

## Owned tests, verified from retained output

```text
diff_tests_executed:
  TestBestStoreWithWorkSelectsTheLegWithServableWork PASS (integration-packages-251f1cf8cede4438b2d5.log, unit-f679cd2551fcc64525aa.log)
  TestBestStoreWithWorkVetoesRowsFromAQueryThatIgnoresTheLegsStore PASS (integration-packages-75f7330b9b673df65e4f.log, unit-2bde55fab377f50b4739.log)
  TestClaimStoreWithFallbackMovesOffALegWhoseRowsAreAllVetoed PASS (integration-packages-568812e4d525bdd1f974.log, unit-23e7d9fc763496dd8095.log)
  TestDemandServeAgreement_BlockedOutcomeBlocker PASS (integration-packages-cb110414c557c0928e82.log, unit-2ef13e1b6898c5544764.log)
  TestFilterBlockedOutcomeHookCandidatesFailsOpenAndReportsWhenBlockerReadFails PASS (integration-packages-12697e2ea0d32c278115.log, unit-1ab526405ace21879f85.log)
  TestFilterBlockedOutcomeHookCandidatesKeepsRowWhoseBlockerIsUnresolved PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-423c6a0da1986b90e9a7.log)
  TestFilterBlockedOutcomeHookCandidatesMakesOneBatchedReadKeyedByDistinctBlocker PASS (integration-packages-b72a67a2f9444af705e3.log, unit-3d6d3036a2f5902472b5.log)
  TestFilterBlockedOutcomeHookCandidatesPassesThroughUndecodableInput PASS (integration-packages-e5330b8c981927473156.log, unit-637c74c578a2b17fc654.log)
  TestFilterBlockedOutcomeHookCandidatesPreservesSurvivorsAndTheirOrder PASS (integration-packages-9326181a736eb30b71ca.log, unit-d9e667428f972d067bd3.log)
  TestFilterBlockedOutcomeHookCandidatesTouchesNoStoreWithoutReadyBlockingEdges PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-27b23e708b477fce4dad.log)
  TestFilterBlockedOutcomeHookCandidatesVetoDecision PASS (integration-packages-830685c60d0c2dd88567.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestHookBlockerReaderBoundsEveryReadWithATimeout PASS (integration-packages-074fc6c34a1fb2577127.log, unit-f93a0ca96ccc61d0cae9.log)
  TestHookBlockerReaderDoesNotReadABlockerNoLegOwns PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-abd97d2cb9b450225d57.log)
  TestHookBlockerReaderReadsASharedBatchOnceAcrossLegs PASS (integration-packages-e4245ddf6a37030111bc.log, unit-752a0bf25b15b465517a.log)
  TestHookBlockerReaderReadsEachBlockerFromTheStoreThatOwnsIt PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-fd986cf5bab8709b8d45.log)
  TestHookBlockerReaderRemembersAnAbsentBlocker PASS (integration-packages-821a75e4a503601af052.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestHookBlockerReaderRetriesAfterAFailedRead PASS (integration-packages-75af79934639abaae69c.log, unit-912ab643330b35bd0b6b.log)
  TestNoHookServeEntryPointBypassesTheBlockedOutcomeVeto PASS (integration-packages-cb110414c557c0928e82.log, unit-4d20b3d28497f844aa39.log)
  TestWithHookBlockedOutcomeVetoFailsOpenAndReportsOnStderr PASS (integration-packages-509ef0a6cd81866833cc.log, unit-10cc7795c0ec199a5530.log)
  TestWithHookBlockedOutcomeVetoLeavesAnUnvetoedOutputByteIdentical PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-cd6930bb0d0da8646500.log)
  TestWithHookBlockedOutcomeVetoPropagatesARunnerErrorUntouched PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-5a9df316125cd0aeff2e.log)
  TestWithHookBlockedOutcomeVetoRemovesVetoedRowsFromTheLegsOutput PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-088cc38d26d094729229.log)
GATE_TEST_EVIDENCE_RESULT ok: 22 diff-owned tests verified by name
```

Full execution record: /var/tmp/gc-heavy-gate/runs/deploy-ga-0ur46m3.cc1bz2we/criterion3/evidence/execution.json. Raw test logs and artifacts are retained; no reviewer result was substituted.

Patch carryover: recomputed whole-diff stable patch-id db156dccb04e9a862aa48aa3d3bd2e1c274d971e matches review. No rebase or source substitution. PR #6469 was independently verified OPEN during this gate; its deferred acceptance remains open.
