---
title: "PackV2: The New Package System for Gas City"
description: How to move an existing Gas City 0.14.0 city or pack to the PackV2 schema and directory conventions.
---

This guide is the practical migration companion for moving from the
0.14.0 PackV1 world into the PackV2 model that first landed in 0.14.1
and is being finished in the 0.15.0 wave.

PackV2 was an initiative to address multiple problems in the way we
write down how a city or a package works. There was a lot of
entanglement between:
- The definition of a pack or a city that can be versioned, shared, and used in many contexts.
- The deployment configuration of how things project directories specific to your machine get rigged to a city.
- The runtime information that Gas City needs to manage opaquely to users.


In 0.14.0 and earlier, a city was kind of a pack, but kind of not.
PackV2 clears that up.

Starting in 0.14.1, Gas City supports the PackV2 model: a city is
defined just like a pack, but with an additional `city.toml`.

The migration has two steps:

1. Move portable definitions (e.g., agents, formulas) into `pack.toml` and the various pack-owned directories (e.g., agents, formulas)
2. leave only deployment information (e.g., rigs) in `city.toml`

There is a third layer, `.gc/`, but that is site binding and runtime
state. It matters to the model, but it is mostly not user migration
work, so this guide keeps the focus on `pack.toml`, `city.toml`, and the
pack directory tree.

The target public migration flow is `gc doctor`, then
`gc doctor --fix` for the safe mechanical rewrites, then `gc doctor`
again to confirm the result. Some old cities may hard-break until
migrated; that is intentional in this wave.

> **Current rollout note:** The doctor-first remediation slice lands
> separately from the Skills/MCP, infix, and rig-path slices. Until that
> remediation work is present on your branch, `gc import migrate` may
> still exist as a transitional command surface even though the target
> model is `gc doctor` followed by `gc doctor --fix`.

> **Scope note:** This guide describes the target PackV2 migration
> shape. Some sections below point at surfaces that are only in the first
> slice of the current rollout. When that is true, the guide calls it out
> inline and links the tracking issue. For release-gated behavior, also
> consult `docs/packv2/skew-analysis.md` and
> `docs/packv2/doc-conformance-matrix.md`.
>
> **First-slice note:** skills and MCP are current-city-pack only in this
> wave. Imported-pack catalogs and provider projection are later slices.
> The dedicated `.gc/site.toml` path split is still tracked in
> [#588](https://github.com/gastownhall/gascity/issues/588).

## Before you start

The important mental shift is:

- **Gas City 0.14.0** centers `city.toml` and a lot of explicit path wiring
- **Gas City 0.14.1 and later** centers `pack.toml`, named imports, and convention-based directories

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

In Gas City 0.14.0, composition is include-based. In the PackV2
rollout, composition is import-based.

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
    ├── prompt.template.md
    └── agent.toml
```

Use `agent.toml` only when the agent needs overrides beyond shared
defaults.

### Migration notes

- move each `[[agent]]` definition into `agents/<name>/`
- move templated prompt content to `agents/<name>/prompt.template.md`
- move agent-local overlay content to `agents/<name>/overlay/`
- keep shared defaults in `[agent_defaults]` (in `pack.toml` for pack-wide, `city.toml` for city-level overrides)
- keep pack-wide providers in `[providers.*]`

If you are migrating a city, city-local agents are still just agents in
the root city pack.

## Formulas

Formulas mostly already fit the new direction.

### Preferred shape

```text
formulas/
└── build-review.toml
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
- use flat files `orders/<name>.toml`

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
└── nightly-sync.toml
```

This gives a consistent pair:

- `formulas/<name>.toml`
- `orders/<name>.toml`

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

The default `commands/<name>/run.sh` discovery path is part of the
current release surface. `command.toml` remains optional for metadata or
explicit overrides. The remaining command manifest symmetry work is
tracked in [#668](https://github.com/gastownhall/gascity/issues/668).

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

The default `doctor/<name>/run.sh` discovery path is part of the
current release surface. `doctor.toml` remains optional for metadata or
explicit overrides. The remaining command/doctor manifest symmetry work
is tracked in [#668](https://github.com/gastownhall/gascity/issues/668).

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

- `skills/` for the current city pack's shared skills
- `mcp/` for the current city pack's shared MCP assets
- `template-fragments/` for pack-wide prompt fragments

and:

- `agents/<name>/skills/`
- `agents/<name>/mcp/`
- `agents/<name>/template-fragments/`

when the asset belongs to one specific agent.

> **First slice:** `skills/` and `agents/<name>/skills/` describe the
> current-city-pack home for skills in this wave. Imported-pack catalogs
> are later, and list-only visibility is the first CLI surface. Pack
> discovery/materialization is tracked in
> [#669](https://github.com/gastownhall/gascity/issues/669).
>
> **First slice:** `mcp/` and `agents/<name>/mcp/` describe the
> current-city-pack home for MCP definitions in this wave. Imported-pack
> catalogs and provider projection are later, and list-only visibility
> is the first CLI surface. The provider-agnostic TOML abstraction and
> projection model are tracked in
> [#670](https://github.com/gastownhall/gascity/issues/670).

## Fragment injection migration

The old three-layer prompt injection pipeline is replaced by explicit
template inclusion.

| Old mechanism | New model |
|---|---|
| `global_fragments` in workspace config | Gone — move content to `template-fragments/` and use explicit `{{ template "name" . }}` in `.template.md` prompts |
| `inject_fragments` on agent config | Gone — same approach |
| `inject_fragments_append` on patches | Gone — same approach |
| All `.md` files run through Go templates | Only `.template.md` files run through Go templates |

For migration convenience, `append_fragments` in `agent.toml` or
`[agent_defaults]` auto-appends named fragments to `.template.md`
prompts without editing each prompt file:

```toml
# pack.toml or city.toml
[agent_defaults]
append_fragments = ["operational-awareness", "command-glossary"]
```

Plain `.md` prompts are inert — no fragments attach, no template engine
runs.

> **NYI in this wave:** `[agent_defaults].append_fragments` is the
> proven migration bridge in the current release. Agent-local
> `append_fragments` is still tracked as a spec/runtime parity gap in
> [#671](https://github.com/gastownhall/gascity/issues/671).

## Assets and paths

This is the positive rule that replaces a lot of 0.14.0 ad hoc path
habits.

### `assets/` is the opaque home for your files

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
because 0.14.0 had it.

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


## Reference: Gas City 0.14.0 `city.toml` elements to PackV2

This is the exhaustive top-level lookup table for the old `city.toml`
schema, plus the qualified rows that matter most during migration.

> **Current rollout note:** Some rows below describe the target PackV2
> destination rather than the exact state of every in-flight branch. In
> the current 15.0 wave, `workspace.name` still lives in `city.toml`.
> Phase A rig-binding work removes machine-local `rigs.path` from newly
> written city configs, but `rigs.prefix` and `rigs.suspended` remain in
> `city.toml` in this release.

| 0.14.0 element | What it did | New home or action |
|---|---|---|
| `include` | Merged extra config fragments into `city.toml` before load | Remove as part of migration. Move real composition to imports and move remaining config to `pack.toml`, `city.toml`, or discovered directories. |
| `[workspace]` | Held city metadata and pack composition in one place | Split across the root `pack.toml`, `city.toml`, and `.gc/`. |
| `workspace.name` | Workspace identity | Transitional in this wave. Keep it in `city.toml` for the current 0.15.0 migration slice. Fresh `gc init` keeps it aligned with `pack.name`; `gc register` keeps it aligned with the registered city name, using `workspace.name` when present and backfilling it from `pack.name` when absent. Full removal from `city.toml` still waits for the broader site-binding cutover; track [#602](https://github.com/gastownhall/gascity/issues/602). |
| `workspace.includes` | City-level pack composition | Move to `[imports.*]` in the root city `pack.toml`. |
| `workspace.default_rig_includes` | Default pack composition for newly added rigs | Move to `[defaults.rig.imports]` in the root city `pack.toml`. This is the target shape, but loader-backed support is still tracked in [#360](https://github.com/gastownhall/gascity/issues/360). |
| `[providers.*]` | Named provider presets | Usually move to `[providers.*]` in the root city `pack.toml`, unless the setting is truly deployment-only. |
| `[packs.*]` | Named remote pack sources used by includes | Collapse into `[imports.*]` entries. There should no longer be a separate `[packs.*]` registry in `city.toml`. |
| `[[agent]]` | Inline agent definitions | Move to `agents/<name>/`, with optional `agent.toml`. |
| `agent.prompt_template` | Path to agent prompt | Move to `agents/<name>/prompt.template.md` for templated prompts. Use `prompt.md` only for plain, non-templated Markdown. |
| `agent.overlay_dir` | Path to overlay content | Move content to `agents/<name>/overlay/` or pack-wide `overlays/`. |
| `agent.session_setup_script` | Path to setup script | Keep as a path-valued field, but point at a pack-local file, usually next to the thing that uses it or under `assets/`. |
| `agent.namepool` | Path to names file | Move toward agent-local content such as `agents/<name>/namepool.txt` if retained. |
| `[[named_session]]` | Named reusable sessions | Move to `[[named_session]]` in the root city `pack.toml`. |
| `[[rigs]]` | Rig deployment entries | Keep in `city.toml`. |
| `rigs.path` | Machine-local project binding | With the Phase A rig-binding slice, new writes stop persisting this in authored `city.toml`; older cities may still carry it until migrated. |
| `rigs.prefix` | Derived rig prefix | Keep in `city.toml` in the current release wave. It is deployment state, but not yet extracted into separate site-binding storage. |
| `rigs.suspended` | Operational toggle | Keep in `city.toml` in the current release wave. It remains deployment/runtime state rather than portable pack definition. |
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
| `[agent_defaults]` | Defaults applied to agents in this city | Lives in both `pack.toml` (pack-wide portable defaults) and `city.toml` (city-level deployment overrides). City layers on top of pack. |

## Reference: Gas City 0.14.0 `pack.toml` elements to PackV2

This is the lookup table for the old shareable-pack schema and the
transitional pack fields that people are likely to have.

| 0.14.0 element | What it did | New home or action |
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
| `agent.prompt_template` | Agent prompt file path | Move to `agents/<name>/prompt.template.md` for templated prompts. Use `prompt.md` only for plain, non-templated Markdown. |
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

## Reference: old top-level directories

This table is the filesystem companion to the two schema tables above.

| Old directory or pattern | What it meant in 0.14.0 | New home or action |
|---|---|---|
| `prompts/` | Shared bucket of prompt templates addressed by path | Move prompt content into `agents/<name>/prompt.template.md` for templated prompts. Use `prompt.md` only for plain, non-templated Markdown. |
| `scripts/` | Shared bucket of helper and entrypoint scripts | Do not preserve as a standard top-level directory. Put entrypoint scripts next to what uses them, and put general helpers under `assets/`. |
| `formulas/` | Formula directory, sometimes path-wired via TOML | Keep as the fixed top-level `formulas/` convention. |
| `formulas/orders/` | Nested order definitions under formulas | Move to top-level `orders/` using flat `*.toml` files. |
| `orders/` | Top-level order directory in some cities | Standardize on this location, but use flat `orders/<name>.toml` files. |
| `overlays/` | Pack-wide overlay bucket | Keep as top-level `overlays/`. |
| `overlay/` | Singular overlay directory seen in some older packs | Remove or migrate to `overlays/` or `agents/<name>/overlay/`. |
| `namepools/` | Shared bucket of agent name pools | Move toward agent-local files if retained. |
| `commands/` with ad hoc scripts | Command helper directory plus TOML wiring | Keep `commands/`, but organize as entry directories such as `commands/<name>/run.sh`. |
| `doctor/` with ad hoc scripts | Doctor helper directory plus TOML wiring | Keep `doctor/`, but organize as entry directories such as `doctor/<name>/run.sh`. |
| `skills/` | Current city pack skills directory in newer layouts | Keep as top-level `skills/`. |
| `mcp/` | Current city pack MCP directory in newer layouts | Keep as top-level `mcp/`. |
| `template-fragments/` | Shared prompt-fragment directory in newer layouts | Keep as top-level `template-fragments/`. |
| `packs/` | Local vendored packs or bootstrap imports | Do not treat as a standard top-level directory. If you need opaque embedded packs, place them under `assets/` and import them explicitly. |
| loose helper files at pack root | Arbitrary files mixed into controlled surface area | Keep standard repo documents like `README.md`, `LICENSE*`, `CONTRIBUTING.md`, and `CHANGELOG*` at pack root. Move other opaque helpers under `assets/`. |

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
