# Release gate: reconciler trace instrumentation

- Deploy bead: `ga-o5s2sy`
- Source bead: `ga-ihbl3e.1`
- Reviewed commit: `505a1f3d517517d3b94ddac7fe86159f46ae1d5b`
- Base: `origin/main@132ba4fc5729479978645b8fdad162e3c9aa9670`
- Existing reviewed PR: `https://github.com/gastownhall/gascity/pull/6238`
- Deploy mode: `remote`; push remote resolved to `fork`
- Result: **PASS** with five non-diff-owned test failures attributed to predating open trackers.

## Pre-flight

- The recorded SHA resolved as a commit to the same full 40-character value.
- PR #6238 is open and unmerged at exactly the reviewed SHA. It is internally authored by `quad341`; its only interactions are two internal `quad341` review comments.
- The current-head mpr review marker is `verdict=auto-merge`; the earlier reviewed head carried `verdict=fix-merge`. No external contributor interaction is present.
- The commit range is one feature theme: trace instrumentation and its call-site/test plumbing for the session-sync and orphan-release phases of the controller reconciler.
- The repository has no `docs/PROJECT_MANIFEST.md`; the standard seven deploy criteria and `engdocs/contributors/release-gate-criteria-conventions.md` were applied.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The recovered deploy record states `Reviewed + PASSED` for the exact SHA. PR #6238 has an internal mpr review marker `head=505a1f3d517517d3b94ddac7fe86159f46ae1d5b verdict=auto-merge`; the PR head still equals that SHA. No review carryover applies. |
| 2 | Acceptance criteria met | **PASS** | Source inspection confirms distinct `recordPhase` spans for `sync_beads_and_update_index.load_existing` and `.load_visible_by_session_name`, including error paths; `release_sweep` records `memoized_count`, `fallback_count`, and accumulated `probe_ms`. Nil recorders preserve non-controller call sites. Twenty-four affected tests each passed twice. The requested live-trace confirmation is explicitly post-merge operational follow-up on parent `ga-ihbl3e`, not a pre-merge implementation criterion. |
| 3 | Tests pass | **PASS** | The documented full command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel` ran on the exact reviewed SHA. Rootless Podman 5.8.4 was healthy and cached Dolt testcontainer images included `dolt-sql-server:1.32.4`. Result: **35/40 jobs PASS, 5 FAIL, 0 SKIP**; **46,002 top-level PASS, 5 FAIL, 209 SKIP**. All 24 diff-owned test functions ran in both the fast and process/full paths: **48 PASS, 0 FAIL, 0 SKIP**. Every raw failure is attributed below under criterion 3a. The 209 skips are suite-controlled opt-ins/platform/provider exclusions; none is diff-owned. `test_cmd_scope: full-suite`; `waiver_ref: n/a`; `ci_lane_run: n/a (no CI-config change)`; `policy_lane: make test-ci-policy — PASS`; `make vet — PASS`. Logs: `/var/tmp/ga-o5s2sy-full.log` and `/var/tmp/gc-local-tests.7nPFHN`. |
| 4 | No high-severity review findings open | **PASS** | PR #6238's current-head mpr verdict is `auto-merge`, with no unresolved HIGH finding and no GitHub review requesting changes. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty before this checklist was written; `git diff --check origin/main...HEAD` passed; `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | Against `origin/main@132ba4fc5729479978645b8fdad162e3c9aa9670`, the candidate is 17 behind / 3 ahead. `git merge-tree --write-tree origin/main 505a1f3d517517d3b94ddac7fe86159f46ae1d5b` returned 0 with tree `74c2606b4d1f45c92f75dbf26952fc961a236ef7`. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | All production changes instrument the controller's session-sync/orphan-release path; the remaining test-file edits are call-site plumbing and coverage for that same instrumentation. |

## Diff-owned test evidence

Every function below reported PASS twice within the full-suite output, with no FAIL or SKIP:

- `TestExecutionStalledDrainConvergesToAReclaimableRow`
- `TestExecutionStalledDrainDoesNotStrandAMidDrainWake`
- `TestOrphanReleaseSparesALiveHoldersBindingResidentClaim`
- `TestProductionOrderDeferredSingletonAliasReclaimsOnSecondTick`
- `TestReleaseOrphanedPoolAssignmentsFallsBackToWorkStoreForLiveness`
- `TestReleaseOrphanedPoolAssignmentsReadsLivenessFromSessionStore`
- `TestReleaseOrphanedPoolAssignmentsReadsLivenessFromWorkOwnerStore`
- `TestReleaseOrphanedPoolAssignmentsReleasesWhenNoStoreHoldsTheSession`
- `TestReleaseOrphanedPoolAssignmentsReopensStaleSlotFormClaim`
- `TestReleaseOrphanedPoolAssignmentsStillReleasesWhenSessionStoreSaysDead`
- `TestReleaseOrphanedPoolAssignmentsWhenSnapshotsComplete_PartialSkipsCompleteReleases`
- `TestReleaseOrphanedPoolAssignments_EmptyProtectedSetStillReleases`
- `TestReleaseOrphanedPoolAssignments_NilSessionsStoreFallsBackToWorkStore`
- `TestReleaseOrphanedPoolAssignments_OwnerStoreMemoDoesNotCollapseDistinctStores`
- `TestReleaseOrphanedPoolAssignments_ProtectionDoesNotCrossStoreIDCollision`
- `TestReleaseOrphanedPoolAssignments_ProtectsAliasAssignedRigWork`
- `TestReleaseOrphanedPoolAssignments_RecordsMemoizedVsFallbackBranchCounts`
- `TestReleaseOrphanedPoolAssignments_RetainsProtectedWakeWork`
- `TestReleaseOrphanedPoolAssignments_SkipsLiveAssigneeStaysAssigned`
- `TestReleaseOrphanedPoolAssignments_SkipsLiveModernPoolSessionWhenLiveListMissesIt`
- `TestSyncDoesNotMintDuplicateForSameCycleSingletonCreate`
- `TestSyncSessionBeadsWithSnapshotAndRigStoresLeavesOrphanedSessionBeadOpenWhenRigStoreWorkAssigned`
- `TestSyncSessionBeadsWithSnapshotAndRigStoresRecordsLoadExistingPhase`
- `TestSyncSessionBeadsWithSnapshotAndRigStoresRecordsLoadVisibleBySessionNamePhase`

`diff_tests_executed: 48 PASS, 0 FAIL, 0 SKIP (24 functions, each selected twice)`

## Failure attribution

- `failure_attribution: TestProxyProcessTickRetriesPublicationRefreshWithoutLosingCurrentURL -> ga-3l0vyy | clause 3(a): internal/workspacesvc cannot import or execute the changed cmd/gc program; clause 4: no path overlap`
- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a): the candidate cannot alter internal/bdflags or the installed bd binary; clause 4: no path overlap`
- `failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-cp7r41 | clause 3(a): the external dolt init subprocess was killed before candidate sweep code ran, and the candidate changes neither the fixture, internal/doltorphan, nor Dolt; clause 4: no path overlap`
- `failure_attribution: TestSQLiteWriterFenceUsesExactKernelLockModes/WAL_without_SHM -> ga-vkhfnj | clause 3(a): internal/storebinding/sqlite cannot import or execute cmd/gc; exact predating ga-nxgims signature; clause 4: no path overlap`
- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a): the proven failure path is suspend-driven retirement in build_desired_state.go/session_reconciler.go, which this instrumentation diff does not touch; clause 4: no test/integration path overlap`

Each sighting was appended to its tracker and read back after the run. The diff changes no resource-census baseline, build-file test target, CI job, matrix, timeout, or required-check list.

## Disposition

All seven criteria pass. Cut isolated branch `deploy/ga-o5s2sy-gate` at the reviewed SHA, commit this checklist, push to the fork, open the deploy PR, publish `release-gate/deploy-clearance=success` on its exact head, and route the merge-request to mayor. The deployer does not merge.
