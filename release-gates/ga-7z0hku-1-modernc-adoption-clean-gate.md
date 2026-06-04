# Release Gate: ga-7z0hku.1

Bead: ga-7z0hku.1 - Deploy clean modernc restart-init adoption branch from current main
Branch: release/ga-7z0hku-1-modernc-adoption-clean
Base: origin/main @ fb0df821cb764d65f2b71611781e8c812a70c6c4
Head before gate commit: b32e1fdaa0b00c9d3affde68cbf11b88ce3ec361

## Source Context

- Source bug bead: ga-spy4rw.
- Review bead: ga-wtuuwv, closed with `### Verdict: PASS`.
- Prior deploy bead ga-7z0hku failed because the earlier branch bundled independent order-dispatch / broader modernc cutover work and conflicted with current main.
- `docs/PROJECT_MANIFEST.md` is not present on this branch; this gate uses the active deployer criteria plus the bead acceptance criteria.

## Gate Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `bd show ga-wtuuwv` contains `## Review: gascity/reviewer - PASS` and `### Verdict: PASS`; reviewed commits were `f7dff357b` and `c94a02794`. |
| 2 | Acceptance criteria met | PASS | Fresh branch cut from current `origin/main`; cherry-picked only `f7dff357b` then `c94a02794`; diff is limited to `cmd/gc/beads_provider_lifecycle.go` and `cmd/gc/beads_provider_lifecycle_test.go`. |
| 3 | Tests pass | PASS | `go build ./cmd/gc`, focused cmd/gc regression test, `make test-fast-parallel`, `go vet ./...`, and isolated restart-init smoke all passed. |
| 4 | No high-severity review findings open | PASS | Review notes contain no HIGH findings; only a cosmetic rename note and an out-of-scope pre-existing broad matcher note. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## release/ga-7z0hku-1-modernc-adoption-clean...origin/main [ahead 2]`. Re-check after committing this file is required before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree `5c153f9f63e53a673ce74ab18531f16c9c7d17c5`. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem and one behavior: adopting already-initialized bd/Dolt rig stores during init/start lifecycle. No order-dispatch, `cmd/gc/main.go`, `internal/beads/sqlite_store*`, or broader modernc cutover files are present. |

## Acceptance Evidence

Cherry-picks:

| Source SHA | New SHA | Purpose |
|------------|---------|---------|
| f7dff357b | 61d7a762a | Regression tests for already-initialized rig adoption. |
| c94a02794 | b32e1fdaa | Lifecycle fix to adopt already-initialized rig stores after validation/finalization. |

Diff scope:

```text
cmd/gc/beads_provider_lifecycle.go
cmd/gc/beads_provider_lifecycle_test.go
```

Forbidden paths absent:

```text
cmd/gc/order_dispatch.go
cmd/gc/order_dispatch_test.go
cmd/gc/main.go
internal/beads/sqlite_store.go
internal/beads/sqlite_store_test.go
```

## Test Evidence

Automated gates:

```text
go build ./cmd/gc
go test ./cmd/gc -run "TestInitAndHookDirAdoptsAlreadyInitialized(DefaultRigBdStore|CanonicalExecBdStore)" -count=1
make test-fast-parallel
go vet ./...
```

Results:

```text
go build ./cmd/gc: PASS
focused cmd/gc regression: ok github.com/gastownhall/gascity/cmd/gc 0.332s
make test-fast-parallel: All fast jobs passed
go vet ./...: PASS
```

Restart-init smoke:

```text
Scratch root: /tmp/gascity-ga-7z0hku-smoke.ZcX0U6
Branch binary: /tmp/gascity-deploy-ga-7z0hku-1-modernc-adoption-clean/gc
Dolt used for smoke: /tmp/gascity-ga-7z0hku-smoke.ZcX0U6/dolt210/dolt version 2.1.0
Isolated GC_HOME: /tmp/gascity-ga-7z0hku-smoke.ZcX0U6/gchome
Isolated supervisor port: 18372
City: /tmp/gascity-ga-7z0hku-smoke.ZcX0U6/city3
Rig: /tmp/gascity-ga-7z0hku-smoke.ZcX0U6/tincan3
```

Smoke steps:

```text
gc init --template minimal --providers codex --default-provider codex --skip-provider-readiness --yes <city>
gc supervisor start
gc rig add <rig> --city <city> --name tincan --prefix tc --start-suspended
gc start <city> --no-auto-restart
gc rig status tincan --city <city>
```

Smoke result:

```text
gc start <city> --no-auto-restart: exit 0; City started under supervisor.
gc rig status tincan: exit 0; rig is suspended; control-dispatcher stopped.
Rig metadata: backend=dolt, database=dolt, dolt_mode=server, dolt_database=tc.
Rig hooks present: on_create, on_update, on_close.
Log search found no already-initialized fatal, abort, or init-rig failure.
```

Notes:

- The previously registered `/home/jaword/sqltest-dolt` and `/home/jaword/sqtest` fixtures could not be used directly because their city configs are stale and missing current provider aliases.
- A first scratch attempt without isolated `GC_HOME` registered `city2` in the default supervisor registry; it was removed with `gc unregister /tmp/gascity-ga-7z0hku-smoke.ZcX0U6/city2`.
- The isolated supervisor was stopped after the smoke.
