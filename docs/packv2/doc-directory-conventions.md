# Pack Structure v.next

**GitHub Issue:** TBD

Title: `feat: Pack structure v.next — principles and standard directory layout for packs and cities`

This is a companion to [doc-pack-v2.md](doc-pack-v2.md), [doc-agent-v2.md](doc-agent-v2.md), and [doc-commands.md](doc-commands.md).

> **Keeping in sync:** This file is the source of truth. When a GitHub issue is created, edit here, then update the issue body from the section between `---BEGIN ISSUE---` and `---END ISSUE---`.

---BEGIN ISSUE---

## Problem

The V2 design keeps moving toward convention-based structure, but we still do not have one document that explains both:

1. **why** packs should be structured a particular way
2. **what** every standard top-level directory means

Right now, the story is scattered:

- `doc-pack-v2.md` explains cities-as-packs and imports
- `doc-agent-v2.md` explains agent directories
- `doc-commands.md` explains command and doctor direction
- `doc-loader-v2.md` assumes convention-based discovery

That creates six problems:

1. **The structure is implied rather than stated.** We keep saying "the new directory conventions" without one canonical reference.
2. **The principle and the mechanism are split apart.** It is hard to evaluate a proposed directory if we do not first state what properties the pack root is supposed to have.
3. **Related proposals can drift.** Agents, formulas, commands, doctor, overlays, skills, and assets all want to snap to one pack structure.
4. **Loader design is underspecified.** If the loader discovers content by convention, we need a stable map of those conventions.
5. **Pack authors do not have one reference.** A pack author should be able to answer "what may exist at the root of a pack?" in one place.
6. **We have not cleanly separated controlled structure from opaque assets.** Without an explicit principle, the pack root risks becoming an unstructured bucket of ad hoc folders.

## Goals

This note defines the structure of a V2 pack at the principle level first, then walks every standard subdirectory.

It aims to answer:

- why the root of a pack is intentionally controlled
- how a city relates to an ordinary pack
- what top-level directories are standard
- what the loader discovers by convention
- where opaque pack-owned files live
- how pack-local paths should behave

## Design principles

### 1. A pack is the unit of portable definition

A pack is the thing that should be importable, versionable, and portable.

That means the pack root should contain:

- the definition itself
- the files that definition depends on
- imports to other packs when the definition needs external content

It should not depend on ambient sibling directories or undocumented filesystem conventions outside the pack boundary.

### 2. A city is a pack plus deployment plus site binding

A city directory has three layers:

- `pack.toml` and pack-owned directories
  - definition
- `city.toml`
  - deployment
- `.gc/`
  - site binding and runtime state

Delete `city.toml` and `.gc/`, and what remains should still be a valid pack.

### 3. The root of a pack is controlled surface area

The top level of a pack should be intentionally designed, not an open junk drawer.

That means:

- standard top-level names are explicitly recognized
- unknown top-level directories should be errors
- arbitrary extra files should live under one well-known opaque directory

This gives us strong structure without forbidding pack-local flexibility.

### 4. Convention should replace path wiring where it actually helps

If a standard directory exists, the loader should understand what it means without additional path wiring in TOML.

This is the core V2 shift away from:

- `prompt_template = "..."`
- `overlay_dir = "..."`
- `scripts_dir = "..."`
- explicit lists of file paths for content that already lives in a standard place

But convention should not force us to invent fake semantic buckets for files that are really just opaque assets.

In particular, V2 does not need `scripts/` as a standard top-level directory. Script files should live either:

- next to the manifest or file that uses them
- or under `assets/` when they are general opaque helpers

### 5. The root city pack and imported packs should look the same

The root should not get a separate filesystem model just because it is the root of composition.

A city pack and an imported pack should share the same pack-owned directory structure. The city merely adds:

- `city.toml`
- `.gc/`

Everything else should mean the same thing.

For the first skills/MCP slice, that "same thing" applies to the current city pack only; imported-pack skills and MCP catalogs are later.

### 6. A pack is strict at the boundary and flexible inside it

Inside a pack, authors should be able to organize files naturally.

Across pack boundaries, the rules should be strict:

- references may point anywhere inside the same pack
- references may use relative paths, including `..`
- after normalization, the resolved path must remain inside the pack root
- escaping the pack root is an error

Imports are the only intended mechanism for crossing pack boundaries.

### 7. Opaque assets need one home

We need one directory where pack authors can place arbitrary files that Gas City does not interpret by convention.

That directory should be:

- clearly named
- standard
- the only opaque top-level asset bucket

The current preferred name is `assets/`.

## Standard layout

### Minimal pack

```text
my-pack/
└── pack.toml
```

This is valid if the pack has no agents, formulas, commands, doctor checks, or other pack-owned assets.

### Typical pack

```text
my-pack/
├── pack.toml
├── agents/
├── formulas/
├── orders/
├── commands/
├── doctor/
├── patches/
├── overlays/
├── skills/
├── mcp/
├── template-fragments/
└── assets/
```

Not every directory is required. If a standard directory exists, the loader understands its role.

### Typical city

```text
my-city/
├── pack.toml
├── city.toml
├── .gc/
├── agents/
├── formulas/
├── orders/
├── commands/
├── doctor/
├── patches/
├── overlays/
├── skills/
├── mcp/
├── template-fragments/
└── assets/
```

The root city pack uses the same pack-owned structure as any imported pack.

## Root files

### `pack.toml`

The definition root for a pack.

Expected to hold pack-level declarative configuration such as:

- pack metadata and identity
- imports
- providers
- agent defaults
- named sessions
- patches
- other pack-level declarative settings that are not better represented as discovered files

It should not become a dumping ground for path declarations that convention can replace.

#### What belongs in `pack.toml`

Given the current V2 direction, `pack.toml` is getting narrower and more declarative.

It should contain things like:

- pack metadata
  - `[pack]`
  - name, version, schema, and other true pack-level identity or compatibility fields
- imports
  - `[imports.<binding>]`
  - source, version constraint, and import/re-export policy
- providers
  - `[providers.*]`
- agent defaults
  - `[agent_defaults]`
  - shared defaults, not individual agent definitions
- named sessions
  - `[[named_session]]`
- patches
  - pack-level modification rules that apply across discovered content
- other pack-wide declarative policy
  - only when it genuinely applies to the pack as a whole and is not better expressed by directory convention

#### What should not live in `pack.toml`

As a rule, `pack.toml` should not inventory or wire content that can be discovered by location.

That means it should trend away from holding:

- individual agent definitions
  - those live under `agents/<name>/`
- prompt file paths
  - use `prompt.md`
- overlay directory declarations
  - use `overlays/` and `agents/<name>/overlay/`
- script directory declarations
  - there is no standard top-level `scripts/` directory in V2
- command inventories for the simple case
  - simple commands should work from `commands/<name>/run.sh`
- doctor inventories for the simple case
  - simple checks should work from `doctor/<name>/run.sh`

Local TOML may still exist below the pack root when needed:

- `agents/<name>/agent.toml`
- `commands/<id>/command.toml`
- `doctor/<id>/doctor.toml`

But those are entry-local overlays, not reasons to turn `pack.toml` back into a full filesystem index.

### `city.toml`

The deployment file for the root city only.

Expected to hold team-shared deployment policy such as:

- rigs
- capacity
- service and substrate decisions
- deployment-oriented operational policy

It should not exist inside ordinary imported packs.

### `.gc/`

Machine-local site binding and runtime state for the root city only.

Expected to hold:

- workspace and rig bindings
- caches
- sockets
- logs
- runtime state
- machine-local config

This is not part of the portable pack definition.

## Standard top-level directories

### `agents/`

Defines agents by convention.

Each immediate child directory is an agent:

```text
agents/
├── mayor/
│   ├── prompt.md
│   ├── agent.toml
│   ├── overlay/
│   ├── skills/
│   ├── mcp/
│   └── template-fragments/
└── polecat/
    └── prompt.md
```

This directory is further specified by [doc-agent-v2.md](doc-agent-v2.md).

### `formulas/`

Holds formula definitions discovered by convention.

Expected contents: `*.toml` files (one per formula). The `.formula.` infix is transitional and targeted for removal. `formulas/` is a fixed convention — the old `[formulas].dir` configurable path is gone.

### `orders/`

Holds order definitions discovered by convention.

Expected contents: `*.toml` files (one per order). The `.order.` infix is transitional and targeted for removal.

Orders are not formulas — they *reference* formulas to schedule dispatch. They live at top level, not nested under `formulas/`.

### `patches/`

Holds prompt replacement files for imported agents.

Patches are distinct from agent definitions — `agents/<name>/` creates YOUR agent; patches in this directory modify SOMEONE ELSE's agent. Patch files are referenced by qualified name from `[[patches.agent]]` in `pack.toml` or `city.toml`.

### `commands/`

Holds pack-provided CLI command definitions and assets.

Current preferred direction:

```text
commands/
├── status/
│   ├── run.sh
│   └── help.md
└── repo/
    └── sync/
        ├── run.sh
        └── help.md
```

Key ideas:

- directories define the default command tree
- each command leaf gets its own local directory
- nested directories imply nested command words
- `run.sh` is the default well-known entrypoint
- `help.md` is the default well-known help file when present
- entry-local scripts and help live next to the entrypoint
- `command.toml` is optional and should exist only when metadata or an explicit override is needed

This keeps the filesystem shape aligned with the CLI shape while giving each command leaf a local asset scope.

The important split is:

- the user-facing command words come from directory shape by default
- the local executable and help file can use simple filename convention by default
- `command.toml` remains available as an escape hatch rather than a requirement

### `doctor/`

Holds pack-provided doctor checks and their assets.

Current preferred direction:

```text
doctor/
├── git-clean/
│   ├── doctor.toml
│   ├── run.sh
│   └── help.md
└── binaries/
    ├── doctor.toml
    └── run.sh
```

Doctor and commands should be designed in tandem. They are structurally sibling surfaces:

- a named operational entry
- a small manifest
- an executable entrypoint
- optional help and local assets

The difference is in exposure:

- commands contribute to the `gc` command surface
- doctor checks contribute to `gc doctor`

As with commands:

- `run.sh` is the default well-known entrypoint
- `help.md` is the default well-known help file when present
- the script that actually runs the check should live naturally alongside the manifest rather than depending on a special top-level `scripts/` directory

### `overlays/`

Holds pack-wide overlay files applied to agents according to the V2 overlay rules.

Use this for shared overlay material that is not specific to one agent.

Per-agent overlays belong under `agents/<name>/overlay/`.

### `skills/`

Holds the current city pack's shared skills.

Use this for reusable skills shipped with the current city pack and made available according to pack and agent composition rules.

Per-agent skills belong under `agents/<name>/skills/`.

### `mcp/`

Holds the current city pack's MCP server definitions or related MCP assets.

Per-agent MCP assets belong under `agents/<name>/mcp/`.

### `template-fragments/`

Holds pack-wide prompt template fragments.

Per-agent template fragments belong under `agents/<name>/template-fragments/`.

### `assets/`

Holds opaque pack-owned assets that Gas City does not interpret by convention.

This is the escape hatch that lets us keep the pack root tightly controlled while still allowing arbitrary files.

Examples:

- helper scripts referenced by relative path
- static data files
- templates not tied to a standard discovery surface
- fixtures and test data
- embedded packs referenced explicitly by relative import path

Gas City should treat `assets/` as opaque. It may validate that references stay inside the pack boundary, but it should not assign special meaning to the internal layout.

This is also the natural place to allow embedded packs while keeping the root directory model simple. For example:

```text
assets/imports/maintenance/pack.toml
```

with:

```toml
[imports.maintenance]
source = "./assets/imports/maintenance"
```

That keeps embedding possible while keeping the pack root simple and uniform.

## Pack-local path behavior

Pack-local references should follow one simple rule:

- a relative path resolves relative to the file or manifest that declares it

More generally:

- any field that accepts a path may point to any file inside the same pack
- that includes files under standard directories
- and it includes files under `assets/`

Examples:

- command `run = "./run.sh"` resolves relative to `command.toml`
- doctor `run = "./run.sh"` resolves relative to `doctor.toml`
- other pack-local references should follow the same locality rule where practical

`..` should be allowed, with one hard constraint:

- after normalization, the resolved path must still stay inside the same pack root

So this is allowed:

```toml
run = "../shared/run.sh"
```

if it still resolves inside the pack.

This is not allowed:

```toml
run = "../../../outside.sh"
```

if it escapes the pack boundary.

The guiding principle is:

- flexible inside the pack
- strict at the pack boundary

## Loader expectations

The V2 loader should treat these directories as standard signals:

- `agents/` means "discover agents"
- `formulas/` means "discover formulas"
- `orders/` means "discover orders"
- `commands/` means "discover command entries"
- `doctor/` means "discover doctor entries"
- `patches/` means "load prompt replacement files for imported agents"
- `overlays/`, `skills/`, `mcp/`, `template-fragments/` mean "load pack-wide assets of those kinds" for the current city pack; imported-pack catalogs are later
- `assets/` means "opaque pack-owned files; no convention-based discovery"

The loader should not require explicit TOML path declarations for standard directories when convention is sufficient.

## Root vs imported pack behavior

The same pack-owned directories should mean the same thing in:

- the root city pack
- a directly imported pack
- a re-exported imported pack

The difference should come from composition and exposure rules, not from different filesystem semantics.

That matters especially for:

- commands
- doctor checks
- overlays
- skills

If a directory means one thing in an imported pack and another in the root city pack, we are probably reintroducing the asymmetry V2 is trying to remove.

## Open questions

### 1. Which surfaces are fully convention-based versus TOML-assisted?

The big remaining question is how far convention goes.

Candidates for fully convention-based discovery:

- agents
- formulas

Candidates still likely to need lightweight manifest metadata:

- commands
- doctor checks

### 2. What is the final command and doctor manifest shape?

Current leaning:

- `commands/<id>/command.toml`
- `doctor/<id>/doctor.toml`
- entry-local `run.sh` and optional `help.md`

Open details include exact field names and optional metadata.

### 3. Should unknown top-level directories be an error?

Current leaning: yes.

The top-level pack surface should stay tightly controlled. Arbitrary files should live under `assets/`.

### 4. Should `assets/` be the only opaque top-level directory?

Current leaning: yes.

If we allow multiple opaque roots, we weaken the point of having a controlled pack structure.

### 5. How much nesting should standard discovery allow?

We still need exact walk rules for:

- `formulas/`
- `commands/`
- `doctor/`

Examples:

- are nested subdirectories allowed freely?
- are names derived from relative path?
- are some subdirectories reserved?

## Working draft summary

The current direction is:

1. a pack is the unit of portable definition
2. a city is a pack plus `city.toml` plus `.gc/`
3. the top-level pack surface should be intentionally controlled
4. convention should replace path wiring where it actually helps
5. the root city pack and imported packs should use the same pack-owned structure
6. `assets/` is the one opaque top-level asset bucket
7. `agents/`, `formulas/`, `orders/`, `commands/`, `doctor/`, `patches/`, `overlays/`, `skills/`, `mcp/`, `template-fragments/`, and `assets/` are the standard pack directories
8. commands and doctor checks currently lean toward per-entry directories with small manifests and local assets
9. path-valued fields may point anywhere inside the same pack, including `assets/`
10. pack-local paths should be flexible inside the pack and strict at the pack boundary

---END ISSUE---
