# Release Gate: Order-Dispatch Tracking Index Race Fix

Date: 2026-06-09
Deploy bead: ga-9rc1br.4
Implementation bead: ga-9rc1br.2
Test bead: ga-9rc1br.1
Review bead: ga-9rc1br.3
GitHub PR: https://github.com/gastownhall/gascity/pull/3263
Branch: fix/order-dispatch-tracking-index-race
Reviewed commit: fd24007417b7234101d22406406e48e4287ee7f3
Base checked: origin/main 9f4ac3e981f6f5d44948a302c03b6302fd5e2e2c
Merge base: 4e8a6a04cf06ad28c8f6cd2e0edad8917402718b

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer prompt criteria and the deploy bead acceptance checklist.

## Scope

The deploy branch contains one reviewed feature theme: making the order-dispatch tracking index safe when bounded open-work gates continue running concurrently after timeout or context cancellation.

The release diff before this gate artifact contains only:

| Path | Purpose |
|---|---|
| `cmd/gc/order_dispatch.go` | Adds a mutex to `orderDispatchTrackingIndex` and keeps lock scope limited to cached map access, not bd list calls. |
| `cmd/gc/order_dispatch_tracking_index_race_test.go` | Adds a race regression that drives concurrent `hasOpenTracking` and `lastRunFunc` access through one shared index. |

No `internal/molecule/*` paths, formula-hash implementation changes, or prior `ga-9rc1br` formula-hash gate artifacts are included in this release diff.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-9rc1br.3` is closed with close reason `pass`, notes `PASS: reviewer verdict 2026-06-09`, and reviewed commit `fd2400741` on `fix/order-dispatch-tracking-index-race`. |
| 2 | Acceptance criteria met | PASS | See acceptance evidence below. The branch is the reviewer-passed order-dispatch commit, diff scope is two `cmd/gc` files, and the PR is scoped to the tracking-index concurrency fix. |
| 3 | Tests pass | PASS | Focused `go test -race` regression passed; `make test-fast-parallel` passed on rerun with short `TMPDIR`; `go vet ./...` passed. GitHub PR #3263 also reports `mergeStateStatus: CLEAN` and green CI rollup for reviewed commit `fd2400741`. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-9rc1br.3` records `NO blockers`; unresolved HIGH findings count is 0. GitHub PR #3263 has no review comments or PR comments. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed `## HEAD (no branch)` with no uncommitted changes before writing this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `8365b0eb685050377778dfd35998ec2538ae7a3b`; GitHub reports PR #3263 `mergeStateStatus: CLEAN`. |
| 7 | Single feature theme | PASS | The commit set is one commit touching only `cmd/gc/order_dispatch.go` and one targeted race test. All behavior is the order-dispatch tracking-index concurrency fix. |

## Acceptance Evidence

| Deploy bead criterion | Result | Evidence |
|---|---|---|
| Gate runs against a reviewer-passed branch/commit for the order-dispatch tracking-index fix. | PASS | Gate ran on `fd2400741`, the exact commit recorded in review bead `ga-9rc1br.3` as PASS. |
| Final diff excludes `internal/molecule/*` and formula-hash release-gate artifacts from `ga-9rc1br`. | PASS | `git diff --name-status origin/main...HEAD` before the gate showed only `cmd/gc/order_dispatch.go` and `cmd/gc/order_dispatch_tracking_index_race_test.go`. |
| Standard deploy gate criteria pass, including single feature theme. | PASS | All seven deploy gate criteria above are PASS. |
| On PASS, opens or updates a PR scoped only to order-dispatch concurrency and routes the merge request to mayor/mpr; no direct merge from an agent session. | PASS | PR #3263 is the existing scoped PR for this branch. This gate commit updates that PR; merge authority remains mayor/mpr and is routed by bead notes/mail after push. |
| On FAIL, records gate artifact and routes back to PM/reviewer with exact failing criteria. | N/A | Gate result is PASS. |

## Test Evidence

- PASS: `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-9rc1br4-testtmp go test -race $(go list -f '{{range .GoFiles}}{{printf "cmd/gc/%s " .}}{{end}}' ./cmd/gc) cmd/gc/order_dispatch_tracking_index_race_test.go -run TestOrderDispatchTrackingIndexConcurrentGatesAreRaceFree -count=1`
- PASS: `TMPDIR=/home/jaword/t/g9 LOCAL_TEST_JOBS=12 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel`
- PASS: `TMPDIR=/home/jaword/t/g9 go vet ./...`
- Environmental retry note: an initial `make test-fast-parallel` run failed only because `TMPDIR=/home/jaword/tmp/gascity-deploy-ga-9rc1br4-testtmp` made `TestStartVeryLongSocketDirFallsBackToTempDir` generate a subprocess socket path longer than the test's 100-character limit. The same command passed with the shorter temp root `/home/jaword/t/g9`.

Gate result: PASS.
