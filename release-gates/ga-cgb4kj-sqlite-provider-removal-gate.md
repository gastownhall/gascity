# Release Gate: SQLite Coordstore Provider Removal

Result: PASS

Date: 2026-06-06

## Target

- Deploy bead: `ga-cgb4kj` - deploy gate for sqlite/coordstore provider removal
- Source bead: `ga-stllj9` - hard-error on removed sqlite/coordstore providers, delete sqlite_store
- Branch: `work/ga-stllj9-remove-sqlite-provider`
- Feature commit: `77968bcf4a6036b4b25143eb34f9c38bba7ffab3`
- Base checked: `origin/main` (`5d4c3eef0b9436f6ef96b665d304a4cc9db05ef3`)

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Acceptance criteria met | PASS | `internal/beads/sqlite_store.go` and `sqlite_store_test.go` are deleted. `modernc.org/sqlite v1.50.1` is retained in `go.mod` (required by doltlite; not removed). Hard-error switch is present at `cmd/gc/main.go:1182` for `"sqlite"`, `"sqlite-cgo"`, and `"coordstore"` provider names. Orphaned test helpers in `cmd/gc/bead_policy_store_test.go` and `cmd/gc/beads_provider_lifecycle_test.go` are pruned. |
| 2 | Tests pass | PASS | `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel`: all cmd/gc shards (6/6) green; `unit-core` had one pre-existing timing flake `TestMailThreadAllRigsStoreSlowReturnsPartial` (5ms deadline too tight under parallel load; passes cleanly in isolation via `go test -run TestMailThreadAllRigsStoreSlowReturnsPartial ./internal/api/...`; not introduced by this change). |
| 3 | No merge conflicts | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0, produced tree `b29c4e96ba2027ded7cb19300a26705648ff4b8b`. `git diff --check origin/main...HEAD` passed. |
| 4 | Single feature theme | PASS | One commit: delete `sqlite_store.go`/`sqlite_store_test.go`, add hard-error switch in `main.go`, prune orphaned test helpers. Diff limited to `cmd/gc/main.go`, `cmd/gc/bead_policy_store_test.go`, `cmd/gc/beads_provider_lifecycle_test.go`, `internal/beads/sqlite_store.go`, `internal/beads/sqlite_store_test.go`. |
| 5 | Branch is clean | PASS | Working tree had no tracked modifications before this gate file. Untracked files are from other sessions and are not staged. |

## Acceptance Evidence

| Check | Result | Evidence |
|-------|--------|----------|
| sqlite_store.go removed | PASS | `ls internal/beads/sqlite_store.go` → `No such file or directory` |
| sqlite_store_test.go removed | PASS | Absent from `git diff --name-only origin/main...HEAD` (file deleted) |
| modernc.org/sqlite retained | PASS | `grep 'modernc.org/sqlite' go.mod` → `modernc.org/sqlite v1.50.1` |
| Hard-error switch in place | PASS | `grep -n 'sqlite\|coordstore' cmd/gc/main.go` → line 1182: `case "sqlite", "sqlite-cgo", "coordstore":` with error message directing users to remove the stanza |
| Scope check | PASS | `git diff --name-only origin/main...HEAD` lists only the 5 files above |

## Test Evidence

- PASS: `go build ./...`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel` — all 6 cmd/gc shards green; one pre-existing timing flake in `internal/api` (isolated run passes)
- PASS: `git diff --check origin/main...HEAD`
- PASS: `git merge-tree --write-tree origin/main HEAD`
