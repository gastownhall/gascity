# Release Gate: ga-x6b7iv backlog-depth dep-check + scope

Date: 2026-06-05
Deployer: gascity/deployer
Deploy bead: ga-x6b7iv
Source/review bead: ga-5bq0f8
PR: https://github.com/gastownhall/gascity/pull/3133
Branch: fix/ga-671hz5-doctor-backlog-depth-dep-check
Reviewed commit: 1712aae00e6b487c99bfd19bbb44ef07afb30896
Base: origin/main at 5334499347c1ec389ff04ce0c11e93989d72b2ae

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the deployer release criteria from the active Gas City deployer prompt.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source/review bead `ga-5bq0f8` is closed with `REVIEW VERDICT: PASS`; deploy bead `ga-x6b7iv` records reviewer PASS metadata. |
| 2 | Acceptance criteria met | PASS | `classifyBacklog` now consumes `store.Ready()` IDs instead of `beads.IsReadyCandidateForTier`; dep-blocked `B-1` fixtures land in `other`, not `real`; result message says `city store`; `store.Ready()` errors return advisory warning. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestClassifyBacklog|TestBacklogDepthCheck'` passed; `make test-fast-parallel` passed; `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Review notes list no Critical findings and one non-blocking Minor coverage follow-up (`ga-j5n5xr`). Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Clean worktree before gate file: `git status --short --branch` reported only `## HEAD (no branch)`. Gate file is committed in the release-gate commit and the worktree is rechecked clean before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` returned successfully before push and after the gate commits; GitHub reports `mergeable=MERGEABLE`. `mergeStateStatus=BLOCKED` is from pending CI/branch protection, not a content conflict. |
| 7 | Single feature theme | PASS | Commit set touches only `cmd/gc/doctor_backlog_depth.go` and `cmd/gc/doctor_backlog_depth_test.go`; one subsystem and one user-visible behavior: `gc doctor` backlog-depth reporting. |

## Acceptance Evidence

- Claimable backlog count is dependency-aware: `Run` calls `store.Ready()`,
  builds a `readyIDs` set, and `classifyBacklog` treats only those IDs as real
  claimable work after filtering control-plane, notification, and epic beads.
- Dep-blocked task-type beads no longer overcount claimable work: both
  `TestClassifyBacklog` and `TestBacklogDepthCheckRunReportsTrueDepth` include
  blocked bead `B-1` and assert it is excluded from claimable details.
- Scope wording is accurate: the operator-facing message now starts with
  `city store:` to avoid implying rig-wide coverage.
- Observability remains non-blocking: store-open/list/ready failures return
  advisory `StatusWarning` results, never a blocking `StatusError`.

## Test Evidence

```text
$ go test ./cmd/gc -run 'TestClassifyBacklog|TestBacklogDepthCheck'
ok  	github.com/gastownhall/gascity/cmd/gc	0.776s

$ make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-core] ok
All fast jobs passed

$ go vet ./...
PASS
```

## Files Reviewed

```text
cmd/gc/doctor_backlog_depth.go
cmd/gc/doctor_backlog_depth_test.go
```
