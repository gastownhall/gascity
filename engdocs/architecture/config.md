---
title: "Config System"
---


> Last verified against code: 2026-03-01

## Summary

The Config system is a Layer 0-1 primitive that serves as Gas City's
universal activation mechanism. It loads, composes, and resolves TOML
configuration from `city.toml`, included fragments, pack directories,
and remote pack sources into a single flat `City` struct that drives
all other subsystems. Capabilities activate progressively based on which
config sections are present (Levels 0-8), and multi-layer override
resolution ensures that pack defaults can be customized per-rig
without forking.

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

- **Pack**: A reusable agent configuration directory containing
  `pack.toml`, prompts, formulas, and orders. City-level
  packs stamp city-scoped agents (dir=""). Rig-level packs
  stamp rig-scoped agents (dir=rig-name). The `city_agents` metadata
  field partitions which agents from a shared pack are city-scoped
  vs rig-scoped.

- **Override Resolution**: A four-layer chain that allows progressively
  more specific customization: builtin provider presets < city-level
  `[providers]` < workspace defaults < per-agent fields. For pack
  agents, rig-level `[[overrides]]` and city-level `[patches]` provide
  additional override points without forking the pack.

- **Provenance**: Every config element (agent, rig, workspace field) is
  tracked back to the source file that defined it. Built into the
  composition API from the start -- enables `gc config show` diagnostics
  and collision warnings.

- **Revision**: A deterministic SHA-256 hash computed from all config
  source file contents plus pack directory contents. The controller
  uses revision changes to detect when a config reload is needed.

- **FormulaLayers**: Ordered formula directory lists per scope
  (city-scoped and per-rig) that control formula symlink
  materialization. Higher-priority layers shadow lower ones by filename.

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
1. Parse root TOML          (parseWithMeta)
    |
    v
2. Load & merge fragments    (mergeFragment for each include)
    |
    v
3. Resolve named packs  (resolveNamedPacks: name -> cache path)
    |
    v
4. Expand city packs    (ExpandCityPacks: stamp dir="" agents)
    |
    v
5. Apply patches             (ApplyPatches: targeted field modifications)
    |
    v
6. Expand rig packs     (ExpandPacks: stamp dir=rig-name agents)
    |
    v
7. Compute formula layers    (ComputeFormulaLayers: build priority stacks)
    |
    v
Flat City struct + Provenance
```

Steps 4 and 6 are ordered deliberately: city packs expand before
patches so that patches can target city-pack agents. Rig packs
expand after patches so that rig-level overrides apply to the final
stamped agents.

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

- **`PackSource`** (`internal/config/config.go`): Defines a remote
  pack repository with git URL, ref, and optional subdirectory path.
  Referenced by name in workspace/rig pack fields.

- **`PackMeta`** (`internal/config/config.go`): Metadata header from
  `pack.toml`. Contains name, version, schema version, optional
  `requires_gc` constraint, and `city_agents` list for partitioning
  agents between city and rig scopes.

- **`Provenance`** (`internal/config/compose.go`): Tracks the source
  file origin of every agent, rig, and workspace field. Built during
  `LoadWithIncludes` and used for diagnostics and collision detection.

- **`FormulaLayers`** (`internal/config/config.go`): Holds resolved
  formula directory stacks for city-scoped agents and per-rig agents.
  Priority order (lowest to highest): city-pack < city-local <
  rig-pack < rig-local.

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
  pack with `schema` > `currentPackSchema` (currently 1).
  Forward-incompatible packs fail loudly.

- **city_agents names must exist.** Every name listed in a pack's
  `city_agents` must match an agent defined in that pack.
  `loadPack` validates this before any agent stamping.

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
| `internal/config/config.go` | Core types: `City`, `Workspace`, `Agent`, `Rig`, `AgentOverride`, `PackSource`, `PackMeta`, `FormulaLayers`, `PoolConfig`, subsystem configs. Load/Parse/Marshal. Validation functions. |
| `internal/config/compose.go` | `LoadWithIncludes`: the main entry point. Fragment merging, path resolution, provenance tracking. Orchestrates the full load pipeline. |
| `internal/config/patch.go` | `Patches`, `AgentPatch`, `RigPatch`, `ProviderPatch`, `PoolOverride` types. `ApplyPatches` and per-type apply functions. |
| `internal/config/pack.go` | `ExpandPacks`, `ExpandCityPacks`, `ComputeFormulaLayers`. Pack loading, agent stamping, city_agents partitioning, override application, collision detection. |
| `internal/config/pack_fetch.go` | `FetchPacks`: git clone/update for remote pack sources. `PackLock` for reproducible builds. Cache management under `.gc/packs/`. |
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
pack = "packs/my-pack"

[packs.shared]
source = "https://github.com/example/packs.git"
ref = "v1.0"
path = "my-pack"

[[rigs]]
name = "project-a"
path = "/home/user/project-a"
pack = "shared"

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

1. City pack formulas (from `workspace.pack` or `workspace.packs`)
2. City local formulas (from `[formulas] dir`)
3. Rig pack formulas (from `rigs[].pack` or `rigs[].packs`)
4. Rig local formulas (from `rigs[].formulas_dir`)

## Testing

Each source file has a companion `_test.go`:

| Test file | Coverage |
|---|---|
| `internal/config/config_test.go` | Parse, Marshal, Load, DefaultCity, ValidateAgents, ValidateRigs, DeriveBeadsPrefix, QualifiedName |
| `internal/config/compose_test.go` | LoadWithIncludes, fragment merging, collision warnings, path resolution, provenance tracking, recursive include rejection |
| `internal/config/patch_test.go` | ApplyPatches for agents/rigs/providers, targeting errors, env merge/remove, pool sub-field patching, provider replace mode |
| `internal/config/pack_test.go` | ExpandPacks, ExpandCityPacks, city_agents partitioning, agent collision detection, override application, formula layer computation |
| `internal/config/pack_fetch_test.go` | FetchPacks, clone/update, PackCachePath, lock read/write, LockFromCache |
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

- **Remote pack fetch requires git.** `FetchPacks` shells out
  to `git clone`/`git fetch`. There is no fallback for environments
  without git.

- **Shallow clones only.** Remote packs are cloned with
  `--depth 1`. Switching to a commit ref that is not at the tip of a
  branch may fail.

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
