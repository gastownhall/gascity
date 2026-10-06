# Release Gate: controller test hang-deadline migration

Date: 2026-07-28  
Deployer: `gascity/deployer`  
Deploy bead: `ga-jhs26o`  
Reviewed commit: `4304df38b9758d2d5fcdfe32453b950f9cddeb40`  
Base checked: `origin/main` at `f68a2ed019a21d9efc41ed1d02c9233eeb8463de`

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This evaluation
therefore uses the deployer release criteria and the repository's canonical
`TESTING.md` policy.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-opw5az` is closed and records `REVIEW VERDICT: PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | Both repository guards pass. The literal-deadline scan now returns exactly four intentional exclusions, all with specific comments. The migration removes six `time.Sleep` calls and adds none; the three fixed-sleep census baselines fall by exactly six and the live census/documentation sync guard passes. `cmd/gc/hangbudget_test.go` and `cmd/gc/cmd_stop_test.go` have no diff. |
| 3 | Tests pass | PASS | `go build ./...`, `go vet ./...`, the two focused controller lint tests, `TestRepositoryLedgerMatchesCensusAndDocumentation`, and `make test-fast-parallel` all passed. The sharded fast run completed 9/9 jobs successfully. |
| 4 | No high-severity review findings open | PASS | The review records no blockers and no HIGH or CRITICAL findings. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty on `deploy/ga-jhs26o-gate` before this checklist was written. This checklist is the deployer's only additional change and will be committed separately. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and rechecked after the test run. `git merge-tree --write-tree origin/main HEAD` succeeded against current `origin/main`, producing tree `d3a7b9095a884df253d8a6913cab4d595496a4b1`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two-commit range has one theme: migrating `cmd/gc/controller_test.go` hang guards to the existing wait helpers. The lint test and synchronized resource-census reductions directly enforce and account for that migration. |

## Acceptance Evidence

- The reviewed range contains two commits and changes five files:
  `cmd/gc/controller_test.go`, its new lint test, and the three synchronized
  resource-census artifacts.
- `grep -cE 'time\.After\([0-9]|time\.Now\(\)\.Add\([0-9]' cmd/gc/controller_test.go`
  returns `4`. Those sites are the documented scenario-input,
  negative-assertion-window, and bounded-best-effort exclusions.
- `TestControllerTestHasNoUnmigratedRawHangDeadlines` and
  `TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline` both pass.
- The diff removes six `time.Sleep(...)` calls and adds zero. The all-source
  fixed-sleep baseline moves `427 -> 421`; both untagged baselines move
  `288 -> 282`.
- `TestRepositoryLedgerMatchesCensusAndDocumentation` passes, proving the live
  source census, `internal/testpolicy/resourcecensus/census.go`,
  `test/test-resources.toml`, and `TESTING.md` agree.

## Commands Run

```text
git fetch origin main
git merge-tree --write-tree origin/main HEAD
git diff --check <merge-base>..HEAD
go test -count=1 ./cmd/gc/... -run 'TestControllerTestHasNoUnmigratedRawHangDeadlines|TestControllerTestNoFunctionMixesHangBudgetWithRawDeadline' -v
go test -count=1 ./internal/testpolicy/resourcecensus/... -run TestRepositoryLedgerMatchesCensusAndDocumentation -v
go build ./...
go vet ./...
make test-fast-parallel
```

## Decision

PASS. The isolated deploy branch is ready for merge-authority review. The
related `ga-003f4o` deploy remains held pending this landing, and `ga-it1j7l`
remains responsible for the subsequent rebase/subsume determination.
