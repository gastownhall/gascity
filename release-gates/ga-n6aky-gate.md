# Release Gate: ga-n6aky

Bead: `ga-n6aky` - Review: PR #1149 session read-path liveness fix

Source PR: https://github.com/gastownhall/gascity/pull/1149

Source branch: `feat/adr-0001-status-routing`

Source commit: `b87a7436c` (`fix(api): keep session reads live during cache priming`)

Release branch: `release/ga-n6aky`

Cherry-picked commit: `7c15a2f90`

Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this worktree; evaluated against the deployer release gate criteria.

## Gate Result

FAIL

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-n6aky` includes `VERDICT: pass` and "No blockers" for builder commit `b87a7436c`. |
| 2 | Acceptance criteria met | PASS | Final diff from `origin/main` changes only `internal/api/huma_handlers_sessions_query.go`, removing `cacheLiveOr503(store)` from session list/get handlers while retaining the `store == nil` guards and partial read-model behavior. `go test ./internal/api -count=1` passed, and `go test -tags integration ./test/integration -run TestGCLiveContract_BeadsAndEvents -count=1` passed. |
| 3 | Tests pass | FAIL | `make test` failed. The failing package is `github.com/gastownhall/gascity/examples/gastown`; failing test is `TestPolecatFormulaHaltsOnAutoPushFalse` with `gastown_test.go:777: missing "**2. Push your branch:**" after byte offset 0`. Reproduced with `go test ./examples/gastown -run TestPolecatFormulaHaltsOnAutoPushFalse -count=1`. The same test/formula mismatch is present in `origin/main` (`origin/main` test expects `**2. Push your branch:**`; `origin/main` formula contains `**3. Push your branch:**`). |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-n6aky` report "No blockers"; only two informational pre-existing findings were noted and neither is introduced by this change. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed `## release/ga-n6aky...origin/main [ahead 1]` with no uncommitted changes. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created from `origin/main`; `git cherry-pick b87a7436c` applied cleanly; `git merge-base --is-ancestor origin/main HEAD` passed; `git diff --check origin/main...HEAD` produced no output. |

## Commands Run

- `git fetch origin main:refs/remotes/origin/main`
- `git checkout -B release/ga-n6aky origin/main`
- `git cherry-pick b87a7436c`
- `go test ./internal/api -count=1` - PASS
- `go test -tags integration ./test/integration -run TestGCLiveContract_BeadsAndEvents -count=1` - PASS
- `make dashboard-check` - PASS
- `go vet ./...` - PASS
- `make test` - FAIL
- `go test ./examples/gastown -run TestPolecatFormulaHaltsOnAutoPushFalse -count=1` - FAIL
- `git merge-base --is-ancestor origin/main HEAD` - PASS
- `git diff --check origin/main...HEAD` - PASS

## Diagnosis

The release change itself is surgical and the targeted regression checks passed. The release gate still fails because the rig's full test command does not pass on the assembled branch. The failure appears to be a pre-existing `origin/main` mismatch in the Gastown example test expectations, outside the session read-path change.
