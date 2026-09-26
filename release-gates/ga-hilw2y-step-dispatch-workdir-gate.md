# Release Gate: ga-hilw2y Step-Dispatch Workdir

Date: 2026-07-24
Bead: ga-hilw2y
Type: single-bead deploy
Branch: deploy/ga-hilw2y-gate
Base: origin/main @ 80e5166473033b9f2807dad048ddcb70dfc3b86e
Deploy source: 196a8cc223859986101bc0a8634a33064723f847
Reviewed source: 82c7c5bf536fe40f910fcdeb9a130ac2f22dbecd

## Summary

This change creates the resolved per-step workdir before a step-dispatch
session starts, or returns a contextual error if the directory cannot be
created. Pool-managed sessions remain exempt because their workdir materializes
through the pool trigger binding path.

The deploy source is a rebased form of the reviewed commit. Stable patch-id
evidence:

| Commit | Stable patch ID |
| --- | --- |
| 82c7c5bf536fe40f910fcdeb9a130ac2f22dbecd | f9142e9596d3c77d557764e3bf74242fb8da1db6 |
| 196a8cc223859986101bc0a8634a33064723f847 | f9142e9596d3c77d557764e3bf74242fb8da1db6 |

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git merge-base --is-ancestor origin/main HEAD` returned 0. `git status --short --branch` showed `deploy/ga-hilw2y-gate...origin/main [ahead 1]`. |
| 1 | Review PASS present | PASS | Review bead `ga-rcj2zt` is closed and notes contain `VERDICT: PASS` for reviewed commit `82c7c5bf5`. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `cmd/gc/session_lifecycle_parallel.go` and `cmd/gc/session_lifecycle_parallel_test.go`. Focused tests cover directory creation, mkdir failure, and pool-managed exemption. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestPrepareStartCandidateForCity_(CreatesMissingStepDispatchWorkDir|FailsLoudlyWhenStepDispatchWorkDirCannotBeCreated|PoolManagedSkipsWorkDirCreation)$' -count=1 -v` passed. `go vet ./...` passed. `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Review notes have PASS verdict and no unresolved HIGH findings; reviewer explicitly did not flag the pre-existing path-traversal behavior as a finding. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only branch/ahead state and no worktree changes. |
| 7 | Single feature theme | PASS | One subsystem: step-dispatch session workdir preparation in `cmd/gc`, with colocated unit tests. No independent feature themes. |

## Commands

```bash
git show --format=email --ignore-space-change 82c7c5bf5 | git patch-id --stable
git show --format=email --ignore-space-change 196a8cc223859986101bc0a8634a33064723f847 | git patch-id --stable
git diff --check origin/main...HEAD
go test ./cmd/gc -run 'TestPrepareStartCandidateForCity_(CreatesMissingStepDispatchWorkDir|FailsLoudlyWhenStepDispatchWorkDirCannotBeCreated|PoolManagedSkipsWorkDirCreation)$' -count=1 -v
go vet ./...
make test-fast-parallel
git merge-base --is-ancestor origin/main HEAD
```
