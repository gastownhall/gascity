---
title: Configure the Gastown Pack
description: Task-oriented config overrides for running the Gastown pack on Gas City — register rigs, scale pools, swap providers, patch agents, and tweak prompts.
---

This page collects the common config edits for the Gastown pack — the changes
you reach for *while editing files*. The conceptual migration story, including
how Gas Town roles and mechanisms map onto Gas City primitives, lives in
[Coming from Gas Town](/getting-started/coming-from-gastown); for the primitives
themselves, see [The six primitives](/getting-started/how-gas-city-works).

![Gastown agents by scope: city-scoped agents (mayor, deacon, boot, dog) load from [imports.gastown]; rig-scoped agents (witness, refinery, polecat) load from a rig import; crew are individually named directory agents plus a named session that you add yourself.](/diagrams/excalidraw-rendered/gastown-agents-by-scope.svg)

## Common Gastown Overrides

If you are using the Gastown pack, these are the most common local changes.

### Register a rig

Import the Gastown pack in the root pack, then bind rigs in `city.toml` and with `gc rig add`:

```toml
# pack.toml
[pack]
name = "my-city"
schema = 2

[imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"
```

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"
```

```bash
gc rig add /path/to/myproject --name myproject
```

### Increase or shrink scalable polecat sessions

This is the cleanest answer to "I want more or fewer polecats for this rig."

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"

[[rigs.patches]]
agent = "gastown.polecat"

[rigs.patches.pool]
max = 10
```

### Change the provider for one rig's polecats

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"

[[rigs.patches]]
agent = "gastown.polecat"
provider = "codex"
```

You can combine that with session scale overrides, env, prompt changes, or hook changes on the same override block.

### Change a city-scoped Gastown agent

City-scoped agents such as `mayor`, `deacon`, and `boot` are easiest to tweak with patches:

```toml
[[patches.agent]]
name = "gastown.mayor"
provider = "codex"
idle_timeout = "2h"
```

Use patches when the target is already a concrete city-scoped agent. Use `[[rigs.patches]]` when the target is a pack agent stamped per rig.

### Add a named crew agent

Crew is usually city-specific, so it often belongs in the root city pack rather than in the shared Gastown pack:

```text
agents/wolf/
├── agent.toml
└── prompt.template.md
```

```toml
# agents/wolf/agent.toml
scope = "rig"
dir = "myproject"
nudge = "Check your hook and mail, then act accordingly."
work_dir = ".gc/worktrees/myproject/crew/wolf"
idle_timeout = "4h"
```

That keeps the shared pack generic while still letting your city have named long-lived workers.

### Change a prompt, overlay, or timeout without forking the pack

This is what rig overrides are for:

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"

[[rigs.patches]]
agent = "gastown.refinery"
idle_timeout = "4h"
```

For prompt or overlay replacement, patch the imported agent from your root city pack rather than editing the shared pack in place.

If that change turns out to be broadly useful across cities, that is when it should move into the pack.

### Default a formula var for one rig

Rig-scoped `formula_vars` fill formula `[vars]` values when a formula runs in
that rig and the caller passed no explicit `--var`. They beat formula-level
defaults and lose to `--var` flags; `gc formula show` renders them as
`(rig default="...")`.

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.formula_vars]
branch = "develop"
```

To layer the same change from a patch, use `[[patches.rigs]]` — the
`formula_vars` merge is additive, so unrelated keys are preserved:

```toml
[[patches.rigs]]
name = "myproject"

[patches.rigs.formula_vars]
branch = "develop"
```

### Change an agent's default sling formula

`default_sling_formula` names the formula sling applies automatically for an
agent. Override it per rig:

```toml
# city.toml
[[rigs]]
name = "myproject"

[[rigs.patches]]
agent = "gastown.polecat"
default_sling_formula = "mol-scoped-work"
```

### Disable or retime a pack order

The Gastown pack ships the `digest-generate` order (cooldown trigger, every
24h). Skip it everywhere it is discovered:

```toml
# city.toml
[orders]
skip = ["digest-generate"]
```

Or retime it with an override. An override with no `rig` matches only the
city-level instance; `rig = "*"` matches every instance; a rig name matches
that rig's instance:

```toml
[[orders.overrides]]
name = "digest-generate"
rig = "*"
interval = "12h"
```

## A Complete Gastown Example

The overrides above are fragments — single edits you splice into an existing
config. This section assembles them into a full, runnable topology: the three
files that express the whole Gastown pack on Gas City.

Read them in order. The **city file** is the normal starting point — the
deployment you boot. The **root pack** wires the Gastown import and the default
rig binding behind it, plus the builtin `core` and `bd` imports `gc init` pins
(`gc doctor --fix` repairs them). The **nested pack** holds the reusable
defaults — the roles, named sessions, and dog pool every Gastown city inherits.

All three use the current pack layout (`schema = 2`, `agents/<name>/`).

### `city.toml` — the deployment

```toml
# Gas Town — expressed as a Gas City configuration.
#
# This proves the Gas City thesis: any orchestration pack is pure config.
# Composable packs:
#   core (builtin) — gc skills, default prompts, core formulas, mechanical
#                    housekeeping orders (gate/orphan/wisp sweeps, branch
#                    pruning, nudge relays); included explicitly below
#   bd (builtin)   — Dolt-backed beads provider; pulls in the dolt pack
#                    (server lifecycle, dog formulas + exec orders + CLI
#                    commands, with its own dolt dog pool)
#   gastown        — domain-specific coding workflow: mayor, deacon, boot,
#                    dog utility pool, witness, refinery, polecat, crew +
#                    digest orders
#
# City-scoped agents: mayor, deacon, boot, and the gastown-owned dog.
# Rig-scoped agents (witness, refinery, polecat) are stamped per-rig.
#
# The sibling pack.toml owns the Gastown import. This city owns the default
# rig binding used by `gc rig add`.
#
# To use: save these three files into a city directory, then run
#   gc start <city-dir>
# Requires rigs to be registered: gc rig add <path>

[workspace]
name = "gastown"
provider = "claude"
global_fragments = ["command-glossary", "operational-awareness"]
# Builtin packs (core, bd) compose through explicit pinned imports in
# pack.toml (gc init writes these; gc doctor --fix repairs them).

[providers.claude]
base = "builtin:claude"

[defaults.rig.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"

[daemon]
patrol_interval = "30s"
max_restarts = 5
restart_window = "1h"
shutdown_timeout = "5s"
# Formulas v2 is the default; this line only makes the choice
# explicit. Set formula_v2 = false only for cities pinned to formula compiler
# v1. v1 molecule formulas keep molecule_id attachment semantics unless
# they declare the v2 requirement.
formula_v2 = true

# Register a rig to activate per-rig agents (witness, refinery, polecat):
# [[rigs]]
# name = "myproject"
# path = "/path/to/your/project"

# Crew members are persistent, individually named directory agents
# (agents/<name>/) plus an always-on named session — see "Add a named crew
# agent" above for the agent.toml and named_session to add here.
```

### `pack.toml` — the root pack

```toml
# Gas Town root pack — wires the gastown pack at both city and rig scope.
#
# City-level: [imports.gastown] expands city-scoped agents (mayor, deacon,
# boot) on city startup.
#
# Rig-level: [defaults.rig.imports.gastown] ensures every new rig
# automatically imports rig-scoped agents (witness, refinery, polecat)
# without hand-editing city.toml.

[pack]
name = "gastown"
schema = 2

# Pinned public registry import; resolved offline from the gc binary's
# bundled copy. The gastown pack is no longer a local directory.
[imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:33d3a430a67d1782ad364556cb566bdb01d0afe3"
```

### The gastown pack — the reusable defaults

The pack itself lives in the gascity-packs registry
(github.com/gastownhall/gascity-packs, `gastown/`); its pack.toml looks
like this:

```toml
# Gas Town — domain-specific coding workflow pack.
#
# Gastown roles: mayor (coordinator), deacon (patrol), boot (watchdog),
# dog (utility pool, owned by this pack), plus rig-scoped agents
# (witness, refinery, polecat). Mechanical housekeeping (gate/orphan/wisp
# sweeps, branch pruning, nudge relays) ships with the builtin core pack.
#
# Imported at both city and rig scope:
#   [imports.gastown] (root pack)              → expands city-scoped agents
#                                                 only (mayor, deacon, boot, dog)
#   [defaults.rig.imports.gastown] / [rigs.imports.gastown]
#                                              → expands rig agents only
#                                                 (witness, refinery, polecat)
#
# Crew members are individually named directory agents (agents/<name>/) plus a
# named session; see the crew member note in the city file above.

[pack]
name = "gastown"
schema = 2

[global]
session_live = [
    "{{.ConfigDir}}/assets/scripts/tmux-theme.sh {{.Session}} {{.Agent}} {{.ConfigDir}}",
    "{{.ConfigDir}}/assets/scripts/tmux-keybindings.sh {{.ConfigDir}}",
]

[[named_session]]
template = "mayor"
scope = "city"
mode = "always"

[[named_session]]
template = "deacon"
scope = "city"
mode = "always"

[[named_session]]
template = "boot"
scope = "city"
mode = "always"

[[named_session]]
template = "witness"
scope = "rig"
mode = "always"

[[named_session]]
template = "refinery"
scope = "rig"
mode = "on_demand"
```
