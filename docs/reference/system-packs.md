---
title: System Packs
description: Built-in packs bundled with gc and included explicitly in city.toml.
---

# System Packs

Gas City ships with a small set of built-in packs. These packs are bundled
with the `gc` binary and materialized into each city under
`.gc/system/packs/`.

Built-in packs are not implicit: nothing splices them into config composition
at load time. They compose only through explicit includes in `city.toml`,
which `gc init` writes for you:

```toml
[workspace]
includes = [".gc/system/packs/core", ".gc/system/packs/bd"]
```

The `bd` entry is written only for cities using the `bd` beads provider (the
default); cities on other providers get only `core`. The `bd` pack pulls in
the `dolt` pack transitively via its own `[imports.dolt]`, so dolt never
needs its own include.

The built-in packs are not public registry dependencies. Do not replace the
includes with `[imports.*]` entries; the canonical include paths above are
the supported composition surface.

## Core Pack

The bundled `core` pack is materialized here after `gc init` or `gc start`:

```
.gc/system/packs/core
```

It contributes the baseline behavior that helps agents operate in a Gas City
workspace:

| Area | What `core` contributes |
|---|---|
| **Skills** | `gc-*` skills that teach agents how to use Gas City workflows and commands. |
| **Prompts** | Default worker prompt assets. |
| **Formulas** | Core formulas such as `mol-do-work`, `mol-scoped-work`, and related workflow formulas. |
| **Orders** | Mechanical housekeeping orders folded in from the former `maintenance` pack — `gate-sweep`, `orphan-sweep`, `cross-rig-deps`, `order-tracking-sweep`, `spawn-storm-detect`, `prune-branches`, `wisp-compact`, `nudge-mail-sweep`, `nudge-on-route`, `cascade-nudge-on-blocker-close` — plus built-in orders such as `beads-health`. |
| **Doctor checks** | The `check-binaries` check (reported by `gc doctor` as `core:check-binaries`), which verifies the binaries the housekeeping orders need. |
| **Provider overlays** | Per-provider hook and instruction overlays for supported coding agents. |

The `core` pack deliberately ships no agents. Packs that need long-lived
utility workers define their own — for example, the `gastown` pack owns the
`dog` utility pool and the `mol-shutdown-dance` formula, and the `dolt` pack
ships its own separate `dog` agent for Dolt maintenance formulas.

## Doctor Repair And Migration

`gc doctor` includes a fixable check named `builtin-pack-includes`. It flags
required built-in includes missing from `city.toml` and stale includes that
point at the retired `.gc/system/packs/maintenance` pack, and
`gc doctor --fix` repairs `city.toml`.

For existing cities created before the explicit-include model, run
`gc doctor --fix` once: it adds the missing includes and removes stale
`maintenance` references. Stale `.gc/system/packs/maintenance` directories
are pruned automatically by materialization.

Config load still self-heals the materialized `.gc/system/packs` content on
disk, and emits a once-per-city warning when a required built-in include is
missing from `city.toml`.

## Inspect The Files

To inspect the exact core-pack files your city received:

```
$ find .gc/system/packs/core -maxdepth 2 -type f | sort
$ sed -n '1,120p' .gc/system/packs/core/pack.toml
```

The materialized files are implementation assets owned by `gc`. They are useful
for learning and debugging, but local edits are not a stable customization
surface. Put custom behavior in your own city files or packs instead.

## Related Commands

Some commands show the artifacts after the system pack is loaded:

| Command | What it reveals |
|---|---|
| `gc skill list` | Skills contributed by loaded packs, including `core.gc-*` skills. |
| `gc formula list` | Available formulas, including formulas from system packs. |
| `gc order list` | Available orders, including orders from system packs. |

`gc pack registry ...` commands discover public registry entries. They do not
make the built-in `core` pack a registry dependency.
