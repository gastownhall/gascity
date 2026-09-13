# Release Gate - ga-m3gz4 Dolt cleanup test DB prefixes

Date: 2026-05-25

- Source bead: `ga-m3gz4`
- Review bead: `ga-r96vg`
- Branch: `builder/ga-m3gz4`
- Reviewed commit: `b33f17f16a74`
- Base: `origin/main` at `d48bcb4c5432`
- Diff: `cmd/gc/dolt_cleanup_drop_planner.go`, `cmd/gc/dolt_cleanup_drop_planner_test.go`
- Project manifest: `docs/PROJECT_MANIFEST.md` is absent in this checkout; gate uses the deployer prompt criteria plus `TESTING.md`.

## Gate Checklist

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-r96vg` contains `Reviewer verdict: PASS` for commit `b33f17f16` on `origin/builder/ga-m3gz4`; findings are `None`. |
| 2 | Acceptance criteria met | PASS | `defaultStaleDatabasePrefixes` now includes `test_guard_` and `test_federation_`; `TestPlanDoltDrops_DefaultPrefixesIncludeGuardAndFederationTests` asserts those names are dropped while live rig DB names `hq` and `gascity` are protected; `TestDefaultStaleDatabasePrefixes_MirrorsBeadsCleanDatabases` includes the beads-side convergence note for `be-hjj-3`. |
| 3 | Tests pass | PASS, no branch regression | Focused release checks passed: `go test ./cmd/gc/ -run 'DoltCleanup|Planner' -count=1` (`ok github.com/gastownhall/gascity/cmd/gc 2.141s`), `go vet ./cmd/gc/`, `go vet ./...`, and `git diff --check origin/main..HEAD`. `make test-fast-parallel` failed only in `examples/gastown` on `TestPolecatFormulaHaltsOnAutoPushFalse`; the same focused test reproduces on `origin/main` at `d48bcb4c5432`, this branch has no diff under `examples/gastown`, and the failure is tracked by closed bead `ga-q0aoo`. |
| 4 | No high-severity review findings open | PASS | Review notes report `Findings: None`; unresolved HIGH finding count is 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## builder/ga-m3gz4...origin/builder/ga-m3gz4`. |
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...HEAD` returned `0 1`; `git merge-tree --write-tree origin/main HEAD` exited 0 with tree `bdcbc1f9e99bf25aa0a62bf9aa5a9cc5f15edb0e`. |

## Acceptance Trace

| Done-when | Result |
|-----------|--------|
| `defaultStaleDatabasePrefixes` includes `test_guard_` and `test_federation_`. | PASS - both prefixes are present in `cmd/gc/dolt_cleanup_drop_planner.go`. |
| Planner unit test asserts those names are planned for drop and live rig DB names are preserved. | PASS - `TestPlanDoltDrops_DefaultPrefixesIncludeGuardAndFederationTests` covers `test_guard_abc123`, `test_federation_xyz`, `testdb_old`, `hq`, and `gascity`. |
| `go test ./cmd/gc/ -run 'DoltCleanup|Planner' -count=1` passes. | PASS. |
| `go vet ./cmd/gc/` clean. | PASS. |
| PR description references the needed beads-side convergence (`be-hjj-3`). | PASS - this is called out in the release gate and must remain in the PR review notes. |

## Validation Commands

| Command | Result |
|---------|--------|
| `go test ./cmd/gc/ -run 'DoltCleanup|Planner' -count=1` | PASS |
| `go vet ./cmd/gc/` | PASS |
| `go vet ./...` | PASS |
| `git diff --check origin/main..HEAD` | PASS |
| `make test-fast-parallel` | Existing main-red failure in `examples/gastown`: `TestPolecatFormulaHaltsOnAutoPushFalse` expects `**2. Push your branch:**`; the same failure reproduces on `origin/main` and this branch does not touch that package. |
| `git merge-tree --write-tree origin/main HEAD` | PASS |

## Verdict

PASS. Open a PR from `builder/ga-m3gz4` to `main`, with the PR body noting that the Gas City stale-prefix list now mirrors the beads-side cleanup prefixes tracked by `be-hjj-3`.
