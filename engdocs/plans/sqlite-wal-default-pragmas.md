# SQLite WAL DefaultPragmas Landing Plan

Root bead: `ga-c52q2s`
Architecture source: `ga-qe54tg`
Design source: `gascity/designer`, completed 2026-05-27

## Goal

Land the SQLite WAL/checkpoint fix from `spike/ga-2s6sz-sqlite-tuning` onto
the main-bound branch, including the missing pure-Go `DefaultPragmas`
amendment. The fix must bound WAL growth without schema changes, public API
changes, or caller-visible behavior changes through `StoreAdapter`.

## Child Beads

| Order | Bead | Route | Title |
| --- | --- | --- | --- |
| 1 | `ga-c52q2s.1` | `gascity/builder` | Apply SQLite checkpoint infrastructure cherry-pick |
| 2 | `ga-c52q2s.2` | `gascity/builder` | Land WAL regression tests with pure-Go contrast adaptation |
| 3 | `ga-c52q2s.3` | `gascity/builder` | Add gated EQP diagnostic test from spike |
| 4 | `ga-c52q2s.4` | `gascity/builder` | Amend DefaultPragmas for the pure-Go SQLite path |
| 5 | `ga-c52q2s.5` | `gascity/builder` | Verify SQLite WAL fix and prepare PR handoff |

Dependency graph:

- `ga-c52q2s.2` depends on `ga-c52q2s.1`
- `ga-c52q2s.3` depends on `ga-c52q2s.2`
- `ga-c52q2s.4` depends on `ga-c52q2s.3`
- `ga-c52q2s.5` depends on `ga-c52q2s.4`

## Acceptance Summary

The implementation is complete when:

- Commits `7fc628a14`, `400df826f`, and `06fd9de9e` are applied in the
  documented order.
- Spike commit `d93b37b21` is omitted.
- `DefaultPragmas` uses `mmap_size=0` and `wal_autocheckpoint=1000`.
- `DefaultPragmas` and `FullSyncPragmas` differ only by `synchronous`.
- `TestWALUnboundedWithCheckpointerDisabled` uses the unexported,
  test-only `brokenPragmasForContrast` constant.
- `brokenPragmasForContrast` has no production-code occurrences.
- `go test ./internal/benchmarks/coordstore/adapters/sqlite/...` passes.
- `go vet ./internal/benchmarks/...` is clean.
- The PR title is:
  `fix(coordstore): bound SQLite WAL on pure-Go path; land WAL regression tests (refs ga-qe54tg)`.

## Out Of Scope

- No schema changes.
- No `StoreAdapter` API changes.
- No soak rerun before landing.
- No removal of `COORDSTORE_SQLITE_CHECKPOINT_INTERVAL`.
- No WAL ceiling tightening beyond the existing `<= 16MiB` regression
  assertion.

## Risks

- The contrast regression must not accidentally keep pointing at
  `DefaultPragmas` after that production constant is fixed.
- The builder must preserve the cherry-pick order to avoid avoidable
  conflicts and to keep the test adaptation local to the regression slice.
- The cgo path should only receive the spike's selected `FullSyncPragmas`
  behavior, not any extra pure-Go amendment.
