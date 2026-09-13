# Release Gate: Pre-commit OpenAPI/npm fail-closed behavior

- Deploy bead: `ga-x86bjw`
- Source review: `ga-jg89a5`
- Reviewed commit: `9600c301cc85581fe52b0c476c92aeac9f5d651e`
- Candidate base: `f68a2ed019a21d9efc41ed1d02c9233eeb8463de`
- Main evaluated: `origin/main@a72480ec884e5f6369f23b84cb18786affa49df5`
- Deploy branch: `deploy/ga-x86bjw-gate`
- Evaluated: `2026-07-28T05:05:30Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this checklist applies the deployer role's release-gate criteria.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first after fetching `origin/main`. `git merge-tree --write-tree origin/main 9600c301cc85581fe52b0c476c92aeac9f5d651e` exited 0 and produced tree `b5b736b0de846d01868ff8815338659ea532fc90`. No self-rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-jg89a5` records the earlier request-changes verdict for `d6dd43a87`, followed by an independent re-review with `REVIEW VERDICT: PASS (re-review of rework)` and `FINAL VERDICT: PASS` for exact commit `9600c301cc85581fe52b0c476c92aeac9f5d651e`. |
| 2 | Acceptance criteria met | **PASS** | The pre-commit hook now re-reads staged `internal/api/openapi.json` after its Go generation block and shares that fresh result between both npm branches. With npm absent, a directly staged spec or a spec staged as the Go block's side effect fails closed with the recovery command; unrelated changes remain warn-only. End-to-end contract tests cover both fail-closed paths and the warning boundary. Contributor guidance now points to the current dashboard path and `make dashboard-ci`. The three resource-ledger counters each rise by exactly six, matching the six new `exec.Command` call sites (five → eleven in `scripts/precommit_contract_test.go`). |
| 3 | Tests pass | **PASS** | First-attempt checks on the exact reviewed SHA passed: `gofmt -l` was empty; `bash -n .githooks/pre-commit` passed; five focused hook contracts passed; full `go test ./scripts/...`, `go test ./internal/testpolicy/resourcecensus/...`, and `go test ./test/docsync/...` passed; `go build ./...` and `go vet ./...` passed; `make test-fast-parallel` passed all nine jobs. |
| 4 | No high-severity review findings open | **PASS** | The prior blocking finding was fixed and independently RED/GREEN verified during re-review. The final exact-SHA review reports no security findings, no coverage gaps, and no blockers. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, detached `9600c301c` had an empty `git status --porcelain=v1`; `git diff --check` against its merge base passed. The configured hook path is `.githooks`; this checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | The two reviewed commits and nine touched files form one contributor-safety change: prevent stale generated dashboard clients when OpenAPI changes cannot be regenerated locally, pin the behavior in hook-contract tests, update its resource ledger, and correct the matching contributor instructions. No independent product feature is bundled. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main 9600c301cc85581fe52b0c476c92aeac9f5d651e
git diff --check f68a2ed019a21d9efc41ed1d02c9233eeb8463de..9600c301cc85581fe52b0c476c92aeac9f5d651e
gofmt -l scripts/precommit_contract_test.go internal/testpolicy/resourcecensus/census.go
bash -n .githooks/pre-commit
go test ./scripts/... -run 'TestPreCommitFailsClosedWhenGoBlockStagesSpecAsSideEffectAndNpmAbsent|TestPreCommitFailsClosedWhenSpecStagedButNpmAbsent|TestPreCommitWarnsOnlyWhenNpmAbsentAndSpecNotStaged|TestPreCommitRegeneratesDashboardClientOnSpecChange|TestPreCommitReachesDashboardBlockWhenOnlySpecFileStaged' -count=1 -v
go test ./scripts/... -count=1
go test ./internal/testpolicy/resourcecensus/... -count=1
go test ./test/docsync/... -count=1
go build ./...
go vet ./...
make test-fast-parallel
```
