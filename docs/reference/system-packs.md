---
title: System Packs
description: Built-in packs bundled with gc and loaded by the runtime.
---

# System Packs

Gas City ships with a small set of built-in packs. These packs are bundled
with the `gc` binary, materialized into each city under `.gc/system/packs/`,
and loaded by the runtime as system behavior.

The `core` pack is the main built-in pack most users should know about. It is
not a public registry dependency and it should not be added to `city.toml` as a
durable import. Gas City includes it automatically.

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
| **Orders** | Built-in orders such as `beads-health`. |
| **Provider overlays** | Per-provider hook and instruction overlays for supported coding agents. |

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
