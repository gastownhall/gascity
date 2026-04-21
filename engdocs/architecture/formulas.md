---
title: "Formulas & Molecules"
---


> Last verified against code: 2026-03-17

## Summary

Formula files are reusable workflow definitions stored as
`*.formula.toml`. Gas City resolves those files through ordered formula
layers, stages the active winners into `.beads/formulas/`, and asks the
configured beads backend to instantiate molecules from them.

Current merge-wave status:

- The in-flight Pack/City v2 merge still uses `*.formula.toml` and
  `orders/*.order.toml`.
- We decided to remove the `.formula.` / `.order.` infix after the
  merge, not during it.
- That follow-up is tracked in
  [gastownhall/gascity#586](https://github.com/gastownhall/gascity/issues/586).

The important current-state boundary is this:

- Gas City owns formula discovery and layer resolution.
- The beads backend owns formula materialization.
- `bd` is the full-featured backend for real formula execution today.

## Key Concepts

- **Formula file**: A `*.formula.toml` file selected through formula
  layers. This is the current on-disk naming; simplification is tracked
  separately in `#586`.
- **Formula layers**: Ordered directories computed from packs, city config,
  and rig config. Higher-priority layers shadow lower-priority files by name.
- **Molecule**: A runtime instance created from a formula.
- **Wisp**: An ephemeral molecule created for dispatch or order
  execution.
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
  when formula-backed orders fire. In the current merge wave, orders are
  discovered from `orders/*.order.toml`; removal of the `.order.`
  infix is deferred to `#586`.

### Garbage Collection

Closed wisps are purged by the controller's wisp GC in
[`cmd/gc/wisp_gc.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/wisp_gc.go). The interval and TTL come from
`[daemon].wisp_gc_interval` and `[daemon].wisp_ttl`.

### Running tests in containers

Most formula steps run in the pool worker's own shell. That is correct
for steps whose work IS the agent's state (git operations on the host
checkout, bd mutations, message sends) — host == work context. It is
NOT correct for steps that are supposed to mirror a clean build
environment, because the worker shell inherits city/session state
(`GC_CITY_PATH`, `GC_DOLT_*`, `GC_ALIAS`, …) that CI does not.

The `gastownhall-upstream` formula's `run-tests` step is the first step
to adopt a containerized execution contract. The contract is:

- `make upstream-test-image` builds `gc-upstream-test:<deps-hash>` on
  first pour and tags `:latest`. Cached by
  `sha256(deps.env + contrib/upstream-container/Dockerfile.upstream)`, so
  subsequent pours with unchanged versions skip the build.
- `scripts/upstream-test-run` invokes `docker run` with the worktree
  bind-mounted read-only and `--env-file /dev/null` (no host env leaks
  into the test process). `GOCACHE`/`GOMODCACHE` are named-volume
  mounts — cache warmth across pours, single-host for Phase 1.
- Preflight hard-fails if `docker` is missing. There is no host
  fallback: fallback is what containerization exists to eliminate.
- Host-side operations stay on host. The `rebuild-integration` step
  uses git remotes and push credentials the container does not have;
  it produces the worktree the container then bind-mounts. This split
  is deliberate (see
  [`engdocs/design/gastownhall-upstream-containerize.md`](../design/gastownhall-upstream-containerize.md)).

Other formulas are not containerized. Adopt this contract only for
steps whose work SHOULD be environment-insensitive — most commonly,
steps that are mirroring CI.

Phase 2 (tracked as follow-on work in the design doc above) decouples
the per-shipment `rebuild-integration` step from the running-binary
patch set: `integration-refs.toml` becomes `deploy-refs.toml`, a
`rebuild-deploy` step runs at binary-install time rather than every
shipment, and the per-shipment path simplifies to a clean-checkout +
container-run. Phase 1 does not anticipate that split; the container
contract documented here stands as-is.

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
| `contrib/upstream-container/Dockerfile.upstream` | Test image used by the containerized `run-tests` step |
| `scripts/upstream-test-run` | Wrapper around `docker run` for the containerized `run-tests` step |

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
