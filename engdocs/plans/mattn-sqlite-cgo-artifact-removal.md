# mattn SQLiteCGOStore Artifact Removal Plan

Root bead: `ga-apo4sa`
Architecture source: `ga-197yuu`
Design source: `gascity/designer`, completed 2026-06-03

## Goal

Remove the dead SQLite CGO coordstore path and benchmark artifacts so Gas City
can build cleanly with `CGO_ENABLED=0` and no `github.com/mattn/go-sqlite3`
dependency. The user-visible `sqlite-cgo` provider value remains accepted as a
pure-Go modernc-backed alias.

## Child Beads

| Order | Bead | Route | Title |
| --- | --- | --- | --- |
| 1 | `ga-apo4sa.1` | `gascity/builder` | As a developer, I can build Gas City without mattn SQLiteCGOStore artifacts |
| 2 | `ga-apo4sa.2` | `gascity/validator` | Validate CGO-free SQLite cleanup and mattn dependency removal |

Dependency graph:

- `ga-apo4sa.2` depends on `ga-apo4sa.1`

## Acceptance Summary

The work is complete when:

- The listed SQLiteCGOStore files, stubs, tests, CGO benchmark adapter, and
  CGO Phase B gate are deleted when present.
- Branch-local files that are already absent are documented in bead notes.
- The coordstore config comment documents `sqlite` and `sqlite-cgo` as pure-Go
  modernc-backed provider aliases.
- `go mod tidy` has removed `github.com/mattn/go-sqlite3` from `go.mod` and
  `go.sum`.
- `CGO_ENABLED=0 go build ./...`, focused coordstore tests, and `go vet ./...`
  pass.

## Out Of Scope

- No schema changes.
- No bead ID format changes.
- No WAL behavior changes.
- No `city.toml` migration for operators.

## Risks

- `go mod tidy` will not prune mattn until all import sites are removed.
- Some files named in the design may be branch-local and absent on a main-based
  worktree; the builder should treat absence as expected when documented.
