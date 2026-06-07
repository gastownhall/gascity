---
title: Gastown on Gas City — Config Recipes
description: Task-oriented config overrides for running the Gastown pack on Gas City — register rigs, scale pools, swap providers, patch agents, and tweak prompts.
---

This page collects the common config edits for the Gastown pack — the changes
you reach for *while editing files*. The conceptual migration story, including
how Gas Town roles and mechanisms map onto Gas City primitives, lives in
[Coming from Gas Town](/getting-started/coming-from-gastown).

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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

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
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

[[rigs.patches]]
agent = "gastown.refinery"
idle_timeout = "4h"
```

For prompt or overlay replacement, patch the imported agent from your root city pack rather than editing the shared pack in place.

If that change turns out to be broadly useful across cities, that is when it should move into the pack.

## A Complete Gastown Example

The overrides above are fragments — single edits you splice into an existing
config. This section assembles them into a full, runnable topology: the three
files that express the whole Gastown pack on Gas City.

Read them in order. The **city file** is the normal starting point — the
deployment you boot. The **root pack** wires the Gastown import and the default
rig binding behind it. The **nested pack** holds the reusable defaults — the
roles, named sessions, and dog pool every Gastown city inherits.

All three are PackV2 (`schema = 2`, `agents/<name>/`).

### `city.toml` — the deployment

```toml
# Gas Town — expressed as a Gas City configuration.
#
# This proves the Gas City thesis: any orchestration pack is pure config.
# Three composable packs:
#   maintenance  — generic infrastructure: dog pool, shutdown dance, exec
#                  orders (gate-sweep, orphan-sweep, prune-branches,
#                  wisp-compact)
#   dolt         — reusable Dolt database management (dog formulas + exec
#                  orders + CLI commands), requires a dog pool from
#                  maintenance
#   gastown      — domain-specific coding workflow: mayor, deacon, boot,
#                  witness, refinery, polecat, crew + digest orders
#
# City-scoped agents come from both packs: mayor, deacon, boot, plus the
# effective dog definition from gastown. Maintenance still supplies the
# fallback dog shape and shared dog formulas/prompts that gastown reuses.
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

[providers.claude]
base = "builtin:claude"

[defaults.rig.imports.gastown]
source = "packs/gastown"

[daemon]
patrol_interval = "30s"
max_restarts = 5
restart_window = "1h"
shutdown_timeout = "5s"
# Enable compiler-v2 formulas from imported packs. Legacy molecule formulas keep
# molecule_id attachment semantics unless they declare a compiler-v2 requirement.
formula_v2 = true

# Register a rig to activate per-rig agents (witness, refinery, polecat):
# [[rigs]]
# name = "myproject"
# path = "/path/to/your/project"

# Crew members are persistent, individually named workers, so they can't be
# pack-stamped. Each one is a directory agent under agents/<name>/ plus a
# named session that keeps it alive. To add a crew member "wolf" bound to a
# registered rig "myproject":
#
#   1. Create agents/wolf/agent.toml (relative paths resolve against this
#      city directory):
#
#        scope = "rig"
#        dir = "myproject"
#        nudge = "Check your hook and mail, then act accordingly."
#        work_dir = ".gc/worktrees/myproject/crew/wolf"
#        idle_timeout = "4h"
#        prompt_template = "packs/gastown/assets/prompts/crew.template.md"
#        pre_start = ["{{.CityRoot}}/packs/gastown/assets/scripts/worktree-setup.sh {{.RigRoot}} {{.WorkDir}} {{.AgentBase}} --sync"]
#
#      tmux theming comes from the gastown pack's [global] session_live hooks,
#      so crew members need no session_setup wiring of their own.
#
#   2. Keep the crew session alive by declaring a named session here. The
#      dir must match the agent's so the session resolves to "myproject/wolf":
#
# [[named_session]]
# template = "wolf"
# dir = "myproject"
# scope = "rig"
# mode = "always"
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

[imports.gastown]
source = "packs/gastown"
```

### `packs/gastown/pack.toml` — the reusable defaults

```toml
# Gas Town — domain-specific coding workflow pack.
#
# Gastown roles: mayor (coordinator), deacon (patrol), boot (watchdog),
# plus rig-scoped agents (witness, refinery, polecat).
# Dog (utility pool) is defined here with tmux theming; maintenance provides
# the fallback (unthemed) dog. Mechanical housekeeping lives in maintenance.
#
# Referenced by both workspace.pack and rigs[].pack:
#   workspace.pack → expands city-scoped agents only (mayor, deacon, boot)
#   rigs[].pack    → expands rig agents only (witness, refinery, polecat)
#
# Crew members are individually named directory agents (agents/<name>/) plus a
# named session; see the crew member note in the city file above.

[pack]
name = "gastown"
schema = 2

[imports.maintenance]
source = "../maintenance"

[global]
session_live = [
    "{{.ConfigDir}}/assets/scripts/tmux-theme.sh {{.Session}} {{.Agent}} {{.ConfigDir}}",
    "{{.ConfigDir}}/assets/scripts/tmux-keybindings.sh {{.ConfigDir}}",
]

[[patches.agent]]
name = "dog"
wake_mode = "fresh"
work_dir = ".gc/agents/dogs/{{.AgentBase}}"

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
