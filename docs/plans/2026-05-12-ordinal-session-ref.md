---
title: Ordinal Session Reference
date: 2026-05-12
issue: https://github.com/gastownhall/gascity/issues/2031
status: in-progress
---

# Ordinal Session Reference

## Problem

Users want short ordinal references for sessions (`gc session attach 1`)
instead of typing bead IDs or full aliases.

## Mental model (settled with JPB)

Ordinals are **purely ephemeral indices into the most recently rendered
`gc session list`**. They are not stable IDs, not predicates, not
filters. They mean exactly: "the bead I just saw in row N."

If the list changes between renders, the same number refers to a
different bead. That is intentional — same as a screen cursor.

## Design (Path 1: snapshot file)

- **Snapshot file.** `gc session list` writes the displayed bead IDs
  (in display order) to `<cityPath>/.gc/last-session-list.json`. Atomic
  via temp file + rename. Last write wins; no TTL.
- **Per-city scope.** Each city's `.gc/` holds its own snapshot. No
  cross-city contamination.
- **Pure-digit ordinal resolution.** When `resolveSessionIDWithOptions`
  is given a canonical non-negative integer (no leading zeros, no signs,
  no whitespace), it reads the snapshot and returns `snapshot[N]`.
- **Existence guard.** Stale snapshot entries (bead no longer in store)
  fall through to `ErrSessionNotFound` rather than returning a phantom
  ID.
- **Resolver precedence in `resolveSessionIDWithOptions`:**
  1. exact ID
  2. configured named session
  3. alias / partial ID (existing `ResolveSessionID`)
  4. qualified alias basename
  5. **new: pure-digit ordinal via snapshot** (consulted only after the
     above branches return `ErrSessionNotFound`)
  6. allow-closed (when opt set)

  Alias wins over ordinal: if a session is aliased `"1"`,
  `gc session attach 1` resolves to that alias.
- **Closed beads.** If the snapshot references a closed bead, the
  resolver returns it (same as `gc session attach <closed-id>`).
  Downstream commands decide what to do with closed sessions — not the
  resolver's concern.
- **Display.** Add `#` column (leftmost) to `gc session list` table
  output. JSON output unchanged.

## Scope

Wired only through `resolveSessionIDWithOptions` — the config-aware
path used by `attach`, `suspend`, `resume`, `stop`, `wake`, etc.

`resolveSessionID(store, ...)` (plain, no cityPath) — used by
`cmd_handoff`, `cmd_mail`, `cmd_nudge` for internal lookups of known
session names — is not extended. These call sites take session-names
from bead metadata, not user-typed ordinals, so ordinal support there
is out of scope for #2031.

## TDD plan

1. ✅ `writeSessionListSnapshot` / `readSessionListSnapshot` round trip.
2. ✅ `resolveOrdinalFromSnapshot` resolves 0/1/2 against snapshot.
3. ✅ Out-of-range ordinal → `ErrSessionNotFound`.
4. ✅ Non-canonical digits (`"01"`, `"+0"`, whitespace) rejected.
5. ✅ Missing snapshot → `ErrSessionNotFound`.
6. ✅ Full resolver: alias `"1"` beats ordinal `1`.
7. ✅ Full resolver: closed bead in snapshot still resolves.
8. ✅ Full resolver: stale snapshot entry (missing bead) falls through.
9. ✅ `gc session list` prints `#` column and writes snapshot in
   display order.

## Files

- `cmd/gc/session_ordinal.go` — snapshot read/write + ordinal resolver
- `cmd/gc/session_ordinal_test.go` — snapshot + resolver tests
- `cmd/gc/session_resolve.go` — wire ordinal branch into
  `resolveSessionIDWithOptions`
- `cmd/gc/cmd_session.go` — `#` column + snapshot write in
  `cmdSessionList`
- `cmd/gc/cmd_session_test.go` — list-output + snapshot test
- `docs/plans/2026-05-12-ordinal-session-ref.md` — this file
