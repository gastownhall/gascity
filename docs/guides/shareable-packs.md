---
title: "Shareable Packs"
description: Create, import, and customize PackV2 Gas City packs.
---

A pack is a portable definition of behavior: agents, prompt templates,
providers, formulas, orders, commands, doctor checks, overlays, skills, and
other reusable assets. A city is the root pack plus a `city.toml` deployment
file and machine-local `.gc/` bindings.

PackV2 separates three concerns:

- `pack.toml` and pack directories define what the system is.
- `city.toml` defines how this deployment runs.
- `.gc/` stores local site bindings and runtime state managed by `gc`.

Legacy `includes`, `[packs.*]`, and `[[agent]]` examples may still load for
migration compatibility, but new docs and new packs should use PackV2 imports
and `agents/<name>/` directories.

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
`agents/<name>/`, not in `[[agent]]` tables.

```toml
[pack]
name = "code-review"
schema = 2
version = "1.0.0"

[agent_defaults]
provider = "claude"
scope = "rig"
```

`schema = 2` is the current PackV2 format. `[agent_defaults]` applies to
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
[imports.maintenance]
source = "../maintenance"
export = true
```

Local imports use a path relative to the importing pack. Remote imports use a
source plus a version constraint:

```toml
[imports.gastown]
source = "github.com/gastownhall/gastown"
version = "^1.2"
```

Imports are transitive by default. Set `transitive = false` only when the
import is internal to the pack and should not be visible to consumers.

## City Usage

A city imports packs at the root pack level and declares deployment details in
`city.toml`.

```toml
# pack.toml
[pack]
name = "bright-lights"
schema = 2

[imports.gastown]
source = "./assets/gastown"

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
source = "./assets/gastown"

[rigs.imports.review]
source = "./assets/code-review"
```

Rig-level imports create rig-scoped identities such as
`backend/gastown.polecat` and `backend/review.reviewer`.

## Composing Packs and Work

When you need domain-specific behavior, extend an existing pack instead of
copying it. Import the base pack, add your own agents and formulas next to it,
then patch only the imported definitions that need local policy.

```toml
[pack]
name = "backend-delivery"
schema = 2

[imports.gastown]
source = "../gastown"

[[patches.agent]]
name = "gastown.polecat"
default_sling_formula = "backend-implementation"

[patches.agent.env]
BACKEND_TEST_TARGET = "./..."
```

Put the new behavior in this pack's conventional directories:

```text
backend-delivery/
├── pack.toml
├── formulas/
│   └── backend-implementation.toml
├── template-fragments/
│   └── backend-standards.template.md
└── agents/
    └── reviewer/
        └── prompt.template.md
```

This keeps the imported pack upgradeable. Use unique formula and agent names for
new behavior; use patches when you intentionally want to change an imported
definition.

Work composition follows the same rule: choose the smallest grouping that has a
single lifecycle.

- Use one convoy when the work shares the same owner, target branch, merge
  policy, and landing decision.
- Use separate convoys when different teams, target branches, release gates, or
  owners can finish independently.
- Link separate convoys or beads with `related` or `tracks` dependencies when
  you need visibility without blocking.
- Use a `blocks` dependency only when downstream work must not become ready
  until upstream work closes.

```bash
gc convoy create backend-api --owned --target integration/backend-api
gc convoy create backend-docs --owned --target integration/backend-docs
bd dep add <docs-convoy-or-bead> <api-convoy-or-bead> --type related
```

For formulas, prefer a wisp when the workflow is short-lived and one agent can
drive it from the source bead. Prefer a molecule when individual steps need
their own beads for independent routing, review, recovery, or dashboard
visibility.

If upstream work changes after a workflow already ran, clean up the old workflow
for the source bead, reopen the source, and sling it again:

```bash
gc convoy delete-source <source-bead-id> --apply
gc convoy reopen-source <source-bead-id>
gc sling <target-agent> <source-bead-id> --on <formula>
```

Use `--store-ref city:<name>` or `--store-ref rig:<name>` with
`delete-source` and `reopen-source` when the source bead is not uniquely
resolvable from the current directory.

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
`[formulas].dir` declaration is needed for PackV2 packs.

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
- `[[agent]]`
- `[formulas].dir`

Treat those as migration surfaces. New shareable packs should use PackV2:
`schema = 2`, `[imports.*]`, `agents/<name>/`, conventional `formulas/`, and
patches for customization.
