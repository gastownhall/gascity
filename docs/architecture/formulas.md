# Formulas & Molecules

<!--
Current-state architecture document. Describes how formulas, molecules,
wisps, variable substitution, formula resolution, and wisp garbage
collection work TODAY. For proposed changes, write a design doc in
docs/design/ instead.

Audience: Gas City contributors (human and LLM agent).
Update this document when the implementation changes.
-->

> Last verified against code: 2026-03-01

## Summary

Formulas & Molecules is a Layer 2-4 derived mechanism that defines
multi-step workflows as TOML files (formulas) and instantiates them at
runtime as bead trees (molecules). A formula specifies a DAG of named
steps with dependency ordering; a molecule is that DAG materialized as
one root bead plus one child bead per step, persisted through the Bead
Store. Wisps are ephemeral molecules created by dispatch or automations,
garbage-collected after a configurable TTL.

## Key Concepts

- **Formula**: A parsed definition from a `*.formula.toml` file. Contains
  a Name (unique identifier), Description, and a Steps array. Each
  formula defines a reusable workflow template. Defined in
  `internal/formula/formula.go`.

- **Step**: One step within a formula. Has an ID (unique within the
  formula), Title, Description, and a Needs list of step IDs that must
  complete before this step can start. Steps form a directed acyclic
  graph of intra-formula dependencies.

- **Molecule**: A formula instantiated at runtime. Consists of one root
  bead (Type=`molecule`, Ref=formula name) and one child bead per step
  (Type=`task`, ParentID=root, Ref=step ID). Progress is tracked by
  closing step beads. Defined through `Store.MolCook()` and
  `formula.ComposeMolCook()`.

- **Wisp**: An ephemeral molecule. Created by `gc sling --formula` or
  automation dispatch. Wisps auto-close and are garbage-collected by wisp
  GC after a configurable TTL (`wisp_ttl`). There is no structural
  difference between a wisp and a molecule -- "wisp" is the operational
  term for molecules created ephemerally by the system.

- **Vars**: Template variables in formula step descriptions. Written as
  `{{key}}` placeholders, substituted at cook time from `key=value`
  pairs. Substitution produces a shallow copy; the original formula is
  never modified.

- **FormulaLayers**: The four-priority-level resolution system that
  determines which `*.formula.toml` files are active for a given scope.
  Higher-priority layers shadow lower ones by filename. Defined in
  `internal/config/config.go`.

- **Automation**: A formula (or shell script) dispatch triggered by a
  gate condition. Lives in formula directories as
  `automations/<name>/automation.toml`. Inherits the formula layer
  resolution system. See
  [Health Patrol architecture](./health-patrol.md) for gate evaluation
  details.

## Architecture

### Data Flow

The lifecycle of a formula from definition to execution:

```
*.formula.toml       Parse()         Validate()       ComposeMolCook()
   (TOML file)    ──────────>  Formula  ──────────>  ──────────────────>
                                struct    (check       root bead
                                          cycles,      + step beads
                                          dups,        in Store
                                          refs)

FormulaLayers     ResolveFormulas()    symlinks in
  (4 layers)    ──────────────────>   .beads/formulas/
                                       (per scope)

gc sling --formula    instantiateWisp()    Store.MolCook()
  (CLI)             ────────────────────> ───────────────> root bead ID

Automation.dispatch   dispatchWisp()    instantiateWisp()
  (controller tick) ──────────────────> ────────────────> root bead ID
```

**Parse path**: TOML bytes are decoded by `formula.Parse()` into a
`Formula` struct, then validated by `formula.Validate()` which checks
for: non-empty name, at least one step, no duplicate step IDs, all
`Needs` references resolve to existing step IDs, and no dependency
cycles (via Kahn's algorithm).

**Cook path**: `formula.ComposeMolCook()` resolves a formula by name,
applies variable substitution via `SubstituteVars()`, creates a root
bead (Type=`molecule`, Ref=formula name), then creates one child bead
per step (Type=`task`, ParentID=root, Ref=step ID, Needs=step.Needs,
Description=step.Description).

**Resolution path**: `ResolveFormulas()` takes ordered layer directories
and creates symlinks in `<targetDir>/.beads/formulas/` so that the
bead store (bd) finds formulas natively. Higher-priority layers
overwrite lower ones by filename. Stale symlinks are cleaned up; real
files (non-symlinks) are never overwritten.

**Progress tracking**: `CurrentStep()` scans step beads and returns the
first open step whose `Needs` are all closed. `CompletedCount()` counts
closed step beads. `StepIndex()` returns the 1-based position of a step
by its Ref.

### Key Types

- **`formula.Formula`** (`internal/formula/formula.go`): The parsed
  formula definition. Fields: `Name string`, `Description string`,
  `Steps []Step`.

- **`formula.Step`** (`internal/formula/formula.go`): One step in a
  formula. Fields: `ID string`, `Title string`, `Description string`,
  `Needs []string`.

- **`formula.Resolver`** (`internal/formula/compose.go`): A function
  type `func(name string) (*Formula, error)` that loads a formula by
  name. Implementations read from a directory of `*.formula.toml` files.

- **`config.FormulaLayers`** (`internal/config/config.go`): Resolved
  formula directories per scope. Fields: `City []string` (city-scoped
  layers), `Rigs map[string][]string` (per-rig layers).

- **`wispGC`** (`cmd/gc/wisp_gc.go`): Interface for wisp garbage
  collection. Methods: `shouldRun(now)` and `runGC(cityPath, now)`.
  Production implementation: `memoryWispGC`.

## Invariants

- A formula must have a non-empty name and at least one step.
  (`Validate()` rejects empty name or zero steps.)

- Step IDs within a formula are unique. (`Validate()` rejects
  duplicates.)

- Every `Needs` entry in a step must reference an existing step ID
  within the same formula. (`Validate()` rejects unknown references.)

- The step dependency graph is acyclic. (`Validate()` uses Kahn's
  algorithm and rejects formulas where topological sort visits fewer
  nodes than total steps.)

- `CurrentStep()` returns nil when all steps are closed (molecule
  complete) or when all open steps have unsatisfied dependencies
  (blocked).

- A molecule root bead always has `Type="molecule"` and
  `Ref=<formula name>`. Step beads always have `Type="task"`,
  `ParentID=<root ID>`, and `Ref=<step ID>`.

- `SubstituteVars()` never modifies the original formula. It returns
  a shallow copy with substituted step descriptions, or the same
  pointer if no variables are provided.

- `ResolveFormulas()` never overwrites real files (non-symlinks) in
  the target directory. Only symlinks are created, updated, or removed.

- Wisp GC only deletes closed molecules whose `created_at` timestamp
  precedes `now - wisp_ttl`. Open or in-progress molecules are never
  garbage-collected.

## Interactions

| Depends on | How |
|---|---|
| `internal/beads` (Store) | Molecules are persisted as bead trees via `Store.Create()` and `Store.MolCook()`. Step progress is tracked by `Store.Close()`. |
| `internal/config` (FormulaLayers) | Formula resolution layers are computed during pack expansion in `ComputeFormulaLayers()`. |
| TOML parser (`github.com/BurntSushi/toml`) | `formula.Parse()` decodes formula TOML files. |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_sling.go` (Dispatch) | `gc sling --formula` instantiates wisps from formulas via `instantiateWisp()` -> `Store.MolCook()`. |
| `cmd/gc/automation_dispatch.go` (Automations) | Formula automations create wisps via `dispatchWisp()` -> `instantiateWisp()`. |
| `cmd/gc/formula_resolve.go` (Resolution) | `ResolveFormulas()` materializes formula layer winners as symlinks. |
| `cmd/gc/wisp_gc.go` (Garbage Collection) | Wisp GC purges closed molecules past TTL. |
| `cmd/gc/cmd_formula.go` (CLI) | `gc formula list` and `gc formula show` use `Parse()` and `Validate()`. |
| `internal/beads/exec` (exec Store) | `exec.Store.MolCook()` uses `formula.ComposeMolCook()` when a formula resolver is set. |
| Agent prompts | `CurrentStep()` and `CompletedCount()` are used to render molecule progress into agent prompts. |

## Code Map

| Package / File | Responsibility |
|---|---|
| `internal/formula/formula.go` | `Formula` and `Step` structs, `Parse()`, `CurrentStep()`, `CompletedCount()`, `StepIndex()` |
| `internal/formula/validate.go` | `Validate()` -- structural checks including cycle detection |
| `internal/formula/compose.go` | `Resolver` type, `SubstituteVars()`, `ComposeMolCook()` -- molecule instantiation |
| `cmd/gc/formula_resolve.go` | `ResolveFormulas()` -- symlink materialization from formula layers |
| `cmd/gc/wisp_gc.go` | `wispGC` interface, `memoryWispGC` -- TTL-based garbage collection of closed molecules |
| `cmd/gc/cmd_formula.go` | CLI commands: `gc formula list`, `gc formula show` |
| `cmd/gc/cmd_sling.go` | `instantiateWisp()` -- wisp creation during dispatch |
| `cmd/gc/automation_dispatch.go` | `dispatchWisp()` -- wisp creation from automation triggers |
| `internal/config/config.go` | `FormulaLayers` struct, `DaemonConfig.WispGCInterval`, `DaemonConfig.WispTTL` |
| `internal/config/pack.go` | `ComputeFormulaLayers()` -- builds formula layers from pack expansion |
| `internal/automations/automation.go` | `Automation` struct -- references formulas by name for gate-triggered dispatch |
| `internal/beads/beads.go` | `Store.MolCook()` interface method |
| `internal/beads/memstore.go` | `MemStore.MolCook()` -- in-memory molecule creation |
| `internal/beads/bdstore.go` | `BdStore.MolCook()` -- delegates to `bd mol cook` CLI |
| `internal/beads/exec/exec.go` | `exec.Store.MolCook()` -- composed via `ComposeMolCook()` or delegated to script |

## Configuration

### Formula file (`*.formula.toml`)

```toml
formula = "code-review"
description = "Multi-step code review workflow"

[[steps]]
id = "analyze"
title = "Analyze changes"
description = "Review the diff for {{repo}}"

[[steps]]
id = "test"
title = "Run tests"
description = "Execute test suite"
needs = ["analyze"]

[[steps]]
id = "report"
title = "Write report"
description = "Summarize findings"
needs = ["test"]
```

See [Formula TOML schema](../reference/formula.md) for the full field
reference.

### Formula resolution layers

Formula layers are ordered lowest-to-highest priority. For each
`*.formula.toml` filename, the highest-priority layer wins.

| Priority | Layer | Source |
|----------|-------|--------|
| 1 (lowest) | City pack | `formulas/` dirs from city-level packs |
| 2 | City local | `[formulas] dir` in `city.toml` |
| 3 | Rig pack | `formulas/` dirs from rig-level packs |
| 4 (highest) | Rig local | `formulas_dir` in `[[rigs]]` entry |

City-scoped agents see layers 1-2. Rig-scoped agents see layers 1-4.
`ComputeFormulaLayers()` in `internal/config/pack.go` builds these
layers during pack expansion. `ResolveFormulas()` in
`cmd/gc/formula_resolve.go` materializes the winners as symlinks.

### Wisp garbage collection (`city.toml`)

```toml
[daemon]
wisp_gc_interval = "5m"   # how often GC runs
wisp_ttl = "24h"          # how long closed molecules survive
```

Both `wisp_gc_interval` and `wisp_ttl` must be set to non-zero
durations for wisp GC to activate. See
[Config reference](../reference/config.md) for the full `[daemon]`
schema.

### Variable substitution

Variables are passed as `key=value` strings via `gc sling --var` or
programmatically through `ComposeMolCook()`. Placeholders in step
descriptions use `{{key}}` syntax and are replaced at cook time.

```sh
gc sling my-agent code-review --formula --var repo=gascity --var pr=42
```

### Automation formula dispatch

Automations reference formulas by name in their `automation.toml`:

```toml
[automation]
formula = "code-review"
gate = "cooldown"
interval = "1h"
pool = "reviewer"
```

See [Health Patrol architecture](./health-patrol.md) for gate evaluation
and dispatch mechanics.

## Testing

- **`internal/formula/formula_test.go`**: Unit tests for `Parse()`,
  `CurrentStep()`, `CompletedCount()`, and `StepIndex()`. Covers valid
  parsing, invalid TOML, dependency chain navigation, all-done and
  all-blocked states.

- **`internal/formula/validate_test.go`** (tests in
  `formula_test.go`): Validation tests for missing name, no steps,
  duplicate IDs, unknown needs references, and cycle detection.

- **`internal/formula/compose_test.go`**: Tests for `ComposeMolCook()`
  and `SubstituteVars()`. Covers root bead creation, step bead linking,
  needs propagation, variable substitution, default title fallback, and
  resolver errors.

- **`cmd/gc/cmd_formula_test.go`**: CLI tests for `gc formula list`
  (empty dir, missing dir, success with filtering) and `gc formula show`
  (success with dependency display, missing formula).

- **`cmd/gc/wisp_gc.go`**: Wisp GC tests exercise `shouldRun()`
  interval checking and `runGC()` TTL-based purging.

- **`internal/beads/memstore_test.go`**, **`internal/beads/bdstore_test.go`**,
  **`internal/beads/exec/exec_test.go`**: MolCook tests across all three
  store implementations, covering formula instantiation, title defaults,
  variable passing, empty output handling, and composed vs delegated
  modes.

- **`cmd/gc/automation_dispatch_test.go`**: Tests automation-triggered
  wisp dispatch including tracking bead creation and label stamping.

See [TESTING.md](../../TESTING.md) for overall testing philosophy and
tier boundaries.

## Known Limitations

- **No cross-formula dependencies.** Steps can only depend on other
  steps within the same formula via `Needs`. There is no mechanism for
  one formula's step to depend on another formula's completion.

- **No runtime formula modification.** Once cooked into a molecule, the
  step structure is fixed. Adding or removing steps requires creating a
  new molecule.

- **Variable substitution is string-only.** `{{key}}` placeholders are
  replaced via `strings.ReplaceAll` -- there is no type system, no
  default values, and no validation that all placeholders are satisfied.
  Unmatched placeholders remain as literal `{{key}}` text.

- **MemStore MolCook is simplified.** `MemStore.MolCook()` creates only
  the root bead (no step beads), unlike `ComposeMolCook()` which creates
  the full bead tree. This is sufficient for unit tests but does not
  exercise the full molecule lifecycle.

- **Wisp GC uses polling, not events.** The GC runs on a timer
  (`wisp_gc_interval`), not in response to molecule close events. This
  means closed molecules persist for up to `interval + ttl` before
  deletion.

## See Also

- [Bead Store architecture](./beads.md) -- how molecules are persisted
  as bead trees and how `MolCook()` is implemented across store backends
- [Config architecture](./config.md) -- pack expansion and
  `ComputeFormulaLayers()` that builds the formula resolution chain
- [Health Patrol architecture](./health-patrol.md) -- automation gate
  evaluation and dispatch mechanics that trigger formula instantiation
- [Formula TOML schema](../reference/formula.md) -- auto-generated
  field reference for `*.formula.toml` files
- [Config reference](../reference/config.md) -- `[daemon]` section
  covering `wisp_gc_interval` and `wisp_ttl` configuration
- [Glossary](./glossary.md) -- authoritative definitions of formula,
  molecule, wisp, automation, gate, and related terms
