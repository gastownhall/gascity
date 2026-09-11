# Release Gate: extract internal/warmup package from cmd/gc

Bead: ga-kkj4fm
Source review: ga-kn8yy6.2
Source branch: builder/ga-kn8yy6-extract-warmup-package
Deploy branch: deployer/ga-kkj4fm-extract-warmup-package
Reviewed commit: ef5dd12b9e55f49683b6508d448330e7ee694a44
Deployed patch commit: 25b70e286959fdaa72a4cae0daa718f9f1923d4c
Patch ID: 76733dcd9245ca9c1f111f48f5186efed04a8bce
Base at gate: origin/main 1c5c768ef622a07449ac1e0ac96aefa22b9cd529
Merge base: 2b9a7d9263e2c3309a0e2997f5c5fd2adca849b9
Merge-tree result: 7cdd02781c3d691439a7b3aee1acd13120db8b51

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer prompt's release-gate criteria.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` succeeded. `git merge-tree --write-tree origin/main HEAD` returned tree `7cdd02781c3d691439a7b3aee1acd13120db8b51` with exit 0 and no conflicts. |
| 1 | Review PASS present | PASS | `bd show ga-kn8yy6.2 --json` notes contain `REVIEW VERDICT: PASS` and `No blockers. Routing to deploy.` |
| 2 | Acceptance criteria met | PASS | Verified the diff is the stated 9-file warmup extraction. `internal/warmup` contains `WarmupCheckResult`, `ScopeWarmupResult`, `WarmupReport`, `WarmupOpts`, `RunWarmupChecks`, and `CustomWarmupMail`. `cmd/gc/cmd_start_warmup.go` now contains only `defaultMailProvider`; `cmd/gc/cmd_start.go` builds doctor checks and calls `warmup.RunWarmupChecks`. The reviewed commit and deployed commit have the same stable patch ID. |
| 3 | Tests pass | PASS | `go vet ./...` passed. Focused check passed: `go test ./internal/warmup ./cmd/gc -run 'TestRunWarmupChecks|TestDefaultMailProviderUsesStartedCityPath|TestBuildDoctorChecks_NameSetUnchanged'`. First `make test-fast-parallel` run hit the known shard-load timeout in `TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity`; standalone rerun passed 3/3. Retried `make test-fast-parallel`; all fast jobs passed. |
| 4 | No high-severity review findings open | PASS | Review bead notes contain no unresolved HIGH/request-change finding markers and explicitly state there are no blockers. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; only this gate file is present for the gate commit. |
| 7 | Single feature theme | PASS | Commit set is one refactor theme: move warmup runner/types from `cmd/gc` into `internal/warmup` so internal doctor producers can depend on the warmup report/mail contract without importing `cmd/gc`. Two test-only `fmt.Fprintf` cleanups in `cmd/gc` are mechanical lint fixes noted by the reviewer. |

## Test Evidence

```text
go vet ./...
PASS

go test ./internal/warmup ./cmd/gc -run 'TestRunWarmupChecks|TestDefaultMailProviderUsesStartedCityPath|TestBuildDoctorChecks_NameSetUnchanged'
ok  	github.com/gastownhall/gascity/internal/warmup	0.627s
ok  	github.com/gastownhall/gascity/cmd/gc	0.419s

go test ./cmd/gc -run '^TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity$' -count=3 -v
--- PASS: TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity (0.17s)
--- PASS: TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity (0.12s)
--- PASS: TestCityRuntimeRun_PanicInStartupDoesNotShutdownCity (0.14s)

make test-fast-parallel
All fast jobs passed
```
