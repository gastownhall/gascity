# Release gate — probe a user DB so `__gc_probe` stops hosting stats (ga-42gi / ga-hivi)

**Verdict:** PASS with maintainer fixups

Branch: `release/ga-hivi-probe-user-db` rebased for review on `origin/main` @ `936dea150`.

Commits under review:

- `390278080` — fix(dolt/health): probe a user db so __gc_probe stops hosting stats (ga-42gi). Rebased cherry-pick of source SHA `db6831b0` from `fork/gc-builder-1-01561d4fb9ea`.
- `842002634` — chore(fmt): align map literals after ga-42gi cherry-pick. Pure formatting for `golangci-lint fmt`.
- `3d319fad` — chore: release gate PASS for ga-hivi (ga-42gi). Original gate artifact from the contributor branch.
- Current maintainer fixup commit — `fix(dolt): harden user-db health probe`,
  resolving the PR-review loop findings for CSV database parsing, Dolt system
  database exclusions, reserved existing metadata, no-user-database diagnostics,
  and stale user-database `__gc_read_only_probe` cleanup.

Diff vs `origin/main` now covers these files:

- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/beads_provider_lifecycle_test.go`
- `cmd/gc/cmd_dolt_state.go`
- `cmd/gc/cmd_dolt_state_test.go`
- `cmd/gc/dolt_sql_health.go`
- `cmd/gc/dolt_sql_health_test.go`
- `cmd/gc/embed_builtin_packs_test.go`
- `examples/bd/assets/scripts/gc-beads-bd.sh`
- `examples/dolt/commands/cleanup/run.sh`
- `examples/dolt/commands/gc-nudge/run.sh`
- `examples/dolt/commands/health/run.sh`
- `examples/dolt/commands/list/run.sh`
- `examples/dolt/commands/sync/run.sh`
- `examples/dolt/formulas/mol-dog-doctor.toml`
- `examples/dolt/formulas/mol-dog-stale-db.toml`
- `release-gates/ga-hivi-probe-user-db-gate.md`

## Review

| Review source | Verdict | Notes |
|---------------|---------|-------|
| Original ga-hivi review | PASS | Reviewed source commit `db6831b0`; formatter follow-up addressed the style note. |
| PR-review synthesis `ga-5sq14q2` attempt 1 | request_changes | Major findings are resolved by the local maintainer fixup and require another review iteration before merge. |

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Probe no longer writes `__gc_probe` | PASS | Go probe SQL targets a selected user DB; bash fallback now runs `SHOW DATABASES` and writes `<user_db>.__gc_read_only_probe`; tests reject legacy create/write targets. |
| 2 | System databases are not probe targets | PASS | Go and shell skip `information_schema`, `mysql`, `dolt_cluster`, `performance_schema`, `sys`, and `__gc_probe`. |
| 3 | CSV database names parse correctly | PASS | CLI `SHOW DATABASES -r csv` output is parsed with `encoding/csv`; tests cover comma and quote escaped names. |
| 4 | Existing reserved metadata is rejected except legacy `__gc_probe` | PASS | Canonical metadata normalization now preserves only the legacy `__gc_probe` migration case; existing `mysql` metadata is rejected by regression coverage. |
| 5 | No-user-database probes are diagnostic, not writable | PASS | Go and shell fallback probes now return an unknown/diagnostic state without issuing a write probe when `SHOW DATABASES` contains no user database. |
| 6 | Probe-table cleanup covers rotated user DBs | PASS | `gc dolt-state reset-probe` still drops legacy `__gc_probe` and now drops `__gc_read_only_probe` tables from each discovered user database. It deliberately does not drop generic `__probe` tables because those can be user-owned. |
| 7 | Review-loop major findings closed locally | PASS | Reserved metadata, no-user-database behavior, and stale probe-table cleanup findings are fixed; the workflow must run a fresh review/scorecard before final approval. |
| 8 | Branch evidence is current | PASS | This artifact records the rebased base SHA, current commit chain, and full changed-file set. |

## Upgrade remediation

Managed Dolt servers upgraded from a build that wrote `__gc_probe` must run this
once per server after the new binary is available:

```bash
gc dolt-state reset-probe --host <host> --port <port> --user <user> --force
```

That command removes the legacy `__gc_probe` database and the GC-owned
`__gc_read_only_probe` table from discovered user databases. It is idempotent.
Do not manually drop generic `__probe` tables; they are outside the GC reserved
contract and may belong to user data.

## Validation

- `git diff --check` → pass.
- `sh -n examples/bd/assets/scripts/gc-beads-bd.sh` → pass.
- `sh -n examples/dolt/commands/health/run.sh` → pass.
- `sh -n examples/dolt/commands/{cleanup,gc-nudge,list,sync}/run.sh` → pass.
- `bash -n examples/gastown/packs/maintenance/assets/scripts/{jsonl-export,reaper}.sh` → pass.
- `go test ./cmd/gc -run 'TestManagedDolt|TestDoltStateReadOnlyCheckCmd|TestDoltStateHealthCheckCmd|TestGcBeadsBdReadOnlyFallback|TestGcBeadsBdHealthNoUserDatabaseWarnsAndContinues|TestGcBeadsBdReadOnlyHelperErrorIsDiagnostic|TestGcBeadsBdInitRejectsManagedProbeDatabaseName|TestEnsureCanonicalScopeMetadataRejectsManagedSystemDatabases|TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase|TestDoltSyncRejectsManagedProbeDatabaseFilter|TestNormalizeCanonicalBdScopeFilesRejectsExistingManagedSystemDatabase' -count=1` with workflow `GC_*` / `BEADS_*` environment stripped → pass.
- `GC_FAST_UNIT=0 go test ./cmd/gc -run 'TestDoltStateRecoverManagedCmdNoUserDatabaseHealthSucceeds' -count=1` with workflow `GC_*` / `BEADS_*` environment stripped → pass.
- `go test ./cmd/gc -run 'TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase|TestDoltSyncRejectsManagedProbeDatabaseFilter'` → pass.
- `go test ./test/docsync -count=1` → pass.
- `go test ./examples/dolt ./examples/gastown -count=1` → pass.

## Known Environment Noise

`go test ./...` fails in this rig outside the changed surface. With the workflow
`GC_*` / `BEADS_*` environment stripped, it narrows to
`TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag`,
which fails because a `mayor` session is already active in the local runtime.
Without stripping the workflow environment, many unrelated command tests also
fail from `GC_RIG=gascity`, the rig-local `bd` behavior, and local managed-Dolt
startup state. The focused checks above cover the changed files and the review
findings.

## Push target

`fork` (quad341/gascity) — `origin` (gastownhall/gascity) is read-only from this rig. PR cross-repo target remains `--head quad341:release/ga-hivi-probe-user-db --base main`.
