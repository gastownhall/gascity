# Pack Commands v.next

> **Status:** design note / rationale only. This file is not the release-gating
> authority for the shipped command or doctor surface.
>
> **Durable truth lives in:**
> - `docs/packv2/doc-directory-conventions.md`
> - `docs/packv2/skew-analysis.md`
> - `docs/packv2/doc-conformance-matrix.md`
> - `docs/guides/migrating-to-pack-vnext.md`

**GitHub Issue:** TBD

Title: `feat: Pack commands v.next — command identity, extension points, and CLI structure`

This is a companion to [doc-pack-v2.md](doc-pack-v2.md), which covers the pack/city model redesign, and to [doc-loader-v2.md](doc-loader-v2.md), which describes the proposed v.next loader against the current release-branch behavior.

> **Keeping in sync:** treat this file as the historical design note for command
> and doctor rationale. If a GitHub issue is created from it, keep the issue
> body in sync with the section between `---BEGIN ISSUE---` and `---END ISSUE---`,
> but do not treat this note as the release-gating source of truth.

---BEGIN ISSUE---

## Problem

Gas City's current pack command model works, but it is still shaped by the V1 composition model rather than the import-oriented structure we want in Pack/City v.next.

Today, a pack can declare `[[commands]]` in `pack.toml`, and the `gc` CLI discovers those commands from the resolved pack directories and registers them as:

```text
gc <pack-name> <command-name>
```

That creates eight problems:

1. **Command namespace comes from pack definition, not import binding.** In V2, packs are supposed to enter a city through named imports. The local binding is the durable identity. Commands still key off `[pack].name` instead.

2. **Aliasing does not carry through to commands.** If a city imports `gastown` as `gs`, the current model gives us no natural way to expose `gc gs ...`. The command surface ignores the user's chosen composition name.

3. **Flattening leaks through the CLI.** V1 composition expands packs into directories and then discovers commands from the resulting pack dirs. That is implementation-shaped, not model-shaped.

4. **Collision handling is under-specified.** The current implementation protects only against a pack name shadowing a core `gc` command. It does not yet define the right rules for imported aliases, duplicate command names across imports, or repeated import of the same pack under multiple bindings.

5. **Command structure is less mature than the rest of the pack model.** Agents, formulas, prompts, and imports now have an explicit design direction. Commands still need a clear answer to basic questions like identity, transitivity, export, and scope.

6. **Packs have too little control over how they contribute to the `gc` surface area.** Today a command definition mostly implies CLI exposure. We need a model where a pack can define commands without necessarily exporting all of them the same way.

7. **The current model conflates the unit of implementation with the unit of CLI exposure.** A single subsystem may want to share named sessions, providers, prompts, overlays, assets, and helpers while still contributing to more than one user-facing command tree.

8. **Doctor and commands are being designed separately even though they are structurally parallel.** Both are pack-provided operational entrypoints with metadata and assets, but we do not yet have one coherent story for them.

## Current state

This section describes the current behavior so we have a firm reference point while redesigning it.

### Definition

A V1 pack may declare commands directly in `pack.toml`:

```toml
[[commands]]
name = "status"
description = "Show pack status"
long_description = "commands/status-help.txt"
script = "commands/status.sh"
```

The implementation currently treats commands as script-backed leaves with minimal metadata:

- `name`
- `description`
- `long_description`
- `script`

### Discovery and registration

At CLI startup, `gc`:

1. loads city config
2. collects all resolved pack directories
3. reads `[[commands]]` from each `pack.toml`
4. groups entries by the pack's `[pack].name`
5. registers a top-level namespace command for each pack

That means the visible CLI shape is:

```text
gc gastown status
gc dolt logs
gc dolt sql
```

### Invocation model

Each command currently runs a script from the providing pack directory with:

- passthrough argv
- passthrough stdio
- a small set of city and pack environment variables

This is intentionally simple and has worked well as an escape hatch for pack-specific operational tooling.

### Current limits

The current model does **not** yet answer:

- whether commands belong to a pack definition or an import binding
- whether commands are re-exported transitively
- how aliases should affect command names
- whether the same pack imported twice produces two command namespaces
- whether city-local commands and imported commands should follow the same naming rules
- whether rig imports can expose rig-specific commands
- whether packs may contribute to top-level command trees such as `gc import ...`
- how commands and doctor checks should share structure

## Design principles

The redesign should follow the same principles as Pack/City v.next.

### 0. Command behavior must be loader-shaped in V2, not retrofitted afterward

This redesign has to absorb the import and loader changes rather than bolting commands on afterward.

That means:

- the loader should discover commands as part of pack loading
- the composed command surface should be derived from the import graph
- city-pack commands and imported-pack commands should be handled by one model
- the CLI should register what the composed model exposes, not rediscover a separate pack-dir view later

If the V2 loader says "this city imports `gs` and re-exports `maintenance` but not its commands", the command surface should follow that model directly.

### 1. Commands are defined by packs, but exposed through imports

A pack owns its command definitions. A city or pack import decides how those commands become visible in the composed CLI surface.

This is the central shift:

- V1 thinking: "the `gastown` pack creates `gc gastown ...`"
- V2 thinking: "the `gastown` import binding exposes a command namespace"

### 2. Import binding is the public namespace

If a pack is imported as:

```toml
[imports.gs]
source = "./packs/gastown"
```

then its commands should be exposed under `gs`, not under the pack's internal `[pack].name`.

The user-facing CLI surface should reflect how the city was composed.

### 3. Command structure should be model-shaped, not cache-shaped

The loader and CLI may still materialize packs into directories and run scripts from those directories, but that should be an implementation detail. The conceptual model should be expressed in terms of imports, bindings, and exported command surfaces.

### 4. Commands should obey the same closure rules as other imported content

If V2 imports are closed by default, commands should be closed by default too. A pack's internal imports should not silently enlarge the consumer's CLI surface unless that export is explicit.

### 5. Simple invocation is still a feature

The current script-backed execution model is useful and should be preserved unless there is a strong reason to replace it. Richer metadata can be added later without discarding the core model.

The important subtlety is that "script-backed" does not imply a special top-level `scripts/` surface. In V2, command entrypoints can live under `commands/`, and arbitrary helper executables can live under the opaque `assets/` directory and be referenced by relative path.

### 6. Every pack can define commands, including the root city pack

The city pack is still a pack. It should not need a special one-off mechanism for commands just because it is the root of composition.

The model should support:

- commands defined by imported packs
- commands defined by transitive imported packs when explicitly re-exported
- commands defined by the root city pack itself

The difference between those cases should be one of exposure and namespace, not one of entirely separate mechanisms.

### 7. Packs need explicit control over how they contribute to CLI surface area

Defining a command and exposing a command should be related, but not identical, concepts.

We likely need a way for a pack to say some combination of:

- this command exists for local/internal use
- this command should be exposed when the pack is directly imported
- this command should remain hidden unless explicitly re-exported
- this pack prefers a particular namespace or exposure style, subject to importer control

The importer still owns final composition, but the pack should be able to express intent.

### 8. Command placement and implementation should be separable

One pack may reasonably implement multiple outward-facing features if they want to share:

- named sessions
- providers
- prompts
- overlays
- local assets
- helper agents

That means we should not force "one pack = one command root" as a hard rule.

A pack's implementation boundary and its CLI placement boundary are related, but they are not the same concept.

### 9. Commands and doctor checks should use one structural pattern

Commands and doctor checks look like sibling surfaces:

- a named operational entry
- an entrypoint
- metadata
- optional help
- local assets

They should be designed together so we do not invent two unrelated conventions for the same kind of thing.

## Proposed direction

This section captures the current state of the new design, not a final locked spec.

### Loader integration

In V2, commands should be loaded as part of the same composition pass that builds the rest of the city model.

Conceptually:

1. the root city pack is loaded
2. its direct imports are resolved and loaded
3. each pack contributes command definitions plus command exposure metadata
4. the loader computes the composed command surface from the import graph
5. the CLI registers commands from that composed command surface

This replaces the current split where:

- the loader resolves packs for runtime behavior
- the CLI separately scans pack directories and reconstructs a command namespace

That separation is acceptable in V1 but becomes the wrong abstraction in V2.

### Command identity

Commands have two identities:

- **definition identity**
  - the command as declared by a pack
- **exposed identity**
  - the command as made available through an import binding

The pack declares:

```text
status
logs
sql
```

The importing city exposes them as:

```text
gc <binding> <command>
```

Examples:

```text
gc gastown status
gc gs status
gc dolt sql
```

where `gastown`, `gs`, and `dolt` are import bindings, not intrinsic pack names.

The root city pack also has command identity, but its exposure rules are separate because it is the root of the CLI surface rather than an imported binding.

### Exposure modes

The current preferred direction is that a command may be placed in one of two broad ways:

1. **binding-scoped**
   - exposed under the import binding
   - example: `gc gs status`
2. **root extension**
   - exposed under an approved top-level command tree
   - example: `gc import add`
   - example: `gc import upgrade`

This keeps the binding model available by default while leaving room for shared product surfaces that are not awkwardly tied to pack boundaries.

### Exposure rules

The current preferred direction is:

1. **Commands are exposed through direct imports.**
2. **Commands are not transitively exposed by default.** This diverges from the general import model, where agents are transitive by default. The reason: CLI surface exposure has higher collision and discoverability risk than agent composition. Agents compose into a qualified namespace; commands compete for user-facing verb space.
3. **Re-export of commands, if supported, should be explicit and follow pack re-export semantics rather than inventing a separate command-only mechanism.**

This keeps command behavior aligned with the broader import model while being deliberately more conservative about transitive exposure.

For binding-scoped commands, the binding is the namespace. For root-extension commands, the top-level tree is the namespace.

### Pack contribution and exposure control

We need a distinction between:

- **command definition**
  - "this pack implements a command"
- **default exposure**
  - "this command is normally exposed when the pack is directly imported"
- **re-export behavior**
  - "this command can or cannot be re-exposed through parent packs"

The exact schema is still open, but the conceptual need is clear: a pack should have more control over how it contributes to `gc` than just "every declared command becomes public."

Possible controls include:

- pack-level policy for command export
- per-command visibility or export flags
- importer-side filtering or remapping

We should be careful not to overdesign this too early, but the design needs a place for it.

### Extension roots

If packs are allowed to contribute under top-level command trees, that should happen through explicit extension points rather than arbitrary attachment to any built-in command.

The likely categories are:

1. **owned roots**
   - built-in command trees not open to pack extension by default
2. **extension roots**
   - top-level trees explicitly intended for pack contribution
3. **binding roots**
   - command trees rooted at an import binding

This avoids a world where any imported pack can quietly attach itself under arbitrary built-in commands.

### Repeated imports and aliasing

If the same pack is imported more than once under different bindings, each binding should be able to expose its own command namespace.

Example:

```toml
[imports.prod]
source = "github.com/example/deploy-pack"

[imports.staging]
source = "github.com/example/deploy-pack"
```

Then both of these may exist:

```text
gc prod status
gc staging status
```

This is a feature, not a bug. The command namespace belongs to the binding.

### City-local commands

The city pack itself should be able to define commands. The exact exposure shape is still open, but the likely options are:

1. expose them as top-level `gc <command>`
2. expose them under an explicit local namespace
3. reserve a distinguished namespace for the root city pack

This needs a deliberate choice because top-level command exposure has different collision and discoverability tradeoffs than imported namespaces.

What is no longer open is whether the city pack can define commands at all: it should.

### Manifest and on-disk shape

The current preferred direction is a directory-shaped command tree with optional manifest metadata, parallel to doctor:

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

Key properties:

- nested directories imply nested command words
- `run.sh` is the default well-known entrypoint
- `help.md` is the default well-known help file when present
- entry-local scripts and help live next to the entrypoint
- command-local assets can live in the same directory
- the simplest case should work without TOML
- `command.toml` should appear only when the default directory-based mapping is not enough

This keeps the filesystem shape aligned with the CLI shape while still allowing a local asset scope for each command leaf.

The intended progression is:

1. **zero-config command**
   - `commands/status/run.sh` becomes `status`
   - `commands/repo/sync/run.sh` becomes `repo sync`
2. **manifest-assisted command**
   - `command.toml` appears only when metadata or an explicit override is needed

### Manifest field direction

The current preferred direction is that the filesystem provides the default command words, and the manifest is an escape hatch rather than the primary source of command placement.

That means:

- nested directories provide the default user-facing command words
- the executable entrypoint usually should not need an explicit TOML field when the default `run.sh` is present
- the manifest itself should be optional in the simple case
- if we later need an override field, it should be introduced deliberately and only for cases the directory layout cannot express cleanly

This keeps the common case obvious from the tree while preserving room for explicit metadata when the defaults are not enough.

### Import command examples

Import commands are useful stress tests because they can be modeled either as:

- two different packs
- one shared implementation pack contributing to the same top-level command tree

#### Case 1: separate packs

```toml
[imports.import_add]
source = "https://github.com/gastownhall/gc-import-add"

[imports.import_upgrade]
source = "https://github.com/gastownhall/gc-import-upgrade"
```

Commands in the add pack:

```text
commands/import/add/run.sh
```

Commands in the upgrade pack:

```text
commands/import/upgrade/run.sh
```

Result:

```text
gc import add
gc import upgrade
```

This is the cleanest case when the user-facing feature split and the
pack split are the same.

Nested commands should work naturally by directory shape too, for example:

```text
commands/repo/sync/run.sh
```

becoming:

```text
gc <binding> repo sync
```

with `command.toml` only added when the default mapping is not enough.

#### Case 2: one shared implementation pack

```toml
[imports.packman]
source = "https://github.com/gastownhall/gc-packman"
```

If this pack contributes only under its binding, the result would be:

```text
gc packman import add
gc packman import upgrade
```

If this pack is allowed to contribute to extension roots, it could instead define entries that land under:

```text
gc import add
gc import upgrade
```

This is the reason command placement and pack implementation should be separable.

### Execution model

The current default assumption is that commands remain script-backed:

```text
commands/status/
└── run.sh
```

The loader/CLI should:

- treat `run.sh` as the default entrypoint when present
- allow an explicit override only when the default is not sufficient
- resolve command-local references relative to the manifest file when a manifest exists
- otherwise resolve them relative to the command entry directory
- run the resolved entrypoint with pack and city context

This keeps pack commands lightweight and operationally useful while leaving room for richer metadata later.

For entry-local manifests, the more specific preferred rule is:

- default `run.sh` resolves from the command leaf directory
- explicit `run = "..."` overrides, if supported, also resolve relative to the command leaf directory
- other command-local asset references resolve relative to the command leaf directory
- `..` is allowed as long as the normalized path stays inside the same pack root

Without a manifest, the equivalent default rule is:

- the command leaf directory is the base
- the relative path from `commands/` to that leaf provides the default command words
- `run.sh` is the default entrypoint

That preserves locality while keeping the pack boundary strict.

### Directory conventions

This proposal assumes command definitions snap to the V2 pack directory conventions described in [doc-directory-conventions.md](doc-directory-conventions.md).

In particular:

- commands belong under `commands/`
- doctor checks should be designed in tandem with commands as sibling operational surfaces
- command assets should live with commands rather than being scattered through unrelated directories
- the city pack and imported packs should follow the same on-disk conventions
- arbitrary helper files should live under `assets/` rather than requiring a special pack-wide `scripts/` convention

The command model should not require a special V1-only directory layout.

## Open questions

These are the main unresolved design questions as of this draft.

### 1. What exact syntax should command exposure use?

The current favored shape is:

```text
gc <binding> <command> [args...]
```

Open alternatives:

- `gc command <binding> <command>`
- `gc <binding>:<command>`
- `gc run <binding>.<command>`

The default should probably stay close to today's `gc <namespace> <leaf>` shape unless we uncover a strong reason to centralize under a dedicated verb.

### 2. How should city-pack commands be exposed?

Options include:

1. top-level commands
2. a reserved namespace such as `gc city <command>`
3. a namespace derived from the root pack name

This is mostly a question of ergonomics versus collision safety.

### 2a. How much control does a pack get over exposure?

We still need to decide where the main knobs live:

1. only importer-side
2. mostly pack-side with importer override
3. a mix of pack defaults plus importer policy

My current leaning is pack expresses defaults, importer owns final exposure.

### 2b. Which top-level command trees are extension roots?

If packs may contribute under top-level trees, we need to decide which roots are open for extension and which are protected.

Current leaning:

- not every built-in root should be implicitly extendable
- extension roots should be explicit
- binding-scoped exposure should always remain available

### 3. Should command re-export flatten or preserve provenance?

If pack `A` re-exports pack `B`, and `B` provides a `status` command, should the user see:

```text
gc a status
```

or should the nested provenance remain visible somehow?

This is the same family of question as re-export naming for agents and formulas and should likely be answered consistently.

### 4. Are rig-level commands in scope?

If city imports can expose commands, should rig imports also expose commands? If yes, what is the addressing form?

Candidates:

- `gc <rig> <binding> <command>`
- `gc rig <rig> <binding> <command>`
- commands remain city-scoped only

My current leaning is to keep commands city-scoped first and only add rig-scoped command exposure if a real use case demands it.

### 5. What metadata should commands support beyond script execution?

Possible future fields:

- argument schema
- machine-readable help
- provider/runtime requirements
- working directory policy
- output mode
- interactive vs non-interactive declaration

For now, the question is whether we should keep the schema deliberately small in V1 of this redesign and add structure later.

### 5a. Do commands remain `[[commands]]` in `pack.toml`, or do they become convention-based?

The broader V2 direction prefers filesystem convention over explicit TOML declaration where possible.

Open options:

1. keep `[[commands]]` in `pack.toml`
2. define commands by per-entry directories under `commands/`
3. use convention for discovery plus optional small per-entry manifests

This question now depends on the directory-conventions doc and should be answered consistently with agents, formulas, scripts, and doctor checks.

### 5b. Should commands and doctor checks share one underlying shape?

Current leaning: yes.

They look like two variants of the same structural concept:

- a named operational surface
- an entrypoint
- metadata
- help text
- local assets

If they do share one shape, we should avoid making one convention for `commands/` and a completely different one for `doctor/`.

### 5c. What should the manifest field names be?

In particular:

- do we need an override field for user-facing command words?
- what do we call placement mode?
- what do we call extension-root targeting?

Current leaning:

- do not require a command-words field for the common case
- let directory shape provide the default command words
- use filename convention for obvious local files like `run.sh` and `help.md` when possible
- add an explicit override field only if a real placement need emerges that directory shape cannot express well

### 6. How should collisions be handled?

We need explicit rules for:

- imported binding colliding with a core `gc` command
- city-local command colliding with a core command
- command name collisions inside a single namespace
- collisions introduced by re-export

The likely direction is:

- core commands always win
- namespace collisions are load-time errors
- leaf collisions within a namespace are errors unless one source is explicitly shadowed by the composition model

That still needs to be specified carefully.

## Non-goals

This doc is not currently trying to solve:

- package discovery or catalog surfaces
- remote fetch and lockfile behavior in detail
- a full structured command-argument DSL
- provider-specific command execution abstractions
- replacement of ordinary shell scripts for operational tasks

Those may connect to command design later, but they are not the focus of this note.

## Working draft summary

The current design direction is:

1. packs define commands
2. the loader composes commands through the import graph
3. imports expose commands
4. import bindings, not pack names, define the command namespace
5. every pack, including the root city pack, can define commands
6. packs need explicit control over how they contribute to `gc` surface area
7. command placement and pack implementation should be separable
8. some top-level command trees may become explicit extension roots
9. commands and doctor checks should be designed as sibling operational surfaces
10. commands are closed by default with the rest of the import model
11. script-backed execution remains the default starting point, with `run.sh` and `help.md` as likely default local conventions and without requiring a special pack-wide `scripts/` directory

The main unresolved areas are command exposure syntax, city-pack exposure, extension-root policy, per-pack exposure control, manifest field naming, convention-vs-TOML declaration, the shared command/doctor shape, re-export behavior, rig scope, and collision policy.

---END ISSUE---
