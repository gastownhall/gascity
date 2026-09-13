# Release Gate: GC_FAST_UNIT Dolt lifecycle test guards

- Deploy bead: `ga-vmc5vi`
- Source review bead: `ga-c1d2sc`
- Source fix bead: `ga-j0uv9n`
- PR: https://github.com/gastownhall/gascity/pull/3518
- Branch: `builder/ga-j0uv9n-unit-cover-guards`
- Evaluated source commit: `47474383aa45e58fced50d54a50c01829586400a`
- Local `origin/main` at gate time: `1ff917a8913d69443392829c26a2b9f2e2196ab8`
- Merge base with `origin/main`: `aafabd2a43f862535ac077ab49ce9e8d267eaf0c`
- Manifest note: `docs/PROJECT_MANIFEST.md` is absent in this worktree; the release criteria from the deployer prompt were used.

## Scope

The change is test-only: 13 `cmd/gc` tests that start real Dolt lifecycle
infrastructure now call `skipSlowCmdGCTest(t, "starts real Dolt lifecycle")`
as their first line. Under `GC_FAST_UNIT=1` they are skipped from the unit-cover
preflight; under normal process/integration runs they still execute.

Changed files:

- `cmd/gc/api_state_test.go`
- `cmd/gc/cmd_convoy_dispatch_test.go`
- `cmd/gc/cmd_doctor_test.go`
- `cmd/gc/cmd_prime_test.go`
- `cmd/gc/cmd_reload_test.go`
- `cmd/gc/controller_test.go`
- `cmd/gc/script_resolve_test.go`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-c1d2sc` is closed and records `Review verdict: PASS` for commit `47474383aa45e58fced50d54a50c01829586400a`. |
| 2 | Acceptance criteria met | PASS | The diff is limited to seven `cmd/gc` test files and adds 13 fast-unit skip guards for tests that start real Dolt lifecycle infrastructure. Targeted smoke under `GC_FAST_UNIT=1` confirmed all 13 named tests skip immediately with reason `starts real Dolt lifecycle`. |
| 3 | Tests pass | PASS | `GC_FAST_UNIT=1 go test ./cmd/gc -run '<13 guarded tests>' -count=1 -timeout=2m -v` passed; `make test` passed with observable log `/tmp/gascity-test.jsonl.tO6VhV`; `go vet ./...` passed; `git diff --check origin/main...HEAD` passed. PR checks on the reviewed source tip were clean before adding this gate commit. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-c1d2sc` record no open concerns and no high-severity findings. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed a clean `deploy/ga-vmc5vi-unit-cover-guards` worktree tracking `origin/builder/ga-j0uv9n-unit-cover-guards`; `.githooks` is active via `core.hooksPath`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed successfully with tree `1d5b32ccf66ca1561214be1cd1d970b9c317756c`; GitHub reported PR #3518 `mergeStateStatus: CLEAN` on the reviewed source tip. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem and one behavior: `cmd/gc` fast-unit coverage excludes real Dolt lifecycle tests while leaving slower process/integration coverage intact. |

## Guarded Tests

- `TestControllerStateRuntimeUpdateAcceptsBuiltinAwareRevision`
- `TestControllerStateMutationRefreshKeepsBuiltinOrdersAndClearsPending`
- `TestRunWorkflowServeReturnsControlErrorWithoutQuarantine`
- `TestDoDoctorRegistersStaleLocalPackDirCheck`
- `TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport`
- `TestDoPrimeWithHook_DeliveredStartupPromptCodexJSONHookFormat`
- `TestDoPrimeWithHook_CodexJSONFormatInfersAgentFromWorkDir`
- `TestSendReloadControlRequestNoChange`
- `TestReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged`
- `TestSendReloadControlRequestInvalidConfig`
- `TestControllerReloadInvalidConfig`
- `TestControllerReloadCityNameChange`
- `TestPrepareCityForSupervisorPrunesLegacyScripts`
