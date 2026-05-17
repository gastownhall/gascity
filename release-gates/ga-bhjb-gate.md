# Release gate: ga-bhjb - TestMain rig env scrub (ga-d02c)

Feature bead: **ga-d02c** (closed) - Fix: scrub gc rig env vars in cmd/gc TestMain to stop dolt sql-server orphans
Review bead: **ga-bhjb** (needs-deploy)
PR: <https://github.com/gastownhall/gascity/pull/1199>
Branch: `release/ga-bhjb`, rebased onto `origin/main` at `0c60ee793`

## Rebase disposition

PR #1199 originally added a cmd/gc `TestMain` scrub helper. Current `main`
already contains the generalized test-environment scrub helpers, so the rebase
keeps the canonical `cmd/gc/testenv_test.go` implementation and adapts the PR
coverage to that shape:

- `cmd/gc/testenv_test.go` now covers `clearProcessLiveEnvForTests` scrubbing
  inherited GC/BEADS/DOLT state while preserving test-control variables.
- `cmd/gc/testenv_test.go` keeps coverage for `isTestscriptCommandInvocation`
  so testscript command reinvocations are not scrubbed.
- `cmd/gc/path_helpers_test.go` routes `clearInheritedBeadsEnv` through
  `liveEnvKeysForTests()` and preserves `GC_HOME`, which cmd/gc package tests
  require during supervisor registry setup.

The duplicate pre-rebase helper file (`cmd/gc/testmain_test.go`) is no longer
needed after the rebase because the canonical helper already lives in
`cmd/gc/testenv_test.go`.

## Verification

| Check | Result |
|-------|--------|
| Rebase onto current `origin/main` | PASS |
| Focused pre-rebase PR tests | PASS |
| Focused post-conflict regression tests | PASS |
| Pre-commit hook | PASS |
| Branch working tree | Clean |

Focused tests run after resolving conflicts:

```text
go test ./cmd/gc -run 'TestCityRuntimeRunStartupPreflightsManagedDoltBeforeSessionSnapshot|Test(ClearProcessLiveEnvForTestsUnsetsInheritedState|IsTestscriptCommandInvocation)' -count=1
```

The first post-rebase `go test ./cmd/gc -count=1` exposed a conflict-resolution
bug where `clearInheritedBeadsEnv` scrubbed `GC_HOME` too broadly. The final
branch fixes that by preserving `GC_HOME`, then the focused regression above
passed.

The local pre-commit hook also passed with:

- `lint-changed: ./cmd/gc`
- OpenAPI/schema sync checks
- `go vet ./...`
- observable fast unit sweep: `GC_FAST_UNIT=1 scripts/go-test-observable test -- -p=4 -count=1 ./...`
- docs sync tests
- dashboard generation, build, typecheck, and `go test ./cmd/gc/dashboard/...`

## Disposition

Release gate remains PASS after rebase. The PR branch is ready to push back to
`quad341:release/ga-bhjb`.
