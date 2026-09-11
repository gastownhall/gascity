# Release Gate: nudge enqueue maintenance budget

Gate date: 2026-07-03

Deploy bead: ga-rlr3i2.2
Review bead: ga-1anpqe
Candidate branch: builder/ga-1k4paf-nudge-enqueue-deadline
Release branch: release/ga-rlr3i2-2-nudge-enqueue-deadline
Reviewed commits: ff04494c3, b7abe4d30
Candidate tip before this gate: b7abe4d302137e472bed9bcea7a9de62d4dcc3e7
Base checked: origin/main @ cdc933ba5ceaacefe0e4c17de63e2be29d379cc1

Note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in
this checkout. This gate uses the deployer release criteria from the active
role prompt plus the repository testing guidance in `TESTING.md`, matching
prior gates in this repository.

## Summary

This release bounds the foreground `gc sling --nudge` enqueue maintenance pass
to a two-second budget while preserving the queued work it does not process in
that pass. Non-foreground queue operations keep the previous full-drain
maintenance behavior through a far-future deadline.

The final release diff is one feature theme:

| Path | Change |
|---|---|
| `cmd/gc/cmd_nudge.go` | Adds `nudgeEnqueueMaintenanceBudget`, threads maintenance deadlines through queue cleanup helpers, and only applies the short deadline to the foreground enqueue path. |
| `cmd/gc/cmd_nudge_test.go` | Updates existing helper calls for the new deadline argument. |
| `cmd/gc/sling_nudge_backlog_test.go` | Adds regression coverage showing enqueue duration is bounded independent of backlog size. |
| `cmd/gc/sling_nudge_budget_test.go` | Adds empty-backlog and item-preservation coverage for budgeted enqueue maintenance. |

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-1anpqe` records `REVIEW VERDICT: PASS (independent re-review after supersede)` for `builder/ga-1k4paf-nudge-enqueue-deadline` at `b7abe4d30`. |
| 2 | Acceptance criteria met | PASS | The gate ran against the isolated branch recorded by `ga-rlr3i2.1`; `git diff --name-only origin/main...origin/builder/ga-1k4paf-nudge-enqueue-deadline` is limited to the four nudge implementation/test files listed above. The branch contains the reviewed nudge enqueue budget implementation and direct validator-style coverage only. |
| 3 | Tests pass | PASS | `gofmt -l` on changed files produced no output. `git diff --check origin/main...origin/builder/ga-1k4paf-nudge-enqueue-deadline` produced no output. `go test ./cmd/gc/ -run Nudge -count=1 -v` passed. `go test ./internal/nudgequeue/... -count=1` passed. `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel` passed. |
| 4 | No high-severity review findings open | PASS | `ga-1anpqe` notes say "No blocking findings on the code itself" and "No blocking findings either way"; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed `## release/ga-rlr3i2-2-nudge-enqueue-deadline...origin/builder/ga-1k4paf-nudge-enqueue-deadline` with no worktree changes. This gate file is the only deployer-authored change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main origin/builder/ga-1k4paf-nudge-enqueue-deadline` exited 0 and produced tree `0e066c9b35c6ed8b14acb42b94fc808180953f71`; no merge conflicts with current `origin/main`. |
| 7 | Single feature theme | PASS | The commit set is confined to `gc sling --nudge` foreground enqueue budget behavior plus direct nudge queue tests. No unrelated Dolt, runbook, docs, or release-gate artifacts are present in the reviewed diff. |

## Acceptance Checklist

- [x] Gate evaluated the isolated candidate branch from `ga-rlr3i2.1`, not the previously contaminated branch.
- [x] Release diff is limited to the nudge enqueue maintenance budget implementation and direct validator coverage.
- [x] Candidate branch has no untracked worktree files or unrelated release-gate artifacts.
- [x] Candidate branch merges cleanly with current `origin/main`.
- [x] Required build, vet, focused tests, and fast unit baseline passed.
- [x] Deployer will open a PR and route merge authority to mayor/mpr, not merge directly.

## Test Log

```text
gofmt -l cmd/gc/cmd_nudge.go cmd/gc/cmd_nudge_test.go cmd/gc/sling_nudge_backlog_test.go cmd/gc/sling_nudge_budget_test.go
# no output

git diff --check origin/main...origin/builder/ga-1k4paf-nudge-enqueue-deadline
# no output

go test ./internal/nudgequeue/... -count=1
ok  	github.com/gastownhall/gascity/internal/nudgequeue	0.034s

go vet ./...
# no output

go build ./...
# no output

go test ./cmd/gc/ -run Nudge -count=1 -v
PASS
ok  	github.com/gastownhall/gascity/cmd/gc	14.100s

Focused nudge evidence from the same run:
- TestSlingNudgeEnqueueBoundedByBacklog: backlog=40 -> 2.038s; backlog=160 -> 2.045s.
- TestSlingNudgeEnqueueBudgetPreservesQueuedItems: PASS.
- TestSlingNudgeEnqueueEmptyBacklogFast: PASS.

make test-fast-parallel
[fsys-darwin-compile] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-core] ok
All fast jobs passed
```
