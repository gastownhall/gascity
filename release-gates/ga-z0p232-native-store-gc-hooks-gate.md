# Release Gate: native-store gc-stamped bd hooks

Bead: ga-z0p232
Source review bead: ga-97ahsp
Branch: builder/ga-exfzfw
Reviewed commit: 22105c206c3e40c0e1d4ead7954014b81be7fc8b
PR: https://github.com/gastownhall/gascity/pull/3270
Gate date: 2026-06-09

## Summary

PASS. This is a single-theme native-store selection fix. Gas City now ignores
the executable bd hook scripts it installs itself when deciding whether native
in-process bead storage is safe, while preserving the existing fallback for
non-stamped third-party hooks.

`docs/PROJECT_MANIFEST.md` is not present on this branch; this gate uses the
deployer role release criteria and the repository testing guidance in
`TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-97ahsp is closed with close reason `pass`; notes contain `Review verdict: PASS`; reviewer reported no blockers. |
| 2 | Acceptance criteria met | PASS | Diff is limited to `internal/beads/factory.go` and `internal/beads/factory_test.go`. Existing non-stamped hook fallback remains covered by `TestOpenStoreAtForCityExecutableHooksBlockNativeStore`; new gc-stamped hook path is covered by `TestOpenStoreAtForCityGCStampedHooksDoNotBlockNativeStore`. Focused command passed: `go test ./internal/beads -run 'TestOpenStoreAtForCity(ExecutableHooksBlockNativeStore|GCStampedHooksDoNotBlockNativeStore)'`. |
| 3 | Tests pass | PASS | `make test-fast-parallel` initially reported two non-deterministic local failures; both failed tests passed on direct `-count=1` rerun, and a clean full `make test-fast-parallel` retry passed all fast jobs. `go vet ./...` passed. GitHub PR #3270 shows CI required checks passing. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blockers and no security concerns; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Clean detached gate worktree started at `origin/builder/ga-exfzfw`; after committing gate evidence, `git status --short --branch` showed no modified or untracked files. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded against the current `origin/main`; PR #3270 has no merge conflict and is blocked only by post-push checks while they run. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem and behavior: native bead store preflight handling for gc-owned bd hooks. |

## Test Details

- PASS: `go test ./internal/beads -run 'TestOpenStoreAtForCity(ExecutableHooksBlockNativeStore|GCStampedHooksDoNotBlockNativeStore)'`
- PASS after retry: `make test-fast-parallel`
- PASS: `go vet ./...`

## Initial Fast-Gate Retry Note

The first `make test-fast-parallel` run failed in:

- `internal/beads`: `TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand`
- `internal/runtime/tmux`: `TestDoStartSession_TreatsDeadlineAfterReadyAsSuccessWhenSessionAlive`

Both failed tests passed immediately when rerun directly with `-count=1`, and
the full fast-parallel command passed on the next run.
