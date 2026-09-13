# Release Gate: ga-tftexi.1 - ListQuery.Assignees contract

Deploy bead: `ga-tftexi.1` - `Deploy isolated ListQuery.Assignees contract slice`
Parent deploy bead: `ga-tftexi`
Source review bead: `ga-km7pwx`
Reviewed commit: `105d0224d` on `builder/ga-2znrco.1-listquery-assignees`
Release branch: `release/ga-tftexi-1-listquery-assignees-v7`
Release commit before gate: `d84cfaa4125edc4c0824988c514699286ad957e6`
Base: `origin/main` at `882955678403fc4327ac57422bbbf668ac0231de`
Evaluated: `2026-06-01T04:33:21Z`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the release criteria from the active deployer instructions plus the repository
testing guidance in `TESTING.md`.

## Scope

The original parent deploy bead failed scope because
`builder/ga-2znrco.1-listquery-assignees` was stacked on unrelated
coordstore/SQLite work. PM split this isolated child bead with a contamination
guard. This release branch was cut fresh from current `origin/main` and
cherry-picked only the reviewed ListQuery commit.

The resulting code diff before this gate file contains exactly:

- `internal/beads/query.go`
- `internal/beads/beads_test.go`

It does not include coordstore/SQLite PR #2738 work or any release-gate/doc
artifacts from the stacked builder branch.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-km7pwx` is closed with `PASS (reviewer: gascity/reviewer)` for commit `105d0224d`; parent `ga-tftexi` records the reviewed PASS and PM split into this isolated deploy child. |
| 2 | Acceptance criteria met | PASS | `ListQuery.Assignees` is mutually exclusive with `Assignee` via `Validate`, participates in `HasFilter`, and `Matches` accepts any listed assignee. Tests cover all three behaviors, including the exact mutual-exclusion error. |
| 3 | Tests pass | PASS | Focused `TestListQuery` passed; `internal/beads` and `internal/beads/exec` passed; `make test-fast-parallel` passed all fast shards; `go vet ./...` exited clean. |
| 4 | No high-severity review findings open | PASS | The review notes on `ga-km7pwx` state security clean and list no HIGH findings. Unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty after the cherry-pick and before writing this release-gate file. This file is the only deployer-authored addition and is committed with the gate. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created from current `origin/main`; `git merge-base origin/main HEAD` equals `882955678403fc4327ac57422bbbf668ac0231de`. `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `348ff661fe99d977218aa35f8a07f63ac31ac96d`. |
| 7 | Single feature theme | PASS | The release branch touches one subsystem theme: adding a multi-assignee selector to the beads `ListQuery` contract. The diff is limited to the query contract and its tests. |

## Acceptance Evidence

- `git diff --stat origin/main...HEAD` before this gate showed only two
  `internal/beads` files and 62 insertions / 4 deletions.
- `git diff --check origin/main...HEAD` exited 0.
- `rg -n "Assignees|Validate\(" internal/beads` found the new field,
  validation guard, filter/match logic, and tests only in `internal/beads`.
- `TestListQueryHasFilterIncludesAssignees` verifies `HasFilter`.
- `TestListQueryMatchesAnyAssignee` verifies any-listed-assignee matching.
- `TestListQueryValidateRejectsAssigneeAndAssignees` verifies mutual exclusion
  with the exact error message.

## Commands

```text
gh auth status
git fetch origin main
git worktree add -b release/ga-tftexi-1-listquery-assignees-v7 /tmp/gascity-deploy-ga-tftexi-1-v7 origin/main
git cherry-pick 105d0224d
git status --short --branch
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
rg -n "Assignees|Validate\(" internal/beads
GOTOOLCHAIN=auto go test ./internal/beads -run TestListQuery -count=1
GOTOOLCHAIN=auto go test ./internal/beads ./internal/beads/exec -count=1
GOTOOLCHAIN=auto make test-fast-parallel
GOTOOLCHAIN=auto go vet ./...
git config core.hooksPath
```

## Test Summary

```text
go test ./internal/beads -run TestListQuery -count=1
ok  	github.com/gastownhall/gascity/internal/beads	0.040s

go test ./internal/beads ./internal/beads/exec -count=1
ok  	github.com/gastownhall/gascity/internal/beads	5.936s
ok  	github.com/gastownhall/gascity/internal/beads/exec	7.155s

make test-fast-parallel
All fast jobs passed

go vet ./...
clean

git config core.hooksPath
.githooks
```
