# Release Gate: retire [maintenance.dolt] subsystem

Gate evaluated: 2026-06-17T22:10:58Z

## Candidate

- Deploy bead: `ga-wjms2g`
- Source bead: `ga-84xwd5.1`
- Review bead: `ga-sramea`
- Reviewed commit: `19d5cf8c4443a917b46cc1b76c1fff602fb347de`
- Deploy branch: `deploy/ga-wjms2g-retire-maintenance-dolt`
- Base: `origin/main` at `cdcf685f54e956737941a7ca4654a76b545c8c9d`

The source builder branch `origin/builder/ga-84xwd5.1` advanced after review
to `159e75ca7c2bae66a029bd2ecdd74f8223eca9f9`, a docs runbook commit for
`ga-gx25ij.1`. This deploy gate intentionally pins the candidate to the
reviewed maintenance-retirement commit `19d5cf8c4` and excludes that newer
runbook commit from this release unit.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-sramea` re-review verdict is PASS for `19d5cf8c4`. The prior MEDIUM blocker was fixed by deleting `internal/api/huma_types_maintenance.go`. |
| 2 | Acceptance criteria met | PASS | Deletion targets are absent; maintenance config/API/event/storehealth surfaces were removed or regenerated; generated OpenAPI/dashboard/schema artifacts are current; breaking change is documented in `CHANGELOG.md`. |
| 3 | Tests pass | PASS | `make dashboard-check`, `go build ./...`, `go vet ./...`, and `go test ./...` all passed on the deploy branch. |
| 4 | No high-severity review findings open | PASS | Review notes contain no HIGH findings; the only blocker was MEDIUM and is fixed in `19d5cf8c4`. |
| 5 | Final branch is clean | PASS | `git status --short --branch` reported no uncommitted changes before writing this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` completed successfully with `merge-tree clean`; `git diff --check origin/main...HEAD` produced no output. |
| 7 | Single feature theme | PASS | The reviewed diff is one Dolt maintenance-retirement release unit: remove the retired `[maintenance.dolt]` subsystem and rely on the compact-backed replacement surfaces in the same Dolt maintenance stack. The later runbook commit on the builder branch is excluded. |

## Acceptance Evidence

- Required deleted files are absent:
  - `cmd/gc/cmd_maintenance.go`
  - `cmd/gc/cmd_maintenance_test.go`
  - `cmd/gc/maintenance_startup_test.go`
  - `internal/api/decode_maintenance.go`
  - `internal/api/decode_maintenance_test.go`
  - `internal/api/handler_maintenance.go`
  - `internal/api/handler_maintenance_test.go`
  - `internal/api/huma_types_maintenance.go`
  - `internal/config/maintenance_test.go`
  - `internal/supervisor/maintenance.go`
  - `internal/supervisor/maintenance_alert_test.go`
  - `internal/supervisor/maintenance_events_test.go`
  - `internal/supervisor/maintenance_gc_test.go`
  - `internal/supervisor/maintenance_snapshot.go`
  - `internal/supervisor/maintenance_snapshot_test.go`
  - `internal/supervisor/maintenance_test.go`
  - `internal/supervisor/maintenance_trigger_test.go`
- Removed endpoint/config/schema terms are absent from generated wire artifacts:
  - `maintenance/status`
  - `maintenance/dolt-gc`
  - `maintenance.dolt`
  - `MaintenanceConfig`
  - `DoltMaintenance`
- `CHANGELOG.md` documents the breaking removal of:
  - `GET /v0/city/{city}/maintenance/status`
  - `POST /v0/city/{city}/maintenance/dolt-gc`
- `make dashboard-check` regenerated and verified dashboard generated types
  from `internal/api/openapi.json`.
- `StatusStoreHealth` in the Huma/OpenAPI/dashboard wire surface no longer
  exposes the removed maintenance GC fields.

## Test Evidence

Commands run on `deploy/ga-wjms2g-retire-maintenance-dolt` at
`19d5cf8c4443a917b46cc1b76c1fff602fb347de`:

| Command | Result | Notes |
|---------|--------|-------|
| `make dashboard-check` | PASS | Ran `npm ci`, OpenAPI generation, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...`. |
| `go build ./...` | PASS | No output. |
| `go vet ./...` | PASS | No output. |
| `go test ./...` | PASS | Full package sweep passed. Slow packages included `cmd/gc` at 449.444s, `examples/bd/dolt` at 89.479s, and `internal/api` at 50.746s. |

## Scope Notes

The deploy branch is intentionally not tracking the current tip of
`origin/builder/ga-84xwd5.1`, because that branch now contains an additional
docs runbook commit for a different bead. The PR for this gate should use
`deploy/ga-wjms2g-retire-maintenance-dolt` so the release unit remains limited
to the reviewed maintenance subsystem retirement.
