# Release gate: internal/beads hang-detector budgets

- Deploy bead: `ga-0rgsd2`
- Review bead: `ga-d2a050`
- Reviewed commit: `3824c5456342dbfa3bb5c7263e7f9f672cfa2bef`
- Gate base: `origin/main` at `7d3dae1d1179c8aed144264b75e19502b074afcc`
- Evaluated: 2026-08-22
- Result: **PASS**

The remote-mode pre-flight found no pull request carrying the reviewed commit.
Criterion 6 was evaluated first and passed without a self-rebase. The remaining
criteria were evaluated independently on the exact reviewed commit; the
builder's earlier re-verification notes were treated as hypotheses, not gate
evidence.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-d2a050` is closed with reason `pass`; its notes record `verdict: pass` and `tests_green: true` for reviewed commit `3824c5456342dbfa3bb5c7263e7f9f672cfa2bef`. |
| 2 | Acceptance criteria met | **PASS** | The four-file, test-only diff replaces 12 uses of global race-floor constants with package-scoped pure hang-detector budgets. `beadsHangBudget` covers cancellable goroutine/lock waits and `beadsSequenceFloorHangBudget` covers the re-exec child protocol, both at the documented 6x floor precedent. No production code or latency assertion changes. |
| 3 | Tests pass | **PASS** | Rootless Podman was configured with `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`, `TESTCONTAINERS_RYUK_DISABLED=true`, and cached Dolt image `2.1.7`. `LOCAL_TEST_JOBS=4 make test-local-full-parallel` completed **37 PASS / 3 FAIL / 0 SKIP jobs**. All three raw failures satisfy all four criterion-3a clauses: they are not diff-owned; each has a tracker; each reproduces on exact `origin/main`; and the diff touches only `internal/beads/*_test.go`, with no path overlap. `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + exact-base FAIL with identical installed-bd manifest drift`; `failure_attribution: TestGetKeyBinding_CapturesDefaultBinding -> ga-afqddr + exact-base FAIL with identical empty default binding`; `failure_attribution: TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-k3fxvj + exact-base FAIL with identical empty binding`. `waiver_ref: none`. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` PASS; `go vet ./...` PASS; dedicated testenv import check PASS; changed-file `gofmt` PASS; prospective-merge `fmt-check-changed` PASS. `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` ran against synthetic merge `62fc2b24d6d8ab86ef36757b8a75082bab73024d` with a new disk-backed golangci-lint cache, selected the full 190-package reverse-dependency closure, and reported `0 issues`. |
| 4 | No high-severity review findings open | **PASS** | The independent review closed PASS and records no high-severity open finding. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty at the reviewed commit before this gate file was written. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 3824c5456342dbfa3bb5c7263e7f9f672cfa2bef` exited 0 and produced tree `3e9e7e894c82cbf4e313011c7e61a450de980d59`. `assert_deploy_ancestry_scope` passed for accepted bead IDs `ga-0rgsd2` and `ga-42mt5x.4`. |
| 7 | Single feature theme | **PASS** | One commit changes only four `internal/beads` test files to distinguish package hang detectors from global race-floor inputs. No independent feature is bundled. |

## Diff-owned test evidence

The named affected run completed **18 PASS / 0 FAIL / 0 SKIP** results:

- `TestCachingStoreCountContextCancelsWhileWaitingForLock` PASS.
- `TestMemStoreReadyContextCancelsWhileWaitingForLock` PASS.
- `TestSQLiteStoreReadyContextCancelsWhileWaitingForAConnection` PASS.
- `TestSQLiteStoreSequenceFloorSIGKILLAtBoundaries` and all 13 boundary
  subtests PASS.
- `TestSQLiteStoreSetSequenceFloorNeverLowersAcrossProcesses` PASS.

The new `hangbudget_test.go` declares constants only and contains no test
function. `TestRequiresDedicatedTestenvImportFile` separately PASSed for the
changed test package. `diff_tests_executed: 18 PASS, 0 FAIL, 0 SKIP;
waiver_ref: none`.

## Gate decision

Every gate criterion passes. The three full-suite failures are measured,
tracked, exact-base-reproduced, and path-disjoint from this test-only diff.
The reviewed commit is eligible for an isolated deploy branch and pull request.
