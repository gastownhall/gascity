# Release Gate: nudge status maintenance budget

Gate date: 2026-07-03

Deploy bead: ga-7ibasi
Review bead: ga-5mn9vi
Source beads: ga-7p2ahd.1, ga-7p2ahd.2, ga-7p2ahd.3
Candidate branch: release/ga-cigcno-nudge-drain-budget
Existing PR: https://github.com/gastownhall/gascity/pull/3906
Reviewed commits: 03bbadf42, 9a182a7db, c7f57828c
Candidate tip before this gate: c7f57828c2b2bddc272ac8d6c045aa7370d955e6
Base checked: origin/main @ 1dfce8f962d31d7df683f91dd003f20429be2b5b

Note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in
this checkout. This gate uses the deployer release criteria from the active
role prompt plus the repository testing guidance in `TESTING.md`, matching
prior gates in this repository.

## Summary

This release bounds foreground `gc nudge status` queue maintenance to the same
short maintenance budget used by other foreground nudge operations while keeping
poller and delivery paths explicitly unbounded. Status reads still return the
requested pending, in-flight, and dead queue rows, and skipped terminal backlog
items remain queued for later maintenance instead of being dropped.

The final release diff is one feature theme:

| Path | Change |
|---|---|
| `cmd/gc/cmd_nudge.go` | Threads maintenance deadlines through nudge status listing helpers and uses the foreground budget for status/liveness reads while keeping poller delivery full-drain. |
| `cmd/gc/cmd_nudge_status_compat_test.go` | Adds compatibility coverage for status bucket reads, target matching, table output shape, and explicitly unbounded poller-style maintenance. |
| `cmd/gc/cmd_nudge_test.go` | Updates helper call sites for explicit maintenance deadlines. |
| `cmd/gc/nudge_drain_backlog_test.go` | Covers bounded foreground drain maintenance and unbounded poller drain behavior from the existing PR branch. |
| `cmd/gc/nudge_status_budget_test.go` | Adds the direct regression proof for bounded status maintenance with no dropped queue rows. |
| `cmd/gc/sling_nudge_budget_test.go` | Updates existing budget coverage for the explicit deadline plumbing. |

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-5mn9vi` records `REVIEWER VERDICT (round 2): PASS` for `release/ga-cigcno-nudge-drain-budget` at `c7f57828c`. |
| 2 | Acceptance criteria met | PASS | `listQueuedNudges` and `listQueuedNudgesForTarget` now receive a caller-supplied maintenance deadline; foreground status/liveness reads pass `now.Add(nudgeForegroundMaintenanceBudget)`; poller delivery remains unbounded through `noMaintenanceDeadline()`. Focused tests cover bounded status maintenance, preservation of skipped queue rows, compatibility matching, and poller full-drain behavior. |
| 3 | Tests pass | PASS | `gofmt -l` on changed files produced no output. `git diff --check origin/main...HEAD` produced no output. Focused nudge tests passed. `go vet ./cmd/gc/...` passed. `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel` passed. |
| 4 | No high-severity review findings open | PASS | The round-1 deployability finding was remediated by moving the commits onto the real pushed PR branch; round-2 review records no outstanding blockers. Unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## HEAD (no branch)` with no worktree changes. This gate file is the only deployer-authored change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `5d49b24278af68614efdb7082756a64cab986f95`; PR #3906 reported `mergeable=MERGEABLE` and `mergeStateStatus=CLEAN` at candidate tip `c7f57828c`. |
| 7 | Single feature theme | PASS | The commit set is confined to `cmd/gc` nudge foreground maintenance budget behavior plus direct nudge tests. No unrelated Dolt, docs, generated artifacts, API, or dashboard changes are present in the reviewed diff. |

## Acceptance Checklist

- [x] Gate evaluated the real PR branch, not the stale round-1 builder branch.
- [x] Status listing uses a foreground maintenance budget.
- [x] Poller/delivery maintenance remains explicitly unbounded.
- [x] Status output behavior and target matching remain covered.
- [x] Skipped maintenance rows remain queued for a later maintenance pass.
- [x] Candidate branch merges cleanly with current `origin/main`.
- [x] Required focused tests, build, vet, and fast unit baseline passed.
- [x] Deployer will route merge authority to mayor/mpr, not merge directly.

## Test Log

```text
gofmt -l cmd/gc/cmd_nudge.go cmd/gc/cmd_nudge_status_compat_test.go cmd/gc/cmd_nudge_test.go cmd/gc/nudge_drain_backlog_test.go cmd/gc/nudge_status_budget_test.go cmd/gc/sling_nudge_budget_test.go
# no output

git diff --check origin/main...HEAD
# no output

git merge-tree --write-tree origin/main HEAD
5d49b24278af68614efdb7082756a64cab986f95

go test ./cmd/gc/ -run "Nudge(Status|Drain|Poller|Queued)" -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	9.113s

go test ./cmd/gc/ -run 'Test(ListQueuedNudgesStatusConsumerReadsAllBucketsForExactAgent|ListQueuedNudgesForTargetStatusConsumerMatchesTargetKeys|ClaimDueQueuedNudgesMatchingKeepsPollerMaintenanceUnbounded|ClaimDueQueuedNudgesForTargetClaimsHistoricalAlias|ListQueuedNudges_CategorizesPendingAndDead|NudgePollHelpersCloseEveryStoreTheyOpen|DispatchAllQueuedNudgesDeliversAndAcks)$' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.296s

go vet ./cmd/gc/...
# no output

go build ./...
# no output

go vet ./...
# no output

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-core] ok
All fast jobs passed
```
