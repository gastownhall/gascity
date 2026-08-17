# Release Gate: gc status provider cache adoption

Bead: `ga-3ojbg1` - needs-deploy for `ga-soqo5g`
Review bead: `ga-qapqi0`
Branch: `deploy/ga-3ojbg1-provider-cache`
Reviewed source branch: `origin/gc-builder-2-6fa6ba1e8999`
Reviewed commit: `d7bb52816619a566f437c723400ffc5be49d4693`
Base checked: `origin/main` at `3be7535bc41a2210af21c33bbf67d94022c84ef2`
Merge base: `1ce90331a13e08ba8c1ba649a210e656080c32a5`
Gate date: 2026-07-04

## Summary

This release routes `gc status` CLI provider-transport resolution through
`config.ResolvedProviderCached` where the resolved-provider cache is populated,
with fallback to the existing uncached `config.ResolveProvider` path. It also
computes the ACP route-name set once during status provider construction instead
of recomputing it for each wrapper decision.

The diff is limited to `cmd/gc/providers.go` and
`cmd/gc/providers_test.go`. `docs/PROJECT_MANIFEST.md` is not present in this
checkout, so this gate uses the deployer release criteria and the `TESTING.md`
sharded-runner guidance.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-qapqi0` is closed with `REVIEW VERDICT: PASS` for commit `d7bb52816619a566f437c723400ffc5be49d4693` on `gc-builder-2-6fa6ba1e8999`. |
| 2 | Acceptance criteria met | PASS | `resolveProviderForACPTransport` and `agentSessionCreateTransport` now prefer `config.ResolvedProviderCached` and fall back to `config.ResolveProvider` on cache miss or the `StartCommand` escape hatch. `resolveSessionTransportProvider` computes ACP route names once for provider construction. New tests cover cache preference, cache fallback, workspace-provider fallback, `StartCommand` bypass, and cached/raw transport equivalence. |
| 3 | Tests pass | PASS | Branch-scoped checks passed: focused provider/ACP tests, `go test ./internal/config/...`, `go build ./cmd/gc`, and `go vet ./...`. The first `make test-fast-parallel` run failed in unrelated supervisor/runtime tests; the exact focused failure set reproduces on current `origin/main`. A later pre-push dry-run reran `make test-fast-parallel` on this branch and passed all fast jobs. |
| 4 | No high-severity review findings open | PASS | Review notes state "No findings" and no HIGH/request-changes findings are listed for `ga-qapqi0` or `ga-3ojbg1`. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed a clean branch at the reviewed source commit. After this file is committed, the only extra delta is this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` produced tree `49509a3283df55fab67f536c2660cb00f195735c` with no conflicts. `git diff --check origin/main...HEAD` passed. |
| 7 | Single feature theme | PASS | Commit set is one CLI status provider-resolution performance fix touching only `cmd/gc/providers.go` and `cmd/gc/providers_test.go`. |

## Acceptance Mapping

| Done-when item | Result | Evidence |
|---|---|---|
| Status provider build performs O(1) cached lookups when populated | PASS | Cache-preference tests use deliberately divergent raw specs and cache entries, so ACP results prove the cached path was used. |
| `configuredACPRouteNames` computed once per status build | PASS | `resolveSessionTransportProvider` computes `acpRouteNames` once and reuses it for wrapper decisions and route registration during provider construction. The separate `registerStatusProviderACPRoutes` refresh path remains intentionally separate because it runs against a later snapshot. |
| Transport-decision equivalence and no per-item recompute covered | PASS | `TestAgentSessionCreateTransport_CachedMatchesRawForOverrideAgent`, fallback tests, and existing ACP route/provider tests passed. |

## Commands Run

```text
gc prime
bd prime
gc hook gascity/deployer --claim --json
bd show ga-3ojbg1
bd show ga-qapqi0
bd show ga-soqo5g
gh auth status
git fetch origin main gc-builder-2-6fa6ba1e8999
git rev-parse origin/main origin/gc-builder-2-6fa6ba1e8999 d7bb52816
git show --stat --oneline --decorate --no-renames d7bb52816
git diff --name-status origin/main...origin/gc-builder-2-6fa6ba1e8999
git merge-tree --write-tree origin/main origin/gc-builder-2-6fa6ba1e8999
git diff --check origin/main...HEAD
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp make test-fast-parallel
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp go test ./cmd/gc -run '^(TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted|TestRegisterCityWithSupervisorRejectsStandaloneController|TestRegisterCityWithSupervisorRejectsStandaloneControllerDuringSupervisorStartupPhase|TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity|TestSupervisorCreatesControllerSocketForManagedCity)$' -count=1 -v
env TMPDIR=/var/tmp/gc-main-ga-3ojbg1-tmp go test ./cmd/gc -run '^(TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted|TestRegisterCityWithSupervisorRejectsStandaloneController|TestRegisterCityWithSupervisorRejectsStandaloneControllerDuringSupervisorStartupPhase|TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity|TestSupervisorCreatesControllerSocketForManagedCity)$' -count=1 -v
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp go test ./cmd/gc/... -run 'TestResolveProviderForACPTransport|TestAgentSessionCreateTransport|TestConfiguredACPRouteNames|TestNewSessionProvider|TestSessionProviderContext|TestConfiguredACPSessionNames' -count=1 -v
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp go test ./internal/config/... -count=1
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp go build ./cmd/gc
env TMPDIR=/var/tmp/gc-deploy-ga-3ojbg1-tmp go vet ./...
git config core.hooksPath
```

## Test Results

```text
go test ./cmd/gc/... -run 'TestResolveProviderForACPTransport|TestAgentSessionCreateTransport|TestConfiguredACPRouteNames|TestNewSessionProvider|TestSessionProviderContext|TestConfiguredACPSessionNames' -count=1 -v
PASS
ok  	github.com/gastownhall/gascity/cmd/gc	0.722s

go test ./internal/config/... -count=1
ok  	github.com/gastownhall/gascity/internal/config	2.482s

go build ./cmd/gc
PASS

go vet ./...
PASS

make test-fast-parallel
FAIL: unit-cmd-gc-2-of-6
  TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity
  TestSupervisorCreatesControllerSocketForManagedCity

FAIL: unit-cmd-gc-4-of-6
  TestRegisterCityWithSupervisorRejectsStandaloneControllerDuringSupervisorStartupPhase

FAIL: unit-cmd-gc-5-of-6
  TestRegisterCityWithSupervisorRejectsStandaloneController

FAIL: unit-cmd-gc-6-of-6
  TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted
```

Pre-push dry-run rerun after the gate commit:

```text
make test-fast-parallel
All fast jobs passed
```

Focused reproduction on this branch:

```text
TestCityRuntimeRun_ConvergenceStartupErrorDoesNotBlockStarted: PASS in isolation.
The four supervisor tests above fail with `bd schema not visible for hq after init`,
`city did not become ready under supervisor within 1m0s`, and missing
`controller.sock` diagnostics.
```

Focused reproduction on current `origin/main`:

```text
The same four supervisor tests fail with the same `bd schema not visible for hq
after init`, one-minute readiness timeout, and missing `controller.sock`
diagnostics. This is a repo-wide supervisor/bd test-state failure, not a
regression from the provider-cache diff.
```

## Notes

`core.hooksPath` is `.githooks`; the gate commit runs the local pre-commit hook.
The deployer did not rebase the reviewed source branch. The branch merges
cleanly with current `origin/main` according to `git merge-tree`.
