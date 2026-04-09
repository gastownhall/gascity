---
title: "Migrating to Gas City 0.13.6"
description: How to move an existing Gas City 0.13.5 city or pack to the Gas City 0.13.6 pack/city schema and directory conventions.
---

This guide is the practical migration companion to the Gas City 0.13.6
pack/city, agent, command, and directory-structure work.

The migration has two layers:

1. split portable definition into `pack.toml` and pack-owned directories
2. leave only deployment in `city.toml`

There is a third layer, `.gc/`, but that is site binding and runtime
state. It matters to the model, but it is mostly not user migration
work, so this guide keeps the focus on `pack.toml`, `city.toml`, and the
pack directory tree.

## Before you start

The important mental shift is:

- **Gas City 0.13.5** centers `city.toml` and a lot of explicit path wiring
- **Gas City 0.13.6** centers `pack.toml`, named imports, and convention-based directories

The clean target shape is:

- `pack.toml`
  - portable definition, imports, and pack-wide policy
- `city.toml`
  - deployment decisions for this city
- pack-owned directories
  - agents, formulas, orders, commands, doctor checks, overlays, skills, MCP, template fragments, assets

## First: split `city.toml` and `pack.toml`

This is the most important migration step. Everything else hangs off it.

In the new model, a city is a deployed pack. That means the root city
directory has its own `pack.toml`, and the old "everything lives in
`city.toml`" model gets broken apart.

### What belongs in `pack.toml`

`pack.toml` is now the home for portable definition:

- pack identity and compatibility metadata
- imports
- providers
- pack-wide agent defaults
- named sessions
- pack-level patches
- other pack-wide declarative policy

It should not be a registry of every file in the pack. If convention can
find something, prefer convention.

### What belongs in `city.toml`

`city.toml` is now the home for deployment:

- rigs
- rig-specific composition and patches
- substrate choices
- API/daemon/runtime behavior
- capacity and scheduling policy

It should no longer be the place where the pack's portable definition
lives.

## First concrete step: move includes to imports

For most existing cities, the first change you will actually make is
composition.

In Gas City 0.13.5, composition is include-based. In Gas City 0.13.6,
composition is import-based.

### Old city-level include

```toml
# city.toml
[workspace]
name = "my-city"
includes = ["packs/gastown"]
```

### New root pack import

```toml
# pack.toml
[pack]
name = "my-city"

[imports.gastown]
source = "../shared/gastown"
```

The key change is that the import gets a local name, here `gastown`.
That local name is what the rest of the pack uses when it needs to refer
to imported content.

### Old rig-level include

```toml
# city.toml
[[rigs]]
name = "api-server"
path = "/srv/api"
includes = ["../shared/gastown"]
```

### New rig-level import

```toml
# city.toml
[[rigs]]
name = "api-server"

[rigs.imports.gastown]
source = "../shared/gastown"
```

Use the city pack's `pack.toml` for city-wide imports. Use rig-scoped
imports in `city.toml` when a pack should compose only into one rig.

Rigs are the main thing that remain in `city.toml`. As you migrate, the
usual pattern is:

- move portable definition into `pack.toml` and pack-owned directories
- leave rigs and other deployment choices in `city.toml`

## Then: migrate area by area

Once the root split is in place, the rest of the work gets much more
mechanical.

## Agents

Agents move out of inline TOML inventories and into agent directories.

### Old shape

```toml
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
overlay_dir = "overlays/default"
```

### New shape

```text
agents/
└── mayor/
    ├── prompt.md
    └── agent.toml
```

Use `agent.toml` only when the agent needs overrides beyond shared
defaults.

### Migration notes

- move each `[[agent]]` definition into `agents/<name>/`
- move prompt content to `agents/<name>/prompt.md`
- move agent-local overlay content to `agents/<name>/overlay/`
- keep shared defaults in `[agents]`
- keep pack-wide providers in `[providers.*]`

If you are migrating a city, city-local agents are still just agents in
the root city pack.

## Formulas

Formulas mostly already fit the new direction.

### Preferred shape

```text
formulas/
└── build-review.formula.toml
```

### Migration notes

- keep formulas in top-level `formulas/`
- stop treating formula location as configurable path wiring
- move nested orders out of formula space

## Orders

Orders are being refactored to look more like formulas.

The current direction, also captured in the consistency audit, is:

- move orders out of `formulas/orders/`
- standardize on top-level `orders/`
- use flat files `orders/<name>.order.toml`

### Old shape

```text
formulas/
└── orders/
    └── nightly-sync/
        └── order.toml
```

### New shape

```text
orders/
└── nightly-sync.order.toml
```

This gives a consistent pair:

- `formulas/<name>.formula.toml`
- `orders/<name>.order.toml`

## Commands

Commands are moving toward convention-first entry directories.

### Simple case

```text
commands/
└── status/
    └── run.sh
```

This is enough for a default single-word command.

### Richer case

```text
commands/
└── repo-sync/
    ├── command.toml
    ├── run.sh
    └── help.md
```

Use `command.toml` only when the default mapping is not enough, for
example:

- multi-word command placement
- extension-root placement
- richer metadata
- non-default entrypoint

### Migration notes

Old:

```toml
[[commands]]
name = "status"
description = "Show status"
script = "commands/status.sh"
```

New simple case:

```text
commands/status/run.sh
```

New richer case:

```text
commands/repo-sync/
├── command.toml
├── run.sh
└── help.md
```

## Doctor checks

Doctor checks are moving in parallel with commands.

### Simple case

```text
doctor/
└── binaries/
    └── run.sh
```

### Richer case

```text
doctor/
└── git-clean/
    ├── doctor.toml
    ├── run.sh
    └── help.md
```

The migration rule is the same as commands:

- keep the entrypoint local to the check that uses it
- use local TOML only when the default mapping is not enough

## Overlays

Overlays move away from being a global path bucket and toward a clearer
split between pack-wide and agent-local content.

Use:

- `overlays/` for pack-wide overlay material
- `agents/<name>/overlay/` for agent-local overlay material

If your old config depends on `overlay_dir = "..."`, the migration step
is usually to relocate those files into one of those places.

## Skills, MCP, and template fragments

These mostly follow the new directory structure directly.

Use:

- `skills/` for pack-wide skills
- `mcp/` for pack-wide MCP assets
- `template-fragments/` for pack-wide prompt fragments

and:

- `agents/<name>/skills/`
- `agents/<name>/mcp/`
- `agents/<name>/template-fragments/`

when the asset belongs to one specific agent.

## Assets and paths

This is the positive rule that replaces a lot of 0.13.5 ad hoc path
habits.

### `assets/` is the opaque home

If a file is not part of a standard surface Gas City uses for discovery, it belongs in
`assets/`.

Examples:

- helper scripts
- static data files
- fixtures and test data
- imported pack payloads carried inside another pack

### Path-valued fields

Any field that accepts a path may point to any file inside the same
pack.

That includes:

- files under standard directories
- files under `assets/`
- relative paths that use `..`

The hard constraint is:

- after normalization, the path must still stay inside the pack root

### Examples

```toml
run = "./run.sh"
help = "./help.md"
run = "../shared/run.sh"
source = "./assets/imports/maintenance"
```

## Common migration gotchas

### "I still have a lot in `city.toml`"

That usually means definition and deployment are still mixed together.

Ask:

- is this portable definition?
- is this deployment?

Then move it to:

- `pack.toml` and pack-owned directories
- `city.toml`

respectively.

### "I used to rely on `scripts/`"

Do not recreate `scripts/` as a standard top-level convention just
because 0.13.5 had it.

Instead:

- put entrypoint scripts next to the command or doctor entry that uses them
- put general opaque helpers under `assets/`

For example, this old pattern:

```text
scripts/
└── setup.sh
```

plus:

```toml
session_setup_script = "scripts/setup.sh"
```

becomes either:

```text
commands/status/run.sh
```

or:

```text
assets/scripts/setup.sh
```

depending on whether the script is entry-local or a general helper.

### "Do I need TOML everywhere?"

No.

Simple cases should work by convention:

- `agents/<name>/prompt.md`
- `commands/<name>/run.sh`
- `doctor/<name>/run.sh`

Use TOML when you actually need:

- defaults
- overrides
- metadata
- explicit placement

## Reference: Gas City 0.13.5 `city.toml` elements to 0.13.6

This is the exhaustive top-level lookup table for the old `city.toml`
schema, plus the qualified rows that matter most during migration.

| 0.13.5 element | What it did | New home or action |
|---|---|---|
| `include` | Merged extra config fragments into `city.toml` before load | Remove as part of migration. Move real composition to imports and move remaining config to `pack.toml`, `city.toml`, or discovered directories. |
| `[workspace]` | Held city metadata and pack composition in one place | Split across the root `pack.toml`, `city.toml`, and `.gc/`. |
| `workspace.name` | Workspace identity | Managed site binding, not portable definition. Do not model this as pack content. |
| `workspace.includes` | City-level pack composition | Move to `[imports.*]` in the root city `pack.toml`. |
| `workspace.default_rig_includes` | Default pack composition for newly added rigs | Move to `[defaults.rig.imports]` in the root city `pack.toml`. |
| `[providers.*]` | Named provider presets | Usually move to `[providers.*]` in the root city `pack.toml`, unless the setting is truly deployment-only. |
| `[packs.*]` | Named remote pack sources used by includes | Collapse into `[imports.*]` entries. There should no longer be a separate `[packs.*]` registry in `city.toml`. |
| `[[agent]]` | Inline agent definitions | Move to `agents/<name>/`, with optional `agent.toml`. |
| `agent.prompt_template` | Path to agent prompt | Move to `agents/<name>/prompt.md`. |
| `agent.overlay_dir` | Path to overlay content | Move content to `agents/<name>/overlay/` or pack-wide `overlays/`. |
| `agent.session_setup_script` | Path to setup script | Keep as a path-valued field, but point at a pack-local file, usually next to the thing that uses it or under `assets/`. |
| `agent.namepool` | Path to names file | Move toward agent-local content such as `agents/<name>/namepool.txt` if retained. |
| `[[named_session]]` | Named reusable sessions | Move to `[[named_session]]` in the root city `pack.toml`. |
| `[[rigs]]` | Rig deployment entries | Keep in `city.toml`. |
| `rigs.path` | Machine-local project binding | Managed site binding, not portable pack definition. |
| `rigs.prefix` | Derived rig prefix | Managed site binding, not portable pack definition. |
| `rigs.suspended` | Operational toggle | Managed site binding, not portable pack definition. |
| `rigs.includes` | Rig-scoped pack composition | Move to rig-scoped imports in `city.toml`. |
| `rigs.overrides` | Rig-specific customization of imported agents | Keep as rig-level deployment customization in `city.toml`. |
| `[patches]` | Post-merge modifications | Move pack-definition patches to `pack.toml`. Keep rig-specific patches with the rig in `city.toml`. |
| `[beads]` | Bead store backend choice | Keep in `city.toml`. |
| `[session]` | Session substrate config | Keep in `city.toml`, except site-local bindings. |
| `[mail]` | Mail substrate config | Keep in `city.toml`. |
| `[events]` | Events substrate config | Keep in `city.toml`. |
| `[dolt]` | Dolt connection defaults | Keep in `city.toml`. |
| `[formulas]` | Formula directory config | Prefer convention. Keep only if a remaining pack-wide formula policy survives; otherwise remove. |
| `formulas.dir` | Formula directory path | Replace with the fixed top-level `formulas/` convention. |
| `[daemon]` | Controller daemon behavior | Keep in `city.toml`. |
| `[orders]` | Order runtime policy such as skip lists and timeouts | Keep in `city.toml`. |
| `[api]` | API server deployment config | Keep in `city.toml`, except machine-local bind details. |
| `[chat_sessions]` | Chat session runtime policy | Keep in `city.toml`. |
| `[session_sleep]` | Sleep policy defaults | Keep in `city.toml`. |
| `[convergence]` | Convergence limits | Keep in `city.toml`. |
| `[[service]]` | Workspace-owned service declarations | Keep in `city.toml` if they are deployment-owned services. |
| `[agent_defaults]` | Defaults applied to agents in this city | Move to `[agents]` in the root city `pack.toml`. |

## Reference: Gas City 0.13.5 `pack.toml` elements to 0.13.6

This is the lookup table for the old shareable-pack schema and the
transitional pack fields that people are likely to have.

| 0.13.5 element | What it did | New home or action |
|---|---|---|
| `[pack]` | Pack metadata | Keep in `pack.toml`. |
| `pack.name` | Pack identity | Keep in `[pack]`. |
| `pack.version` | Pack version | Keep in `[pack]`. |
| `pack.schema` | Pack schema version | Keep in `[pack]`, updated to the new schema as needed. |
| `pack.requires_gc` | Minimum supported gc version | Keep in `[pack]`. |
| `pack.city_agents` | City-vs-rig stamping hint in the old pack system | Revisit during migration. The new model prefers agent-local definition and scope rules instead of this field. |
| `pack.includes` | Pack-to-pack composition | Replace with `[imports.*]` in `pack.toml`. |
| `pack.requires` | Pack requirements | Keep in `[pack]` if the requirement model survives unchanged; otherwise migrate to the current requirement shape in the design docs. |
| `[imports.*]` | Named imports in transitional configs | Keep in `pack.toml`. This is the new composition surface. |
| `[[agent]]` | Inline pack agent definitions | Move to `agents/<name>/`, with optional `agent.toml`. |
| `agent.prompt_template` | Agent prompt file path | Move to `agents/<name>/prompt.md`. |
| `agent.overlay_dir` | Agent overlay path | Move content to `agents/<name>/overlay/` or `overlays/`. |
| `agent.session_setup_script` | Agent setup script path | Keep as a path-valued field pointing at a pack-local file. |
| `[[named_session]]` | Pack-defined named sessions | Keep in `pack.toml`. |
| `[[service]]` | Pack-defined services | Keep only if services remain pack-defined in the new model. Otherwise move city-owned services to `city.toml`. |
| `[providers.*]` | Provider presets used by the pack | Keep in `pack.toml`. |
| `[formulas]` | Formula directory config | Prefer convention. Remove directory wiring and use top-level `formulas/`. |
| `formulas.dir` | Formula directory path | Replace with top-level `formulas/`. |
| `[patches]` | Pack-level patching rules | Keep in `pack.toml`. |
| `[[doctor]]` | Pack doctor inventory | Move toward `doctor/<name>/run.sh` by default, with optional `doctor.toml` when needed. |
| `doctor.script` | Path to doctor entrypoint | Keep as a pack-local path, usually `doctor/<name>/run.sh`. |
| `[[commands]]` | Pack command inventory | Move toward `commands/<name>/run.sh` by default, with optional `command.toml` when needed. |
| `commands.script` | Path to command entrypoint | Keep as a pack-local path, usually `commands/<name>/run.sh`. |
| `[global]` | Pack-wide session-live behavior | Keep in `pack.toml` if the pack-global surface survives as designed. |
| `[agents]` | Pack-wide agent defaults in transitional configs | Keep in `pack.toml`. |

## Reference: old top-level directories

This table is the filesystem companion to the two schema tables above.

| Old directory or pattern | What it meant in 0.13.5 | New home or action |
|---|---|---|
| `prompts/` | Shared bucket of prompt templates addressed by path | Move prompt content into `agents/<name>/prompt.md`. |
| `scripts/` | Shared bucket of helper and entrypoint scripts | Do not preserve as a standard top-level directory. Put entrypoint scripts next to what uses them, and put general helpers under `assets/`. |
| `formulas/` | Formula directory, sometimes path-wired via TOML | Keep as the fixed top-level `formulas/` convention. |
| `formulas/orders/` | Nested order definitions under formulas | Move to top-level `orders/` using flat `*.order.toml` files. |
| `orders/` | Top-level order directory in some cities | Standardize on this location, but use flat `orders/<name>.order.toml` files. |
| `overlays/` | Pack-wide overlay bucket | Keep as top-level `overlays/`. |
| `overlay/` | Singular overlay directory seen in some older packs | Remove or migrate to `overlays/` or `agents/<name>/overlay/`. |
| `namepools/` | Shared bucket of agent name pools | Move toward agent-local files if retained. |
| `commands/` with ad hoc scripts | Command helper directory plus TOML wiring | Keep `commands/`, but organize as entry directories such as `commands/<name>/run.sh`. |
| `doctor/` with ad hoc scripts | Doctor helper directory plus TOML wiring | Keep `doctor/`, but organize as entry directories such as `doctor/<name>/run.sh`. |
| `skills/` | Pack-wide skills directory in newer layouts | Keep as top-level `skills/`. |
| `mcp/` | Pack-wide MCP directory in newer layouts | Keep as top-level `mcp/`. |
| `template-fragments/` | Shared prompt-fragment directory in newer layouts | Keep as top-level `template-fragments/`. |
| `packs/` | Local vendored packs or bootstrap imports | Do not treat as a standard top-level directory. If you need opaque embedded packs, place them under `assets/` and import them explicitly. |
| loose helper files at pack root | Arbitrary files mixed into controlled surface area | Move them under `assets/`. |

## Suggested migration order

For a real city or pack, the most practical order is:

1. add a root `pack.toml`
2. move `workspace.includes` and `rigs.includes` to imports
3. move agent definitions into `agents/`
4. move orders to top-level flat files
5. move commands and doctor checks into `commands/` and `doctor/`
6. move opaque helpers into `assets/`
7. clean up whatever remains in `city.toml` and `pack.toml` using the reference tables above

That gets the big structural changes done before you spend time on the
smaller cleanup work.
