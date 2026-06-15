---
title: Builtin Packs
description: Built-in packs bundled with gc and composed via pinned imports.
---

# Builtin Packs

Gas City ships with a small set of built-in packs. These packs are embedded
in the `gc` binary and served from the user-global pack cache under
`$GC_HOME/cache/repos/` (default `~/.gc/cache/repos/`). Nothing is
materialized into the city; the binary pre-seeds the cache with its own
embedded content at each pack's canonical pinned commit, so the pinned
imports below resolve offline.

Built-in packs are not implicit: nothing splices them into config composition
at load time. They compose only through explicit pinned imports in
`pack.toml`, which `gc init` writes for you:

```toml
[imports.core]
source = "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core"
version = "sha:<pinned commit>"

[imports.bd]
source = "https://github.com/gastownhall/gascity.git//examples/bd"
version = "sha:<pinned commit>"
```

`gc init` also writes a matching `packs.lock`. The canonical pin is served
offline from the binary's embedded copy; pinning a bundled source at any
other commit makes it an ordinary remote import; `gc import install`
fetches that exact commit from git, so editing the pin always does what it
says.

The `bd` entry is written only for cities using the `bd` beads provider (the
default); cities on other providers get only `core`. The `bd` pack pulls in
the `dolt` pack transitively via its own `[imports.dolt]`, so dolt never
needs its own import. The `gastown` and `gascity` packs are also bundled but
never required -- they arrive via the templates that use them (`gc init`
gastown/gascity options) or an explicit import.

## Core Pack

The bundled `core` pack contributes the baseline behavior that helps agents
operate in a Gas City workspace:

| Area | What `core` contributes |
|---|---|
| **Skills** | `gc-*` skills that teach agents how to use Gas City workflows and commands. |
| **Prompts** | Default worker prompt assets. |
| **Formulas** | Core formulas such as `mol-do-work`, `mol-scoped-work`, and related workflow formulas. |
| **Orders** | Mechanical housekeeping orders folded in from the former `maintenance` pack, plus built-in orders such as `beads-health`. |
| **Doctor checks** | The `check-binaries` check (reported by `gc doctor` as `core:check-binaries`), which verifies the binaries the housekeeping orders need. |
| **Provider overlays** | Per-provider hook and instruction overlays for supported coding agents. |

The `core` pack deliberately ships no agents. Packs that need long-lived
utility workers define their own -- for example, the `gastown` pack owns the
`dog` utility pool and the `mol-shutdown-dance` formula, and the `dolt` pack
ships its own separate `dog` agent for Dolt maintenance formulas.

## Doctor Repair And Migration

`gc doctor` includes a fixable check named `builtin-pack-imports`. It flags
required built-in imports missing from the composed config and legacy
`workspace.includes` entries that point at the retired per-city
`.gc/system/packs` tree, and `gc doctor --fix` migrates the city: it strips
the legacy includes from `city.toml`, adds the missing pinned imports to
`pack.toml` (creating a minimal one for legacy cities), and refreshes
`packs.lock` plus the cache.

Config load re-seeds the user-global cache for canonically pinned bundled
sources, prunes any leftover `.gc/system/packs` tree, and emits a
once-per-city warning when a required built-in import is missing from the
composed config. Bundled sources pinned at non-canonical commits are left
to `gc import install` like any other remote import.

## Inspect The Files

To inspect the exact core-pack files your city composes, resolve the import
and look inside the cache:

```shell
$ gc import check
$ find "$(gc config show --json | jq -r '.pack_dirs[] | select(test("packs/core"))')" -maxdepth 2 -type f | sort
```

The cached files are implementation assets owned by `gc`. They are useful
for learning and debugging, but local edits are not a stable customization
surface (the binary restores its embedded content). Put custom behavior in
your own city files or packs instead.

## Related Commands

Some commands show the artifacts after the builtin packs are loaded:

| Command | What it reveals |
|---|---|
| `gc skill list` | Skills contributed by loaded packs, including `core.gc-*` skills. |
| `gc formula list` | Available formulas, including formulas from builtin packs. See the [Formula Specification](/reference/specs/formula-spec-v2#11-file-naming-and-layers). |
| `gc order list` | Available orders, including orders from builtin packs. See [Tutorial 07 - Orders](/tutorials/07-orders). |

`gc pack registry ...` commands discover public registry entries. They do not
make the built-in `core` pack a registry dependency.
