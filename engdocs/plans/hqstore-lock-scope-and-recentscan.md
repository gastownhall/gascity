# HQStore Lock Scope and RecentScan Plan

Date: 2026-05-27

PM intake source:
- Aggregate designer handoff mail: `gm-wisp-n49o82`
- Aggregate designer root: `ga-0zt9w`
- Earlier designer handoff mail: `gm-wisp-7q5ogc`
- Designer roots: `ga-u2991`, `ga-lld7b`
- Architecture source: `ga-vbuxn`
- Tracker import: no installed tracker skill found

## Goal

Turn the completed HQStore concurrency designs into serialized builder work.
`ga-u2991` lands first because moving `cloneBead` outside the read lock is the
larger p99 latency lever and is the prerequisite for the bounded RecentScan path
in `ga-lld7b`. The aggregate handoff `ga-0zt9w` adds the final benchmark and
900s soak evidence gate before Phase 2 lock sharding can be considered.

## Work Packages

### Clone outside HQStore read locks

Root: `ga-u2991`

1. `ga-u2991.1` - Builder: As a maintainer, I can list beads without cloning under the HQStore read lock.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `List()` keeps candidate discovery and raw `Bead` struct copies under `s.mu.RLock()`, releases the read lock before `cloneBead`, `query.Matches`, sorting, or limit application, preserves existing query semantics, and does not change `memstore.go`, `ListQuery`, or public APIs.

2. `ga-u2991.2` - Builder: As an agent, I can query ready work without clone allocations blocking HQStore writers.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `Ready()` collects candidate IDs, dependency status, and raw `Bead` struct copies under `s.mu.RLock()`, deep-copies only after unlocking, preserves ready filtering behavior, and adds the concise `upsertOwnedLocked` write-invariant comment.

3. `ga-u2991.3` - Builder: As a maintainer, I can verify List and Ready are race-safe with concurrent writers.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: adds `TestListReadyConcurrentWithWriters` or equivalent coverage with at least 1000 seeded beads and concurrent `List`, `SetMetadataBatch`, and `Create`; asserts non-empty results; reports goroutine errors; verifies with `go test -race ./internal/beads -run TestListReadyConcurrentWithWriters` plus focused non-race package tests.

### Bounded RecentScan fast path

Root: `ga-lld7b`

4. `ga-lld7b.1` - Builder: As a mail user, recent scans return newest beads without full HQStore scans.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `List()` has a fast path for `SortCreatedDesc`, `Limit > 0`, and `AllowScan=true`; it walks `s.order` newest-first under `s.mu.RLock()`, performs only tier and `IncludeClosed` prechecks under the lock, collects bounded raw struct copies without `cloneBead`, filters and clones after unlocking, preserves ordering and tier semantics, and does not change `ListQuery`, `iterationIDsLocked`, `Ready()`, or public APIs.

5. `ga-lld7b.2` - Builder: As a maintainer, I can verify RecentScan fast-path ordering and race safety.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: adds regression coverage for newest-N results from a large mixed dataset, expected CreatedAt-descending IDs on a small dataset, and race-detector coverage for concurrent `List`, `Create`, and `SetMetadataBatch`; verifies with focused `go test ./internal/beads` and `go test -race ./internal/beads -run TestListRecentDescConcurrentWithWriters`.

### Phase 1 benchmark and soak gate

Root: `ga-0zt9w`

6. `ga-0zt9w.1` - Validator: As a maintainer, I can validate HQStore Phase 1 with benchmark and soak evidence.
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: adds or confirms `BenchmarkHQStoreRecentScan` with a 28k-bead setup and the RecentScan query, captures benchmark allocation/output evidence after implementation, runs the focused race-detector commands for the builder-added concurrent tests, runs the PR #2642 chaos harness at 28k beads for a 900s soak or records the exact environmental blocker, and confirms Phase 2 is not requested unless Create p99 remains above 5ms.

## Dependency Graph

- `ga-u2991.2` depends on `ga-u2991.1`.
- `ga-u2991.3` depends on `ga-u2991.1` and `ga-u2991.2`.
- `ga-lld7b.1` depends on `ga-u2991.3`.
- `ga-lld7b.2` depends on `ga-lld7b.1`.
- `ga-0zt9w.1` depends on `ga-lld7b.2`.

This serializes work touching `internal/beads/hqstore_core.go` and keeps the
RecentScan optimization behind the clone-outside-lock invariant proof and race
coverage. The validator gate runs only after both implementation slices and
their focused tests have landed.

## Handoff Targets

Builder:
- `ga-u2991.1`
- `ga-u2991.2`
- `ga-u2991.3`
- `ga-lld7b.1`
- `ga-lld7b.2`

Validator:
- `ga-0zt9w.1`

Builder beads have `source:actual-pm`, `ready-to-build`, `coordstore`, and
`gc.routed_to=gascity/builder`. The validator bead has `source:actual-pm`,
`needs-tests`, `coordstore`, and `gc.routed_to=gascity/validator`.

## Guardrails

- Do not add new primitives, public query fields, or hardcoded role behavior.
- Keep the work inside `internal/beads/hqstore_core.go` and the focused beads
  tests unless implementation proves an adjacent test helper is necessary.
- Preserve the write invariant that stored `Metadata` and `Labels` are not
  mutated in place after insertion.
- Treat race-detector commands as required verification for the concurrency
  slices before closing their beads.
- Treat the 28k-bead benchmark and 900s chaos soak as the Phase 2 gate; do not
  ask for tier-level lock sharding unless Phase 1 still misses Create p99 <= 5ms.
