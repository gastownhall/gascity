---
title: "Config System"
---


> Last verified against code: 2026-05-22

> **PackV2 format source of truth:** The public PackV2 format and loader
> semantics are specified in
> [Gas City Pack Specification (2.0)](../../docs/reference/specs/pack-spec.md). This
> page describes config loading and should defer to that specification for
> PackV2 file-format details.

## Summary

The Config system is a Layer 0-1 primitive that serves as Gas City's
universal activation mechanism. It loads, composes, and resolves TOML
configuration from `city.toml`, included fragments, PackV2 directories,
and materialized import metadata into a single flat `City` struct that
drives all other subsystems. Capabilities activate progressively based
on which config sections are present (Levels 0-8), and multi-layer
patch/override resolution lets city and rig policy customize imported
agents without forking their packs.

## Key Concepts

- **Progressive Activation**: Capabilities emerge from config section
  presence. An empty `city.toml` with just `[workspace]` and
  `[[agent]]` gives Level 0-1 (agent + tasks). Adding `[daemon]`
  activates health monitoring. Adding `[[rigs]]` with packs
  activates formulas and orders. No feature flags -- the config IS
  the feature flag.

- **Composition**: Multiple TOML files are merged into one `City` struct.
  The root `city.toml` declares `include` paths to fragments. Fragments
  cannot include other fragments (no recursive includes). Arrays
  concatenate, providers deep-merge per-field, workspace fields merge
  with collision warnings.

- **Pack**: A reusable configuration directory with `pack.toml` and
  well-known content directories such as `agents/`, `formulas/`,
  `commands/`, `skills/`, `assets/`, and `defaults/`. PackV2 imports
  are source-first (`source`, optional `version`) and are described in
  the public pack specification. City-owned config such as
  `[agent_defaults]`, `[defaults.rig.imports]`, and city/rig patches
  belongs in `city.toml`, not normal imported `pack.toml` files.

- **Override Resolution**: A layered chain that allows progressively
  more specific customization: builtin provider presets < city-level
  `[providers]` < workspace defaults < per-agent fields. City-level
  `[patches]` and rig-scoped `[[overrides]]` provide additional
  modification points for imported agents without forking the pack.

- **Provenance**: Every config element (agent, rig, workspace field) is
  tracked back to the source file that defined it. Built into the
  composition API from the start -- enables `gc config show` diagnostics
  and collision warnings.

- **Revision**: A deterministic SHA-256 hash computed from all config
  source file contents plus pack directory contents. The controller
  uses revision changes to detect when a config reload is needed.

- **FormulaLayers**: Ordered formula directory lists per scope
  (city-scoped and per-rig) that control formula symlink
  materialization. Pack and city formulas use the well-known
  `formulas/` directory; `rigs[].formulas_dir` remains the rig-local
  escape hatch. Higher-priority layers shadow lower ones by filename.

## Architecture

The config system is implemented entirely in `internal/config/`. It has
no upward dependencies -- every other Gas City subsystem depends on
config, but config depends only on `internal/fsys` (filesystem
abstraction) and `github.com/BurntSushi/toml`.

### Data Flow

The primary entry point is `LoadWithIncludes`, which performs the
complete config resolution pipeline:

```
city.toml
    |
    v
1. Parse root city.toml     (parseWithMeta)
    |
    v
2. Read city defaults       ([defaults.rig.imports])
    |
    v
3. Load root pack layer     (pack.toml, if present)
    |
    v
4. Load & merge fragments   (mergeFragment for each include)
    |
    v
5. Expand city imports      (ExpandCityPacks / PackV2 imports)
    |
    v
6. Expand rig imports       (ExpandPacks, defer rig overrides)
    |
    v
7. Apply city patches       (ApplyPatches)
    |
    v
8. Apply rig overrides       (deferred per-rig overrides)
    |
    v
9. Apply pack globals        (commands, skills, MCP config, defaults)
    |
    v
10. Compute formula layers   (ComputeFormulaLayers)
    |
    v
11. Inject implicit agents
    |
    v
12. Apply agent defaults
    |
    v
Flat City struct + Provenance
```

City imports and rig imports are both expanded before city-level
patches are applied, so patches can target imported agents in either
scope. Rig overrides are deferred until after city-level patches so
rig-local policy wins when both set the same field. Pack-level agent
patches are applied inside recursive pack loading and do not expose
city-only patch targets such as rigs or providers.

Provider resolution happens later, at agent startup time, via
`ResolveProvider`:

```
1. agent.StartCommand set?     -> escape hatch, use directly
2. Determine provider name:    agent.Provider > workspace.Provider > auto-detect
3. Look up ProviderSpec:       cityProviders[name] > BuiltinProviders()[name]
4. Merge agent-level overrides (non-zero fields replace; env merges additively)
5. Default prompt_mode to "arg"
```

### Key Types

- **`City`** (`internal/config/config.go`): Top-level config struct.
  Contains Workspace, Agents, Rigs, Providers, Packs, Patches,
  FormulaLayers, and subsystem configs (Beads, Session, Mail, Events,
  Daemon, Formulas, Orders). The single struct that all subsystems
  read from after loading.

- **`Agent`** (`internal/config/config.go`): Defines a configured agent.
  Fields cover identity (Name, Dir), lifecycle (Suspended, PreStart,
  SessionSetup), provider selection (Provider, StartCommand), prompt
  delivery (PromptTemplate, Nudge, PromptMode), scaling (Pool), work
  routing (WorkQuery, SlingQuery), and hooks (InstallAgentHooks,
  HooksInstalled).

- **`AgentPatch`** (`internal/config/patch.go`): Targets an existing
  agent by (Dir, Name) for field-level modification after composition.
  Uses pointer fields to distinguish "not set" from "set to zero value."

- **`AgentOverride`** (`internal/config/config.go`): Modifies a
  pack-stamped agent for a specific rig. Same pointer-field
  semantics as AgentPatch. Applied during `ExpandPacks`.

- **`ProviderSpec`** (`internal/config/provider.go`): Defines a named
  provider's startup parameters (Command, Args, PromptMode, Env, etc.).
  Built-in presets exist for claude, codex, gemini, cursor, copilot,
  amp, and opencode.

- **`ResolvedProvider`** (`internal/config/provider.go`): The
  fully-merged, ready-to-use provider config produced by
  `ResolveProvider`. All fields populated after resolution through the
  builtin + city + agent override chain.

- **`ImportSpec` / `PackSource`** (`internal/config/config.go`):
  `ImportSpec` is the authored PackV2 import shape (`source`, optional
  `version`). `PackSource` remains as legacy/internal plumbing for
  cached or older pack references. Registry handles such as
  `main:gascity` are command-time lookup handles and should not be
  persisted in authored `pack.toml`.

- **`PackMeta`** (`internal/config/config.go`): Metadata header from
  `pack.toml`. Contains name, version, schema version, optional
  `requires_gc` constraint, and import/export metadata. Agent
  city-vs-rig availability is declared on agent definitions rather
  than partitioned through legacy `city_agents` metadata.

- **`Provenance`** (`internal/config/compose.go`): Tracks the source
  file origin of every agent, rig, and workspace field. Built during
  `LoadWithIncludes` and used for diagnostics and collision detection.

- **`FormulaLayers`** (`internal/config/config.go`): Holds resolved
  formula directory stacks for city-scoped agents and per-rig agents.
  Priority order (lowest to highest): imported/root pack formulas <
  city-local `formulas/` < rig-imported pack formulas <
  `rigs[].formulas_dir`.

## Invariants

- **Agent identity uniqueness.** No two agents in the resolved config
  may share the same (Dir, Name) pair. `ValidateAgents` enforces this.
  When duplicates arise from pack expansion, provenance (SourceDir)
  is included in the error.

- **Rig prefix uniqueness.** No two rigs may produce the same bead ID
  prefix. The HQ prefix (derived from city name) also participates in
  collision detection. `ValidateRigs` enforces this.

- **No recursive includes.** If a fragment's `include` array is
  non-empty, `LoadWithIncludes` returns an error. Composition is
  exactly one level deep.

- **Patches target existing resources.** If an `AgentPatch` references
  an agent that does not exist in the merged config, `ApplyPatches`
  returns an error. Patches never create new resources.

- **Pack schema compatibility.** `loadPack` rejects any
  pack with `schema` > `currentPackSchema` (currently 2).
  Forward-incompatible packs fail loudly.

- **Imported pack files have a narrower authoring surface.** Normal
  PackV2 `pack.toml` files may define source-first imports and pack
  content, but city-owned constructs such as `[agent_defaults]`,
  `[defaults.rig.imports]`, `[formulas].dir`, `[[patches.rigs]]`, and
  `[[patches.providers]]` are rejected during pack loading.

- **Pool query symmetry.** Pool agents must set both `sling_query` and
  `work_query`, or neither. `ValidateAgents` rejects mismatched pairs.

- **Field sync across Agent, AgentPatch, AgentOverride.** Every
  overridable field on `Agent` must also appear on `AgentPatch` and
  `AgentOverride`. `TestAgentFieldSync` enforces this at the struct
  level via reflection. The corresponding apply functions
  (`applyAgentPatchFields`, `applyAgentOverride`) and the `poolAgents`
  deep-copy in `cmd/gc/pool.go` must be checked manually when adding
  fields.

- **Revision determinism.** Given identical file contents, `Revision`
  always produces the same SHA-256 hash. Source paths are sorted before
  hashing, and pack content is hashed recursively with sorted
  relative paths.

- **Provider resolution is side-effect-free.** `ResolveProvider` only
  reads config and probes PATH (via `lookPath`). It never modifies the
  `City` struct or writes to disk.

## Interactions

| Depends on | How |
|---|---|
| `internal/fsys` | Filesystem abstraction for `Load`, `LoadWithIncludes`, pack loading, and revision hashing |
| `github.com/BurntSushi/toml` | TOML parsing and encoding for all config files |

| Depended on by | How |
|---|---|
| `cmd/gc/controller.go` | Loads config via `LoadWithIncludes`, watches for changes via `WatchDirs`, detects reloads via `Revision` |
| `cmd/gc/pool.go` | Reads `Agent.Pool` for scaling; deep-copies agent fields when spawning pool instances |
| `cmd/gc/reconciler.go` | Reads resolved agent list and rig list to start/stop agents |
| `internal/city/` | Uses `Load` for basic config operations (init, add rig) |
| `internal/hooks/` | Reads agent config for hook installation decisions via `ResolveInstallHooks` |
| `internal/runtime/` | Receives `ResolvedProvider` output to determine runtime startup parameters |
| `internal/orders/` | Reads `OrdersConfig` skip list and formula layers |
| `cmd/gc/formula_resolve.go` | Uses `FormulaLayers` to resolve formula directory symlinks |
| `cmd/gc/cmd_sling.go` | Reads `Agent.EffectiveSlingQuery` for bead routing |

## Code Map

All implementation lives in `internal/config/`:

| File | Purpose |
|---|---|
| `internal/config/config.go` | Core types: `City`, `Workspace`, `Agent`, `Rig`, `AgentOverride`, `ImportSpec`, `PackSource`, `PackMeta`, `FormulaLayers`, `PoolConfig`, subsystem configs. Load/Parse/Marshal. Validation functions. |
| `internal/config/compose.go` | `LoadWithIncludes`: the main entry point. Fragment merging, path resolution, provenance tracking. Orchestrates the full load pipeline. |
| `internal/config/patch.go` | `Patches`, `AgentPatch`, `RigPatch`, `ProviderPatch`, `PoolOverride` types. `ApplyPatches` and per-type apply functions. |
| `internal/config/pack.go` | `ExpandPacks`, `ExpandCityPacks`, `ComputeFormulaLayers`. Pack loading, agent stamping, scope handling, override application, collision detection. |
| `internal/config/pack_fetch.go` | Legacy V1 remote-pack fetch and lock helpers. Schema-2 import bootstrap/repair belongs to `gc import`; config load consumes already-materialized imports. |
| `internal/config/provider.go` | `ProviderSpec`, `ResolvedProvider`, `BuiltinProviders`. Built-in provider presets for seven CLI agents. |
| `internal/config/resolve.go` | `ResolveProvider`: the five-step provider resolution chain. `AgentHasHooks` for hook detection. Auto-detection via PATH scanning. |
| `internal/config/revision.go` | `Revision`: deterministic SHA-256 config hashing. `WatchDirs`: filesystem watch targets for config change detection. |
| `internal/config/field_sync_test.go` | `TestAgentFieldSync`: reflection-based enforcement that Agent, AgentPatch, and AgentOverride stay in sync. |

## Configuration

The config system is self-describing -- it IS the configuration. The
root file is always `city.toml` at the city directory root.

Minimal example (Level 0-1):

```toml
[workspace]
name = "my-city"

[[agent]]
name = "worker"
prompt_template = "prompts/worker.md"
```

Multi-rig with pack and overrides (Level 5+):

```toml
[workspace]
name = "my-city"
provider = "claude"

[imports.shared]
source = "https://github.com/example/packs/my-pack"
version = "^1"

[[rigs]]
name = "project-a"
path = "/home/user/project-a"

[rigs.imports.shared]
source = "https://github.com/example/packs/my-pack"
version = "^1"

[[rigs.overrides]]
agent = "worker"
suspended = true

[patches.agent]
dir = ""
name = "overseer"
idle_timeout = "30m"
```

Fragment composition:

```toml
# city.toml
include = ["rigs/extra-rigs.toml", "env/prod.toml"]

[workspace]
name = "my-city"
```

FormulaLayers priority (lowest to highest):

1. Imported/root pack formulas from well-known `formulas/` directories
2. City local formulas from the city root `formulas/` directory
3. Rig-imported pack formulas from well-known `formulas/` directories
4. Rig local formulas from `rigs[].formulas_dir`

## Testing

Each source file has a companion `_test.go`:

| Test file | Coverage |
|---|---|
| `internal/config/config_test.go` | Parse, Marshal, Load, DefaultCity, ValidateAgents, ValidateRigs, DeriveBeadsPrefix, QualifiedName |
| `internal/config/compose_test.go` | LoadWithIncludes, fragment merging, collision warnings, path resolution, provenance tracking, recursive include rejection |
| `internal/config/patch_test.go` | ApplyPatches for agents/rigs/providers, targeting errors, env merge/remove, pool sub-field patching, provider replace mode |
| `internal/config/pack_test.go` | ExpandPacks, ExpandCityPacks, agent scope handling, agent collision detection, override application, formula layer computation |
| `internal/config/pack_fetch_test.go` | Legacy fetch/lock helper coverage for the V1 `[packs]` path |
| `internal/config/provider_test.go` | BuiltinProviders completeness, BuiltinProviderOrder coverage |
| `internal/config/resolve_test.go` | ResolveProvider chain (all five steps), escape hatches, auto-detect, agent-level overrides, env additive merge |
| `internal/config/revision_test.go` | Revision determinism, WatchDirs deduplication |
| `internal/config/field_sync_test.go` | TestAgentFieldSync: reflection-based struct field parity between Agent, AgentPatch, AgentOverride |

All tests are unit tests using `t.TempDir()` and `fsys.MemFS` (no
integration tags needed). See [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) for
overall testing philosophy.

## Known Limitations

- **No config validation beyond structural checks.** The config system
  validates field presence, uniqueness, and pool bounds, but does not
  verify that referenced paths (prompt_template, overlay_dir) actually
  exist on disk. Path existence is checked at agent startup time.

- **Schema-2 import repair is out of band.** PackV2 load/start/config
  flows do not fetch imports. Missing import state must be repaired with
  `gc import install`.

- **Legacy V1 remote fetch requires git.** The older `FetchPacks` path
  shells out to `git clone`/`git fetch`. There is no fallback for
  environments without git.

- **No hot-reload of pack content.** The controller watches config
  source files and reloads on change, but pack directories are only
  re-hashed during revision computation. Changes to files within a
  pack directory are detected, but new files added outside the
  watched directories require a manual reload.

- **`applyAgentPatchFields` and `applyAgentOverride` must be manually
  synced.** `TestAgentFieldSync` enforces struct-level field parity via
  reflection, but the apply functions and `poolAgents` deep-copy in
  `cmd/gc/pool.go` cannot be checked automatically. Adding a field to
  `Agent` requires manual updates to all three locations.

## See Also

- [Glossary](glossary.md) -- authoritative definitions of all Gas City
  terms, including Config, Pack, Rig, and Provider
- [CLAUDE.md](https://github.com/gastownhall/gascity/blob/main/CLAUDE.md) -- progressive capability model (Levels
  0-8), design principles (ZFC, Bitter Lesson), and the "Adding agent
  config fields" convention
- [TESTING.md](https://github.com/gastownhall/gascity/blob/main/TESTING.md) -- testing philosophy and tier
  boundaries for config tests
