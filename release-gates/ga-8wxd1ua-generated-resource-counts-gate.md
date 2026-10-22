# Generated per-file resource census release gate

**Verdict:** **PASS**

Bead: ga-8wxd1ua. Build: ga-nnzehf. Review: ga-4nla8cl.
Evaluated independently by gascity/deployer on 2026-10-15T14:20:25.135920+00:00.
DEPLOY_MODE: remote. Push remote: fork (quad341/gascity); base repository: gastownhall/gascity.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — live review records verdict pass on exact source `05f687ce8df1728491cc94dd5fcf85e72460a14a`; no review carryover required. |
| 2 | Acceptance criteria met | PASS — all 14 checks below, independent old-base proof and merge experiments; PR content checked against the exact protocol. |
| 3 | Tests pass | PASS — fast lanes first, four complete forced remote phases and mandatory native floor; all 61 diff-owned tests PASS by name. No waiver or failure attribution. |
| 4 | No high-severity findings | PASS — 0 unresolved HIGH/blocking review findings; ga-6n31ofp is the non-blocking test-strength follow-up. |
| 5 | Final branch clean | PASS — reviewed source and merged test tree clean; deploy addition is this gate record only. Hooks run on the gate commit and final clean status verified before push. |
| 6 | Clean divergence from main | PASS — canonical merge builds and vets at pinned gate base; fresh merge and both census repository checks also PASS against current main below. No self-rebase. |
| 7 | Single feature theme | PASS — one resource-census v3 cutover; 18 reviewed source paths, no independent feature or CI-config change. |

## Commit and tree identity

- Reviewed source, resolved as a commit: `05f687ce8df1728491cc94dd5fcf85e72460a14a`.
- Isolated deploy branch: `deploy/ga-8wxd1ua-gate`, cut at that exact source; provenance branch is not a push target.
- Pinned full-gate base: `00709e32f4bd48a8f9a51c85648609f0ebcf2d10`.
- Canonical scratch merge: `cb29884913f849ecd4820e90ee400a1d8f32623c`; tree `2c8cbcc3d38326eb6e2ed01a08b78d2e7e4aaa92`.
- All required lanes use that immutable merge and base via gate-base.sh. Reviewer runs are not credited.
- Fresh current-main check: `30b8ac715e84c80759bbc09b7f733eaf5f4e2aed`; clean merge tree `5641896c49fd3817dd327d77dbe6d6c23ed35df0`. Both repository census tests PASS (2 PASS, 0 FAIL, 0 SKIP), with unchanged SHA256-verified census-binary inputs. This is supplemental focused freshness evidence; the full gate remains pinned above.
- Own-code preflight found no associated PR. No contributor PR read or modified.
- No PROJECT_MANIFEST.md exists in this tree; canonical TESTING.md, Makefile, Bazel workflow and the deploy criteria define the lanes.

## Acceptance checks

| AC | Check | Result |
|---|---|---|
| 1 | Canonical gates | PASS — make check and all four Bazel phases PASS; active hooks, BUILD drift and docsync PASS. |
| 2 | Old-base equivalence | PASS — Independent throwaway TestDeployProbe_BaseCountsEquivalence PASS before stored fields were removed: 27 rows, 605 records, 432 files, 2095 governed and 101 ungoverned occurrences. Output carried in PR body. |
| 3 | Canonical generated counts | PASS — TestRepositoryCountsFileIsCanonicalAndDerivesEveryCountedRow, round-trip and idempotent-update tests PASS. |
| 4 | Strict parser | PASS — TestParseCountsRejectsMalformedFiles and conflict-marker tests PASS with all requested malformed cases. |
| 5 | Validator diagnostics | PASS — Growth/staleness/missing/extra/tagged/Small-only/cap/summary/structural/failure-ending tests PASS; all owned names below. |
| 6 | Update path | PASS — Temp-directory write/idempotence/atomicity/refusal/no-write tests PASS. |
| 7 | Hermetic code | PASS — TestCountsSourceIsHermetic PASS; counts implementation adds no exec/clock dependency. |
| 8 | Stored counts removed | PASS — Targeted removed-field parser rejection and TestParseLedgerRejectsTheRemovedPerRowCounts PASS; source structural check confirms removed fields. |
| 9 | Policy v3 | PASS — Version, exact row sets, sorting, owner and policy-drift tests PASS; data-only policy move verified in review, policy values retained. |
| 10 | Owner liveness | PASS — Seven TestCensusOwnerLivenessCheck tests PASS in cmd/gc full lane. Owner-liveness script unchanged; architect ruling ga-cp3hwi read back OPEN. |
| 11 | Two merge experiments | PASS — Different tracked test files: clean merge, census PASS. Same test file: clean merge, expected source census grew calls=3 baseline=2; regenerate then PASS. Independent scratch branches never pushed. |
| 12 | Documentation and scope | PASS — 18 source paths match reviewed theme; listed comments/docs updated; excluded policy-test files, existing release gates, .gitattributes and CODEOWNERS unchanged by source. This deploy adds its required gate record only. |
| 13 | PR cutover evidence | PASS — PR body contains why, AC2/AC11 proof, exact protocol, no-window/no-comment statement, unchanged expiries and optional CODEOWNERS sentence. Mayor heads-up precedes any auto-merge; auto-merge belongs to MPR and is not enabled by deployer. |
| 14 | Testing guide | PASS — Updating the resource census heading, update/conflict protocol and banks-growth statement checked by TestTestingMDCarriesTheSectionTheFailureMessagePointsAt and TestRebaseProtocolIsOneParagraphWithFourNamedSteps; legacy-field parse recovery tested. |

## Fast lanes and merge validation

Criterion 6: `bazel build --config=remote-exec //cmd/gc:gc_test`, `make vet`, `make check-hooks` PASS on the canonical merge (full flags retained in c6.sh).

policy_lane: PASS — pinned `make fmt-check`; `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks test-ci-policy lint`; 11 policy Bazel results PASS, including formatting, core boundary, native dependency surface and CI coverage.

drift_lane: PASS — `make bazel-sync`, `git diff --exit-code`, empty tracked/untracked status; 6 generated schema/spec/dashboard/client/lockfile drift results PASS. `make openapi-breaking-check` against the pinned base PASS (1 result). Each lane ran through fresh-tree-run.sh in its own view, before runtime setup or suite.

ci_lane_run: n/a — no CI job, matrix, timeout or required-check change.
heavy_mode: none (classifier run and retained).

## Full-suite execution

test_cmd_scope: full-suite

test_cmd: gate-criterion3.sh through gate-base.sh and isolated-test-run.sh; four phases:

| Phase | CI-equivalent command | Remote results | Cached | Retries | Verdict |
|---|---|---:|---:|---:|---|
| unit | make check (`bazel test //...` plus shell guards/nogo) | 367 | 0 | 0 | PASS |
| acceptance | bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests | 24 | 0 | 0 | PASS |
| integration-packages | bazel test --config=integration //test:integration_packages | 78 | 0 | 0 | PASS |
| integration-full | bazel test --config=integration --test_arg=-test.v //test/integration:integration_test | 14 | 0 | 0 | PASS |

Each phase used --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --remote_download_outputs=toplevel, plus the retained phase-specific flags. No smoke filter or narrowed suite. Integration sets GC_FAST_UNIT=0 and executes the cmd/gc process coverage, including TestTutorial01 and the owned pinned-bd build test. Pinned test bd: v1.3.1.

test_exec_mode: remote-exec
fallback_reason: none
farm_test_seconds: 7361.747
criterion3_valid: true
waiver_ref: none (gascity has no waiver path)
failure_attribution: none

BEP predicate recomputed during final recovery: 483/483 terminal PASSED results, strategy remote, no remote/local cached flag, attempt 1, each start inside its own phase window. All 1002 saved test.log/test.xml artifact hashes rechecked across required and fast phases.

admission: {"class": "remote", "floor_gib": 25.0, "free_gib": 118.22658920288086, "waited_seconds": 0.001, "cap": 2}

rbe_state: {"rc_marker_present": true, "kill_switch_absent": true, "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}

owned_topup: [] — all 61 owned names already executed and passed.

floor_topup: complete; 16 steps, all exit 0. Every environment-absence skip was rerun by package (parent selected for subtests), plus beadstest, Darwin compile and runner self-tests. push-gate-lock: 55 PASS/0 FAIL; local-concurrency: 25 PASS/0 FAIL; fsys Darwin compile exit 0.

Runtime prepared before suite: rootless podman socket active, testcontainer-sweep enabled, pinned dolthub/dolt-sql-server:2.2.0 present; Ryuk disabled with paired BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1.

Isolation truth: wrapper reports `PASS-THROUGH -- NOT ISOLATED` for the scratch checkout. This gate claims remote execution and immutable tree/base, not a clean-clone container.

Local native-floor load metrics (not farm load):

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=75.44 run_max_load=73.68 run_mean_load=61.09 run_readings=11 wait_first_load=39.84 wait_max_load=132.54 wait_mean_load=100.97 wait_readings=61 read_errors=0
```

load_threshold: 15; load_waited_seconds: 1801; load_wait_timed_out: true; run_start_load: 75.44; run_max_load: 73.68; run_mean_load: 61.09; run_readings: 11 (verbatim sampler values). The documented ordinary soft wait timed out then proceeded; this is not the heavy path or a waiver. Remote phases used remote admission without a host-load wait.

test_counts: 112202 PASS, 0 FAIL, 340 SKIP (top-level events summed over 499 retained per-job logs; a test run by several jobs counts once per job).

skip_inventory: 326 farm skip entries (71 environment-absence, 255 other); raw inventory and native reasons retained. Remaining native skips are unrelated replay/simulation inputs, live-herdr opt-ins, missing br, and tmux bind-key tests requiring an ambient tmux session. These do not touch the census behavior; the full scanner/parser/validator/update tests all execute. The unit-lane skip for TestBuildPinnedBDBinaryForTestsUsesGoModSource is covered by its actual integration PASS, not excused.

## Diff-owned tests

```text
diff_tests_executed:
  TestBuildPinnedBDBinaryForTestsUsesGoModSource PASS (integration-packages-514989cc035c0e9a8ff5.log; also SKIP: TestBuildPinnedBDBinaryForTestsUsesGoModSource in unit-cd6930bb0d0da8646500.log (PASS in another job))
  skip_reason: TestBuildPinnedBDBinaryForTestsUsesGoModSource (unit-cd6930bb0d0da8646500.log): cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage
  TestBootstrapPolicyOwnsListenerHelperDebt PASS (unit-776861a2f8c8f5484f43.log)
  TestBootstrapPolicyOwnsNetListenConfigDebt PASS (unit-776861a2f8c8f5484f43.log)
  TestBootstrapPolicyOwnsNetListenDebtAndExactMediumOwners PASS (unit-776861a2f8c8f5484f43.log)
  TestBootstrapPolicyOwnsNetListenPacketDebt PASS (unit-776861a2f8c8f5484f43.log)
  TestBootstrapPolicyOwnsSyscallListenDebt PASS (unit-776861a2f8c8f5484f43.log)
  TestBootstrapPolicyOwnsTmuxDebtAndExactMediumSetup PASS (unit-776861a2f8c8f5484f43.log)
  TestCollectExpiriesHandsEachRowToTheClockOnce PASS (unit-776861a2f8c8f5484f43.log)
  TestCollectExpiriesSkipsMalformedRows PASS (unit-776861a2f8c8f5484f43.log)
  TestCountsRoundTripIsCanonical PASS (unit-776861a2f8c8f5484f43.log)
  TestCountsSourceIsHermetic PASS (unit-776861a2f8c8f5484f43.log)
  TestDerivedCountsEqualTheScanForEveryCountedRow PASS (unit-776861a2f8c8f5484f43.log)
  TestDerivedSmallCountsDropMediumFromCallsAndFiles PASS (unit-776861a2f8c8f5484f43.log)
  TestGenerateCountsMediumFollowsTheSmallCountRule PASS (unit-776861a2f8c8f5484f43.log)
  TestGenerateCountsRecordsOnlyGovernedOccurrences PASS (unit-776861a2f8c8f5484f43.log)
  TestGenerateCountsRejectsPathsTheFormatCannotRender PASS (unit-776861a2f8c8f5484f43.log)
  TestGenerateCountsSortsBytewiseByPathThenResource PASS (unit-776861a2f8c8f5484f43.log)
  TestGeneratedLedgerBlockRoundTrips PASS (unit-776861a2f8c8f5484f43.log)
  TestParseCountsNamesTheConflictMarkerRecovery PASS (unit-776861a2f8c8f5484f43.log)
  TestParseCountsReadsEveryField PASS (unit-776861a2f8c8f5484f43.log)
  TestParseCountsRejectsMalformedFiles PASS (unit-776861a2f8c8f5484f43.log)
  TestParseLedgerAcceptsMediumAndSmallDebtRows PASS (unit-776861a2f8c8f5484f43.log)
  TestParseLedgerRejectsTheRemovedPerRowCounts PASS (unit-776861a2f8c8f5484f43.log)
  TestParseLedgerRejectsUndeclaredClassificationFields PASS (unit-776861a2f8c8f5484f43.log)
  TestPolicyIsVersionThree PASS (unit-776861a2f8c8f5484f43.log)
  TestRebaseProtocolIsOneParagraphWithFourNamedSteps PASS (unit-776861a2f8c8f5484f43.log)
  TestRenderCountsMatchesTheDesignExample PASS (unit-776861a2f8c8f5484f43.log)
  TestRenderMarkdownIsDeterministic PASS (unit-776861a2f8c8f5484f43.log)
  TestRepositoryCountsFileIsCanonicalAndDerivesEveryCountedRow PASS (unit-776861a2f8c8f5484f43.log)
  TestRepositoryLedgerMatchesCensusAndDocumentation PASS (unit-776861a2f8c8f5484f43.log)
  TestStructuralFailuresCarryTheRegenerateLineAndProtocol PASS (unit-776861a2f8c8f5484f43.log)
  TestTestingMDCarriesTheSectionTheFailureMessagePointsAt PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedIsIdempotent PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedRefusesOnAScanProblemBeforeWritingAnything PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedRefusesOnStructuralFailureBeforeWritingAnything PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedReplacesAFileWithConflictMarkers PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedSaysWhenTheSourceTreeIsReadOnly PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedWritesAtomicallyThroughATempFileAndRename PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedWritesNothingWhenTheDocumentationBlockIsMissing PASS (unit-776861a2f8c8f5484f43.log)
  TestUpdateGeneratedWritesTheCountsFileAndTheDocumentationBlock PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateAcceptsExactSourceRatchets PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateAgainstPolicyWiresReviewedHermeticPolicyAndSourceChecks PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateAllowsHistoricalNeedleToDifferFromASTCensus PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateAllowsNarrowerHistoricalCmdGCNeedle PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsAcceptsAMatchingFile PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsCapsRecordLinesAtTwenty PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsCatchesAnOccurrenceMovingOutOfAMediumTest PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsParseFailuresCarryTheRegenerateLineAndProtocol PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsPrintsAggregateLinesBeforeRecordLines PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsReportsATaggedMismatch PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsReportsGrowthAndStalenessPerRecord PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsReportsMissingAndExtraRecords PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateCountsReportsNonCanonicalFormLast PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateKeepsStructuralProblemsFatal PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRejectsBootstrapPolicyDriftBeforeLiveCensus PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRejectsMediumPolicyDriftBeforeLiveCensus PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRequiresExactMediumAndSmallDebtRowSets PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRequiresSortedMediumRowsAndNamesTheFirstOutOfOrderRow PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRequiresSortedReviewedHermeticRows PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateRequiresTheExactBootstrapRowSet PASS (unit-776861a2f8c8f5484f43.log)
  TestValidateUsesRawAuditAndExactMediumFilteredSmallDebt PASS (unit-776861a2f8c8f5484f43.log)
GATE_TEST_EVIDENCE_RESULT ok: 61 diff-owned tests verified by name
```

## Evidence retained

- Context and independent acceptance probes: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3`.
- Detached driver: `/var/tmp/gc-heavy-gate/runs/deploy-ga-8wxd1ua.fresh.ithqwvg3/fast-suite`; status complete, units 1/1, failed 0; service result success, ExecMainStatus=0, MainPID=0.
- test_exec_bep: `/var/tmp/gc-heavy-gate/runs/deploy-ga-8wxd1ua.fresh.ithqwvg3/criterion3/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json` with reports and phase windows in corresponding stdout.
- test_log_dir: `/var/tmp/gc-heavy-gate/runs/deploy-ga-8wxd1ua.fresh.ithqwvg3/criterion3/evidence/remote/logs` (499 logs retained; not deleted).
- skip_inventory: `/var/tmp/gc-heavy-gate/runs/deploy-ga-8wxd1ua.fresh.ithqwvg3/criterion3/evidence/remote/skip-inventory.json`; floor_topup: `/var/tmp/gc-heavy-gate/runs/deploy-ga-8wxd1ua.fresh.ithqwvg3/criterion3/evidence/remote/floor-topup.json` and floor-logs/.
- Freshness: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3/final-main-census.json` and raw stdout/stderr.
- AC2: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3/ac2-evidence/logs/ac2-63eb792021cbf5010614.log` (27 rows exact).
- AC11: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3/merge-experiment-evidence/results.json` and raw merge/update/check output.
- Final hashes/BEP: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3/final-artifact-verification.json`; by-name proof: `/var/tmp/deploy-ga-8wxd1ua.fresh.ithqwvg3/final-by-name.txt`.

## Landing contract

Rebase protocol for a census conflict (TESTING.md section "Checked source-level resource ratchets"): (1) discard your edits to bootstrapPolicy (internal/testpolicy/resourcecensus/policy.go), to the per-row count fields of test/test-resources.toml and to the TESTING.md census block, taking main's versions of all three; (2) re-apply any [[medium]] or [[reviewed_hermetic_body]] rows you added by hand, in sorted (package_dir, package_name, owner) order, in both policy.go and test/test-resources.toml; (3) run go test ./internal/testpolicy/resourcecensus -run TestRepositoryLedgerMatchesCensusAndDocumentation -update; (4) commit test/test-resource-counts.toml with the result, and never hand-edit or hand-merge it.

No separate cutover window and no comments on open PRs. Expiries are unchanged; renewal is separate. Target landing by 2026-10-23, outside the last 48 hours before 2026-10-31. A stale v2 rig ledger may produce an advisory owner-liveness skip until updated, never a hard failure. The mayor receives the PR heads-up before any MPR auto-merge. The deployer does not merge; clearance and merge-request pin the exact published head. Ruling ga-cp3hwi stays open; notify mayor and architect after actual landing. Non-blocking follow-up ga-6n31ofp does not hold this deploy.
