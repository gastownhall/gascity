# Dolt Maintenance Retirement and Runbook

Date: 2026-06-17

PM intake source:
- Designer handoff mail: gm-wisp-5n12slf
- Root beads: ga-84xwd5, ga-gx25ij
- Source: actual designer completed builder contracts

## Goal

Retire the unused `[maintenance.dolt]` subsystem after `gc dolt compact`
receives the disk-preflight and backup-sync safety features, then document the
single surviving Dolt storage maintenance path for operators.

The retirement work is ordered after the compact parity work. The runbook is
ordered after retirement so the published operator story describes only the
current system.

## Work Packages

### Retire maintenance.dolt

- ga-84xwd5.1 -> builder: Remove retired Dolt maintenance subsystem.

Acceptance focus:
- Delete the 11 files listed in the designer contract.
- Remove remaining config, duration validation, event constants/payloads,
  event registrations, API state/routes/types, storehealth maintenance coupling,
  and `cmd/gc/api_state.go` maintenance loop startup/accessors.
- Preserve and reword store-health warning semantics as size-to-row ratio, not
  maintenance overdue state.
- Regenerate OpenAPI/dashboard artifacts and city schema/reference output after
  source changes.
- Remove dashboard/web references to deleted maintenance endpoints or fields if
  present.
- Include the breaking API removal note for the two maintenance endpoints.
- Pass `go build ./...`, `make dashboard-check`, `go test ./...`, and
  `go vet ./...`.

Dependencies:
- ga-84xwd5.1 depends on ga-wfdunn.1.
- ga-84xwd5.1 depends on ga-hn88dw.1.

### Add operator runbook

- ga-gx25ij.1 -> builder: Add Dolt Storage Maintenance operator runbook.

Acceptance focus:
- Add `docs/runbooks/dolt-compact.md` with the designer-specified frontmatter
  and operator-first structure.
- Explain the current `mol-dog-compactor`/`gc dolt compact` path only.
- Include the focused six-row operator configuration table.
- Cover optional backup sync, disk preflight, duration alerts, quarantine,
  doctor checks, and the troubleshooting quick reference.
- Add outbound links to the manual recovery runbook and config reference.
- Add a backlink from `docs/troubleshooting/dolt-bloat-recovery.md`.
- Avoid mentions of `[maintenance.dolt]`, deleted maintenance API routes, bead
  IDs, contributor-only internals, and excluded advanced compact flags.

Dependency:
- ga-gx25ij.1 depends on ga-84xwd5.1.

## Handoff Targets

Builder:
- ga-84xwd5.1
- ga-gx25ij.1

Both child beads carry `source:actual-pm`, `ready-to-build`, and
`gc.routed_to=gascity/builder`. No design or validator routing is needed because
the designer handoff already supplied complete builder contracts.
