---
title: "Create and Share Packs"
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

Packs compose other packs with named imports. The `[imports.<binding>]` key is
the local binding you choose; it qualifies the imported agents' names so
`gastown.polecat` and `review.polecat` coexist. See
[Understanding Packs](/guides/understanding-packs#names) for how bindings,
qualified names, and collisions work — this section is about *authoring* the
imports.

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

## Registry Discovery

Registries help you *find* packs; they never change the authored import shape.
When you add a pack from a registry, `pack.toml` stores the resolved durable
`source` and optional `version`, not the registry handle. The `main` registry
(the public `gascity-packs` catalog) is configured by default:

```text
gc pack registry search gastown
gc pack registry show main:gastown      # prints a paste-ready import command
gc pack registry publish .              # submit a pack (after gc pack registry login)
```

See [Public Registry Packs](/guides/registry-showcase) for the first-party
catalog and cache-freshness controls (`--refresh`, `GC_REGISTRY_FRESHNESS`), and
[Understanding Packs](/guides/understanding-packs#registries-handles-and-sources)
for the handle-vs-source model.

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
template = "builder"
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

Treat the listed fields as migration surfaces for your own packs. `gc doctor
--fix` migrates root `pack.toml` legacy inline agent definitions into
`agents/<name>/agent.toml`; legacy definitions inside config fragments still
need a hand edit. New shareable packs should use `schema = 2`, `[imports.*]`,
`agents/<name>/`, conventional `formulas/`, and patches for customization.
