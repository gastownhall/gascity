---
title: "Formulas & Molecules"
---


> Last verified against code: 2026-03-17

## Summary

Formula files are reusable workflow definitions stored as
`*.formula.toml`. Gas City resolves those files through ordered formula
layers, stages the active winners into `.beads/formulas/`, and asks the
configured beads backend to instantiate molecules from them.

The important current-state boundary is this:

- Gas City owns formula discovery and layer resolution.
- The beads backend owns formula materialization.
- `bd` is the full-featured backend for real formula execution today.

## Key Concepts

- **Formula file**: A `*.formula.toml` file selected through formula layers.
- **Formula layers**: Ordered directories computed from packs, city config,
  and rig config. Higher-priority layers shadow lower-priority files by name.
- **Molecule**: A runtime instance created from a formula.
- **Wisp**: An ephemeral molecule created for dispatch or order execution.
- **Attached molecule**: A formula instantiated onto an existing bead via
  `Store.MolCookOn`.
- **Convergence formula subset**: The subset of formula metadata used by the
  convergence subsystem, validated in
  [`internal/convergence/formula.go`](https://github.com/gastownhall/gascity/blob/main/internal/convergence/formula.go).

## Architecture

```
formula layers
  from config + packs
        |
        v
ResolveFormulas()
cmd/gc/formula_resolve.go
        |
        v
.beads/formulas/*.formula.toml
        |
        v
Store.MolCook / Store.MolCookOn
        |
        +--> BdStore     -> bd mol wisp / bd mol bond
        +--> exec.Store  -> script mol-cook / mol-cook-on
        \--> Mem/File    -> simplified molecule root for tests/tutorials
```

### Resolution

`ComputeFormulaLayers()` in `internal/config/pack.go` computes the ordered
layer set for the city and each rig. `ResolveFormulas()` in
[`cmd/gc/formula_resolve.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/formula_resolve.go) then:

1. Scans each layer for `*.formula.toml`
2. Keeps the highest-priority winner for each filename
3. Symlinks winners into `<target>/.beads/formulas/`
4. Removes stale formula symlinks without touching real files

This keeps the active formula set visible to backend tools such as `bd`.

### Instantiation

The store interface is the runtime seam:

- `Store.MolCook(formula, title, vars)` creates a new molecule or wisp
- `Store.MolCookOn(formula, beadID, title, vars)` attaches a molecule to an
  existing bead

Current implementations behave as follows:

- **`BdStore`** in [`internal/beads/bdstore.go`](https://github.com/gastownhall/gascity/blob/main/internal/beads/bdstore.go)
  delegates to `bd mol wisp` and `bd mol bond`, then parses the returned root
  bead ID.
- **`exec.Store`** in [`internal/beads/exec/exec.go`](https://github.com/gastownhall/gascity/blob/main/internal/beads/exec/exec.go)
  forwards `mol-cook` and `mol-cook-on` to a user script.
- **`MemStore`** and **`FileStore`** create a simplified molecule root bead.
  They are suitable for tests and tutorials, not full formula execution.

### Dispatch and Orders

Formulas are consumed in two main places:

- [`cmd/gc/cmd_sling.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_sling.go) creates wisps during
  `gc sling --formula` and attached molecules via `--on`.
- [`cmd/gc/order_dispatch.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/order_dispatch.go) creates wisps
  when formula-backed orders fire.

### Garbage Collection

Closed wisps are purged by the controller's wisp GC in
[`cmd/gc/wisp_gc.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/wisp_gc.go). The interval and TTL come from
`[daemon].wisp_gc_interval` and `[daemon].wisp_ttl`.

## Invariants

- Formula resolution is last-wins by filename across ordered layers.
- `ResolveFormulas()` only mutates symlinks under `.beads/formulas/`; it never
  overwrites real files.
- Molecule creation always goes through the configured `beads.Store`.
- Full multi-step formula execution is backend-dependent today; `BdStore` is
  the production path.
- Wisp garbage collection only targets closed molecules past the configured
  TTL.

## Interactions

| Depends on | How |
|---|---|
| `internal/config` | Computes formula layers from city, packs, and rigs |
| `internal/beads` | Instantiates formulas via `MolCook` and `MolCookOn` |
| `internal/convergence` | Validates convergence-specific formula metadata |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_sling.go` | Creates wisps and attached molecules from formulas |
| `cmd/gc/order_dispatch.go` | Fires formula-backed orders |
| `cmd/gc/wisp_gc.go` | Purges expired closed molecules |
| Contributor docs | Reference formula layout and resolution behavior |

## Code Map

| Path | Responsibility |
|---|---|
| `cmd/gc/formula_resolve.go` | Layer winner selection and symlink staging |
| `cmd/gc/cmd_sling.go` | Formula-backed sling and attached-molecule flows |
| `cmd/gc/order_dispatch.go` | Formula-backed order dispatch |
| `cmd/gc/wisp_gc.go` | TTL-based cleanup for closed molecules |
| `internal/config/config.go` | `FormulaLayers` data shape |
| `internal/config/pack.go` | `ComputeFormulaLayers()` |
| `internal/beads/beads.go` | `MolCook` / `MolCookOn` store interface |
| `internal/beads/bdstore.go` | Production formula instantiation via `bd` |
| `internal/beads/exec/exec.go` | Script-backed formula instantiation |
| `internal/beads/memstore.go` | Simplified in-memory molecule creation |
| `internal/beads/filestore.go` | Persistent wrapper over `MemStore` |
| `internal/convergence/formula.go` | Convergence-specific formula validation |

## Configuration

Formula layers are assembled from:

- city packs
- `[formulas].dir` in `city.toml`
- rig packs
- `[[rigs]].formulas_dir`

Wisp cleanup is configured in `city.toml`:

```toml
[daemon]
wisp_gc_interval = "5m"
wisp_ttl = "24h"
```

See [Formula Files](../../docs/reference/formula.md) for the file format itself.

## Testing

- `cmd/gc/formula_resolve_test.go` verifies winner selection, stale cleanup,
  and real-file preservation
- `internal/beads/bdstore_test.go` verifies `bd mol wisp` / `bd mol bond`
  wiring and root ID parsing
- `internal/beads/memstore_test.go` and `internal/beads/filestore_test.go`
  verify simplified molecule creation for test-oriented stores
- `cmd/gc/order_dispatch_test.go` and `cmd/gc/cmd_sling_test.go` cover the
  higher-level formula dispatch paths

## Known Limitations

- Gas City does not currently own a general in-process formula parser for the
  main runtime path.
- Step-bead materialization is backend-dependent; production behavior comes
  from `bd`.
- Tutorial and in-memory stores intentionally implement a smaller molecule
  model than the production backend.

## See Also

- [Formula Files](../../docs/reference/formula.md) for the file layout
- [Dispatch](dispatch.md) for sling-based formula routing
- [Orders](orders.md) for formula-backed scheduled work
- [Bead Store](beads.md) for the `MolCook` interface boundary
