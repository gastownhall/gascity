---
title: "Pack And Registry CLI Surface"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-16 |
| Author(s) | Donna Box, Ag Nora, Ag Noreen |
| Issue | — |
| Supersedes | earlier `gc pack`-only and `pack`/`store` explorations |

This document builds on the **PackV2** work from the 15.0 release and makes no change to the semantics of package format or loading semantics. 

It does however do two things:
1. Introduce the notion of a *registry* where Gas City packs can be published and discovered.
2. Propose a coherent CLI interface across all package operations (discovery, import, upgrade, et al) 

## Registries
A Gas City registry is simply a `registry.toml` file that is typically fetched over HTTP.

A `registry.toml` file is simply a list of packages with a name, version info, description, and the URL of the source.  Registries don't store packs, they simply are an directory of packs. Once the source URL is read from registry.toml, the registry is out of the loop.

A minimal `registry.toml` should have at least a couple of entries so search and
catalog behavior are concrete:

```toml
schema = 1

[[pack]]
name = "maintenance"
description = "Health checks and baseline operational tooling."
source = "https://github.com/gastownhall/maintenance"

  [[pack.release]]
  version = "1.2.0"
  commit = "abc123"
  hash = "sha256:..."
  description = "Adds doctor checks and improves stale-db handling."

[[pack]]
name = "observatory"
description = "Metrics and telemetry helpers."
source = "https://github.com/gastownhall/observatory"

  [[pack.release]]
  version = "0.4.0"
  commit = "def456"
  hash = "sha256:..."
  description = "First public release."
```

A registry can advertise multiple versions of the same pack, with distinct notes on each version:

```toml
[[pack]]
name = "maintenance"
description = "Health checks and baseline operational tooling."
source = "https://github.com/gastownhall/maintenance"

  [[pack.release]]
  version = "1.2.0"
  commit = "abc123"
  hash = "sha256:..."
  description = "Adds doctor checks and improves stale-db handling."

  [[pack.release]]
  version = "1.1.0"
  commit = "def456"
  hash = "sha256:..."
  description = "Stabilizes patrol behavior and stale-db handling."
```

To faciliate east dicovery,  Gas City implementation maintains a list of registries that are consulted when searching for or enumerating available packs. That list of registries is a system managed file ()`~/.gc/registries.toml`) and has the standard `add`/`list`/`remove` operations. A fresh Gas City installation will have one entry in `registries.toml` pointing to the Gac City-managed registry:

```toml
schema = 1

[[registry]]
name = "main"
source = "https://github.com/gastownhall/gascity-packs"
```






## Command Trees

The operations one wants to do wrt managing imports from one package to another have *some* overlap with the operations wants to do on a registry. To that end,  this design relies on two command trees:
* `gc pack` which is focused exclusively on managign package-to-package import graphs
* `gc registry` which is focused on discovery of pacakages based on name, description or version. 

The two work in tandem: the result of a registry search is a qualified name that can be passed directly to the add command that creates the import.

Note that `gc pack` subsumes the functionality of `gc pack` and `gc import` in the 0.15.0 release.

### `gc registry`

```text
gc registry list
gc registry add <registry-name> <source>
gc registry remove <registry-name>
gc registry search [query] [--registry <name>]
gc registry show <qualified-pack-name>
```

### `gc pack`

```text
gc pack add <source-or-name> [--name <import-name>] [--pack <path>] [--rig <name-or-path>]
gc pack remove <import-name> [--pack <path>] [--rig <name-or-path>]
gc pack list [--transitive] [--pack <path>] [--rig <name-or-path>]
gc pack show <import-name> [--pack <path>] [--rig <name-or-path>]
gc pack fetch [<import-name>] [--pack <path>] [--rig <name-or-path>]
gc pack outdated [<import-name>] [--pack <path>] [--rig <name-or-path>]
gc pack upgrade [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

## Current Proposal

This is the cohesive model the mock and the rest of this note now reflect.

- `gc pack` is the local workflow noun
- `gc registry` is the machine-known registry config and catalog-browsing noun
- registries are index only
- caches are fetched content only
- imports declare intent
- full materialization is optional and demand-driven

The overall design should follow npm/pub-like user expectations for pack
workflow, while keeping registry configuration and discovery as a separate
supporting surface.

## `gc pack`

`gc pack` owns imports, fetched state, and upgrade flow for the selected pack.

### Scope model

The baseline target is always the ambient pack discovered from the current
working directory.

Working rules:

- ambient behavior always targets the current pack
- `--pack <path>` targets another pack explicitly
- `--rig <name-or-path>` opts into rig-scoped import behavior
- rig-scoped imports only happen when `--rig` is passed
- `--rig` refines pack behavior; it is never ambient

### Verb set

| Verb | Meaning |
|---|---|
| `add` | Add an import to the selected scope and fetch it by default. |
| `remove` | Remove an import from the selected scope. |
| `list` | List imports in the selected scope. |
| `show` | Show one imported pack in the selected scope. |
| `fetch` | Fetch resolved pack content into cache for the selected scope. |
| `outdated` | Show which imported packs could be upgraded. |
| `upgrade` | Upgrade imported packs in scope and fetch the new resolved result. |

### Signatures and semantics

#### `gc pack add`

```text
gc pack add <source-or-name> [--name <import-name>] [--pack <path>] [--rig <name-or-path>]
```

- adds an import to the selected scope
- fetches the resolved content into cache by default
- accepts:
  - qualified registry names like `main:maintenance`
  - unqualified registry names when resolution is unambiguous
  - direct source URLs
  - local paths
- `--name` gives an explicit local import name when needed

#### `gc pack remove`

```text
gc pack remove <import-name> [--pack <path>] [--rig <name-or-path>]
```

- removes an import from the selected scope
- does not imply eager cache deletion
- remains strict about inbound reference blockers

#### `gc pack list`

```text
gc pack list [--transitive] [--pack <path>] [--rig <name-or-path>]
```

- with no flags, lists direct imports in scope
- with `--transitive`, lists the full resolved transitive set

#### `gc pack show`

```text
gc pack show <import-name> [--pack <path>] [--rig <name-or-path>]
```

- shows one imported pack in the selected scope
- local inspection only
- does not reach out to registry catalog views

Expected output shape:

- imported name
- source
- resolved version or release
- fetched status
- scope

#### `gc pack fetch`

```text
gc pack fetch [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

- with no target, fetches all imports in scope
- with a target, fetches one imported pack in scope
- does not edit imports
- is the explicit warm-cache and reconcile command

#### `gc pack outdated`

```text
gc pack outdated [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

- shows what `upgrade` could move
- does not mutate imports or cache
- with no target, reports all outdated imports in scope
- with a target, reports one imported pack

#### `gc pack upgrade`

```text
gc pack upgrade [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

- with no target, upgrades all imports in scope
- with a target, upgrades one imported pack
- is transitive as needed for coherent re-resolution
- fetches the new resolved result into cache

## `gc registry`

`gc registry` owns machine-known registry configuration and catalog browsing.

### Surface area

| Command | Meaning |
|---|---|
| `gc registry list` | List configured registries from `~/.gc/registries.toml`. |
| `gc registry add <registry-name> <source>` | Add one configured registry entry. |
| `gc registry remove <registry-name>` | Remove one configured registry entry. |
| `gc registry search [query] [--registry <name>]` | Search pack entries across all registries by default; with no query, return everything. |
| `gc registry show <qualified-pack-name>` | Show one exact pack catalog entry from a registry. |

### Signatures and semantics

#### `gc registry list`

```text
gc registry list
```

- lists configured registries from `~/.gc/registries.toml`
- this is the full configured-registry view for POR

Expected output:

```text
Name   Source
main   https://github.com/gastownhall/gascity-packs
acme   https://github.com/acme/gascity-packs
```

#### `gc registry add`

```text
gc registry add <registry-name> <source>
```

- adds one configured registry entry
- edits `~/.gc/registries.toml`

#### `gc registry remove`

```text
gc registry remove <registry-name>
```

- removes one configured registry entry
- edits `~/.gc/registries.toml`

#### `gc registry search`

```text
gc registry search [query] [--registry <name>]
```

- uses a plain text query, not regex
- with no query, returns all available pack entries
- searches across all configured registries by default
- `--registry <name>` narrows the search to one registry
- returns multiple results

Expected output:

```text
Registry  Name         Latest  Description
main      maintenance  1.2.0   Health checks and baseline operational tooling.
acme      maintenance  2.0.1   Acme-flavored maintenance tasks and patrol tooling.
```

#### `gc registry show`

```text
gc registry show <qualified-pack-name>
```

- exact-address lookup for one pack catalog entry
- requires a qualified name like `main:maintenance`
- unqualified names are intentionally not accepted here

Expected output:

```text
Pack:         acme:maintenance
Registry:     acme
Name:         maintenance
Latest:       2.0.1
Description:  Acme-flavored maintenance tasks and patrol tooling.
Source:       https://github.com/acme/maintenance

Releases:
- 2.0.1  abc123  Adds patrol upgrades and doctor fixes.
- 1.9.0  def456  Stabilizes maintenance workflows.
```

### Registry resolution rules

- there is no `default` registry
- first-party registry name is `main`
- unqualified names only resolve when exactly one registry matches in contexts
  that allow unqualified resolution
- collisions require qualified names like `acme:maintenance`

## File Formats

These are the POR file-format rules.

### `registries.toml`

This is the machine-known registry config.

- lives under `~/.gc/registries.toml`
- is edited by `gc registry add` / `gc registry remove`
- does not carry pack descriptions
- does not define a default registry

POR example:

```toml
schema = 1

[[registry]]
name = "main"
source = "https://github.com/gastownhall/gascity-packs"

[[registry]]
name = "acme"
source = "https://github.com/acme/gascity-packs"
```

### `registry.toml`

This is the published registry catalog file.

- each `[[pack]]` entry has a required `description`
- each `[[pack.release]]` entry has a required `description`
- `pack.toml` does not need a required description field for POR

POR example:

```toml
schema = 1

[[pack]]
name = "maintenance"
description = "Health checks and baseline operational tooling."
source = "https://github.com/gastownhall/maintenance"

  [[pack.release]]
  version = "1.2.0"
  commit = "abc123"
  hash = "sha256:..."
  description = "Adds doctor checks and improves stale-db handling."
```


## Cache And Materialization

The storage model is intentionally light:

| Thing | Role |
|---|---|
| registry | what exists |
| import | what this scope wants |
| cache | fetched bytes already available |
| materialization | optional realized local tree when runtime behavior needs it |

Working rules:

- keep a machine-global cache under `~/.gc`
- do not introduce a first-class machine-wide store
- do not assume `.gc` should be kept wholesale in source control
- treat `fetch` as cache-oriented, not as full materialization

If materialization happens:

- it is a runtime concern rather than the primary pack CLI story
- rig behavior is still explicit through `--rig`

## Parked Questions

These are intentionally parked to the side for now. The current design pass is
primarily trying to settle command names, signatures, and output shapes.

1. This design has two top-level command trees corresponding to the two nouns
   in play (packages and registries). If desired, we could embed the registry
   commands under pack, but it's unclear whether creating a secondary/scoped
   noun is better than having two peer nouns.
2. The current design relies on implicit caching of a lowered or processed form
   of a package, all stored under `.gc`. We lack an explicit mechanism for
   embedding any form of the imported packages into package content (a.k.a.
   vendoring), making the transitive closure of imported packages into a single
   deployable unit. Our import mechanism supports the scenario by embedding
   pack content under `assets/` and using path references in the `import`
   directive in `pack.toml`.
