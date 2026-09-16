# Plan: Split nudge enqueue budget clean deploy target (`ga-rlr3i2`)

> Owner: `gascity/pm` - Created: 2026-07-03
> Source: deployer fail-back `ga-rlr3i2`; source review `ga-nr4996`; validator follow-up `ga-t2rfd8`

## Goal

Produce a deployable, single-feature release target for the nudge-queue
foreground enqueue maintenance budget fix.

The reviewed implementation is commit `93be1b652` on branch
`builder/ga-1k4paf-nudge-enqueue-budget`. Reviewer passed the change, but
the deployer rejected the release unit because the branch is not
releaseable: it carries unrelated Dolt maintenance/runbook commits, has
untracked files in the feature worktree, and does not merge cleanly with
`origin/main` due to conflicts in `cmd/gc/cmd_nudge.go` and
`cmd/gc/cmd_nudge_test.go`.

The validator follow-up `ga-t2rfd8` produced commit `8fbe53349` with the
missing executable coverage for budget-cap item preservation and the
empty-backlog fast path. Builder then applied that coverage to the
contaminated branch as commit `0ad8da96b`. The clean deploy candidate should
include that applied test commit with the reviewed implementation.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-rlr3i2.1` | Isolate nudge enqueue budget deploy branch | builder | ready-to-build |
| `ga-rlr3i2.2` | Deploy isolated nudge enqueue budget candidate | deployer | needs-deploy |

## Dependency Graph

```text
ga-rlr3i2.1 -> ga-rlr3i2.2
```

The deploy re-check waits for builder to record the isolated branch, final
commit SHA, and PR-ready evidence.

## Acceptance Summary

1. The builder produces a clean branch whose reviewer-visible diff is limited
   to the nudge enqueue maintenance budget implementation and its direct
   validator coverage.
2. The clean candidate includes reviewed implementation commit `93be1b652`
   and applied validator coverage commit `0ad8da96b` (source validator
   commit `8fbe53349`), adapted only as required to merge with current
   `origin/main`.
3. The candidate excludes unrelated Dolt maintenance changes, unrelated
   runbook/docs changes, prior release-gate artifacts, and untracked files.
4. The candidate branch merges cleanly with current `origin/main`; the known
   conflicts in `cmd/gc/cmd_nudge.go` and `cmd/gc/cmd_nudge_test.go` are
   resolved while preserving the reviewed behavior and validator assertions.
5. The builder records the final branch name, commit SHA, diff scope summary,
   and targeted verification evidence in the builder bead notes.
6. The deployer runs the standard deploy gate only against the isolated
   candidate.
7. On deploy PASS, the deployer opens or updates the PR for the isolated
   candidate and routes merge authority to mayor/mpr. No rig agent merges to
   `main` directly.

## Out Of Scope

- Shipping the unrelated Dolt maintenance or runbook commits from the failed
  deploy branch.
- Reopening the already-passed reviewer decision for `ga-nr4996`.
- Additional architecture work for the deeper nudge-queue flock/store-I/O
  issue, which remains outside this deploy slice.
