# Release gate: account for live external worktrees in reconciler capacity

**Verdict:** **PASS**

- Deploy bead: `ga-qw96c9`; build bead: `ga-1xaqgo.3`; review bead: `ga-8nlp4r`.
- Reviewed source: `18137b75509a5a4402290843d057da4334783b0b`.
- Current main: `f48cec1a6ecd253c745b6a353369b54ee56db74a`.
- Current merge scratch: `fde9137f09a1e31c93d68ffc863d7f4ecd775d0c`, tree `5fea72cd7d6a3241960c03cf4bc4568c574a4f72`.
- Gate method: full suite on main `ded4b491745f2bba8a17736b0241915180413183`; mayor-authorized scoped carry-forward on main `9a7faec4406da5408e6e25f5b5a3b0871ab89c17`; current cmd/gc interaction recheck on `f48cec1a6ecd253c745b6a353369b54ee56db74a`. Rulings: mayor mails `gm-wisp-lj8w17` and `gm-wisp-x244ak`.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-8nlp4r` closed with `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | The new capacity seeding matches live external worktree paths from both `gc.work_dir` and legacy `work_dir`, consumes the shared liveness result, counts occupancy under the configured cap, and creates no session for or terminates external work. Tests cover live/stale and unrelated template cases, one scan per build, no teardown, priority, and cap cascade. |
| 3 | Tests pass | PASS | Full suite, 9a7 scoped recheck, d653 deterministic lane, and 915a and f48 deterministic plus cmd/gc-wide rechecks PASS. One d653 shard failure was attributed with tracked, measured coverage evidence; f48 recheck has zero failures. See evidence below. |
| 4 | No high-severity review findings open | PASS | Review bead records no blocking or high-severity finding. Non-blocking follow-up `ga-quk26t` is tracked separately. |
| 5 | Final branch clean | PASS | f48 merge scratch clean; isolated deploy branch verified clean after the gate commit and before push. |
| 6 | Clean divergence from main | PASS | `git merge-tree --write-tree` against f48 returned tree `5fea72cd7d6a3241960c03cf4bc4568c574a4f72`; canonical materialized merge has this same tree and parents f48 plus the reviewed source. |
| 7 | Single feature theme | PASS | Four reviewed commits touch eight files, all under `cmd/gc`, for external worktree occupancy in reconciler capacity. |

## Test evidence

- `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=2` on the canonical merge tree of reviewed source plus `ded4b491745f2bba8a17736b0241915180413183`; `test_cmd_scope: full-suite`; `heavy_mode: none`.
- Full suite: 40/40 jobs passed, 55,321 top-level PASS, 0 FAIL, 236 SKIP. No candidate-owned test failed or skipped. The skipped tests are pre-existing platform, helper, and prerequisite-gated paths; none exercises a newly added or edited test body in this diff. Rootless Podman and the pinned test bd were available before the run.
- Full-suite load: threshold 15, waited 0 seconds, timeout 0, run start 9.72, max 32.27, mean 22.28, 121 readings.
- Deterministic lane at ded4: 12/12 PASS: hooks, Bazel BUILD sync and generated diff, pinned-base policy, format, pinned lint, build, vet, spec, schema, and clean tree. Load threshold 15, waited 90 seconds, timeout 0, run start 14.36, max 13.98, mean 11.03, 19 readings.
- Main moved from ded4 to 9a7 through #6925. Delta files: `cmd/gc/BUILD.bazel`, `cmd/gc/async_start_command_gate_test.go`, `cmd/gc/session_lifecycle_parallel.go`, and its release-gate document. None overlaps this bead's eight files. Mayor ruling `gm-wisp-lj8w17` required the full cmd/gc package and both sides' owned tests by name, twice, with deterministic checks carried forward from the previous full suite.
- 9a7 deterministic lane: 12/12 PASS. The six cmd/gc process shards plus productmetrics testhook and six cmd/gc integration shards passed. Process: 11,336 top-level PASS / 0 FAIL / 15 SKIP. Integration: 11,246 PASS / 0 FAIL / 115 SKIP. Each of the 22 candidate-owned tests and two #6925 async-start tests passed once in each lane, with no owned FAIL or SKIP. Load threshold 15, waited 480 seconds, timeout 0, run start 14.79, max 33.03, mean 23.71, 43 readings.
- Main moved from 9a7 through dd18 (new `internal/workqueue`, root `BUILD.bazel`) to d653 (#6928: `internal/beads` cache and `cmd/gc/api_state_test.go`). Neither move overlaps the candidate files; canonical d653 merge remains clean. The candidate does not import `internal/workqueue`. Mayor ruling `gm-wisp-x244ak` requires the full deterministic lanes and all cmd/gc process and integration shards on d653, with the candidate 22, #6925 two, and #6928 four cmd/gc owned tests each verified twice. It explicitly leaves `internal/beads` package tests to #6928 upstream CI and drops the obsolete dd18 workqueue package lane.
- d653 deterministic lane: 12/12 PASS: hooks, Bazel BUILD sync and generated diff, pinned-base policy, format, pinned lint, build, vet, spec, schema, and clean tree.
- d653 scoped process run: five cmd/gc process shards and productmetrics passed. Shard 4 failed only `TestHookTimeoutKillsTheWholeProcessGroup/control: without it the descendant survives` at `pool_hook_processgroup_test.go:86`; integration shards were not launched after the fail-fast. `failure_attribution: TestHookTimeoutKillsTheWholeProcessGroup/control -> ga-gy7798`, an open exact-condition tracker created before this run. Clause 1: the test file is untouched by the candidate. Clause 3(c), measured coverage: the focused test passed and every production function changed by the candidate reported 0.0%, including `liveExternalWorkDirSet`, `buildDesiredStateWithSessionBeadsAt`, `computePoolDesiredStatesAt`, `seedExternalLiveWorkOccupancy`, and `externalWorkBeadDir`. Clause 4: same-package overlap, but no new test file, test target, or census baseline in the candidate diff. The tracked failure is attributed under the non-diff-owned protocol. Coverage profile: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T1037Z/hook-control-coverage.out`.
- d653 scoped load: threshold 15, waited 1805 seconds and timed out, run start 23.17, max 23.63, mean 21.35, 16 readings. The high load does not itself decide attribution; coverage above does.
- Main moved again to 915a through #6929. Delta: `cmd/gc/BUILD.bazel`, `cmd/gc/reconcile_router.go`, and `cmd/gc/reconcile_router_test.go`; no candidate file overlap. Per mayor ruling `gm-wisp-x244ak`, deterministic lanes and all cmd/gc process/integration shards ran, with candidate 22, #6925 two, #6928 four, and #6929 30 tests verified twice on the 915a merge tree. Detached runs: `/var/tmp/gc-heavy-gate/runs/ga-qw96c9.915a-deterministic` and `/var/tmp/gc-heavy-gate/runs/ga-qw96c9.915a-scoped`.
- 915a deterministic lane: 12/12 PASS: hooks, Bazel BUILD sync and generated diff, pinned-base policy, format, pinned lint, build, vet, spec, schema, and clean tree.
- 915a scoped lane: six cmd/gc process shards and productmetrics passed (11,366 top-level PASS / 0 FAIL / 15 SKIP); six cmd/gc integration shards passed (11,276 PASS / 0 FAIL / 115 SKIP). Candidate 22, #6925 two, #6928 four, and #6929 30 named tests each passed exactly once in both lanes. Load threshold 15, waited 1,801 seconds, timed out=1, run start 17.89, max 23.27, mean 18.50, 26 readings.
- Main moved again to f48 through #4690: `cmd/gc/wisp_gc.go`, `cmd/gc/wisp_gc_test.go`, and three `internal/sling` files. None overlaps this bead's files; merge tree is clean. The candidate's cmd/gc code cannot be imported by internal/sling, so #4690's internal/sling tests remain its upstream CI evidence. Per mayor `gm-wisp-x244ak`, repeat deterministic and all cmd/gc process/integration shards; verify the candidate 22, #6925 two, #6928 four, #6929 30, and #4690's 10 diff-owned cmd/gc tests twice on the f48 merge tree before clearance.
- First f48 scratch attempt failed before policy evaluation because the throwaway commit subject did not match the required scratch materialization format; `gate-base.sh` refused it with rc 65. Recreated the identical tree with the required subject (`fde9137f09a1e31c93d68ffc863d7f4ecd775d0c`) and restarted the f48 lanes. The refusal was a setup error and produced no candidate test result.
- f48 deterministic lane: 12/12 PASS: hooks, Bazel BUILD sync and generated diff, pinned-base policy, format, pinned lint, build, vet, spec, schema, and clean tree.
- f48 scoped lane: six cmd/gc process shards and productmetrics passed (11,375 top-level PASS / 0 FAIL / 15 SKIP); six cmd/gc integration shards passed (11,285 PASS / 0 FAIL / 115 SKIP). Candidate 22, #6925 two, #6928 four, #6929 30, and #4690 10 named tests each passed exactly once in both lanes. Load threshold 15, waited 450 seconds, timeout=0, run start 14.77, max 30.01, mean 24.32, 43 readings.
- Mayor amended the convergence rule after the f48 run: the 915a PASS was already final for the local gate because #4690 had no file overlap and a clean merge tree. The f48 run above completed before that amendment was read; it supplies additional passing evidence. Fresh GitHub checks on the PR merge ref are still required.
- `policy_lane: make test-ci-policy` PASS at ded4, 9a7, d653, 915a, and f48.
- `ci_lane_run: n/a` for criterion 3c; this candidate does not modify CI configuration. Fresh merge-ref GitHub checks are still required by the mayor's scoped ruling.
- `waiver_ref: none`.

## Owned test results

| Source | Test | Process | Integration / package |
|---|---|---|---|
| Candidate | `TestBuildDesiredState_LiveExternalWorktreeOccupiesSoleSlotWithoutTeardown` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStatesCarriesWorktreeOwnerEvidence` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_CapsNewDemandBeforeMaterializingRequests` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveOccupancyAdmitsMostUrgentBeadFirst` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveOccupancyConsumesSuppliedLivenessWithoutRescanning` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveOccupancyDoesNotAffectUnrelatedTemplate` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveOccupancyNeverDisplacesRealSession` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveWorktreeLegacyKeySuppressesDuplicate` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ExternalLiveWorktreeSuppressesDuplicateNewSpawn` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_InFlightDemandRecordsTrace` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_InFlightDemandRecordsTraceWhenCapsSuppressReuse` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_InactiveExternalWorktreeDoesNotSuppressDemand` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_PostCreateProtectionAdvancesDemandIndex` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_PostCreateProtectionAllocatesDemandByTriggerIdentity` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_PostCreateProtectionBindingPreservesScaleDemandIndex` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_PostCreateProtectionRebindsUnmatchedConcreteDemand` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_TraceListsActiveCapacityBlockers` | PASS | PASS |
| Candidate | `TestComputePoolDesiredStates_ZeroDemandRecordsSkipDecision` | PASS | PASS |
| Candidate | `TestLiveExternalWorkDirSet_FailsClosedWhenScanIndeterminate` | PASS | PASS |
| Candidate | `TestLiveExternalWorkDirSet_GathersLivenessOncePerCallAcrossRigs` | PASS | PASS |
| Candidate | `TestLiveExternalWorkDirSet_IncludesLiveExcludesNotLive` | PASS | PASS |
| Candidate | `TestLiveExternalWorkDirSet_SkipsRigOnScanErrorContinuesOthers` | PASS | PASS |
| #6925 base delta | `TestAsyncStartCommandGate_OnlyAChangeDuringStartupIsStale` | PASS | PASS |
| #6925 base delta | `TestCommitAsyncStartResult_CommitsWhenStoredCommandUnchangedDuringStartup` | PASS | PASS |
| #6928 base delta | `TestControllerStateAppliesBeadEventsOnlyToOwningCache` | PASS | PASS |
| #6928 base delta | `TestControllerStateAppliesHyphenatedPrefixEventsOnlyToOwningCache` | PASS | PASS |
| #6928 base delta | `TestControllerStateBeadEventsRespectStorePrefixes` | PASS | PASS |
| #6928 base delta | `TestControllerStateBeadEventsUseScopePrefixWhenConfiguredPrefixDrifts` | PASS | PASS |
| #6929 base delta | `TestRouterSessionBeadEventEnqueuesItsRowAndAllocator` | PASS | PASS |
| #6929 base delta | `TestRouterReplayRoutesEnqueueOnlyAndNeverWrites` | PASS | PASS |
| #6929 base delta | `TestRouterWorkReassignmentEnqueuesOldAndNewAssignee` | PASS | PASS |
| #6929 base delta | `TestRouterAssignedWorkStatusChangeEnqueuesAssignee` | PASS | PASS |
| #6929 base delta | `TestRouterKeylessAndAllocatorKeysWakeAllocatorOnly` | PASS | PASS |
| #6929 base delta | `TestRouterControlDispatchKeyMapsToAllocator` | PASS | PASS |
| #6929 base delta | `TestRouterSessionIDKeyNeedsNoIndex` | PASS | PASS |
| #6929 base delta | `TestRouterUnresolvedNameWakesAllocatorAndRequestsResync` | PASS | PASS |
| #6929 base delta | `TestRouterSupervisorReloadWakesAllocatorAndResync` | PASS | PASS |
| #6929 base delta | `TestRouterObservationFlipPrefersOwnerThenNameThenAllocator` | PASS | PASS |
| #6929 base delta | `TestRouterDuplicateIdentityEnqueuesEveryHolder` | PASS | PASS |
| #6929 base delta | `TestRouterClosedSessionDropsIdentities` | PASS | PASS |
| #6929 base delta | `TestRouterRebuildDuringEventsLosesNothing` | PASS | PASS |
| #6929 base delta | `TestRouterPartialCensusKeepsPreviousEntries` | PASS | PASS |
| #6929 base delta | `TestRouterEventGapRequestsResync` | PASS | PASS |
| #6929 base delta | `TestRouterUndecodablePayloadWakesAllocator` | PASS | PASS |
| #6929 base delta | `TestRouterRelicSessionRowOnOtherLegIsCensusOnly` | PASS | PASS |
| #6929 base delta | `TestRouterMappingPanicIsRecovered` | PASS | PASS |
| #6929 base delta | `TestRouterRetiredRowGoneNameNeedsNoResync` | PASS | PASS |
| #6929 base delta | `TestRouterTombstonesAreBounded` | PASS | PASS |
| #6929 base delta | `TestRouterMinimalCloseOfIndexedSessionIsThatSession` | PASS | PASS |
| #6929 base delta | `TestRouterRebuildOverflowAbandonsAndRetries` | PASS | PASS |
| #6929 base delta | `TestRouterRebuildReplaysLogInOrder` | PASS | PASS |
| #6929 base delta | `TestRouterClosingOneHolderKeepsSharedIdentity` | PASS | PASS |
| #6929 base delta | `TestRouterAllocatorWakesCarryUrgency` | PASS | PASS |
| #6929 base delta | `TestRouterCountsLiveAndReplayApart` | PASS | PASS |
| #6929 base delta | `TestRouterUntrackedNameWakesOwner` | PASS | PASS |
| #6929 base delta | `TestRouterFailedLegKeepsEntriesReadableLegsSupersede` | PASS | PASS |
| #6929 base delta | `TestRouterRebuildEndsCleanlyAndResyncsOutsideItsLock` | PASS | PASS |
| #6929 base delta | `TestRouterIndexConsistentUnderConcurrentEventsAndRebuilds` | PASS | PASS |
| #4690 base delta | `TestWispGC_ClosesAbandonedSteplessUnclaimedRootPastTTL` | PASS | PASS |
| #4690 base delta | `TestWispGC_ClosesAssignedButUnclaimedSteplessRootPastTTL` | PASS | PASS |
| #4690 base delta | `TestWispGC_ClosesSteplessRootWhenAttachmentSourceTerminal` | PASS | PASS |
| #4690 base delta | `TestWispGC_DryRunDefaultDoesNotCloseSteplessRoot` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessClaimedRootPastTTL` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessExemptRootPastTTL` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessRoot` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessRootWhenAttachmentQueryFails` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessRootWithLiveAttachmentSourcePastTTL` | PASS | PASS |
| #4690 base delta | `TestWispGC_LeavesSteplessRootWithLiveGraphV2AttachmentSourcePastTTL` | PASS | PASS |

## Evidence paths

- Full suite and first deterministic run: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T0755Z`.
- 9a7 scoped recheck: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T0919Z`.
- d653 recheck: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T1037Z`.
- 915a recheck: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T1120Z`.
- f48 recheck: `/home/jaword/projects/gc-management/.gc/deploy-evidence/ga-qw96c9/20261002T1240Z`.
