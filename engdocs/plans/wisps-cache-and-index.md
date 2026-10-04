# Wisps Cache And Index Plan

Source beads: `ga-oz6rf`, `ga-n3ru6`
Design source: `source:actual-designer`
Target agent: `gascity/builder`
Priority: P1 for cache fix, P2 for follow-on index migration

## Goal

Stop the management dolt connection storm caused by uncached wisps reads, then
add a defensive wisps table index for fallback paths after the cache fix lands.

## Work Packages

1. `ga-oz6rf.1` - Builder can lock in wisps cache behavior with focused tests
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: cover PrimeWisps, TierWisps/TierBoth reads, CachedList,
     write-through, ApplyEvent, reconciliation, bypass paths, and wisps stats
     without requiring a real dolt instance.

2. `ga-oz6rf.2` - Builder can serve TierWisps and TierBoth reads from CachingStore
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-oz6rf.1`
   - Acceptance: add wisps cache state, priming, read paths, CachedList support,
     TierBoth merge behavior, fallback behavior, and stats exposure while
     preserving existing TierIssues behavior.

3. `ga-oz6rf.3` - Builder can keep wisps cache current across mutations and events
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-oz6rf.2`
   - Acceptance: ephemeral Create, Update, Close, Reopen, Delete, CloseAll,
     transaction refresh, and ApplyEvent paths update `c.wisps` safely without
     holding `c.mu` across backing-store I/O.

4. `ga-oz6rf.4` - Builder can reconcile wisps cache freshness without excess dolt scans
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-oz6rf.2`
   - Acceptance: run a separate wisps reconciliation cadence, atomically replace
     open wisps from backing data, handle degraded state and recovery, and keep
     closed wisps out after replacement.

5. `ga-oz6rf.5` - Builder can verify the wisps cache fix against the dolt connection storm
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-oz6rf.3`, `ga-oz6rf.4`
   - Acceptance: project quality gates pass and an operator-visible check shows
     repeated wisps reads no longer create a new dolt connection per read after
     cache prime.

6. `ga-n3ru6.1` - Builder can add an idempotent wisps composite-index migration
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-oz6rf.5`
   - Acceptance: provide an executable, idempotent migration for
     `idx_wisps_type_status_assignee`, verify it with `SHOW INDEX`, commit the
     schema change to dolt history, and document rollback.

7. `ga-n3ru6.2` - Builder can verify wisps index planner behavior before rollout
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-n3ru6.1`
   - Acceptance: capture EXPLAIN output for the mail-check query and the
     `status=open` PrimeWisps query, adding a secondary status index only if the
     planner still shows a full scan.

8. `ga-n3ru6.3` - Builder can roll out and verify the wisps index migration safely
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-n3ru6.2`
   - Acceptance: confirm target dolt instance and rollback path, apply the
     migration, verify dolt history and index state, and record before/after
     query latency or the missing environment needed for an operator handoff.

## Dependency Graph

`ga-oz6rf.1` blocks `ga-oz6rf.2`.
`ga-oz6rf.2` blocks `ga-oz6rf.3` and `ga-oz6rf.4`.
`ga-oz6rf.3` and `ga-oz6rf.4` block `ga-oz6rf.5`.
`ga-oz6rf.5` blocks `ga-n3ru6.1`, which blocks `ga-n3ru6.2`, which blocks
`ga-n3ru6.3`.

## Guardrails

- The index migration is lower priority and must not block the cache fix.
- Keep implementation scoped to `internal/beads/caching_store*.go`, related
  tests, and the later `schemas/wisps-composite-index/` migration unless a bead
  records a directly necessary adjacent change.
- Preserve the zero-hardcoded-role invariant.
- Do not route this back to design; the source design is already complete.
