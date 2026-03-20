---
title: "Life of a Molecule"
---


> Last verified against code: 2026-03-17

## Summary

A molecule starts as a `*.formula.toml` file, becomes active through formula
layer resolution, is instantiated by the configured beads backend, gets routed
to an agent or pool, and eventually closes and ages out through wisp GC.

The crucial current-state detail is that Gas City resolves formula files, but
the store backend performs runtime formula instantiation.

## Phase 1: Definition

Formulas live on disk as `*.formula.toml` files in city, pack, or rig formula
directories.

Example:

```toml
formula = "code-review"
description = "Multi-step code review workflow"

[[steps]]
id = "analyze"
title = "Analyze changes"

[[steps]]
id = "test"
title = "Run tests"
needs = ["analyze"]
```

For the file format itself, see [Formula Files](/reference/formula).

## Phase 2: Resolution

`ComputeFormulaLayers()` in `internal/config/pack.go` builds ordered formula
layers for the city and each rig. `ResolveFormulas()` in
[`cmd/gc/formula_resolve.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go) then:

1. scans all layers for `*.formula.toml`
2. keeps the highest-priority file for each filename
3. stages winners into `.beads/formulas/` as symlinks
4. removes stale formula symlinks

This makes the active formula set visible to backends like `bd`.

## Phase 3: Instantiation

Molecule creation goes through the `beads.Store` interface:

- `Store.MolCook(formula, title, vars)`
- `Store.MolCookOn(formula, beadID, title, vars)`

Backend behavior:

- `BdStore` uses `bd mol wisp` and `bd mol bond`
- `exec.Store` delegates `mol-cook` and `mol-cook-on` to a script
- `MemStore` and `FileStore` create simplified molecule roots for tests and
  tutorials

For a production city, `BdStore` is the normal path.

## Phase 4: Routing

After instantiation, `gc sling` or order dispatch routes the molecule root:

- [`cmd/gc/cmd_sling.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_sling.go) handles explicit user
  dispatch
- [`cmd/gc/order_dispatch.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/order_dispatch.go) handles
  formula-backed scheduled work

The routing step labels or assigns the resulting root bead so an agent or pool
can discover it through its normal work query.

## Phase 5: Execution

The agent picks up the molecule through the bead store and works through the
resulting tasks. In production, `bd` owns the detailed step-bead materialization
and dependency handling. Gas City does not currently provide a separate
in-process formula executor for the main runtime path.

At this layer, the important contributor rule is simple:

- molecule creation is a store concern
- routing is a `cmd/gc/` concern
- step execution is an agent plus backend concern

## Phase 6: Completion

Once the work represented by the molecule is done, the relevant beads are
closed. The root molecule bead then transitions to `closed` as well.

That closed root still matters because it is:

- visible for history and audit
- eligible for TTL-based cleanup if it is a wisp
- useful to order cooldown tracking when created by automation

## Phase 7: Garbage Collection

`cmd/gc/wisp_gc.go` periodically purges closed molecules older than
`[daemon].wisp_ttl`, on the cadence set by `[daemon].wisp_gc_interval`.

This cleanup is intentionally conservative:

- only closed molecules are eligible
- TTL must have elapsed
- cleanup is skipped entirely when the daemon settings are unset

## Function Reference

| Phase | Function | File |
|---|---|---|
| Layer computation | `ComputeFormulaLayers()` | `internal/config/pack.go` |
| Symlink staging | `ResolveFormulas()` | `cmd/gc/formula_resolve.go` |
| Wisp creation | `Store.MolCook()` | `internal/beads/beads.go` |
| Attached molecule creation | `Store.MolCookOn()` | `internal/beads/beads.go` |
| Production backend cook | `BdStore.MolCook()` / `BdStore.MolCookOn()` | `internal/beads/bdstore.go` |
| Script backend cook | `exec.Store.MolCook()` / `exec.Store.MolCookOn()` | `internal/beads/exec/exec.go` |
| User dispatch | `doSling()` | `cmd/gc/cmd_sling.go` |
| Order dispatch | `dispatchWisp()` | `cmd/gc/order_dispatch.go` |
| GC creation | `newWispGC()` | `cmd/gc/wisp_gc.go` |
| GC execution | `memoryWispGC.runGC()` | `cmd/gc/wisp_gc.go` |

## See Also

- [Formulas & Molecules](/architecture/formulas) for the subsystem overview
- [Dispatch](/architecture/dispatch) for sling routing
- [Orders](/architecture/orders) for formula-backed automation
- [Bead Store](/architecture/beads) for the store boundary that owns instantiation
