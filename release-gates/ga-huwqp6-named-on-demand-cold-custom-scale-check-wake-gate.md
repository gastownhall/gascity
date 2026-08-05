# Release Gate: Named on-demand cold custom-scale-check wake

- Deploy bead: `ga-huwqp6`
- Source review: `ga-k3jb5n.1.1`
- Reviewed commit: `b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe`
- Candidate base: `af42a94245a547a0c47ec26054afa5fd1347b567`
- Main evaluated: `origin/main@a72480ec884e5f6369f23b84cb18786affa49df5`
- Deploy branch: `deploy/ga-huwqp6-gate`
- Evaluated: `2026-07-28T04:46:33Z`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this checklist applies the deployer role's release-gate criteria.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first after fetching `origin/main`. `git merge-tree --write-tree origin/main b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe` exited 0 and produced tree `47cf1c92546e38bd376d179996de5c4fd014fd43`. No self-rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-k3jb5n.1.1` is closed with `REVIEW VERDICT: PASS` and `FINAL VERDICT: PASS` for exact commit `b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe`. |
| 2 | Acceptance criteria met | **PASS** | The cold-wake probe for an `on_demand` named-session-backing pool with a custom `scale_check` now feeds `defaultScaleTargets` and records the template in `coldWakeTemplates`, allowing generic `gc.routed_to` demand to reach the existing named-session wake signal. The new regression test proves the routed-demand count and guards against phantom named-identity materialization. The `namedSessionMode == "always"` suppression boundary remains green. The retired deploy's unrelated parent `7eb9f2d7e3d07b2ec7ab175b6897531c3b56c6c5` is absent from the reviewed commit's ancestry. |
| 3 | Tests pass | **PASS** | First-attempt checks on the exact reviewed SHA passed: `gofmt -l` on both changed files was empty; the focused regression plus two `always`-mode boundary tests passed; `go build ./...` passed; `go vet ./...` passed; and `make test-fast-parallel` passed all nine jobs (`fsys-darwin-compile`, `push-gate-lock-selftest`, `unit-core`, and all six `unit-cmd-gc` shards). |
| 4 | No high-severity review findings open | **PASS** | The exact-SHA review reports no security findings, no coverage gaps, and no blockers. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, detached `b14fc3390` had an empty `git status --porcelain=v1`; `git diff --check` against its merge base passed. The configured hook path is `.githooks`; this checklist is the only deployer-authored release commit. |
| 7 | Single feature theme | **PASS** | The reviewed commit is one commit touching two files in one subsystem: `cmd/gc/build_desired_state.go` and its unit test (+82/-1). It fixes only cold routed-demand visibility for named on-demand pools with a custom `scale_check`. |

## Commands

```bash
git fetch origin main
git merge-tree --write-tree origin/main b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe
git merge-base origin/main b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe
git merge-base --is-ancestor 7eb9f2d7e3d07b2ec7ab175b6897531c3b56c6c5 b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe
git diff --check af42a94245a547a0c47ec26054afa5fd1347b567..b14fc3390fdea034dcd5e4fa6638fde9bb4e8afe
gofmt -l cmd/gc/build_desired_state.go cmd/gc/build_desired_state_test.go
go test ./cmd/gc/... -run 'TestBuildDesiredState_OnDemandNamedSession_ColdCustomScaleCheckWakesOnRoutedDemand|TestBuildDesiredState_IncludesImportedAlwaysNamedSessions|TestBuildDesiredState_AlwaysNamedSession_MaterializesWithoutWorkBeads' -count=1 -v
go build ./...
go vet ./...
make test-fast-parallel
```
