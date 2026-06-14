---
title: "Shareable Packs"
description: Create, import, and customize Gas City packs.
---

A pack is a portable definition of behavior: agents, prompt templates,
providers, formulas, orders, commands, doctor checks, overlays, skills, and
other reusable assets. A city is the root pack plus a `city.toml` deployment
file and machine-local `.gc/` bindings.

Packs separate three concerns:

- `pack.toml` and pack directories define what the system is.
- `city.toml` defines how this deployment runs.
- `.gc/` stores local site bindings and runtime state managed by `gc`.

Legacy include and pack registry fields may still load for migration
compatibility, but new docs and new packs should use imports and
`agents/<name>/` directories.

## Pack Layout

Pack structure is convention-based. Standard directories are loaded by name;
opaque helper files belong under `assets/`.

```text
code-review-pack/
├── pack.toml
├── agents/
│   └── reviewer/
│       ├── agent.toml
│       └── prompt.template.md
├── formulas/
│   └── review-change.toml
├── orders/
│   └── nightly-review.toml
├── commands/
│   └── status/
│       ├── help.md
│       └── run.sh
├── doctor/
│   └── check-review-tools/
│       └── run.sh
├── overlay/
├── skills/
├── mcp/
├── template-fragments/
└── assets/
    └── scripts/
        └── setup-reviewer.sh
```

## Minimal `pack.toml`

Pack metadata and imports live in `pack.toml`. Agent definitions live in
`agents/<name>/`.

```toml
[pack]
name = "code-review"
schema = 2
version = "1.0.0"

[agent_defaults]
provider = "claude"
scope = "rig"
```

`schema = 2` is the current pack format. `[agent_defaults]` applies to
agents discovered from `agents/` unless an agent's own `agent.toml` overrides a
field.

## Agent Directories

A minimal agent is just a directory with a prompt:

```text
agents/reviewer/
└── prompt.template.md
```

Use `agent.toml` for fields that differ from pack defaults:

```toml
# agents/reviewer/agent.toml
scope = "rig"
nudge = "Check your hook, review the assigned change, and leave findings."
idle_timeout = "30m"
min_active_sessions = 0
max_active_sessions = 3
pre_start = ["{{.ConfigDir}}/assets/scripts/setup-reviewer.sh {{.RigRoot}}"]
```

Prompt file discovery prefers `prompt.template.md`. `prompt.md` and
`prompt.md.tmpl` are accepted for compatibility.

## Imports

Packs compose other packs with named imports. Imports preserve provenance, so
consumers can distinguish `gastown.polecat` from `review.polecat`.

```toml
[imports.review]
source = "../code-review"
```

Local imports use a path relative to the importing pack. Remote imports use
`source` plus an optional `version` constraint. For GitHub-hosted packs below a
repository root, prefer the same `/tree/<ref>/<path>` URL a browser can open:

```toml
[imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9"
```

Do not write registry handles such as `main:gastown` into `pack.toml`. Registry
handles are command-time lookup shortcuts; authored pack TOML stores the
resolved durable `source` and, when needed, `version`.

Packs own their agents. Collision detection keys on the binding-qualified
name, so two imports that each define a `polecat` agent coexist as
`gastown.polecat` and `review.polecat`. Composition fails with a
duplicate-agent error only when two source directories produce the same
qualified name on the same surface — for example, two unbound legacy includes
that both define `polecat` — and there is no fallback-agent resolution.

The `[imports.<name>]` key is the local binding chosen by the importing pack.
An imported pack's own name, or the name displayed in a registry, is display
metadata and a suggested binding only. It does not override the import
binding.

## Registry Discovery

Registries help you find packs, but they do not change the authored import
shape. The registry commands available in this release cover discovery, cache
management, authentication, and publish submission:

```text
gc pack registry add example https://raw.githubusercontent.com/gastownhall/gascity-packs/main/registry.toml
gc pack registry refresh main
gc pack registry search gastown
gc pack registry show main:gastown
gc pack registry login
gc pack registry publish .
gc pack registry whoami
gc pack registry list
gc pack registry remove example
```

When a registry entry is used to add or migrate a pack, the durable
`pack.toml` entry stores the entry's resolved `source` and optional `version`,
not the registry handle.

The first public registry is the `gascity-packs` catalog, configured by default
as the built-in `main` registry, so there is nothing to add:

```text
gc pack registry refresh main
gc pack registry search gascity
gc pack registry show main:gascity
```

Registry caches are local. Search and show warn when a registry cache is older
than the freshness window. The default window is 24 hours. Set
`GC_REGISTRY_FRESHNESS` to a positive Go duration string, such as `1h` or
`30m`, to change that warning window. Invalid, zero, or negative values warn.
Pass `--refresh` to `gc pack registry search` or `gc pack registry show` when
you want that command to fetch the latest catalog before reading it.

See [Public Registry Packs](/guides/registry-showcase) for the first-party
packs currently advertised through the public registry.

## City Usage

A city imports packs at the root pack level and declares deployment details in
`city.toml`.

```toml
# pack.toml
[pack]
name = "bright-lights"
schema = 2

[imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9"

[imports.review]
source = "./assets/code-review"
```

```toml
# city.toml
[beads]
provider = "bd"

[[rigs]]
name = "backend"
max_active_sessions = 4
default_sling_target = "backend/gastown.polecat"

[defaults.rig.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9"
```

Machine-local rig paths are site bindings managed by `gc`:

```bash
gc rig add ~/src/backend --name backend
```

## Rig-Level Imports

Use rig-level imports when only one rig should receive a pack's agents or
formulas.

```toml
[[rigs]]
name = "backend"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:fa91a3b4f1fe5cc9d1ba9ffbdd2d26274680adf9"

[rigs.imports.review]
source = "./assets/code-review"
```

Rig-level imports create rig-scoped identities such as
`backend/gastown.polecat` and `backend/review.reviewer`.

Gas City's built-in packs are not implicit. `gc init` writes explicit
pinned imports into `pack.toml` (`core`, plus `bd` for bd-provider
cities), and `gc doctor --fix` repairs
missing or stale entries. The former `maintenance` pack no longer exists; its
housekeeping orders ship in the bundled `core` pack. See
[System Packs](/reference/system-packs) for details.

## Named Sessions

Packs can declare sessions that should exist independent of current work.

```toml
[[named_session]]
template = "mayor"
scope = "city"
mode = "always"

[[named_session]]
template = "polecat"
scope = "rig"
mode = "on_demand"
```

The `template` is an agent name from the same pack or an imported qualified
name when needed.

## Customizing Imported Agents

Use patches to modify imported agents without redefining them.

```toml
[[patches.agent]]
name = "gastown.mayor"
provider = "codex"
idle_timeout = "2h"

[patches.agent.env]
GC_MODE = "coordination"
```

For rig-specific customization, patch under the rig:

```toml
[[rigs]]
name = "backend"

[[rigs.patches]]
agent = "gastown.polecat"
provider = "gemini"

[rigs.patches.pool]
max = 8
```

## Formula and Order Files

Formula files go in `formulas/` and order files go in `orders/`. No
`[formulas].dir` declaration is needed for packs.

```text
formulas/
└── review-change.toml

orders/
└── nightly-review.toml
```

When multiple packs provide the same formula name, the importing pack wins over
its imports. Rig-level imports can override city-level formulas for that rig.

## Compatibility Notes

The loader still exposes some V1 fields for migration and old city support:

- `workspace.includes`
- `[[rigs]].includes`
- `[packs.*]`

`[formulas].dir` is not among them: it does not load at all. A
`[formulas].dir` declaration is a hard parse error in `city.toml`, in every
config fragment, and in `pack.toml` (`[formulas].dir is no longer supported;
use the well-known formulas/ directory`), and `gc doctor` reports any
remaining declaration through the fixable `v2-formulas-dir` check. Put
formulas in the well-known `formulas/` directory.

Treat the listed fields as migration surfaces for your own packs, with one exception:
the built-in system packs compose through explicit `workspace.includes`
entries in `city.toml` (`gc init` writes them; `gc doctor --fix` repairs
them). `gc doctor --fix` can migrate root
`pack.toml` legacy inline agent definitions into `agents/<name>/agent.toml`;
legacy agent definitions inside config fragments still need a hand edit. New
shareable packs should use `schema = 2`, `[imports.*]`,
`agents/<name>/`, conventional `formulas/`, and patches for customization.
