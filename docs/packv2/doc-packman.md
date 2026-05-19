# City/Pack Import Management

**GitHub Issue:** TBD

Title: `feat: gc import — import management for schema-2 Gas City packs`

Companion to [doc-pack-v2.md](doc-pack-v2.md)
([gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360)),
which defines the pack/city model and the schema-2 import surface that
`gc import` operates on.

> **Keeping in sync:** This file is the source of truth. When a GitHub
> issue is created, edit here, then update the issue body with
> `gh issue edit <N> --repo gastownhall/gascity --body-file <(sed -n '/^---BEGIN ISSUE---$/,/^---END ISSUE---$/{ /^---/d; p; }' issues/doc-packman.md)`.

## Status update — 2026-04-19

The launch contract for PackV2 imports is now:

- `gc import` is the schema-2 authoring and remediation surface.
- The canonical user-edited surface is `[imports.<binding>]` in the root
  city's `pack.toml`.
- `packs.lock` is the committed resolution artifact for the full
  transitive graph.
- Normal load, start, and config flows are pure readers of declared
  imports, `packs.lock`, and the local cache. They do not fetch or
  self-heal.
- `gc import check` is the read-only validation surface for declared
  imports, lock state, and local cache state.
- `gc import install` is the single remediation command. It bootstraps
  `packs.lock` from declared imports when needed and restores cache
  state from `packs.lock` when possible.
- There is no public package-registry, discovery, or implicit-import
  story in this launch.

---BEGIN ISSUE---

## Problem

The PackV2 launch needs one written import contract. Earlier design docs
drifted on:

- whether older docs still used the wrong lock-file name
- whether runtime entrypoints may fetch or repair imports implicitly
- whether implicit imports are part of the public model
- whether package-registry or discovery surfaces are part of `gc import`
- whether fresh-clone bootstrap and cache repair share one command path

This document freezes the contract that the rest of the PackV2 docs
should reference.

## Launch contract

1. **`gc import` owns schema-2 import management.** Users declare and
   maintain imported packs through `gc import` and `[imports.<binding>]`
   in the root city's `pack.toml`.
2. **`packs.lock` is authoritative for resolved state.** It records the
   full transitive graph and the exact git commits used for reproducible
   restores.
3. **Normal load/start/config flows are read-only.** They consume
   `pack.toml`, `city.toml`, `packs.lock`, and the local cache. They do
   not clone, resolve, or rewrite imports.
4. **`gc import install` is the only remediation path.** Users run it
   for fresh clones, missing caches, or lock drift. Error text should
   point to this command.
5. **There are no implicit imports.** Every imported pack that matters
   to a city is declared explicitly.
6. **There is no package-registry or discovery story in this launch.**
   Imports are declared by source, not discovered from a public catalog.

## Authoring surface

Schema-2 cities declare imports in the root `pack.toml`:

```toml
[pack]
name = "my-city"
version = "0.1.0"

[imports.gastown]
source = "https://github.com/gastownhall/gastown"
version = "^1.2"

[imports.helper]
source = "./assets/helper"
```

The binding name is part of the public configuration surface:

- it is the TOML key under `[imports.<binding>]`
- it is the name used in `packs.lock`
- it is the namespace qualifier users see in composed content

## Import source model

Imports have one public locator field: `source`.

Common source forms:

- filesystem path such as `./assets/helper`, `../packs/foo`, or
  `/abs/path/bar`
- `file://...`
- `https://...`
- `ssh://...`
- `git@...`
- bare `github.com/org/repo`

Resolution rules:

- plain directory targets remain plain directory imports and do not use
  version selection
- git-backed targets may use semver constraints or explicit
  `sha:<commit>` pins
- `gc import add` is responsible for normalizing stored source strings
  and choosing a default constraint when the caller omits one

Remote imports that need reproducible restore semantics must resolve to
entries in `packs.lock`. Local-path imports remain valid authoring
surfaces, but they are not a substitute for committed remote lock state.

## Lock file contract

`packs.lock` lives at the city root and records one flat resolved graph
for the root city:

```toml
[packs.gastown]
source = "https://github.com/gastownhall/gastown"
commit = "abc123..."
version = "1.4.2"
parent = "(root)"

[packs.polecat]
source = "https://github.com/gastownhall/polecat"
commit = "def456..."
version = "0.4.1"
parent = "gastown"
```

Rules:

- the root city owns one `packs.lock`
- imported packs do not carry their own lock files
- direct imports have `parent = "(root)"`
- transitive imports record the introducing binding name in `parent`
- the loader/runtime use `packs.lock` as input only

## `gc import install`

`gc import install` is both the bootstrap path and the repair path.

When `packs.lock` is present and satisfies the declared imports:

- read `packs.lock`
- materialize the recorded graph into the shared cache
- verify the cached content matches the lock entries

When `packs.lock` is absent, incomplete, or no longer matches the
declared imports:

- resolve the graph from the declared `[imports.<binding>]`
- write a new `packs.lock`
- materialize the resulting graph into the shared cache

Normal load/start/config paths never do this work themselves. If those
entrypoints detect missing lock or cache state, they fail with a clear
hint to run `gc import install`.

## User-facing command semantics

### `gc import add <source>`

- add or update a direct `[imports.<binding>]` entry
- resolve the direct and transitive graph
- write or refresh `packs.lock`
- materialize required cache entries

### `gc import remove <binding>`

- remove the direct import from `[imports.<binding>]`
- recompute the remaining graph
- rewrite `packs.lock`
- prune cache entries that are no longer part of the city graph

### `gc import install`

- bootstrap `packs.lock` from declared imports when needed
- restore cache state from `packs.lock` when possible
- provide the one remediation path used by fresh clones, broken caches,
  and offline-preparation workflows
- repair managed repo-cache entries in place when they drift from
  `packs.lock`; this may discard local edits or untracked files inside
  `$HOME/.gc/cache/repos/<key>` because that directory is machine-managed

### `gc import check`

- validate declared imports against `packs.lock` without fetching
- validate the locked cache entries and cached pack roots already exist
- report stale lock/cache drift with `gc import install` as the repair
  path

### `gc import upgrade [<binding>]`

- re-resolve one binding or the whole graph within the declared
  constraints
- rewrite `packs.lock`
- materialize updated cache entries

### `gc import list`

- read `packs.lock`
- show direct and transitive imports for the current city

### `gc import migrate`

- migration tool for older city layouts
- not the primary day-2 surface for schema-2 cities

## Fresh clone, cold start, and offline behavior

### Fresh clone with committed `packs.lock`

Run `gc import install`. It restores the cache from the committed lock.

### Fresh clone without `packs.lock`

Run `gc import install`. It resolves the declared imports, writes
`packs.lock`, and fills the cache.

### Normal load/start/config with missing import state

Fail fast and tell the user to run `gc import install`.

### Checking import state without mutation

Run `gc import check`. It does not resolve versions, fetch, clone, or
rewrite files. It reports missing lock entries, missing cache entries,
cache checkout drift, missing cached `pack.toml` files, and stale lock
entries. Run `gc import install` to repair the lock/cache state.

### Offline execution

Normal load/start/config remain network-free. If the required lock or
cache state is missing, offline entrypoints still fail; they do not try
to repair themselves.

## Storage layout

```text
my-city/
├── pack.toml
├── city.toml
├── packs.lock
└── .gc/
    └── cache/
        └── repos/
            ├── <sha256(normalized-clone-url+commit)>/
            └── ...
```

The cache is an implementation detail owned by `gc import`. The loader
consumes the resolved directories that `gc import install` prepared.

## Non-goals

This launch does not define:

- implicit imports
- package discovery or registry browsing
- runtime-side network fetch or auto-repair
- vendoring imports into the city tree
- a separate public identity system beyond `source` and binding names

## Migration note

This document describes the schema-2 surface. Older V1-style
`[packs.*]` and `workspace.includes` layouts remain migration input, not
the public authoring contract for new PackV2 cities. Use
[migrating-to-pack-vnext.md](../guides/migrating-to-pack-vnext.md) for
the conversion map.

---END ISSUE---
