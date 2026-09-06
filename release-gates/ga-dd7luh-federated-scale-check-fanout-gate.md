# Release gate: federated custom `scale_check` fan-out

- Deploy bead: `ga-dd7luh`
- Build bead: `ga-drb140`
- Review bead: `ga-gknp2w`
- Reviewed commit: `70e8b787908c09162ebcfdece2e3eef86ef783e9`
- Base: `origin/main@5eec6bba548005c0e26e23a1b09dc1c34d45dc1f`
- Evaluated: 2026-08-17 (America/Los_Angeles)
- Result: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at this commit. This checklist uses
the seven release criteria in the current `mol-deployer-gate` formula and
deployer prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-gknp2w` records `verdict: PASS`, `review_round: 2` at the resolved reviewed commit. Round 2 independently verified the documentation fix against live source and the design bead. |
| 2 | Acceptance criteria met | **PASS** | AC1-AC7 are verified in the matrix below. The candidate implements city + non-suspended-rig fan-out, SUM semantics, shared semaphore bounding, both pool call paths, suspended-rig symmetry, deterministic concurrency coverage, and the architecture invariant. |
| 3 | Tests pass | **PASS with attributed pre-existing failures** | Eight diff-owned tests passed by name (8 PASS, 0 FAIL, 0 SKIP). `make test-cmd-gc-process-parallel` passed all 7 jobs (7 PASS, 0 FAIL, 0 SKIP). `make test-fast-parallel` completed 9 harness jobs PASS and 1 job FAIL, with no SKIP reported; the failing `internal/runtime/herdr` tests satisfy all four attribution clauses below and reproduce on `origin/main`. Docsync passed 13 test/subtest results (13 PASS, 0 FAIL, 0 SKIP). |
| 3a | Pre-existing failures may be attributed | **PASS** | `TestHerdrConformance` is not diff-owned, is tracked by `ga-19onv3`, fails on `origin/main@5eec6bba548005c0e26e23a1b09dc1c34d45dc1f` with the same `workspace_not_found` signature, and has no path overlap. `TestProviderLiveClaudeKindPath` is not diff-owned, is tracked by `ga-cqq3hs.1`, fails on the same base with the same `agent_pane_busy` signature, and has no path overlap. Candidate paths are limited to `cmd/gc/{build_desired_state.go,hook_cross_store.go,hook_cross_store_test.go,pool_scale_check_fanout.go,pool_scale_check_fanout_test.go}` and `engdocs/architecture/dispatch.md`; failures are under `internal/runtime/herdr`. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, `make check-docs`, `LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed`, and `go vet ./...` passed. The first shared-cache `lint-affected` run replayed stale findings from a removed `/var/tmp/ga-j250d0-gate...` worktree; the live source already carried the correct suppressions. The identical target with a fresh on-disk `GOLANGCI_LINT_CACHE` passed with `0 issues` over `./cmd/gc ./internal/runtime/tmux ./scripts`. Shared-cache contamination is tracked by `ga-ffwgw0`. |
| 4 | No high-severity review findings open | **PASS** | Round 2 records no style or security findings and no unresolved HIGH findings. The only round-1 finding was missing AC7 documentation; commit `70e8b787908c09162ebcfdece2e3eef86ef783e9` resolves it and the reviewer passed the result. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain` was empty at the exact reviewed commit. `git diff --check origin/main...HEAD` also passed. The checklist is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Already-merged preflight found 0 PRs for the reviewed SHA. After the final `git fetch origin main`, `git rev-list --left-right --count origin/main...HEAD` returned `1 3`; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `35d93de778a62bb4b66ce683ee1dd2915f1d70d9`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | All three feature commits and six changed paths implement or document one behavior: federating city-scoped custom `scale_check` across the same non-suspended store set used by `work_query`. `assert_deploy_ancestry_scope origin/main HEAD ga-dd7luh ga-drb140` passed; no `.claude/**` or unrelated commit theme is present. |

## Acceptance criteria

| AC | Result | Evidence |
|---|---|---|
| AC1 | **PASS** | `cityScopedFanOutProbes` returns the city target plus non-suspended rig targets. `evaluatePoolFanOutSum` runs one goroutine per target and acquires the existing caller-owned semaphore once per probe, with no nested semaphore. |
| AC2 | **PASS** | Per-store counts are summed; steady-state min/max clamping occurs once on the aggregate, while new-demand totals remain unclamped. `TestEvaluatePoolFanOutSumSumsAcrossProbes` proves `2 + 3 + 5 = 10`, distinguishing SUM from first-hit or winner-takes-all. |
| AC3 | **PASS** | `appendRigHookStores` excludes suspended rigs through `buildSuspendedRigPathsForCity`, the same helper used by desired-state evaluation. This intentionally applies to every city-scoped cross-store-eligible agent, not only triager. |
| AC4 | **PASS** | Both the named-session-backing pool path and the generic pool path construct `cityScopedFanOutProbes` for city-scoped custom checks and pass them through the same `evaluatePendingPools` implementation. |
| AC5 | **PASS** | The multi-valued SUM test uses counts 2, 3, and 5. `TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly` covers city + active rig and excludes the suspended rig. |
| AC6 | **PASS** | `TestEvaluatePoolFanOutSumRunsProbesConcurrently` uses a deterministic barrier to require concurrent starts; `TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested` proves serialization through a caller semaphore of capacity 1. |
| AC7 | **PASS** | `engdocs/architecture/dispatch.md` now documents store-set symmetry as distinct from same-store predicate symmetry and names the shared suspended-rig resolver. Independent docsync validation passed. |

## Test evidence

```text
test_cmd: go test -count=1 -v ./cmd/gc -run '^(TestAppendRigHookStoresExcludesSuspendedRig|TestEvaluatePoolFanOutSumSumsAcrossProbes|TestEvaluatePoolFanOutSumClampsAggregateOnce|TestEvaluatePoolFanOutSumNewDemandLeavesAggregateUnclamped|TestEvaluatePoolFanOutSumBestEffortOnProbeError|TestEvaluatePoolFanOutSumRunsProbesConcurrently|TestEvaluatePoolFanOutSumSharesCallerSemaphoreNotNested|TestCityScopedFanOutProbesIncludesCityAndNonSuspendedRigsOnly)$'
test_counts: 8 PASS, 0 FAIL, 0 SKIP
diff_tests_executed: all eight named tests PASS
waiver_ref: none

test_cmd: make test-fast-parallel
test_counts: 9 harness jobs PASS, 1 harness job FAIL, 0 SKIP reported
failure_attribution: TestHerdrConformance -> ga-19onv3 + origin/main workspace_not_found reproduction; TestProviderLiveClaudeKindPath -> ga-cqq3hs.1 + origin/main agent_pane_busy reproduction

test_cmd: make test-cmd-gc-process-parallel
test_counts: 7 jobs PASS, 0 FAIL, 0 SKIP

test_cmd: go test -count=1 -v ./test/docsync
test_counts: 13 test/subtest PASS, 0 FAIL, 0 SKIP

policy_lane: make test-ci-policy PASS; isolated-cache lint-affected PASS (0 issues); fmt-check-changed PASS; go vet ./... PASS; make check-docs PASS
skip_justification: none; no SKIP was observed in candidate-owned or required named lanes
```

## Deployment preparation

- Recorded SHA resolved with `git rev-parse --verify --quiet <sha>^{commit}`.
- `assert_deploy_ancestry_scope` passed for `ga-dd7luh` and `ga-drb140`.
- Git hooks path is `.githooks`.
- Deploy target is the isolated branch `deploy/ga-dd7luh-gate`; the provenance
  branch `builder/ga-drb140` is not a push target.
