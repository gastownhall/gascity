# Release Gate: ga-wfyuv7

## Summary

- Bead: `ga-wfyuv7` — needs-deploy: RigRootBranchCheck doctor advisory check
- Source bead: `ga-2mv0np`
- Reviewed commits:
  - `42debce64` — feat(doctor): add rig root-branch advisory check
  - `717420fcd1b9bce16cf8b809f22777605fb042b7` — test(doctor): cover rig root branch check
- Source branch: `builder/ga-iq13bl.1-doctor-rig-branch`
- Scope: doctor advisory check for rig branch/default-branch drift
- Gate result: PASS

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate uses
the deployer release-gate criteria from the agent contract and the test
tier guidance in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-2mv0np` is closed with `REVIEW VERDICT: PASS` from `gascity/reviewer` for commits `42debce64` and `717420fcd`. |
| 2 | Acceptance criteria met | PASS | Adds `RigRootBranchCheck` with name `rig:<name>:root-branch`, `SeverityAdvisory`, `WarmupEligible=true`, `CanFix=false`, injectable git path for tests, and wiring through `buildDoctorChecks` in `cmd/gc/cmd_doctor.go`. Tests cover matching branch, clean drift, dirty drift, non-git path, missing git, unset default fallback, and non-main default branch. |
| 3 | Tests pass | PASS | `go test ./internal/doctor -run 'TestRigRootBranchCheck' -count=1` passed. `go build ./cmd/gc/...` passed. `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded two low-severity non-blocking findings only: optional dirty-check error detail is dropped, and unavailable-git/non-git messages share wording. No high-severity findings are open. |
| 5 | Final branch is clean | PASS | Evaluation worktree was clean before writing this gate; final status is checked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | Branch is behind `origin/main` by one commit, but `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` reported a clean merge with no conflict markers. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem: doctor checks and the `cmd/gc` doctor check registration. |

## Test Commands

```text
go test ./internal/doctor -run 'TestRigRootBranchCheck' -count=1
go build ./cmd/gc/...
make test-fast-parallel
go vet ./...
```

## Acceptance Evidence

Implemented surface:

- `internal/doctor/checks_rig_root_branch.go` defines the advisory branch drift check.
- `cmd/gc/cmd_doctor.go` registers the check for rigs.
- `internal/doctor/checks_rig_root_branch_test.go` covers all seven reviewed decision paths.

Unchanged surface:

- No HTTP API schema, dashboard, or generated type changes.
- No automatic fixing behavior; `CanFix=false`.
- Git interactions are read-only (`rev-parse` and `status --porcelain`).
