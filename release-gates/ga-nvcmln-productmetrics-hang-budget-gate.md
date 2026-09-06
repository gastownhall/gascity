# Release Gate: productmetrics hang-budget conversion

Bead: ga-nvcmln
Review bead: ga-vzyz80
Implementation bead: ga-42mt5x.1
Branch under review (provenance only): builder/ga-42mt5x.1
Reviewed commit: 1a76aa8a439bfa9d9e478f94f899c5d1e0b699b3
Base: origin/main@16bfb855bfaf6df981b704bed9b9a9f14090d69a
Tested synthetic merge: 35619b9f4b927343de6a1153ba44202dd92b1e62
Deploy branch: deploy/ga-nvcmln-gate
Gate date: 2026-08-23
Result: PASS

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate uses
the deployer release criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Conflict freedom | PASS | Preflight found no existing PR for the reviewed commit. `git merge-tree --write-tree origin/main 1a76aa8a439bfa9d9e478f94f899c5d1e0b699b3` exited 0 and produced tree `db6318e7d3a375f232b6a64014de05582d5fe60e`. The deploy ancestry scope guard passed. |
| 1 | Review PASS present | PASS | Reviewer bead ga-vzyz80 records `verdict: pass` for the authoritative reviewed commit `1a76aa8a439bfa9d9e478f94f899c5d1e0b699b3`. |
| 2 | Acceptance criteria met | PASS | The three reviewed commits replace 28 productmetrics floor-as-deadline detector waits with the package `hangBudget`. The eight direct floor calls that remain are live inputs or scenario values, not hang detectors. `TestHangBudgetStaysAHangDetector` guards both the floor and the largest replaced multiplied shape. |
| 3 | Tests pass | PASS | The CI-equivalent full local suite ran all 40 jobs: 35 passed and 5 failed. Every failure is either reproduced at the exact base SHA or covered by the active Mayor waiver, with no overlap with the seven changed productmetrics test files. The focused productmetrics run passed 464 tests with 0 failures and 0 skips; all 17 diff-owned top-level tests passed. All six cmd/gc process shards and the productmetrics testhook passed. See attribution below. |
| 4 | No high-severity review findings open | PASS | ga-vzyz80 recorded no blocking finding. Independent inspection found no production-code change or new runtime behavior; the patch only changes test hang-detector budgets and adds their guard test. |
| 5 | Final branch is clean | PASS | `git diff --check origin/main...1a76aa8a439bfa9d9e478f94f899c5d1e0b699b3` and the isolated scope check passed; the synthetic merge worktree was clean before this evidence file was added. |
| 7 | Single feature theme | PASS | Seven files under `internal/productmetrics/` change, all within the single reviewed theme of separating hang-detection budgets from latency floors. Diff size: 113 insertions, 32 deletions. |

## Diff-owned test evidence

The following top-level tests changed by the candidate all passed on the
synthetic merge:

- `TestDisableAndPurgeBoundsInitialAndPostUploaderStateLocks`
- `TestConcurrentOffPendingObserverAcceptsPeerCompletionBeforeInitialStateLock`
- `TestConcurrentOffPendingObserverDoesNotClaimDurabilityAfterPeerEnable`
- `TestConcurrentOffNonPendingCASLoserIsStateConflictWithoutDurabilityClaim`
- `TestDisableAndPurgeMakesBlockedUploadResponseStaleWithoutSettlement`
- `TestHangBudgetStaysAHangDetector`
- `TestRootAtomicWriterCrashReplayAtEveryProtocolOrdinal`
- `TestSpawnUploaderUsesAbsoluteExactSpecAndWaitsAsynchronously`
- `TestStartedPrivateUploaderIsReapedWhenParentDescriptorCloseFails`
- `TestRecordOnceReservesAndStartsAfterReleasingStateTransaction`
- `TestUploadStartWaitsForActualRoundTripEntry`
- `TestUploadStartReturnsPreEntryValidationErrorWithoutDeadlock`
- `TestUploadStartCancellationAbortsBeforeRoundTripWithoutNetwork`
- `TestSpoolDeepPurgeConvergesUnderLowFileDescriptorLimit`
- `TestSpoolNestedPurgeConvergesAtMinimumDirectoryBudget`
- `TestStorageAdvisoryLockIsReleasedWhenProcessDies`
- `TestGreaterEpochResumeWaitsForUploaderLockBeforeCleanup`

## Full-suite failure attribution

The full run used `LOCAL_TEST_JOBS=4 make test-local-full-parallel` with the
cached Podman-backed Dolt test service. Its five red jobs are attributable as
follows:

| Failing test | Disposition | Evidence |
|--------------|-------------|----------|
| `TestBdFlagManifestCurrent` | Pre-existing; ga-f0uceo | Reproduced at exact base `16bfb855bfa`; the installed `bd` exposes flags absent from the checked-in manifest. No candidate path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding` | Pre-existing; ga-afqddr | Reproduced at the exact base with the same empty-value result. No candidate path overlap. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | Pre-existing; ga-afqddr | Reproduced at the exact base with the same empty-value result. No candidate path overlap. |
| `TestE2E_SuspendResume_City` | Pre-existing; ga-yc0e3a | Reproduced at the exact base with the same timeout and missing `.gc-reports/citysus.report`; the tracker was updated with the paired evidence. No candidate path overlap. |
| `TestProviderLiveClaudeKindPath` | Waived | Exact `agent_pane_busy` signature covered by active waiver `mayor-2026-08-20-herdr-pane-standing`. The candidate does not touch Herdr or pane handling; PR #5437, whose landing expires the waiver, remains open. |

Test counts: 35 passing jobs, 5 attributed/waived failing jobs, 0 skipped
jobs. Focused package counts: 464 passing tests, 0 failing tests, 0 skipped
tests.

## Policy and static checks

The following checks passed on the synthetic merge:

```text
make test-ci-policy
make fmt-check
make vet
go build ./...
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected
make lint
```

Both lint runs used a fresh on-disk cache under `/var/tmp` and reported zero
issues. An initial full lint invocation had read stale shared-cache paths for
already-removed sibling worktrees; no candidate file appeared in those
findings, and the isolated rerun resolved the cache contamination without
clearing the shared Go build cache.
