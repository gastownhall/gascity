# Release gate — probe a user DB so `__gc_probe` stops hosting stats (ga-42gi / ga-hivi)

**Verdict:** PASS

Branch: `release/ga-hivi-probe-user-db` (cut fresh off `origin/main` @ `a4d32733`)
Commits on the branch:

- `81f5bcb6` — fix(dolt/health): probe a user db so __gc_probe stops hosting stats (ga-42gi). Cherry-pick of source SHA `db6831b0` from `fork/gc-builder-1-01561d4fb9ea`. Clean (no conflicts).
- `8047de53` — chore(fmt): align map literals after ga-42gi cherry-pick. Pure formatting — `golangci-lint fmt` normalizer flagged the new map literal alignment that the original commit missed. No behavior change.

Diff vs `origin/main`: 3 files changed, +302/-38 lines (cherry-pick) plus +8/-8 (formatting):

- `cmd/gc/dolt_sql_health.go`
- `cmd/gc/dolt_sql_health_test.go`
- `cmd/gc/cmd_dolt_state_test.go`

## Review

| Review bead | Reviews | Verdict | Reviewer           |
|-------------|---------|---------|--------------------|
| ga-hivi     | ga-42gi | PASS    | gascity/reviewer-1 |

Reviewer message (`gm-wisp-mbo`): *"Reviewed db6831b0. Tests + vet + build clean. Indirect-enforcement verified. Ready for release gate."*

Reviewer notes recorded one INFO/style observation that the variable-width key alignment "gofmt accepts" — the deployer-side `golangci-lint fmt --diff ./...` (CI's `make fmt-check`) actually flags it on Go 1.26.2, so the formatter was applied as a follow-up commit. Non-functional.

Gemini second-pass: disabled per current rig configuration.

## Criteria

| # | Criterion                             | Verdict | Evidence |
|---|---------------------------------------|---------|----------|
| 1 | Review PASS present                   | PASS    | reviewer-1 PASS on ga-hivi via mail `gm-wisp-mbo`; verdict recorded in bead notes. |
| 2 | Acceptance criteria met               | PASS    | New probe path doesn't create or write to `__gc_probe` — enforced by `assertNoManagedDoltProbeLegacyTarget` (`cmd/gc/dolt_sql_health_test.go`) AND by `managedDoltSystemDatabases` skiplist (`cmd/gc/dolt_sql_health.go:47-52`) AND case-insensitive lookup at line 152. Existing health-check tests pass. Comment block updated. |
| 3 | Tests pass                            | PASS    | `go vet ./...` clean; `go build ./...` clean; targeted `go test -short -run "TestManagedDolt\|TestDoltStateReadOnlyCheckCmd\|TestDoltStateHealthCheckCmd\|TestGcBeadsBdReadOnlyFallbackDoesNotDropProbeDatabase\|TestGcBeadsBdInitRejectsManagedProbeDatabaseName" ./cmd/gc/...` → ok 2.7s. Pre-existing env-leak failures reproduce identically on `origin/main` (see below). |
| 4 | No high-severity review findings open | PASS    | Reviewer reported no blocking findings. INFO note on alignment resolved by `8047de53`. |
| 5 | Final branch is clean                 | PASS    | `git status` clean (only this gate file untracked at write time, plus a stray `.gitkeep` from worktree init). |
| 6 | Branch diverges cleanly from main     | PASS    | Cherry-pick of `db6831b0` onto fresh branch off `origin/main` applied with zero conflicts. |

## Build / vet / fmt

- `go vet ./...` → clean
- `go build ./...` → clean
- `golangci-lint fmt --diff ./cmd/gc/...` → clean after `8047de53`
- Cherry-pick clean (no conflicts)

## Pre-existing failures (not introduced)

Running `go test -short ./cmd/gc/...` surfaces 4 failures (`TestRigAnywhere_ResolveContext` subtests, `TestOpenStoreAtForCityExecProjectsConfiguredTargets`, `TestOpenStoreAtForCityExecBeadsBdProjectsScopedExternalDoltEnv`, `TestOpenStoreAtForCityExecUsesUniversalStoreTargetEnv`, `TestControllerQueryRuntimeEnvReturnsNilForNonBD`).

All four reproduce identically on `origin/main` with the same Go 1.26.2 toolchain and the same shell environment (deployer rig has `GC_RIG=gascity` exported, which leaks into tests that don't scrub it). Not regressions; the broader env-scrubbing fix is being tracked in the builder-rig backlog (ga-d02c, ga-y64o landed on the builder branch but are not yet on `origin/main`).

## Push target

`fork` (quad341/gascity) — `origin` (gastownhall/gascity) is read-only from this rig (`git push --dry-run origin HEAD` → 403).
PR cross-repo: `--head quad341:release/ga-hivi-probe-user-db --base main`.
