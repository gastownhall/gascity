# Release gate - tmux startup deadline test flake (ga-kw7i0z)

**Verdict:** PASS

- Deploy bead: `ga-kw7i0z`
- Source review bead: `ga-8hxfet` (closed PASS)
- PR: https://github.com/gastownhall/gascity/pull/3530
- Branch: `builder/ga-p3z19x-flaky-deadline-test`
- Gate input HEAD: `6a7af8e174afd778feac0911eb35615a555329e6`
- Current `origin/main`: `8c84dc3902149cff17137f0f994c31f2a2ea3733`
- Merge-tree result against current main: `2608c8b5a228adb26859ff26865f18380c4bf329`
- Gate run date: 2026-06-15
- Project manifest note: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the deployer prompt's release criteria plus the bead acceptance context.

## Scope note

The branch history contains two test-stability commits:

- `41114c1ff` - integration `uniqueCityName` prefix collision fix
- `6a7af8e17` - tmux startup deadline-after-ready race fix

Current `origin/main` already contains the integration prefix-collision fix as
`e5bae16a7`, so the clean merge result against current main changes only:

- `internal/runtime/tmux/startup_test.go`

`git diff origin/main 2608c8b5a228adb26859ff26865f18380c4bf329` shows a
single-file merge effect: 5 insertions and 1 deletion in the tmux startup test.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | Review bead `ga-8hxfet` is closed with `VERDICT: PASS` for commit `6a7af8e17`; deploy bead `ga-kw7i0z` records reviewed + passed status. |
| 2 | Acceptance criteria met | PASS | The effective merge changes `TestDoStartSession_TreatsDeadlineAfterReadyAsSuccessWhenSessionAlive` to wait on `<-ctx.Done()` instead of sleeping 5ms, guaranteeing `ctx.Err()` is non-nil before the post-ready branch is evaluated. No production code changes. |
| 3 | Tests pass on final branch | PASS | Local focused tmux test, focused integration smoke, `go vet ./...`, and `make test-fast-parallel` all passed. GitHub CI run `27573629348` completed `success`, including required preflight and integration summaries. |
| 4 | No high-severity review findings open | PASS | Review notes contain only non-blocking INFO follow-ups; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before this gate file was added. This gate file is the only deployer change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed successfully with tree `2608c8b5a228adb26859ff26865f18380c4bf329`; PR #3530 reports `mergeStateStatus: CLEAN`. |
| 7 | Single feature theme | PASS | The effective merge surface is a single tmux test flake fix. The branch's integration-helper commit is already present on main and has no additional merge effect. |

## Validation

- `go test ./internal/runtime/tmux -run 'TestDoStartSession_TreatsDeadlineAfterReadyAsSuccessWhenSessionAlive|TestDoStartSession_TreatsDeadlineAfterPostReadyAsSuccessWhenSessionAlive' -count=20` - PASS
- `go test -tags integration ./test/integration -run 'TestHumaBinary|TestSupervisor' -count=1 -timeout=5m` - PASS (`77.378s`)
- `go vet ./...` - PASS
- `make test-fast-parallel` - PASS (`All fast jobs passed`)
- `git diff --check origin/main...HEAD` - PASS
- GitHub CI run `27573629348` - PASS (`conclusion: success`)

## Non-gating local note

An over-broad local package command, `go test -tags integration ./test/integration -run Test -count=1`, hit the default 10-minute package timeout in `TestE2E_Restart`. That command is not one of the documented sharded local runners and is broader than the deploy smoke requested for this test-only branch. The focused integration smoke and the PR's sharded CI integration suite both passed.

## Push target

PR #3530 is already open from `gastownhall/gascity:builder/ga-p3z19x-flaky-deadline-test` to `main`. Push target should remain `origin` if the deployer dry-run rule confirms write access.
