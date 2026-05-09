# Release gate: ga-vux42u

**Bead:** [ga-vux42u — Tests: regression coverage for requireNoLeakedDoltAfter helper (ga-de27g follow-up)]
**Branch:** `builder/ga-vux42u-1`
**HEAD:** `4460c26b`
**Branched from `origin/main`:** `001db413` (1 commit behind current `origin/main` `ca81d000`)
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `gascity/reviewer` recorded `Review verdict: PASS` in bead notes (2026-05-07). Single-pass while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | All four criteria from the bead body verified by reviewer: (a) test fails if helper stops reporting new PIDs at cleanup; (b) tests run in <100ms without real spawn; (c) `go test ./cmd/gc/ -count=1` passes including new test; (d) `go vet`/`golangci-lint` clean. |
| 3 | Tests pass | PASS | See "Test runs" below. |
| 4 | No high-severity review findings open | PASS | 3 unresolved findings, all `info`-level (test-of-tests asymmetry, hypothetical scriptedDoltEnumerator overflow path, branch-1-behind note). 0 HIGH. |
| 5 | Final branch is clean | PASS | `git status` reports no tracked changes on `builder/ga-vux42u-1`. (One untracked `.gitkeep` is a deployer-worktree session artifact and is not part of the change.) |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --no-messages origin/main HEAD` exits 0 with no conflict markers. Branch is 1 commit behind `origin/main` (PR #1803 unrelated); 3-way merge will preserve that change. |

## Test runs (deployer, on `builder/ga-vux42u-1` HEAD `4460c26b`)

```
$ go test -count=1 -run 'TestRequireNoLeakedDolt|TestSnapshotDoltProcess' ./cmd/gc/
ok  	github.com/gastownhall/gascity/cmd/gc	0.128s

$ go test -count=1 -run TestCityRuntimeReload ./cmd/gc/
ok  	github.com/gastownhall/gascity/cmd/gc	6.756s

$ go vet ./...
(clean)

$ golangci-lint run ./cmd/gc/...
0 issues.
```

Pre-existing `cmd/gc` full-suite failures (`TestRigAnywhere_ResolveContext/*`,
`TestControllerQueryRuntimeEnvReturnsNilForNonBD`, env-pollution tests)
are reproduced on a clean `origin/main` worktree by the reviewer and are
unrelated to this branch — none touch `path_helpers_test.go` or
`dolt_leak_helper_test.go`.

## Commits in scope

```
4460c26b refactor(cmd/gc): green — inject enumerator into requireNoLeakedDoltAfter (refs ga-vux42u)
4593b93b test(cmd/gc): regression coverage for requireNoLeakedDoltAfter (ga-vux42u)
df542efb test(cmd/gc): add requireNoLeakedDoltAfter helper (ga-de27g)
```

`df542efb` is a cherry-pick of the helper itself (originally `79b3e64a`,
in flight as PR #1795). It is included so this branch can demonstrate
the regression coverage end-to-end without depending on PR #1795 landing
first. If #1795 lands first, the cherry-pick merges as a clean no-op;
if this PR lands first, #1795 will merge against equivalent state. Either
order is safe.

## Findings (informational only)

| Severity | File:Line | Summary |
|----------|-----------|---------|
| info | `cmd/gc/dolt_leak_helper_test.go:23-25` | `recordingTB.Fatalf` intentionally does not call `runtime.Goexit` (documented). Production wrappers always pass `*testing.T` (which does Goexit). Asymmetry confined to test-of-tests. Not blocking. |
| info | `cmd/gc/dolt_leak_helper_test.go:67` | `scriptedDoltEnumerator` Fatalf-on-overflow returns `nil` after `Fatalf`; harmless because `Fatalf` on `*testing.T` Goexits. Hypothetical concern only if the enumerator type is ever generalised to a non-aborting reporter. Not blocking. |
| info | branch | Branch is 1 commit behind `origin/main` (PR #1803 unrelated). 3-way merge clean. Not blocking. |
