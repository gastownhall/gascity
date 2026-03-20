---
title: "Formula Migration"
---

## Context

Gas City's mechanism #7 (Formulas & Molecules) is currently split across two
repositories. The formula compilation engine lives in `beads/internal/formula/`
(~3,900 lines, 8 source files), while Gas City shells out to the `bd` CLI for
formula instantiation (`bd mol wisp`, `bd mol bond`). This creates an
unnecessary runtime dependency on the `bd` binary for what is architecturally a
Gas City concern.

The formula package in beads has **zero imports from any beads package** -- it is
a self-contained compilation pipeline that transforms TOML configuration into
step definitions. It belongs at Gas City's Layer 2-4 (derived mechanisms), not
in beads' Layer 0-1 (task store primitive).

### Current flow (bd shell-out)

```
gc sling --formula mol-X
  -> BdStore.MolCook("mol-X", ...)
    -> exec: bd mol wisp mol-X --json
      -> formula.Parse + Resolve + ApplyControlFlow + ...
      -> cookFormulaToSubgraph (steps -> issues)
      -> spawnMoleculeWithOptions (issues -> dolt store)
    <- JSON: {new_epic_id: "..."}
  <- root bead ID
```

### Target flow (native)

```
gc sling --formula mol-X
  -> formula.Compile(ctx, "mol-X", searchPaths, vars)
    -> Parse + Resolve + ApplyControlFlow + ApplyAdvice + ...
  <- *formula.Recipe (compiled steps with {{vars}} intact)
  -> molecule.Cook(ctx, store, recipe, opts)
    -> Substitute(vars) in titles/descriptions
    -> store.Create(root bead)
    -> for each step: store.Create(step bead) + store.DepAdd(...)
  <- molecule.Result{RootID, IDMapping}
```

## Goals

1. Gas City can compile formulas and instantiate molecules without `bd`.
2. Correct architectural layering: formula compilation (Layer 2-4) depends on
   Store (Layer 0-1), not the other way around.
3. MolCook/MolCookOn removed from the `beads.Store` interface -- Store is CRUD,
   not compilation.
4. All existing callers (sling, orders, convergence, API) use the native path.
5. All formula tests from beads pass in Gas City.
6. No behavioral changes from the user's perspective -- `gc sling --formula`
   works identically.

## Non-Goals

- Porting `bd cook --persist` (database-backed proto beads). Gas City uses
  ephemeral in-memory compilation exclusively.
- Porting `bd mol pour` as a standalone CLI command. Instantiation happens via
  `gc sling`, not a separate command.
- Porting `bd mol bond` as a standalone CLI command. Bonding is handled via
  `gc sling --on`.
- Changing the `.formula.toml` file format. Full backward compatibility.
- Porting `bd mol squash`, `bd mol burn`, `bd mol distill`, or other molecule
  lifecycle commands. Those are beads-specific.

## Ownership

Gas City becomes the authoritative owner of the formula compilation engine.
The zero-import property makes this a clean separation:

- **Gas City owns:** `internal/formula/` (compilation) + `internal/molecule/`
  (instantiation). All future formula features land here first.
- **Beads retains:** `internal/formula/` as a frozen copy for backward
  compatibility with `bd cook`/`bd mol wisp`. No new features.
- **Sync strategy:** None. This is a deliberate fork, not a shared module.
  Beads' formula package served its purpose as a prototype. Gas City's copy
  is the production implementation. Beads can deprecate its copy at its own
  pace.
- **Why not a shared Go module:** The formula package will diverge as Gas City
  adds Recipe types, `context.Context` support, and Gas City-specific
  compilation stages. A shared module would couple two projects with different
  release cadences for no benefit -- the package is small enough that
  independent ownership is simpler than coordinated releases.

## Architecture

### New packages

```
internal/
  formula/           # NEW -- ported from beads/internal/formula/
    types.go         # Formula, Step, ComposeRules, VarDef, Gate, ...
    parser.go        # Parser: TOML/JSON loading, inheritance, caching
    condition.go     # Runtime condition evaluation (gates)
    stepcondition.go # Compile-time step filtering
    controlflow.go   # Loop expansion, branch wiring, gate application
    expand.go        # Expansion template application
    range.go         # Range expression parsing
    advice.go        # Aspect advice operators
    compile.go       # NEW -- top-level Compile() entry point
    recipe.go        # NEW -- Recipe type (compiled output)

  molecule/          # NEW -- instantiation layer
    molecule.go      # Cook/CookOn convenience API + Result type
    instantiate.go   # core instantiation logic
```

### Package layering and import constraints

```
Layer 4  cmd/gc/         imports: formula, molecule, beads, config
Layer 3  molecule/       imports: formula, beads
Layer 2  formula/        imports: (stdlib + BurntSushi/toml only)
Layer 1  beads/          imports: (stdlib only)
Layer 0  config/         imports: (stdlib + BurntSushi/toml only)
```

**Invariants:**
- formula/ NEVER imports molecule/, beads/, or config/
- molecule/ NEVER imports cmd/gc/ or config/
- beads/ NEVER imports formula/ or molecule/

### Key types

```go
// formula/recipe.go -- output of compilation
type Recipe struct {
    Name        string
    Description string
    Steps       []RecipeStep            // flattened, ordered (root is Steps[0])
    Deps        []RecipeDep             // all dependency edges
    Vars        map[string]*VarDef      // variable definitions (for default handling)
    Phase       string                  // "vapor" or "liquid"
    Pour        bool                    // formula recommends full materialization
    RootOnly    bool                    // true for patrol wisps (root only, no children)
}

type RecipeStep struct {
    ID          string                  // namespaced: "formula-name.step-id"
    Title       string                  // may contain {{variables}}
    Description string
    Notes       string
    Type        string                  // "task", "bug", "epic", "gate", etc.
    Priority    *int
    Labels      []string
    Assignee    string
    IsRoot      bool                    // true for the root epic
    Gate        *RecipeGate             // async gate spec (if step has a gate)
}

type RecipeGate struct {
    Type    string                      // "all-children", "any-children", etc.
    ID      string
    Timeout string
}

type RecipeDep struct {
    StepID      string
    DependsOnID string
    Type        string                  // "blocks", "parent-child", "waits-for"
    Metadata    string                  // JSON for waits-for gate metadata
}
```

```go
// formula/compile.go -- top-level entry point
// Compile loads a formula by name and runs the full compilation pipeline.
// The returned Recipe contains {{variable}} placeholders -- substitution
// happens at instantiation time, not compilation time.
// vars is used only for compile-time step condition filtering (steps with
// conditions that evaluate to false are excluded).
func Compile(ctx context.Context, name string, searchPaths []string, vars map[string]string) (*Recipe, error)
```

The compilation pipeline has 9 stages (matching beads' resolveAndCookFormulaWithVars):

1. `parser.LoadByName(name)` -- load formula TOML from search paths
2. `parser.Resolve(f)` -- resolve inheritance (`extends` chains)
3. `ApplyControlFlow(steps, compose)` -- loops, branches, gates
4. `ApplyAdvice(steps, advice)` -- inline advice rules
5. `ApplyInlineExpansions(steps, parser)` -- step-level `expand` field
6. `ApplyExpansions(steps, compose, parser)` -- compose.expand/map operators
7. Aspect loading + `ApplyAdvice` for each `compose.aspects` entry
8. `FilterStepsByCondition(steps, vars)` -- compile-time step filtering
9. `MaterializeExpansion` -- standalone expansion formula handling
10. `toRecipe(resolved)` -- flatten step tree to Recipe with namespaced IDs,
    gate siblings, and type promotions (epic for steps with children)

```go
// molecule/molecule.go -- convenience API
type Options struct {
    Title          string              // override root bead title (optional)
    Vars           map[string]string   // variable substitution values
    ParentID       string              // attach to existing bead (for CookOn)
    IdempotencyKey string              // set on root bead metadata (for convergence)
}

type Result struct {
    RootID    string
    IDMapping map[string]string        // recipe step ID -> bead ID
    Created   int
}

// Cook compiles a formula and instantiates it as a molecule in one step.
// This is the convenience wrapper that most callers should use.
func Cook(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error)

// CookOn compiles a formula and attaches it to an existing bead.
func CookOn(ctx context.Context, store beads.Store, formulaName string, searchPaths []string, opts Options) (*Result, error)

// Instantiate creates beads from a pre-compiled Recipe.
// Use this when you need to inspect/modify the Recipe before instantiation.
func Instantiate(ctx context.Context, store beads.Store, recipe *formula.Recipe, opts Options) (*Result, error)
```

### Store interface changes

Remove from `beads.Store`:
```go
// REMOVED
MolCook(formula, title string, vars []string) (string, error)
MolCookOn(formula, beadID, title string, vars []string) (string, error)
```

These are replaced by `molecule.Cook()` / `molecule.CookOn()` /
`molecule.Instantiate()`, which compose `Store.Create()`, `Store.DepAdd()`,
and `Store.SetMetadata()`.

**exec.Store migration:** Per the shipped Option B decision in
`docs/reference/exec-beads-provider.md`, MolCook is a mechanism (Layer 2)
composed from CRUD primitives. The exec store's script only needs Create,
Update, DepAdd -- molecule.Instantiate composes these. No
`MoleculeInstantiator` interface is needed. The `mol-cook` script operation
is deprecated; existing scripts that implement it continue to work during
the transition via the `bd` fallback toggle, then are removed.

### Partial failure semantics

`molecule.Instantiate` calls `Store.Create` N+1 times then `Store.DepAdd` M
times. A failure mid-way leaves orphaned beads. Policy:

- **Best-effort cleanup on failure.** If Create fails on step K, close beads
  0..K-1 with a `molecule_failed` metadata flag. Callers can detect and clean
  up orphans.
- **Idempotency key set atomically with root creation.** For convergence,
  `Options.IdempotencyKey` is set as metadata on the root bead during
  `Store.Create` (via `Bead.Metadata`), not as a separate `SetMetadata` call.
  This narrows the crash window to zero for the idempotency check.
- **Fault-injection tests required.** Test Create-fails-on-Nth-step,
  DepAdd-fails-after-creates, and metadata-set-failure scenarios.

## Complete caller inventory

9 production call sites + 1 convergence adapter:

| # | File | Line | Method | Title Used? | Error Strategy |
|---|------|------|--------|-------------|----------------|
| 1 | `cmd/gc/cmd_sling.go` | 422 | `MolCook` | `opts.Title` | exit 1 |
| 2 | `cmd/gc/cmd_sling.go` | 438 | `MolCookOn` | `opts.Title` | exit 1 |
| 3 | `cmd/gc/cmd_sling.go` | 461 | `MolCookOn` | `opts.Title` | exit 1 |
| 4 | `cmd/gc/cmd_sling.go` | 657 | `MolCookOn` | `opts.Title` | exit 1 (batch) |
| 5 | `cmd/gc/cmd_sling.go` | 669 | `MolCookOn` | `opts.Title` | exit 1 (batch) |
| 6 | `cmd/gc/cmd_order.go` | 440 | `MolCook` | `""` | event + continue |
| 7 | `cmd/gc/order_dispatch.go` | 236 | `MolCook` | `""` | event + continue |
| 8 | `internal/api/handler_sling.go` | 72 | `MolCook` | `body.Formula` | HTTP 500 |
| 9 | `cmd/gc/convergence_store.go` | 156 | `MolCookOn` | `""` | sling failure handler |

Plus test doubles: `cmd/gc/cmd_sling_test.go` (5 refs), `internal/beads/bdstore_test.go` (8 refs),
`internal/beads/exec/exec_test.go` (3 refs), `internal/beads/memstore_test.go` (2 refs).

### Per-caller migration pattern

**Group A (sling CLI, sites 1-5):** Replace with `molecule.Cook`/`CookOn`.
Search paths from `slingDeps.Cfg` via `FormulaLayers.SearchPaths(rig)`.
Title via `Options.Title`. Error handling: unchanged (exit 1).

**Group B (orders, sites 6-7):** Replace with `molecule.Cook`. Search paths
from order's `FormulaLayer`. No title. Error handling: record event, continue.

**Group C (API handler, site 8):** Replace with `molecule.Cook`. Search paths
from API context config. Title from request body. Error handling: HTTP 500.

**Group D (convergence, site 9):** Replace with `molecule.CookOn`. Search
paths from city config. IdempotencyKey via `Options.IdempotencyKey`. Error
handling: sling failure handler (convergence handles retries).

### Search path helper

Add to `internal/config/`:

```go
// SearchPaths returns the ordered formula search directories for a rig.
// Falls back to city-level layers if no rig-specific layers exist.
func (fl *FormulaLayers) SearchPaths(rigName string) []string
```

### Variable handling

Variables appear at two stages:

- **Compile time:** `vars` are used only for `FilterStepsByCondition` (step
  `condition` field evaluation). The Recipe preserves `{{placeholders}}`.
- **Instantiate time:** `Options.Vars` are substituted into titles,
  descriptions, and notes. Defaults from `Recipe.Vars` are applied first.

This matches beads' behavior where substitution happens at pour/wisp time.

The `[]string{"k=v"}` format is replaced with `map[string]string` throughout.
Duplicate keys: last-one-wins (matching Go map semantics). `buildSlingFormulaVars`
updated to return `map[string]string`.

## Migration phases

### Phase 1: Port formula package (PR 1)

Copy `beads/internal/formula/*.go` (source + tests) into
`gascity/internal/formula/`. Adjust:

- Package import paths (no external changes needed -- zero beads deps)
- `github.com/BurntSushi/toml` is already in Gas City's go.mod
- Verify all tests pass with `go test ./internal/formula/...`

Add `compile.go` and `recipe.go` with `Compile()` entry point and Recipe types.

**Files:** 8 source + 7 test files ported. 2 new files added. ~8,000 lines total.

**Risk:** Low. Port is mechanical. New files wrap existing functions.

### Phase 2: Create molecule package (PR 2, additive)

Create `internal/molecule/` with `Cook`, `CookOn`, and `Instantiate`.
Add `config.FormulaLayers.SearchPaths()` helper.

This phase is purely additive -- no existing code changes. New code can be
tested in isolation with MemStore.

**Tests:**
- Happy path: compile + instantiate simple formula
- Variable substitution in titles/descriptions
- Dependency wiring (needs, depends_on, parent-child)
- Nested children (epic with sub-steps)
- Gate step synthesis
- RootOnly mode (patrol wisps)
- Fault injection: Create-fails-on-Nth-step, DepAdd-fails-after-creates
- IdempotencyKey set atomically with root

**Risk:** Medium. New code, needs thorough testing.

### Phase 3: Switch callers with rollback toggle (PR 3)

Migrate all 9 call sites + test doubles to use `molecule.Cook`/`CookOn`.
Add `GC_NATIVE_FORMULA` environment variable toggle:

- `GC_NATIVE_FORMULA=true` (default): use native compilation
- `GC_NATIVE_FORMULA=false`: fall back to `Store.MolCook` (bd shell-out)

This allows instant rollback if native instantiation has behavioral divergence.

Update `buildSlingFormulaVars` to return `map[string]string`.
Update test doubles to use molecule package or accept compiled Recipes.

**Risk:** Medium. Many call sites, but each follows a group pattern.

### Phase 4: Remove MolCook from Store interface (PR 4, after bake period)

After Phase 3 has been running in production with `GC_NATIVE_FORMULA=true`:

- Remove `MolCook` and `MolCookOn` from `beads.Store` interface
- Remove implementations from BdStore, MemStore, FileStore, exec.Store
- Remove `GC_NATIVE_FORMULA` toggle
- Remove `mol-cook`/`mol-cook-on` from exec.Store script operations
- Update exec-beads-provider.md to remove mol-cook from the wire protocol

**Risk:** Medium. Interface change, but all callers already migrated.

### Phase 5: CLI commands (optional, low priority)

Add formula inspection commands to `gc`:

```
gc formula list                    # list available formulas
gc formula show <name>             # preview compiled recipe
gc formula show <name> --var k=v   # preview with variable substitution
```

## Testing strategy

### Golden fixture tests (mandatory, CI gate)

Generate reference output from `bd mol wisp` for a corpus of 10 formulas:

1. Simple (2 steps, no deps)
2. Variables (required + defaults)
3. Dependencies (needs, depends_on)
4. Nested children (3 levels)
5. Loops (fixed count)
6. Conditions (step filtering)
7. Gates (async coordination)
8. Advice/aspects (before/after)
9. Expansions (inline + compose)
10. Inheritance (extends chain)

Check golden outputs into `internal/formula/testdata/golden/`. CI compares
`formula.Compile()` output against golden fixtures. No runtime `bd` dependency.

### Unit tests (ported)

All 7 test files from `beads/internal/formula/` port directly.

### Unit tests (new)

- `internal/molecule/instantiate_test.go` with MemStore
- `internal/formula/compile_test.go` for the Compile entry point
- Fault injection tests for partial failure scenarios

### Integration tests

- Sling with `--formula` flag using MemStore
- Order dispatch with formula-based orders
- Convergence loop iteration with native CookOn
- exec.Store with CRUD-only script (no mol-cook)

### Test double migration

Existing test doubles that implement MolCook:
- `errStore`, `selectiveErrStore`, `recordingStore` in cmd_sling_test.go
- These are updated to compose molecule.Instantiate over their base Store,
  or to inject pre-compiled Recipes via a test helper.

## Risks and mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| Formula behavior divergence | High | Golden fixture tests as CI gate. Port ALL tests. |
| Partial instantiation failure | High | Best-effort cleanup + idempotency key in Options. Fault tests. |
| Caller migration regression | Medium | GC_NATIVE_FORMULA toggle for instant rollback. |
| Store.Create ID assignment | Medium | Molecule package uses server-assigned IDs. Never assumes format. |
| Variable format change | Medium | Isolated in buildSlingFormulaVars update. Map semantics documented. |
| exec.Store mol-cook deprecation | Low | CRUD-only path per Option B. Toggle provides transition period. |
| Cross-repo drift | Low | Deliberate fork with Gas City as sole owner. Beads copy frozen. |

## Migration order and dependencies

```
Phase 1 (formula port + compile) --- no deps, start immediately
     |
Phase 2 (molecule package) --- depends on Phase 1, additive
     |
Phase 3 (switch callers + toggle) --- depends on Phase 2
     |                                 minimum bake period before Phase 4
Phase 4 (remove MolCook) --- depends on Phase 3 bake
     |
Phase 5 (CLI commands) --- depends on Phase 1, independent of 2-4
```

Each phase is a separate PR. Each PR leaves main in a working state.
Phase 4 requires a minimum bake period after Phase 3 to validate parity.
