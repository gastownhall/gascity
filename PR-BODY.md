## Summary

`cmd/gc/pool_desired_state.go`'s `beadPriority()` defaulted a nil `Priority`
to P0 (highest), while `internal/beads/native_dolt_store.go` round-trips the
same nil case as bd's documented mid default, P2. Confirmed live (not
dormant, see ra-vsvjlx notes): the `assignedWorkBeads` slice that feeds
`computePoolDesiredStates` carries store-read `Bead` structs straight through
with no priority normalization step anywhere between the read and
`beadPriority()`'s two call sites (pool_desired_state.go:200, 235). Any bead
created without an explicit `--priority` therefore schedules as P0 in
pool-demand ordering instead of P2 — a genuine scheduling-priority inversion
that could out-schedule an explicitly-labeled P1 bead.

## Fix

One-line change: nil `Priority` now defaults to `2` in `beadPriority()`,
matching `native_dolt_store.go`'s own default semantics.

## Test

`cmd/gc/pool_desired_state_nil_priority_test.go` (new):
- `TestBeadPriority_NilDefaultsToP2` — proven fail-before (returned 0, wanted
  2) / pass-after.
- `TestBeadPriority_ExplicitPriorityPreserved` — explicit P0/P1 beads are
  unaffected by the default.

```
go test ./cmd/gc/ -run TestBeadPriority -v
--- PASS: TestBeadPriority_NilDefaultsToP2
--- PASS: TestBeadPriority_ExplicitPriorityPreserved
```

Full `cmd/gc` package suite also run; the only failure is a pre-existing,
unrelated infra flake (a leaked `dolt sql-server` subprocess from
`TestInitFromWithoutHostedPreservesTemplate`, not any `--- FAIL: TestXxx`
assertion) — reproduces identically on the sibling
`fix/release-guard-destructive-deletes` clone, so it predates this change.

Source bead: ra-vsvjlx.
