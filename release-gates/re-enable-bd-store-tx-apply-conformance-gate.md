# Release Gate: re-enable bd store tx apply conformance

Bead: ga-rvso5lf
Source bead: ga-so5lf
Branch: builder/ga-so5lf-1
Commit under review: 4706f1617

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-rvso5lf` notes contain `VERDICT: pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `test/integration/bdstore_test.go`; `SkipTxApplyConformance`, `SkipTxApplyReason`, and stale skip marker text are absent from that file; `TxRunsCallbackAndAppliesWriteSurface` ran twice and passed. |
| 3 | Tests pass | PASS | `go test -tags integration -v -run 'TestBdStoreConformance/TxRunsCallbackAndAppliesWriteSurface' -count=2 -timeout 120s ./test/integration/...` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS on rerun after an isolated timing-sensitive fast-test failure also passed 5 consecutive runs. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts. |

## Acceptance Evidence

- Changed files: `test/integration/bdstore_test.go` only.
- Diff stat: 1 file changed, 1 insertion, 6 deletions.
- The bd store integration suite now calls the normal conformance path so `TxRunsCallbackAndAppliesWriteSurface` runs instead of being skipped.
- The still-valid sequential ID explanation remains untouched.

## Test Evidence

- `go test -tags integration -v -run 'TestBdStoreConformance/TxRunsCallbackAndAppliesWriteSurface' -count=2 -timeout 120s ./test/integration/...`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: first run failed in `internal/beads.TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand` while other gate checks were running concurrently; isolated rerun of that test with `-count=5` passed; final rerun of `make test-fast-parallel` by itself passed.
- `git diff --check origin/main...HEAD`: PASS.
