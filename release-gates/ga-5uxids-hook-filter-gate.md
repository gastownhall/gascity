# Release Gate: ga-5uxids Hook Work Query Filtering

Bead: `ga-a5i63b` (deploy review for `ga-5uxids`)
Branch: `release/ga-a5i63b` from `fork/builder/ga-5uxids-1`
Commit: `b9490bfe6b3a6fc34b978238e802c128589bb241`
Gate date: 2026-05-14

## Summary

This gate evaluates the hook-side defensive filter that strips unready
`work_query` rows before `gc hook` returns work to an agent. The patch is
limited to `cmd/gc/cmd_hook.go` plus focused tests in
`cmd/gc/hook_defer_blocked_test.go`.

`docs/PROJECT_MANIFEST.md` is not present in this checkout or adjacent
worktree parent; the gate uses the deployer release criteria and `TESTING.md`
sharded-runner guidance.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-a5i63b` notes contain `Verdict: PASS - routing to gascity/deployer`; no request-changes findings were listed. |
| 2 | Acceptance criteria met | PASS | `doHook` now calls `filterUnreadyHookCandidates` after `normalizeWorkQueryOutput` and before `workQueryHasReadyWork`; the filter strips future `defer_until` rows and open/non-closed `blocked_by` rows while preserving malformed/non-array JSON, past deferred rows, and closed blockers. `cmd/gc/hook_defer_blocked_test.go` covers the required RED/GREEN cases plus the past-deferred/closed-blocker preservation edge case. |
| 3 | Tests pass | PASS | Focused hook regression tests pass; broader hook suite passes; `go vet ./...` is clean. `make test-fast-parallel` fails in unrelated `unit-core` and `cmd/gc` shards. The exact failing tests, `TestStartLongSocketPathUsesShortSocketName` and `TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag`, both reproduce on `origin/main`. No hook test failed. |
| 4 | No high-severity review findings open | PASS | `bd list --status open --limit 0` search for this bead/original bead plus HIGH/request-changes markers found no open matching findings. Reviewer notes list no request-changes findings. |
| 5 | Final branch is clean | PASS | After committing this gate file, the branch has no uncommitted tracked changes. A pre-existing untracked root `.gitkeep` was present before deployer work and is not part of the branch. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` produced a merged tree without conflicts. `git diff --check origin/main...HEAD` is clean. |

## Commands Run

```text
bd show ga-a5i63b
bd show ga-5uxids
gh auth status
git fetch origin main
git fetch fork builder/ga-5uxids-1
git show --check --stat b9490bfe
git diff --name-status origin/main...b9490bfe
git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD
git diff --check origin/main...HEAD
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp go test ./cmd/gc/ -run 'TestDoHookFiltersDeferred|TestDoHookFiltersDepBlocked|TestDoHookKeepsPastDeferredAndClosedBlockers' -count=1
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp go test ./cmd/gc/ -run 'TestHook|TestCmdHook|TestDoHook' -count=1
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp go vet ./...
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-go-tmp make test-fast-parallel
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-origin-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-origin-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-origin-go-tmp go test ./internal/runtime/acp -run '^TestStartLongSocketPathUsesShortSocketName$' -count=1
env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deployer-origin-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deployer-origin-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deployer-origin-go-tmp go test ./cmd/gc/ -run '^TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag$' -count=1
git merge-tree --write-tree origin/main HEAD
git diff --check origin/main...HEAD
git config core.hooksPath
bd list --status open --limit 0 | rg -i -- 'ga-a5i63b|ga-5uxids|high|request-changes'
```

## Test Results

```text
go test ./cmd/gc/ -run 'TestDoHookFiltersDeferred|TestDoHookFiltersDepBlocked|TestDoHookKeepsPastDeferredAndClosedBlockers' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.095s

go test ./cmd/gc/ -run 'TestHook|TestCmdHook|TestDoHook' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.578s

go vet ./...
PASS

make test-fast-parallel
FAIL: unit-core
  TestStartLongSocketPathUsesShortSocketName
  acp_test.go:741: failed to construct path where legacy socket is too long but short socket fits

FAIL: unit-cmd-gc-6-of-6
  TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag
  session_model_phase0_spec_test.go:189: resolving configured named session "mayor": session name already exists: "mayor" already active in runtime

origin/main reproduction:
  TestStartLongSocketPathUsesShortSocketName fails with the same path construction error.
  TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag
  fails with the same active-runtime session-name conflict.
```

## Acceptance Mapping

| Done-when item | Result | Evidence |
|---|---|---|
| `cmd/gc/hook_defer_blocked_test.go` exists with required tests | PASS | File added with future-deferred, dep-blocked, and preservation tests. |
| Focused hook filter tests pass | PASS | `go test ./cmd/gc/ -run 'TestDoHookFiltersDeferred|TestDoHookFiltersDepBlocked|TestDoHookKeepsPastDeferredAndClosedBlockers' -count=1` passed. |
| Existing hook behavior is unchanged | PASS | `go test ./cmd/gc/ -run 'TestHook|TestCmdHook|TestDoHook' -count=1` passed. |
| `go vet ./cmd/gc/` / broader vet clean | PASS | `go vet ./...` passed. |
| Fast baseline | PASS with baseline exception | `make test-fast-parallel` failures are pre-existing and outside this hook change; both exact failing tests reproduce on `origin/main`. |
| Pre-commit hook active | PASS | `git config core.hooksPath` reports `.githooks`; pre-commit will run for the gate commit. |
