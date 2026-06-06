# Release gate - supervisor cache reconcile backoff and metadata refresh (ga-b7u6z3 / ga-ru5k3e)

**Verdict:** PASS

- Deploy bead: `ga-b7u6z3`
- Source bead: `ga-ru5k3e` (closed)
- Prior review bead: `ga-smlafz`
- Branch: `quad341:builder/ga-ru5k3e-1`
- HEAD before gate commit: `d7010e1fb` (`fix(supervisor): refresh stores on backend metadata changes`)
- Diff: 8 files, +318 / -23 before this PASS gate update
- Project manifest: `docs/PROJECT_MANIFEST.md` is not present in this checkout; gate uses the deployer prompt's release criteria and the source bead done-when criteria.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `ga-b7u6z3` notes contain `Review Verdict: PASS` from `gascity/reviewer` for commit `d7010e1fb` on `fork/builder/ga-ru5k3e-1`. |
| 2 | Acceptance criteria met | PASS | Log-volume ceiling is covered by `TestCachingStoreRunReconciliationSuppressesDuplicateProblemLogs`; sustained-failure backoff is covered by the same test and `TestCachingStoreNextReconcileDelayUsesFreshnessWatchdog`; backend metadata flips now rebuild controller stores through `TestControllerStateRuntimeUpdateRebuildsStoresWhenBackendMetadataChanges` and `TestCityRuntimeReloadSameRevisionRefreshesStoresWhenMetadataChanges`; existing `internal/beads` tests pass. |
| 3 | Tests pass on final branch | PASS | Focused acceptance tests, `go vet ./...`, `git diff --check origin/main...HEAD`, and `make test-fast-parallel` all passed on this branch. |
| 4 | No high-severity review findings open | PASS | Review notes list only LOW findings and explicitly state no HIGH or MEDIUM findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed only `## deployer/ga-ru5k3e-gate...fork/builder/ga-ru5k3e-1` before this gate file update. A pre-existing empty root `.gitkeep` was preserved as a local ignored artifact and is not part of the release branch. |
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...HEAD` reported `7 3`; `git merge-tree --write-tree HEAD origin/main` completed successfully with tree `809e1edb993c3b9df87c765a019d0360f0614165`, indicating no merge conflicts with current `origin/main`. |

## Acceptance Evidence

| Source criterion | Verdict | Evidence |
|---|---|---|
| Supervisor log volume drops to at most once per minute under sustained reconciler errors. | PASS | `internal/beads.CachingStore` now suppresses duplicate exact problem logs for `cacheProblemLogWindow = time.Minute` while preserving problem stats; `TestCachingStoreRunReconciliationSuppressesDuplicateProblemLogs` passed. |
| After metadata flips from `backend=dolt` to `backend=postgres`, the running supervisor stops using the stale store within at most one reload cycle. | PASS | `controllerState` tracks `.beads/metadata.json` signatures for city and rig stores, rejects store reuse when signatures change, and applies same-revision reloads when bead-store metadata changes; focused controller and runtime reload tests passed. |
| Existing `internal/beads/caching_store_*_test.go` pass. | PASS | `go test ./internal/beads -run 'TestCachingStoreRunReconciliation(RecordsProblemAndDegrades|SuppressesDuplicateProblemLogs)|TestCachingStoreNextReconcileDelayUsesFreshnessWatchdog' -count=1` passed; `make test-fast-parallel` passed. |
| New regression test asserts log-volume ceiling under sustained errors. | PASS | `TestCachingStoreRunReconciliationSuppressesDuplicateProblemLogs` asserts one emitted line during repeated failures and a suppressed duplicate count after the window expires. |

## Validation

- `go test ./cmd/gc -run 'TestControllerStateRuntimeUpdateRebuildsStoresWhenBackendMetadataChanges|TestCityRuntimeReloadSameRevisionRefreshesStoresWhenMetadataChanges|TestControllerStateRuntimeUpdatePreservesCurrentStoresWithoutPendingMutation|TestControllerStateRuntimeUpdateAfterMutationPreservesCurrentStores|TestCityRuntimeReloadSameRevisionIsNoOp' -count=1` - PASS (`ok github.com/gastownhall/gascity/cmd/gc 4.356s`)
- `go test ./internal/beads -run 'TestCachingStoreRunReconciliation(RecordsProblemAndDegrades|SuppressesDuplicateProblemLogs)|TestCachingStoreNextReconcileDelayUsesFreshnessWatchdog' -count=1` - PASS (`ok github.com/gastownhall/gascity/internal/beads 0.013s`)
- `go vet ./...` - PASS
- `git diff --check origin/main...HEAD` - PASS
- `make test-fast-parallel` - PASS (`All fast jobs passed`)
- `git config core.hooksPath` - `.githooks`
