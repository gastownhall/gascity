# Release Gate: ga-mtgtv7.2.1

Bead: ga-mtgtv7.2.1 - Deploy corrected SQLite CGO cleanup split unit from current main
Branch: release/ga-mtgtv7-2-1-sqlite-cgo-cleanup-clean
Base: origin/main @ fb0df821cb764d65f2b71611781e8c812a70c6c4
Head before gate commit: 2c87ec2ee6a5adbc4896530966957d6ee4271772

## Source Context

- Architecture decision: ga-4q6sgc.1, Unit B - SQLite CGO migration.
- Review bead: ga-jrcs88, closed with `REVIEWER VERDICT: PASS`.
- Prior deploy bead ga-mtgtv7.2 failed because the acceptance gate was too strict for the retained `go.sum` checksum residue and the `cmd/gc/main.go` compatibility comment.
- `docs/PROJECT_MANIFEST.md` is not present on this branch; this gate uses the active deployer criteria plus the bead acceptance criteria.

## Gate Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-jrcs88` contains `REVIEWER VERDICT: PASS`; reviewed source head was `396be53ce`, with follow-up commits included by the architecture split. |
| 2 | Acceptance criteria met | PASS | Fresh branch cut from current `origin/main`; cherry-picked only `396be53ce`, `cb4d5140a`, and `717935724` in that order; diff is limited to `cmd/gc/main.go`, `go.mod`, `internal/beads/doltlite_read_store.go`, and `internal/beads/doltlite_read_store_test.go`. |
| 3 | Tests pass | PASS | `CGO_ENABLED=0 go build ./...`, `go test ./internal/beads/ -count=1`, `make test-fast-parallel`, and `go vet ./...` all passed. |
| 4 | No high-severity review findings open | PASS | Review notes contain non-blocking observations only; no HIGH findings are open. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## release/ga-mtgtv7-2-1-sqlite-cgo-cleanup-clean...origin/main [ahead 3]`. Re-check after committing this file is required before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree `2f230690b70d2fbfb2a6539d6b9f40a9605edc7e`. |
| 7 | Single feature theme | PASS | Commit set touches one behavior: removing direct mattn/go-sqlite3 CGO usage from the doltlite read path while preserving the `sqlite-cgo` operator alias. No reaper, event, config, API/schema, generated dashboard, or benchmark files are present. |

## Acceptance Evidence

Cherry-picks:

| Source SHA | New SHA | Purpose |
|------------|---------|---------|
| 396be53ce | 42759588a | Remove direct mattn/go-sqlite3 dependency and switch doltlite read store to modernc. |
| cb4d5140a | b6a0a2572 | Seed before-filter test timestamps in canonical SQLite format. |
| 717935724 | 2c87ec2ee | Add modernc `time.Time.String()` layout to `parseTimeString`. |

Diff scope:

```text
cmd/gc/main.go
go.mod
internal/beads/doltlite_read_store.go
internal/beads/doltlite_read_store_test.go
```

Forbidden paths absent:

```text
internal/benchmarks/
internal/events/
internal/config/
docs/schema/
internal/api/
cmd/gc/dashboard/web/src/generated/
```

`cmd/gc/main.go` diff is limited to the `sqlite` / `sqlite-cgo` compatibility comment above `providerIsCoordStore`.

## mattn/go-sqlite3 Evidence

`github.com/mattn/go-sqlite3` is absent from `go.mod`.

Direct import check:

```text
rg -n '^[[:space:]]*(import[[:space:]]+)?(_[[:space:]]+)?"github\.com/mattn/go-sqlite3"' --glob '*.go'
```

Result: no matches.

Remaining `go.sum` entries:

```text
go.sum:799:github.com/mattn/go-sqlite3 v1.14.8 h1:gDp86IdQsN/xWjIEmr9MF6o9mpksUgh0fu+9ByFxzIU=
go.sum:800:github.com/mattn/go-sqlite3 v1.14.8/go.mod h1:NyWgC/yNuGj7Q9rpYnZvas74GogHl5/Z4A/KQRfk6bU=
```

`go mod why -m github.com/mattn/go-sqlite3` shows the retained checksum residue is transitive/test-only through `github.com/steveyegge/beads` -> `github.com/dolthub/driver` -> `github.com/gocraft/dbr/v2.test` -> `github.com/mattn/go-sqlite3`.

## Test Evidence

Automated gates:

```text
CGO_ENABLED=0 go build ./...
go test ./internal/beads/ -count=1
make test-fast-parallel
go vet ./...
```

Results:

```text
CGO_ENABLED=0 go build ./...: PASS
go test ./internal/beads/ -count=1: ok github.com/gastownhall/gascity/internal/beads 4.430s
make test-fast-parallel: All fast jobs passed
go vet ./...: PASS
```
