# Release Gate - ga-2k9v (mol-dog-stale-db and gc dolt-cleanup)

Deployer: gascity/workflows.codex-max
Date: 2026-05-02
Bead: ga-2k9v / PR #1548
Branch: `release/ga-2k9v-mol-dog-stale-db-cron`

## Verdict: PASS After Maintainer Fixups

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Scope documented | PASS | The PR now ships a Go cleanup CLI plus formula/order wiring, not a TOML-only change. The CLI resolves the Dolt port, scans stale DBs, drops only safe stale names under `--force`, purges dropped DB directories, reaps test-only Dolt SQL processes, and emits `gc.dolt.cleanup.v1` JSON. |
| 2 | Destructive DB safety | PASS | Planner protects registered rig DBs and Dolt internals including `__gc_probe`, narrows `beads_t` to hex protocol-test names, rejects non-conservative identifiers, and reports skipped stale matches. Covered by `TestPlanDoltDrops_*`. |
| 3 | Purge safety | PASS | `USE <rigDB>` and `CALL DOLT_PURGE_DROPPED_DATABASES()` run on one pinned SQL connection; purge skips missing registered rig DB names only when no reclaimable bytes are present, and fails closed when dropped-database bytes remain for a non-live DB. Covered by `TestSQLCleanupDoltClientPurgePinsUseAndCallToOneConnection`, `TestRunDoltCleanup_ForceSkipsPurgeForMissingRigDatabases`, and `TestRunDoltCleanup_ForceFailsPurgeWhenMissingRigDatabaseHasBytes`. |
| 4 | Process reaper safety | PASS | The reaper re-discovers PID command line and listening ports before SIGTERM and before SIGKILL; if the PID is gone before SIGTERM it is reported as vanished, if it exits after this process sends SIGTERM it is counted as reaped, and if it is reclassified as protected no signal is sent. Covered by `TestRunDoltCleanup_ForceRevalidatesPIDBeforeSIGTERM`, `TestRunDoltCleanup_ForceDoesNotCountMissingPIDAfterRevalidation`, and `TestRunDoltCleanup_ForceCountsPostSIGTERMGoneAsReaped`. |
| 5 | Formula contract | PASS | `max_orphans_for_sql` applies to stale dropped database count using `>`, probe-failure JSON is attached before exit, dry-run/escalation done events say "bytes reclaimable", and clean apply closes the work bead. Covered by `go test ./examples/dolt -run StaleDB`. |
| 6 | Burn-in cadence | PASS | The order intentionally runs every four hours (`0 */4 * * *`) during first-week burn-in, with comments stating it can move toward nightly after measured stability. |

## Local Validation

- `go test ./cmd/gc -run '^(TestCleanupReportJSONShape|TestRunDoltCleanup_|TestResolveDoltPort_|TestPlanDoltDrops_|TestDefaultStaleDatabasePrefixes_|TestLoadRigDoltPorts_|TestSplitCmdline_|TestLooksLikeDoltSQLServer|TestExtractConfigPath_|TestIsTestConfigPath_|TestClassifyDoltProcess_|TestPlanReap_)'`
- `go test ./examples/dolt -run 'StaleDB'`

## Notes

This release gate supersedes the earlier TOML-only gate text. The reviewed
surface includes the Go command, JSON report contract, destructive drop/purge
stages, process reaper, formula shell contract, and four-hour cron burn-in.
