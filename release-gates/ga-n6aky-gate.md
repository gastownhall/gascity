# Release Gate: ga-n6aky

Bead: `ga-n6aky` - Review: PR #1149 session read-path liveness fix

Source PR: https://github.com/gastownhall/gascity/pull/1149

Source branch: `feat/adr-0001-status-routing`

Source commit: `b87a7436c` (`fix(api): keep session reads live during cache priming`)

Release branch: `release/ga-n6aky`

Cherry-picked commit: `7c15a2f90`

Additional release support commits:

- `6775a3a39` (`test(gastown): renumber polecat submit Push step in halts-on-auto-push test`)
- `90300aa81` (`test(gc): stabilize order tracking sweep freshness clock`)

Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this worktree; evaluated against the deployer release gate criteria.

## Gate Result

PASS

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-n6aky` includes `VERDICT: pass` and "No blockers" for builder commit `b87a7436c`. |
| 2 | Acceptance criteria met | PASS | Final diff from `origin/main` changes only `internal/api/huma_handlers_sessions_query.go`, removing `cacheLiveOr503(store)` from session list/get handlers while retaining the `store == nil` guards and partial read-model behavior. `go test ./internal/api -count=1` passed, and `go test -tags integration ./test/integration -run TestGCLiveContract_BeadsAndEvents -count=1` passed. |
| 3 | Tests pass | PASS | The Gastown formula expectation mismatch was fixed by cherry-picking `55ae16332`; a wall-clock-sensitive `cmd/gc` watchdog test exposed by the broad suite was stabilized in `90300aa81`. Focused checks, `go vet ./...`, `make dashboard-check`, pre-commit `make test-fast-parallel`, and final `make test` passed. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-n6aky` report "No blockers"; only two informational pre-existing findings were noted and neither is introduced by this change. |
| 5 | Final branch is clean | PASS | Before writing this updated gate file, `git status --short --branch` showed `## release/ga-n6aky...origin/main [ahead 4]` with no uncommitted changes. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created from `origin/main`; `git cherry-pick b87a7436c` applied cleanly; `git merge-base --is-ancestor origin/main HEAD` passed; `git diff --check origin/main...HEAD` produced no output. |

## Commands Run

- `git fetch origin main:refs/remotes/origin/main`
- `git checkout -B release/ga-n6aky origin/main`
- `git cherry-pick b87a7436c`
- `git cherry-pick -x 55ae16332`
- `go test ./internal/api -count=1` - PASS
- `go test -tags integration ./test/integration -run TestGCLiveContract_BeadsAndEvents -count=1` - PASS
- `make dashboard-check` - PASS
- `go vet ./...` - PASS
- `go test ./examples/gastown -run TestPolecatFormulaHaltsOnAutoPushFalse -count=1` - PASS
- `go test ./examples/gastown -count=1` - PASS
- `go test ./cmd/gc -run TestOrderTrackingSweepWatchdogAllowsSweepOrderToCleanStaleTracking -count=20 -v` - PASS
- `go test ./cmd/gc -count=1` - PASS
- `make test` - PASS
- `.githooks/pre-commit` - PASS (`make test-fast-parallel` passed during commit)
- `git merge-base --is-ancestor origin/main HEAD` - PASS
- `git diff --check origin/main...HEAD` - PASS

## Diagnosis

The release change itself remains surgical. The initial release gate failed on a pre-existing Gastown example expectation mismatch, and the broad rerun then exposed a wall-clock-sensitive `cmd/gc` test. Both are fixed on `release/ga-n6aky`; the assembled branch now passes the required local gate commands.
