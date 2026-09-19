# Plan: FR-10 regression test `TestDoltCleanupRefusesLiveBeadsDatabase`

**Source design:** `ga-qg23tv` (closed by pm after decomposition) —
slice 5/5 of architecture `ga-nw4z6` (P0 DATA LOSS: gc dolt cleanup
classifier marked live `beads` DB as orphan).
**Designer:** gascity/designer · 2026-05-11.
**PM:** gascity/pm · 2026-05-11.
**Builder bead (this plan implements):** see PM handoff mail.
**Branch:** `local/integration-2026-04-30` (rig root
`/home/jaword/projects/gascity`).

## Why this slice exists

Slice 5/5 of `ga-nw4z6` is the regression-test proof that the data-loss
bug stays dead. Slices 1–4 fixed and re-shaped the runner:

- Slice 1 (`ga-0txff0`): closed — Go runner verified already on branch.
- Slice 2 (`ga-lyv6d4` → `ga-9h05hk`): closed — live-session probe
  landed.
- Slice 3 (`ga-78stvc`) and slice 4 (`ga-endmgy`): still in design
  queue, do NOT block this slice.

FR-10 of `ga-nw4z6`: the test that pins the original incident
(mol-dog-doctor 2026-05-05) so any future regression of the
stale-prefix classifier surfaces immediately, not at the next data
loss.

## Single builder bead, single PR, mechanical

- ~120 LOC test + ~40 LOC fake = one mechanical PR.
- **No production-code changes** — slice 1 verification confirmed the
  Go runner is already correct on `origin/main` since 2026-05-09.
- The test proves the fix holds.

## Designer's load-bearing decision (`ga-qg23tv` §1)

**Use the existing in-process Go test harness with a
`fakeBeadsCleanupClient`, NOT a txtar.**

- The architect's parent (`ga-nw4z6` §"Slice 5") said "txtar test", but
  the only existing txtar that exercises `gc dolt cleanup`
  (`cmd/gc/testdata/dolt-cleanup-external-rig.txtar`) sets
  `env GC_DOLT=skip` and exercises the legacy shell `cleanup.sh` —
  the destructive path being retired (`ga-08mm01`).
- All existing `TestRunDoltCleanup_*` tests in
  `cmd/gc/cmd_dolt_cleanup_test.go` are in-process Go tests via
  `cleanupOptions.DoltClient` fakes — no real Dolt connection.
- Slice 2's `TestProbeLiveSessions_*` (closed `ga-9h05hk`) follow the
  same pattern.

The in-process Go test:

- Runs as `go test ./cmd/gc -run TestDoltCleanupRefusesLiveBeadsDatabase`
  with no integration build tag, no external Dolt.
- Asserts the same FR-10 invariant (`beads` never enters
  `Dropped.Names`).

## Deliverables (single file: `cmd/gc/cmd_dolt_cleanup_test.go`)

### Test 1 — `TestDoltCleanupRefusesLiveBeadsDatabase` (`ga-qg23tv` §3.1)

Primary FR-10 test the architect named. Reproduces the production
incident: Dolt server hosts `beads` plus stale fixtures; rig `bd-rig`
has `metadata.json` present but no `dolt_database` pin.

Four assertions:

1. `beads` is absent from `Dropped.Names`.
2. `beads` is absent from any `Errors[].DatabaseName`.
3. `beads` is absent from any `ForceBlockers[].DatabaseName`.
4. The fake client's `DropDatabase` is never called with name `beads`.

Plus sanity: stale fixtures (`testdb_abc`, `testdb_xyz`) MUST appear
in `Dropped.Names` (so failure means the runner ran).

### Test 2 — `TestDoltCleanupRefusesLiveBeadsDatabase_NoMetadataAtAll` (`ga-qg23tv` §3.2)

Strictly stronger sibling: even with `bd-rig`'s `metadata.json`
entirely absent, the runner still refuses to drop `beads`. Proves the
safety contract does not depend on metadata quality.

Load-bearing assertions: `beads` absent from `Dropped.Names`;
`dropCallsFor("beads") == 0`.

### Fake client — `fakeBeadsCleanupClient` (`ga-qg23tv` §4.1)

Package-private struct in the same `_test.go` file. Implements
`CleanupDoltClient`:

- `ListDatabases(ctx)` — returns a copy of canned `databases`.
- `DropDatabase(ctx, name)` — records every call; optional
  `dropFailFn` for failure injection.
- `ProbeLiveSessions(ctx)` — returns empty map (FR-10 holds
  independent of probe; `Probe: false` in test).
- `PurgeDroppedDatabases(ctx, rigDB)` — no-op.
- `Close()` — no-op.
- `dropCallsFor(target)` helper for assertions.

Signatures pinned verbatim in `ga-qg23tv` §4.1.

### Helper — `containsString`

Reuse if the package already has one; define inline (package-private)
if not. Builder grep before adding (slice 2's tests may have
introduced an equivalent).

## Harness setup (`ga-qg23tv` §5)

Pin the order so the builder doesn't re-derive:

1. `fsys.NewFake()` for the file system.
2. Seed `dolt-server.port` to match `cleanupOptions.CityPort` (or
   leave CityPort=0 and let the resolver fall back to the file).
3. Optionally seed each rig's `.beads/metadata.json` — minimal `{}`
   for test 1, omit for test 2.
4. Build the fake `DoltClient` with the database list simulating
   production (`beads` + stale fixtures + inert system DBs).
5. Build `cleanupOptions` with `JSON: true`, `Force: true`,
   `Probe: false`, `DoltClient: fake`.
6. Call `runDoltCleanup(opts, &stdout, &stderr)`; assert `code == 0`.
7. Unmarshal stdout into `CleanupReport`.
8. Assert per §3 (1–4); zero `dropCallsFor("beads")`; positive for
   stale fixtures.

## Acceptance criteria (verbatim from `ga-qg23tv` §6)

- [ ] `cmd/gc/cmd_dolt_cleanup_test.go` contains
      `TestDoltCleanupRefusesLiveBeadsDatabase` and
      `TestDoltCleanupRefusesLiveBeadsDatabase_NoMetadataAtAll` with
      the verbatim names from §3.
- [ ] Both tests use the in-process `fakeBeadsCleanupClient` per §4
      (no real Dolt connection, no integration build tag).
- [ ] Both tests pass:
      `go test ./cmd/gc -run 'TestDoltCleanupRefusesLiveBeadsDatabase' -count=1`.
- [ ] All four FR-10 assertions in §3.1 are present in test 1 (and
      the load-bearing two assertions in test 2).
- [ ] No production-code changes in this PR.
- [ ] `go vet ./...` clean.

## Guardrails (from `ga-qg23tv` §8)

- **Do NOT test the protection mechanism via `Protected.Names`.** The
  assertion is *absence* from `Dropped.Names` / `Errors` /
  `ForceBlockers`, NOT presence in `Protected`. Asserting
  `Protected.Names` contains `beads` would test a different mechanism
  (rig allowlist) and would mask a planner-level regression where
  `beads` enters the candidate pool.
- **Keep `Probe: false`.** Slice 2's live-session probe is a separate
  safety layer; turning it on would make the passing assertion
  ambiguous about which mechanism saved `beads`. The test must fail
  loudly if the stale-prefix planner regresses in isolation.
- **No production-code changes.** Runner is already correct; slice
  exists to lock the invariant.
- **Verbatim test name.** `TestDoltCleanupRefusesLiveBeadsDatabase` —
  the architect named it; downstream tooling (mol-dog-doctor reports,
  test-report aggregators) may already match on this string.

## Out of scope (from `ga-qg23tv` §7)

- A real-Dolt integration sweep (future bead under `test/integration/`).
- Refactoring slice 2's `fakeProbeClient` into a shared god-fake.
- Adding a `protected_databases` allowlist to `city.toml`.
- A txtar that drives the shell `cleanup.sh` (legacy path retiring).

## References

- Design: `bd show ga-qg23tv` (closed; verbatim §3 test code + §4 fake
  client signatures).
- Source architecture: `bd show ga-nw4z6` (closed).
- Slice 1: `ga-0txff0` (closed — runner already on branch).
- Slice 2 implementation: `ga-9h05hk` (closed — landed).
- Slices 3 / 4: `ga-78stvc`, `ga-endmgy` (still in design queue —
  independent of this slice).
