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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

Do not write registry handles such as `main:gastown` into `pack.toml`. Registry
handles are command-time lookup shortcuts; authored pack TOML stores the
resolved durable `source` and, when needed, `version`.

Packs own their agents. If two packs composed into the same city or rig
define the same agent name, composition fails with a duplicate-agent error —
there is no fallback-agent resolution. Give pack agents unambiguous names,
or drop one of the conflicting imports.

## Registry Discovery

Registries help you find packs, but they do not change the authored import
shape. The registry commands available in this release are discovery and cache
management commands:

```text
gc pack registry add main https://github.com/gastownhall/gascity-packs.git
gc pack registry refresh main
gc pack registry search gastown
gc pack registry show gastown
gc pack registry list
gc pack registry remove main
```

When a registry entry is used to add or migrate a pack, the durable
`pack.toml` entry stores the entry's resolved `source` and optional `version`,
not the registry handle. Publishing registry content is still a registry-repo
workflow in this wave: edit the registry catalog, review it, and refresh the
local registry cache before searching or showing new entries.

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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

[rigs.imports.review]
source = "./assets/code-review"
```

Rig-level imports create rig-scoped identities such as
`backend/gastown.polecat` and `backend/review.reviewer`.

Gas City's built-in packs are not implicit. `gc init` writes explicit
workspace includes into `city.toml` (`.gc/system/packs/core`, plus
`.gc/system/packs/bd` for bd-provider cities), and `gc doctor --fix` repairs
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
- `[formulas].dir`

Treat those as migration surfaces for your own packs, with one exception:
the built-in system packs compose through explicit `workspace.includes`
entries in `city.toml` (`gc init` writes them; `gc doctor --fix` repairs
them). `gc doctor --fix` can migrate root
`pack.toml` legacy inline agent definitions into `agents/<name>/agent.toml`;
legacy agent definitions inside config fragments still need a hand edit. New
shareable packs should use `schema = 2`, `[imports.*]`,
`agents/<name>/`, conventional `formulas/`, and patches for customization.
