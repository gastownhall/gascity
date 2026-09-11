# Release Gate: apply-plan formula hash/source stamping

Bead: ga-5m7zmq
Source review bead: ga-wderad
PR: https://github.com/gastownhall/gascity/pull/3265
Candidate branch: builder/ga-gcqi4i
Candidate head: bdc71e659feee8b90dafc3f9df9f34d674b8d09f
Base checked: origin/main at 91e64b9a1f9291494e058ad1390e8d16d93557c3
Merge base: f12b2939ff783ea295ff551aee67c72b6fdea87b

## Summary

PASS. The change is a single internal molecule apply-plan fix: root nodes built by `buildRecipeApplyPlan` now receive the same `gc.formula_hash` and `gc.formula_source` metadata that the sequential `Instantiate` path already stamps. The associated tests cover hash/source stamping, empty-value behavior, and the non-root invariant.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-wderad` reports `REVIEW VERDICT: PASS`; review bead is closed with reason `pass`. |
| 2 | Acceptance criteria met | PASS | `internal/molecule/graph_apply.go` stamps `gc.formula_hash` and `gc.formula_source` only when non-empty and only on the root node. `internal/molecule/molecule_test.go` adds `TestBuildRecipeApplyPlanStampsFormulaHash` and `TestBuildRecipeApplyPlanNoHashWhenEmpty`; the package test passes. |
| 3 | Tests pass | PASS | `go test ./internal/molecule` passed. `go vet ./...` passed. `make test` passed with observable log `/tmp/gascity-test.jsonl.nEGIgs`. GitHub PR checks for #3265 are also green at the reviewed head. |
| 4 | No high-severity review findings open | PASS | Review notes list no blocking or high-severity findings. PR review list is empty; the only current human PR comment asks for an opinion, not a blocking finding. The old Blacksmith failure comment is minimized as outdated and current checks are successful. |
| 5 | Final branch is clean | PASS | Clean worktree was used for the gate. After committing this gate file, `git status --short --branch` was rechecked and was clean. |
| 6 | Branch diverges cleanly from main | PASS | `gh pr view 3265` reports `mergeStateStatus: CLEAN`. Local `git merge-tree $(git merge-base origin/main origin/builder/ga-gcqi4i) origin/main origin/builder/ga-gcqi4i` produced a clean merge result with no conflicts. |
| 7 | Single feature theme | PASS | Triple-dot diff from the PR merge-base touches only `internal/molecule/graph_apply.go` and `internal/molecule/molecule_test.go`; the commit set is one molecule metadata-stamping fix. |

## Local Commands

```text
go test ./internal/molecule
go vet ./...
make test
git diff --stat origin/main...origin/builder/ga-gcqi4i
git merge-tree $(git merge-base origin/main origin/builder/ga-gcqi4i) origin/main origin/builder/ga-gcqi4i
```

## Commit Set

| Commit | Purpose |
|--------|---------|
| 55cc80184a911e7eaf6108d0d7b4ea3dbec34c4f | Implement root apply-plan formula hash/source metadata stamping. |
| bdc71e659feee8b90dafc3f9df9f34d674b8d09f | Add molecule tests for root stamping, empty values, and non-root behavior. |

