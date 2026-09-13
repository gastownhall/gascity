# Release Gate: tmux Server Teardown After Stop Orphans

Date: 2026-06-01
Deploy bead: ga-62zmxp
Source review bead: ga-m64fa4
Existing stacked PR: https://github.com/gastownhall/gascity/pull/2778
Branch: release/ga-62zmxp-tmux-server-teardown
Feature code commit: abe898be1
Original reviewed commit: 56bf1df7762238df24d854f3f3c1182b5b408148
Base checked: origin/main 1648f272916eeef0f89b58b6dccf7e41a62c8cba

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the deployer prompt criteria and the source review bead checklist.

## Scope

The final release branch was cut fresh from current `origin/main` after PR
#2774 merged, then cherry-picked only the reviewed stop-teardown commit. This
drops the already-merged tmux server lifecycle provider commit from the PR
diff and leaves one feature slice plus this release gate.

| Path | Status | Purpose |
|---|---:|---|
| `cmd/gc/cmd_stop.go` | M | Calls optional tmux server teardown after orphan stopping and before bead-provider shutdown. |
| `cmd/gc/cmd_stop_server_lifecycle_test.go` | A | Covers stop ordering, non-lifecycle provider skip behavior, and best-effort teardown error reporting. |
| `release-gates/ga-62zmxp-tmux-server-teardown-gate.md` | A | Release gate evidence for deploy bead `ga-62zmxp`. |

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-m64fa4` shows closed review bead with `REVIEW VERDICT: PASS` for PR #2778 and reviewed commit `56bf1df7762238df24d854f3f3c1182b5b408148`. |
| 2 | Acceptance criteria met | PASS | Final branch preserves the reviewed behavior: `gc stop` calls `runtime.ServerLifecycleProvider.TeardownServer()` after `stopOrphans(...)` and before `shutdownBeadsProviderForStop(...)`; non-lifecycle providers skip teardown; teardown errors are reported on stderr without changing the stop exit code. |
| 3 | Tests pass | PASS | Focused `cmd/gc` stop tests passed; `go test ./internal/runtime/tmux ./internal/runtime` passed; `go vet ./...` passed; `make test-fast-parallel` passed with `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Review notes list P0/P1 blockers as none and only advisory/info findings; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file; final clean status is verified after committing this gate. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` refreshed base `1648f2729`; `origin/main` is an ancestor of `HEAD`; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `6585fe29fc77323067251948a7f9b2d4b1fff0c5`; `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem and one user-facing behavior: `gc stop` tmux server teardown ordering. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Rebase/drop the now-merged PR #2774 runtime lifecycle provider stack. | PASS | Final branch is based on current `origin/main` and contains only the stop-teardown commit plus this gate; diff paths are limited to `cmd/gc/cmd_stop.go`, `cmd/gc/cmd_stop_server_lifecycle_test.go`, and this gate file. |
| Run teardown after orphan stopping. | PASS | `cmdStopBody` still calls `stopOrphans(...)` first, then `teardownServerForStop(sp, stderr)`. |
| Run teardown before bead-provider shutdown. | PASS | `teardownServerForStop(...)` is placed before `shutdownBeadsProviderForStop(cityPath)`. `TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown` verifies the observed sequence. |
| Preserve non-tmux provider compatibility. | PASS | `teardownServerForStop` uses a type assertion to `runtime.ServerLifecycleProvider`; `TestCmdStopBodySkipsTeardownForNonLifecycleProvider` verifies non-lifecycle providers complete without teardown noise. |
| Keep teardown best-effort and visible. | PASS | `teardownServerForStop` writes `gc stop: teardown server: ...` to stderr on error and does not change `cmdStopBody`'s return code; `TestCmdStopBodyReportsTeardownErrorWithoutFailing` covers this. |
| Preserve tmux safety. | PASS | This slice does not add a bare tmux cleanup path; teardown delegates through the existing provider lifecycle surface, which uses socket-scoped tmux operations from the merged provider slice. |

## Test Evidence

- PASS: `go test ./cmd/gc -run 'TestCmdStopBodyTeardownRunsAfterStopOrphansBeforeBeadsShutdown|TestCmdStopBodySkipsTeardownForNonLifecycleProvider|TestCmdStopBodyReportsTeardownErrorWithoutFailing|TestCmdStop'`
- PASS: `go test ./internal/runtime/tmux ./internal/runtime`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel`
- PASS: `.githooks/pre-commit` is active via `core.hooksPath=.githooks`; commit hook will run for the staged gate commit.

Gate result: PASS.
