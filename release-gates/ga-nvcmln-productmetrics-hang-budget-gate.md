# Release Gate: productmetrics hang-budget conversion

Refresh bead: ga-ssvpcw

Original deploy bead: ga-nvcmln

Review bead: ga-vzyz80

Implementation bead: ga-42mt5x.1

PR: https://github.com/gastownhall/gascity/pull/5535

Reviewed implementation commit: 1a76aa8a439bfa9d9e478f94f899c5d1e0b699b3

Refreshed PR head evaluated: 1db25fc8a5d9ad13011cb173c26f4ffa7119307d

Base evaluated: origin/main@26542454e5ea740ab512594f1df451d9a7a3e7a7

Synthetic merge tree: e87a56c5abdb84c0d07b60796b100133fc13e57e

Deploy branch: deploy/ga-nvcmln-gate

Gate date: 2026-09-07
Result: PASS

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate uses
the deployer release criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | The target PR is open and unmerged. `git merge-tree --write-tree origin/main 1db25fc8a5d9ad13011cb173c26f4ffa7119307d` exited 0 against `origin/main@26542454e5ea740ab512594f1df451d9a7a3e7a7` and produced tree `e87a56c5abdb84c0d07b60796b100133fc13e57e`. No self-rebase was needed. |
| 1 | Review PASS present | PASS | Review bead ga-vzyz80 records `verdict: pass` for the implementation commit. Maintainer review on PR #5535 also records `verdict=auto-merge` for the exact refreshed head `1db25fc8a5d9ad13011cb173c26f4ffa7119307d`. |
| 2 | Acceptance criteria met | PASS | The refreshed PR retains the reviewed test-only conversion: 28 productmetrics hang-detector waits use the package `hangBudget`; the remaining direct floors are live inputs or scenario values. `TestHangBudgetStaysAHangDetector` guards the floor and largest replaced multiplied shape. The refreshed head includes the prior PR head plus then-current main, and remains conflict-free against current main. |
| 3 | Tests pass | PASS | `LOCAL_TEST_JOBS=4 make test-local-full-parallel` ran the documented full 40-job suite: 38 PASS, 2 attributed raw FAIL, 0 SKIP/omitted. The unit-core job passed `internal/productmetrics`; a supplemental JSON-observable package run recorded 469 top-level PASS, 0 FAIL, 0 SKIP and all 17 diff-owned tests PASS. The required `productmetrics-testhook` job passed. See attribution below. |
| 4 | No high-severity review findings open | PASS | ga-vzyz80 records no security finding and only one non-blocking test-design advisory. The maintainer review on the refreshed head found no blocking change and marked it auto-merge. No external contributor has engaged on the PR. |
| 5 | Final branch is clean | PASS | The worktree was clean before this gate record was updated. `git diff --check origin/main...HEAD`, `make vet`, and `go build ./...` passed. The committed gate tip is rechecked clean before push. |
| 7 | Single feature theme | PASS | The implementation changes only seven files under `internal/productmetrics/`, all for the single test hang-budget conversion and its guard. The only other branch artifact is this release-gate record. |

## Test evidence

```text
test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel
test_cmd_scope: full-suite
test_counts: 38 PASS jobs, 2 attributed raw FAIL jobs, 0 SKIP/omitted jobs
diff_tests_executed: 17 PASS, 0 FAIL, 0 SKIP
waiver_ref: none required; both raw failures satisfy non-diff attribution
ci_lane_run: n/a (no CI-config change in this diff)
```

The full-suite runner is package-verbose rather than test-verbose. Its
`unit-core` job recorded `internal/productmetrics` PASS. A fresh
JSON-observable run of the complete package on the same head mapped every
diff-owned top-level test by name:

- `TestConcurrentOffNonPendingCASLoserIsStateConflictWithoutDurabilityClaim` — PASS
- `TestConcurrentOffPendingObserverAcceptsPeerCompletionBeforeInitialStateLock` — PASS
- `TestConcurrentOffPendingObserverDoesNotClaimDurabilityAfterPeerEnable` — PASS
- `TestDisableAndPurgeBoundsInitialAndPostUploaderStateLocks` — PASS
- `TestDisableAndPurgeMakesBlockedUploadResponseStaleWithoutSettlement` — PASS
- `TestGreaterEpochResumeWaitsForUploaderLockBeforeCleanup` — PASS
- `TestHangBudgetStaysAHangDetector` — PASS
- `TestRecordOnceReservesAndStartsAfterReleasingStateTransaction` — PASS
- `TestRootAtomicWriterCrashReplayAtEveryProtocolOrdinal` — PASS
- `TestSpawnUploaderUsesAbsoluteExactSpecAndWaitsAsynchronously` — PASS
- `TestSpoolDeepPurgeConvergesUnderLowFileDescriptorLimit` — PASS
- `TestSpoolNestedPurgeConvergesAtMinimumDirectoryBudget` — PASS
- `TestStartedPrivateUploaderIsReapedWhenParentDescriptorCloseFails` — PASS
- `TestStorageAdvisoryLockIsReleasedWhenProcessDies` — PASS
- `TestUploadStartCancellationAbortsBeforeRoundTripWithoutNetwork` — PASS
- `TestUploadStartReturnsPreEntryValidationErrorWithoutDeadlock` — PASS
- `TestUploadStartWaitsForActualRoundTripEntry` — PASS

### Full-suite failure attribution

| Failing test | Tracker | Attribution |
|--------------|---------|-------------|
| `TestBdFlagManifestCurrent` | ga-f0uceo | Clause 3(a), mechanism: the installed-`bd` flag surface and `internal/bdflags` manifest are unreachable from package-local productmetrics test changes. The tracker predates this run, the current signature matches it, and there is no path overlap. |
| `TestE2E_SuspendResume_City` | ga-dqd7gf | Clause 3(a), mechanism: package-local productmetrics tests cannot affect the city suspend/resume report path. The 94.23-second missing-`citysus.report` signature matches the pre-existing tracker and has no path overlap. |

Both tracker sightings were appended and read back during this gate session.

## Policy and static lane

```text
policy_lane: PASS with attributed pre-existing diagnostics
make test-ci-policy: PASS
make fmt-check: raw FAIL, attributed below
make vet: PASS
go build ./...: PASS
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected:
  raw FAIL, attributed below
git diff --check origin/main...HEAD: PASS
```

The exact current base independently reproduced the four formatting findings,
the three deprecated-field findings, and the unused-value finding. The stale
deploy head's full-fallback selector additionally scanned the vendored
dashboard `node_modules` Go fixture; the feature diff changes neither the
selector nor that fixture.

| Policy diagnostic | Tracker | Attribution |
|-------------------|---------|-------------|
| Four unchanged-file gofumpt findings | ga-d3m213 | Exact `origin/main@26542454e5ea740ab512594f1df451d9a7a3e7a7` reproduction; no productmetrics path overlap. |
| Three unchanged `ResolvedProvider.Kind` SA1019 findings | ga-t88402 | Exact-base reproduction; the feature does not introduce the uses or change the deprecation. |
| Unused `workDir` SA4006 in `internal/api` | ga-egkp4x | Exact-base reproduction; `internal/api` is outside the feature diff. |
| Three vendored `flatted` govet/revive findings | ga-u8z8j6 | Matches the predating older-head full-fallback tracker. The feature diff changes no Makefile, lint selector, dashboard, or `node_modules` path. |

All four tracker sightings were appended and read back during this gate
session. Raw failures remain recorded; none was retried into green.
