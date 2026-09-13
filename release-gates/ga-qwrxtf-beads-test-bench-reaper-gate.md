# Release Gate: ga-qwrxtf beads_test_bench reaper exclusion

Generated: 2026-06-03T23:57:05Z

## Scope

- Deploy bead: ga-qwrxtf
- Source review bead: ga-t2gq37
- Feature branch: fix/ga-w9k7fi-beads-test-bench-reaper
- Base: origin/main 324e33dab8e0f2c555a9d29470d21e599037a5bb
- Reviewed commit: 44e7e7571
- Clean branch head before gate commit: 215604c9eb92bc755d91e3ac0fec6523dd8eeb5e
- Gate criteria source: deployer prompt. `docs/PROJECT_MANIFEST.md` is absent in this checkout.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-t2gq37 is closed with reason `pass`; notes contain `Verdict: PASS` for commit 44e7e7571. |
| 2 | Acceptance criteria met | PASS | The reviewed change excludes `beads_test_bench_*` in both Gastown maintenance scripts and the Go cleanup planner. Tests cover script filtering, default prefix mirroring, built-in enumerator filtering, and Dolt drop planning. |
| 3 | Tests pass | PASS | Focused checks passed: `go test ./cmd/gc -run 'TestPlanDoltDrops|TestDefaultStaleDatabasePrefixes|TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase'`; `go test ./examples/gastown -run TestMaintenanceDoltScriptsSkipTestPatternDatabases`. Baseline passed: `make test-fast-parallel`. Static check passed: `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Review notes report no blockers and no security concerns. The only observation is a low-priority non-blocking coverage suggestion. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short` was empty. This gate file is the only deployer artifact and is committed as the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` succeeded before the gate commit. The feature branch is a direct one-commit descendant of origin/main. |
| 7 | Single feature theme | PASS | Diff touches one subsystem: Dolt cleanup/database filtering and the corresponding Gastown maintenance-script tests. No unrelated package or user-facing behavior is bundled. |

## Acceptance Evidence

| Surface | Evidence |
|---------|----------|
| `examples/gastown/packs/maintenance/assets/scripts/reaper.sh` | `is_user_database()` excludes `beads_test_bench_*` before the `beads_t*` hex-suffix branch. |
| `examples/gastown/packs/maintenance/assets/scripts/jsonl-export.sh` | Matches the reaper exclusion so JSONL export ignores benchmark fixture databases. |
| `cmd/gc/dolt_cleanup_drop_planner.go` | `defaultStaleDatabasePrefixes` includes `beads_test_bench_`, keeping Go cleanup planning aligned with the maintenance scripts. |
| Tests | `TestMaintenanceDoltScriptsSkipTestPatternDatabases`, `TestDefaultStaleDatabasePrefixes_MirrorsBeadsCleanDatabases`, `TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase`, and the `TestPlanDoltDrops_*` suite cover the affected behavior. |

## Test Log Summary

```text
go test ./cmd/gc -run 'TestPlanDoltDrops|TestDefaultStaleDatabasePrefixes|TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase'
ok github.com/gastownhall/gascity/cmd/gc 0.244s

go test ./examples/gastown -run TestMaintenanceDoltScriptsSkipTestPatternDatabases
ok github.com/gastownhall/gascity/examples/gastown 0.462s

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS
```

## Verdict

PASS. Proceed with PR creation and merge-request handoff.
