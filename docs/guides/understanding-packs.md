---
title: "Understanding Packs"
description: Learn what packs are, how imports work, and how Gas City turns reusable pack files into city behavior.
---

# Understanding Packs

Every reusable capability in Gas City comes from a pack. A pack tells Gas City
what can be loaded: agents, named sessions, formulas, skills, commands, MCP
configuration, defaults, and the files those definitions need while running.

This guide has two parts:

- The pack model: what a pack is, where its definitions live, and how imports
  become city behavior.
- The registry workflow: how to find a pack with `gc`, inspect it, write the
  import, and validate the result.

The [pack specification](/specs/pack-spec) is the public source of truth for
the exact format. This guide explains the same model in a more practical style.

## The Pack Model

A pack is a directory with a `pack.toml` file. Only `pack.toml` is required,
but most useful packs also include agents, prompt templates, formulas,
commands, doctor checks, skills, MCP configuration, or support files.

Here is a small pack:

```text
review-pack/
  pack.toml
  agents/
    reviewer/
      agent.toml
      prompt.md
  formulas/
    review.toml
```

The `pack.toml` file names the pack:

```toml
[pack]
name = "review-pack"
schema = 2
version = "1.0.0"
```

The agent definition lives in `agents/reviewer/agent.toml`:

```toml
scope = "city"
provider = "codex"
default_sling_formula = "review"
```

The agent directory gives the agent its local name, `reviewer`. Because the
directory contains `prompt.md`, the loader discovers that prompt by convention.
If another city imports this pack, it does not need to copy `prompt.md`; the
file still belongs to the pack that declared it.

Every city also has a city pack: the pack rooted at the city directory, next to
`city.toml`. In loader and spec language, this is also called the root pack.
The city pack is where the city keeps reusable definitions, imports, and local
pack metadata.

If the loader cannot understand a pack's `schema`, it stops and reports an
error. That is deliberate: it is better to reject a pack whose format is not
understood than to load only part of it.

## Why Import A Pack?

Import a pack when you want to reuse behavior defined somewhere else without
copying its files into your city.

For example, a review pack might provide:

- a `reviewer` agent
- review formulas
- prompt fragments
- setup scripts
- doctor checks

When the city pack imports the review pack, those definitions become available
to the city according to the imported pack's scope rules.

```text
city pack
  pack.toml
    [imports.review] ──► review pack
                         agents/reviewer/
                         formulas/review.toml
```

## Registries, Handles, And Sources

Registries are catalogs for reusable packs. A registry record tells `gc` the
pack name, summary, version metadata, and source.

A registry handle is a short command argument for a pack record. In
`main:gascity`, `main` is the local registry name on this machine and
`gascity` is the pack name inside that registry. `main` is not a keyword in
`pack.toml`; another machine could call the same registry `first-party` or
`work`.

A source is the durable location written into checked-in TOML. Durable means
the import does not depend on this machine's registry name or cache layout. The
registry helps you find the source, but the committed source is what another
machine uses later.

The distinction looks like this:

| Value | Example | Used in |
|---|---|---|
| Registry handle | `main:gascity` | `gc pack registry` commands, such as search and show. |
| Durable source | `https://github.com/gastownhall/gascity-packs/tree/main/gascity` | Checked-in import TOML. |

For a GitHub-hosted pack inside a repository, use a browser-dereferenceable
tree URL:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
```

That source tells `gc` to clone the `gastownhall/gascity-packs` repository and
use the `gascity` directory on the `main` branch as the pack root. The same URL
also opens the pack directory in a browser.

## City Imports And Rig Imports

A city-level import belongs to the city pack and appears at the top level of
the city pack's `pack.toml`:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
version = "^0.1"
```

If that pack defines a city-scoped agent named `planner`, the runtime agent is
named:

```text
planner
```

A rig-level import appears under the `[[rigs]]` table that needs it:

```toml
[[rigs]]
name = "checkout-service"
path = "../checkout-service"

[rigs.imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
version = "^0.1"
```

If that same pack defines a rig-scoped agent named `planner`, the runtime agent
is stamped with the rig name:

```text
checkout-service/planner
```

The rig `name` becomes the identity prefix. The rig `path` is the filesystem
location of the project. These are different pieces of information.

## Agent Scope

An agent in a pack can say where it is allowed to load.

```toml
scope = "city"
provider = "codex"
prompt_template = "prompt.md"
```

The `scope` field has three useful states.

| Scope | Meaning |
|---|---|
| omitted | The agent is eligible for city-level and rig-level loading. |
| `city` | The agent loads only when the pack is imported at the city level. |
| `rig` | The agent loads only when the pack is imported for a rig. |

The scope says where the definition is available. It does not name a particular
rig. A rig-scoped agent becomes part of a rig only when that rig imports the
pack.

## Names

Agent names are local names. Import bindings do not become part of runtime
agent names.

If a city imports this dependency:

```toml
[imports.review_tools]
source = "../packs/review"
```

and the imported pack defines `agents/reviewer/agent.toml`, the runtime name is:

```text
reviewer
```

Gas City uses the binding to find and order dependencies while loading config.
It does not use the binding as a runtime namespace.

If two packs define city-level agents with the same name, config load fails
after fallback rules are applied. The same rule applies inside a single rig.
Give one of the agents a different public name, or avoid importing both
definitions onto the same surface.

## Defaults And Patches

Defaults fill in blanks after packs have loaded. They are city policy, so
`[agent_defaults]` belongs in `city.toml`.

```toml
[agent_defaults]
default_sling_formula = "review"
```

This default applies only to agents whose `default_sling_formula` is still
blank. If a pack explicitly sets the field on an agent, the explicit value
wins.

A patch changes an agent that already exists. It does not create a new agent.

```toml
[[patches.agent]]
name = "reviewer"
provider = "codex"
session_setup_append = ["tmux set status-left '[review]'"]
```

For a rig-scoped agent, use `dir` to select the rig identity prefix:

```toml
[[patches.agent]]
dir = "checkout-service"
name = "reviewer"
provider = "codex"
```

Here, `dir` is the rig name, not the rig path.

## Loading Order

The loader applies packs, patches, and defaults in a deterministic order. The
details matter when two layers set the same field.

In simplified form, loading works like this:

```text
1. Read `city.toml` and the city pack.
2. Load imported packs.
3. Apply pack-level agent patches inside each pack load.
4. Load city-level imports.
5. Apply city-level patches.
6. Load rig-level imports and stamp rig agents.
7. Apply rig overrides.
8. Apply pack globals.
9. Apply city agent defaults to fields that are still blank.
```

The later operation wins for replacement-style fields. Defaults are last, but
they only fill blanks, so they do not override explicit values from earlier
layers.

## Choosing Where To Put A Change

When you customize a pack, choose the narrowest place that expresses what you
mean.

| If you want to... | Put it here |
|---|---|
| Reuse another pack | `[imports.<binding>]` with `source` and optional `version`. |
| Make a city-wide local policy | `city.toml` defaults or patches. |
| Change one city-level imported agent | `city.toml` `[[patches.agent]]`. |
| Change one rig-level imported agent | The rig's `[[rigs.overrides]]` or a targeted city patch with `dir`. |
| Ship reusable behavior | The pack's own definitions and support files. |
| Pin an exact resolved dependency | The lockfile, not the authored import. |

## Pack CLI
### Find A Pack

Use registry search when you know the kind of capability you want but not the
exact pack name.

```text
$ gc pack registry search gascity
```

Example output:

```text
PACK          VERSION  SUMMARY
main:gascity  0.1.0    Gas City planning and implementation workflow pack
```

The `PACK` column is a registry handle for `gc pack registry` commands. To
inspect that record:

```text
$ gc pack registry show main:gascity
```

Example output:

```text
Pack: main:gascity
Version: 0.1.0
Source: https://github.com/gastownhall/gascity-packs/tree/main/gascity
Summary: Gas City planning and implementation workflow pack
```

The `Source` line is the durable source to commit in TOML. The registry handle
stays out of the file.

### Install Or Check Imports

After changing remote imports, install or repair the imported pack cache:

```text
$ gc import install
```

That command resolves the declared imports, writes or repairs `packs.lock`, and
materializes the imported packs in the local cache.

Use `gc import check` when you want a read-only validation pass:

```text
$ gc import check
```

`gc import check` reports missing, stale, or uncached import state and points
back to `gc import install` when repair is needed. Registry commands remain
discovery commands; they do not install or sync the authored import graph.

After install/check succeeds, validate the composed configuration.

```text
$ gc config show --validate
```

Then inspect the part of the city you expect the pack to provide. For example:

```text
$ gc config show | rg 'planner'
```

### Versioning And Locking

The `[pack].version` field is pack metadata. Import version selection is
controlled by the importing file and by the lockfile, not by comparing
`[pack].version` directly during load.

With no `version` field, the import says "use this source" and leaves the exact
selected revision to the installer and lockfile:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
```

A semver-style constraint says which compatible releases are acceptable:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
version = "^0.1"
```

An exact SHA pin says which revision must be used:

```toml
[imports.gascity]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

The authored import expresses the source and optional constraint. The lockfile
records the exact resolved dependency state. Once the cache and lockfile are
current, normal city loading uses the local resolved pack instead of
re-fetching the remote source on every load.

### Registry State Is Local

Registry commands manage local discovery state. Pack imports manage shared city
state.

| Task | Command or file |
|---|---|
| See configured catalogs | `gc pack registry list` |
| Refresh cached catalog records | `gc pack registry refresh` |
| Search for a reusable pack | `gc pack registry search` |
| Inspect a registry record | `gc pack registry show` |
| Share a chosen dependency with the team | `[imports.<binding>]` in checked-in TOML |
| Install or repair authored imports | `gc import install` |
| Check installed import state without mutating | `gc import check` |
| Validate the composed city | `gc config show --validate` |

This separation keeps local discovery flexible without making shared config
depend on the names or cache layout of one machine.
