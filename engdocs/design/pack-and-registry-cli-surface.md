---
title: "Pack CLI and Pack Registry Surface"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-16 |
| Author(s) | D. Box |
| Issue | — |
| Scope | New pack dependency CLI and pack registry discovery model |

This proposal was originally drafted in a separate exploratory repository and
is mirrored here so the design can be reviewed in the same repo that would
eventually implement it.

This document builds on the **PackV2** work from the 0.15.0 release. The CLI
surface proposed here should work with the existing pack/import model first.
Where registry support needs a file-format change, this document calls that out
as part of the proposal instead of hiding it inside implementation.

It does three things:

1. Introduce the notion of a *pack registry* where Gas City packs can be
   published and discovered.
2. Propose a coherent CLI interface for pack dependency operations.
3. Define the minimum resolver, lockfile, cache, and integrity behavior needed
   before registry-backed dependencies are implementable.

The proposed `gc pack` CLI rationalizes the import-facing surface introduced as
`gc import` in 0.15.0. The primary difference is more precise targeting of
which entity's imports are being impacted (ambient pack, city pack, or targeted
rig). A secondary difference is that `add` accepts the result of a registry
lookup, so you can add an import without having to paste a full URL or path.

This proposal also subsumes the two legacy `gc pack` commands, `fetch` and
`list`, into the same dependency-management surface. Existing `gc import`
commands may remain as compatibility aliases during the transition, but new
documentation and examples should point at `gc pack`.

## Existing Registry-Related Artifacts

This proposal is not claiming that the word `registry` is brand new inside the
repo. The repo already contains a few registry-shaped concepts:

- the machine-wide supervisor registry for cities and rigs under `~/.gc/`
- bootstrap/internal pack artifacts named `import` and `registry`
- older PackV2 docs that discuss implicit-import or local-registry ideas

What does *not* exist yet is a clean, settled, user-facing pack registry
contract for discovery and pack browsing. This proposal is about that public
surface:

- how registries are configured
- how users search and inspect them
- how registry results feed into the newer `gc pack` workflow

So when this document says "pack registry", it means the proposed public pack
discovery model, not the pre-existing supervisor registry or bootstrap
implementation artifacts.

The global registry configuration lives beside other machine-wide Gas City
state under `GC_HOME`, defaulting to `~/.gc`. Tests and scripted environments
may override `GC_HOME`; this proposal does not introduce a separate XDG path.

## Pack Registries

A Gas City pack registry is a `registry.toml` catalog. Registries do not store
packs; they are an index of packs and releases. Once the source locator and
release metadata are read from `registry.toml`, the registry is out of the
content-fetch path except for later catalog refreshes.

Registry catalog sources may be:

- an HTTPS URL to `registry.toml`
- an HTTPS base URL where `registry.toml` is implied
- a `file:` URL to `registry.toml`
- a `file:` URL to a local directory containing `registry.toml`
- a local filesystem path to `registry.toml`
- a local directory containing `registry.toml`

HTTP without TLS is out of scope for remote registries. Local paths and `file:`
URLs are intentionally allowed for development, private mirrors, test fixtures,
and registries backed by local git working directories.

Configured registry names use this grammar:

```text
[a-z0-9][a-z0-9-]*
```

Registry names are case-sensitive, have a maximum length of 64 characters, and
must not contain `:`, `/`, `\`, `.` path segments, or URL scheme characters.

Pack names may be either unscoped or scoped:

```text
[a-z0-9][a-z0-9-]*
[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*
```

Pack names are case-sensitive and each segment has a maximum length of 64
characters. Scoped names such as `acme/lighthouse` are allowed so public and
enterprise registries do not require one flat global namespace. Qualified names
therefore look like `main:lighthouse` or `main:acme/lighthouse`. Bare scoped
names like `acme/lighthouse` are still registry names, not local paths, unless
they start with an explicit path marker such as `./`.

### Catalog Example

```toml
schema = 1

[[pack]]
name = "lighthouse"
description = "Harbor-watch checks and incident response workflows."
source = "https://packages.example/main/lighthouse.git"
source_kind = "git"

  [[pack.release]]
  version = "1.2.0"
  ref = "v1.2.0"
  commit = "0123456789abcdef0123456789abcdef01234567"
  hash = "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"
  description = "Adds dock patrol checks and improves incident triage."

[[pack]]
name = "weatherglass"
description = "Forecasting and telemetry helpers for harbor operations."
source = "https://packages.example/main/weatherglass.git"
source_kind = "git"

  [[pack.release]]
  version = "0.4.0"
  ref = "v0.4.0"
  commit = "89abcdef0123456789abcdef0123456789abcdef"
  hash = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  description = "First public release."
```

A local catalog may point at local git repositories:

```toml
schema = 1

[[pack]]
name = "weatherglass"
description = "Local test mirror for forecasting helpers."
source = "file:///Users/example/packs/weatherglass.git"
source_kind = "git"

  [[pack.release]]
  version = "0.4.0"
  ref = "release-0.4"
  commit = "89abcdef0123456789abcdef0123456789abcdef"
  hash = "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
  description = "First public release."
```

A registry can advertise multiple versions of the same pack, with distinct
notes on each version. Within one pack entry, each `version` must be unique and
immutable: after publication, the tuple `(pack.name, release.version)` must not
change `source`, `source_kind`, `ref`, `commit`, or `hash`.

### Source Semantics

Registry v1 supports git pack sources only. `source_kind = "git"` means:

- `source` is an HTTPS git URL, a `file:` URL for a local git repository, or a
  local path to a git repository
- `ref` is the publisher-facing label for the release, usually a tag, branch,
  or other git ref
- `commit` is the full resolved commit SHA that `ref` must resolve to when the
  release is fetched
- `hash` is the SHA-256 digest of the canonical exported pack tree at that
  commit, using the canonical pack tree hash defined below

The fetcher must verify both that `ref` resolves to `commit` and that the
fetched pack tree matches `hash`. A mismatch is a hard error and must not update
the lockfile.

Non-git archives, OCI artifacts, or other package source kinds are future
format work. They should add explicit `source_kind` values and hash rules
rather than overloading the git fields.

Remote HTTPS catalogs may only publish HTTPS git sources. Local catalogs loaded
from a local path or `file:` URL may publish `file:` or local-path git sources.
Clients must reject a remote catalog that publishes a `file:` or local-path pack
source.

Direct imports entered through `gc pack add` continue to accept existing PackV2
git locator forms, including `https://...`, `ssh://...`, `git@...`, bare
`github.com/org/repo`, `file://...`, and explicit local paths. The HTTPS/file
restriction above applies to registry catalog entries, not to direct PackV2
imports.

### Canonical Pack Tree Hash

The catalog `hash` uses this v1 algorithm:

1. Resolve the git source to the full `commit`.
2. Identify the pack root at that commit.
3. Walk the pack root in bytewise lexical path order.
4. Exclude `.git/`, `.gc/`, runtime/cache directories, OS metadata files, and
   other files outside the pack boundary.
5. Include regular file bytes exactly as stored in git; do not normalize line
   endings.
6. Include executable mode bits and symlink targets in the digest stream.
7. Reject unreadable files, unresolved symlinks, submodules, and paths that
   escape the pack root.
8. Prefix the digest stream with the algorithm identifier
   `gc-pack-tree-sha256-v1`.

Future changes to path inclusion, symlink handling, or mode encoding require a
new algorithm identifier. Catalog entries may later add an explicit
`hash_algorithm` field if more than one algorithm is supported.

### Trust Model

Registry v1 provides integrity, not a full signing or delegation system.

- remote registry catalogs must be fetched over HTTPS
- local and `file:` registries are trusted as local input
- release entries are immutable once published
- release entries may set `withdrawn = true` with an optional `withdrawn_reason`
  when a publisher wants to stop new installs without rewriting the release
- clients must hard-fail if an already-locked release later appears with a
  different commit or hash
- clients must hard-fail if fetched content does not match the catalog hash
- new resolutions must skip withdrawn releases unless the user has explicitly
  pinned the withdrawn commit with `sha:<hex>`
- existing lockfiles may continue to use a withdrawn release, but `outdated`
  and `gc doctor` must flag it with remediation text

In v1, trusting a registry source means trusting the registry operator and the
origin server. HTTPS protects the transport to that origin; it does not prove
that the catalog was published by a particular party or that the operator cannot
serve malicious metadata. Hash checks verify that fetched pack content matches
the catalog entry. Lockfiles mitigate later catalog mutation by pinning the
selected `(name, version, ref, commit, hash)` tuple and failing closed when a
refreshed catalog changes it.

Signing, attestations, and TUF-style delegation are deliberately out of scope
for this proposal, but the file format should leave room for a later
`signatures` or `attestations` section.

## CLI Surface

The dependency verbs are grouped by the lifecycle of an existing pack's
dependencies:

- **edit the dependency manifest:** `add`, `remove`
- **reconcile or verify local state with the manifest and lockfile:** `sync`,
  `check`
- **inspect current state:** `list`, `show`
- **inspect and apply available changes:** `outdated`, `upgrade`

Registry verbs are nested under `gc pack registry` and only manage configured
catalogs and catalog browsing.

### `gc pack`

```text
gc pack add <source-or-name> [--name <import-name>] [--version <constraint>] [--export] [--transitive] [--no-transitive] [--transitive-default] [--shadow <warn|silent>] [--no-sync] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack remove <import-name> [--force] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack sync [<import-name>] [--refresh] [--verify-only] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack check [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack list [--transitive] [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack show <import-name> [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack outdated [<import-name>] [--refresh] [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack upgrade [<import-name>] [--refresh] [--pack <path>] [--city <path>] [--rig <name-or-path>]
gc pack why <name-or-path> [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

Current implementation checkpoint: the registry workstream PR includes
`gc pack registry list/add/remove/refresh/search/show` plus thin dependency
wrappers for `gc pack add/remove/sync/check/upgrade/why`. Dependency
`gc pack show` and `gc pack outdated` are intentionally deferred to the later
read-command wave. Canonical dependency `gc pack list` is also deferred because
the existing `gc pack list` command remains the legacy `[packs]` status command
until the compatibility plan moves that surface.

`sync` is the dependency-reconciliation verb. It replaces the old "fetch all
declared imports into cache" meaning of `gc import install` and the legacy
`gc pack fetch` command. Compatibility aliases may keep `gc import install` and
`gc pack fetch` working during transition, but the canonical verb is `sync`.

### `gc pack registry`

```text
gc pack registry list [--json]
gc pack registry add <registry-name> <source> [--no-validate]
gc pack registry remove <registry-name>
gc pack registry refresh [<registry-name>]
gc pack registry search [query] [--registry <name>] [--refresh] [--limit <n>] [--all] [--json]
gc pack registry show <pack-name> [--refresh] [--json]
```

There are intentionally no commands here for publishing packs into a registry.
Each registry owns and manages its published `registry.toml`. A future
publishing proposal should define publisher tooling, signing/attestation, and
first-party registry operations.

## Scope Model

The baseline target is the ambient pack, when one can be discovered from the
working directory.

Working rules:

- if the current directory is inside a pack definition, the ambient pack is the
  nearest surrounding pack definition
- if the current directory is inside a rigged directory but not inside a
  different pack definition, there is no implicit rig edit; commands that would
  mutate imports fail and ask for `--city`, `--pack`, or `--rig`
- if the current directory is inside a city but not a nested pack, the ambient
  target is the city's pack
- if the current directory is in neither a pack directory nor a city, there is
  no ambient target and the user must pass `--pack`, `--city`, or `--rig`

Rows in the write-target matrix are matched top to bottom; the first match
wins. The explicit rig-directory error intentionally wins over the broader
"inside a city" row.

Mutating commands use this write-target matrix:

| User intent | Required context or flags | Write target |
|---|---|---|
| Ambiguous rig directory | CWD inside a rig and not inside a nested pack definition, no flags | error with explicit `--rig` / `--city` / `--pack` guidance |
| Ambient pack dependency | CWD inside a pack, no `--city` or `--rig` | nearest `pack.toml` |
| City pack dependency | CWD inside a city root and not inside a rig, or `--city <path>` without `--rig` | the city pack's `pack.toml` |
| Explicit pack dependency | `--pack <path>` | that pack's `pack.toml` |
| Rig-scoped dependency by name | `--city <path> --rig <name>` or CWD in city plus `--rig <name>` | rig import section in that city's config |
| Rig-scoped dependency by path | `--rig <path>` | rig import section associated with that rig's owning city |
| Pack plus rig refinement | `--pack <path> --rig <name-or-path>` where `<path>` is a city root | rig import section in that city |
| Invalid pack plus rig refinement | `--pack <path> --rig <name-or-path>` where `<path>` is not a city root | error; use `--city --rig` or target the pack without `--rig` |
| No ambient context | CWD outside pack/city, no flags | error |

`--rig` always opts into rig-scoped import behavior. It never becomes ambient
only because the shell is currently in a rig directory. Rig names are resolved
through the selected city's rig configuration. Cities are discovered through
the supervisor registry at `$GC_HOME/cities.toml`; rigs declared in unregistered
cities are not considered. If more than one city can claim a rig path or name,
the command fails and asks for `--city`.

Read-only commands use the same target resolution. Without a resolvable scope,
text/table output exits non-zero with explicit targeting guidance. JSON output
uses the platform `--json` contract and writes a structured failure object on
stdout so tooling can distinguish "no scope" from parse failure without scraping
stderr.

## Dependency Semantics

### `gc pack add`

```text
gc pack add <source-or-name> [--name <import-name>] [--version <constraint>] [--export] [--transitive] [--no-transitive] [--transitive-default] [--shadow <warn|silent>] [--no-sync] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- adds or updates an import in the selected scope
- runs `gc pack sync <import-name>` after editing unless `--no-sync` is passed
  or the target is a standalone pack outside a city
- accepts:
  - qualified registry names like `main:lighthouse`
  - unqualified registry names when resolution is unambiguous
  - existing PackV2 direct git source forms
  - `file:` URLs for local git repositories
  - explicit local paths
- `--name` gives an explicit local import name when needed
- `--export`, `--transitive`, `--no-transitive`, `--transitive-default`, and
  `--shadow` map to existing PackV2 import fields
- `--version` records a version constraint for registry-backed and git-backed
  imports
- accepted version forms are exact semver, semver constraints supported by the
  resolver, and existing SHA pins of the form `sha:<hex>`
- SHA pins must use a full 40-character lowercase hex commit:
  `sha:[0-9a-f]{40}`
- `--version` is not accepted for plain local path imports unless the path is
  detected as a git repository and normalized to a git source form
- registry-backed adds use the registry handle only as a command-time selector;
  the manifest stores the concrete pack `source` selected from the catalog and
  `packs.lock` retains registry/ref/commit/hash metadata

Input classification is intentionally conservative:

1. strings matching `<registry-name>:<pack-name>` are qualified registry names
2. strings beginning with `registry:` are explicit command-time registry
   selectors
3. existing PackV2 git locator forms are direct git sources, including
   `https://...`, `ssh://...`, `git@...`, bare `github.com/org/repo`, and
   `file://...`
4. strings beginning with `./`, `../`, `/`, `~/`, or a Windows drive/path form
   are local paths
5. all other strings are unqualified registry names

Bare names do not resolve as local paths merely because a same-named directory
exists. If a user wants a local path, they must spell it as a path. If no
registry match exists but a same-named local directory exists, the error should
include a "did you mean `./name`?" hint.

For registry-backed imports, the manifest stores the durable concrete source
selected from the catalog plus the user's version constraint. Registry identity
is retained in `packs.lock` and in command output, not in public import TOML:

```toml
[imports.lighthouse]
source = "https://packages.example/main/lighthouse.git"
version = "^1.2"
```

`gc pack add` and registry-aware command paths may resolve registry selectors
before writing manifests. Packman sync/check/load paths consume concrete
sources and `packs.lock`; the loader remains a lockfile reader and must not
perform registry lookups or network fetches during normal load/start/config
flows.

When a local path is detected as a git working tree or bare repository and the
user passes `--version`, the CLI normalizes it to an absolute `file:` git source
before recording it. Otherwise local paths remain plain directory imports and
`--version` is rejected with remediation text.

If the derived import name collides with an unrelated existing import, `add`
fails and asks the user to pass `--name`. Re-adding the same import name updates
that import. Fields provided on the command line replace existing values;
fields omitted on the command line are preserved. Changing `source` or
`version` invalidates the lock entry for that import until the next successful
`sync`.

If the post-add sync fails, the manifest edit remains in place and the command
exits non-zero with remediation text telling the user to run `gc pack sync
<import-name>` after fixing the fetch or registry issue.

When the target is a standalone pack outside a city, `add` edits the manifest
only and implies `--no-sync`. Standalone packs do not own a `packs.lock`; their
dependencies resolve when a city consumes them.

### `gc pack remove`

```text
gc pack remove <import-name> [--force] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- removes an import from the selected scope
- removes that import's lock entry from the selected lockfile
- does not imply eager cache deletion
- blocks removal when local configuration still references entities provided by
  that import

The reference-blocker check covers at least direct references from pack
configuration, city/rig configuration, formulas, commands, and agent
definitions. If complete reference analysis is unavailable, the command must be
conservative and explain what it could not verify. `--force` bypasses blockers
after printing the affected references and a warning. Forced removal leaves
dangling references intact; subsequent commands should fail clearly until the
user removes or rewrites those references.

### `gc pack sync`

```text
gc pack sync [<import-name>] [--refresh] [--verify-only] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- reconciles the selected manifest imports, lockfile entries, and local cache
- with no target, syncs all direct imports in scope
- with a target, syncs one imported pack in scope
- fetches missing content and verifies locked content
- validates registry-backed releases against recorded registry origin, commit,
  and hash
- updates stale or missing lock entries only after successful verification
- does not change manifest constraints
- with `--verify-only`, validates manifest, lockfile, and cache state without
  resolving, fetching, or writing

`sync` is the explicit "make this dependency state real on this machine" verb.
It is safe to rerun.

When no lock entry exists for a manifest import, `sync` resolves it using the
same candidate selection policy as `upgrade` against the currently cached
catalog or git metadata. `sync` does not imply `--refresh`; if the relevant
catalog is stale, it warns before locking a selection. It never selects a
release or commit forbidden by the manifest constraint.

### `gc pack list`

```text
gc pack list [--transitive] [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- with no flags, lists direct imports in scope
- with `--transitive`, lists the full resolved transitive set from the lockfile
- table output is default; `--json` emits one JSON object on stdout and declares
  its result schema through `gc pack list --json-schema`

Expected table output:

```text
Name          Constraint  Resolved  Origin            Source
lighthouse    ^1.2        1.2.0     main:lighthouse   https://packages.example/main/lighthouse.git
weatherglass  sha:89ab    0.4.0     file              file:///Users/example/packs/weatherglass.git
```

### `gc pack show`

```text
gc pack show <import-name> [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- shows one imported pack in the selected scope
- reads local manifest and lockfile state
- does not reach out to registry catalog views unless the user runs `sync`,
  `outdated --refresh`, or `registry show --refresh`

Expected output:

```text
Import:         lighthouse
Origin:         main:lighthouse
Source:         https://packages.example/main/lighthouse.git
Constraint:     ^1.2
Resolved:       1.2.0
Commit:         0123456789abcdef0123456789abcdef01234567
Hash:           sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7
Synced:         yes
Scope:          city pack
```

### `gc pack outdated`

```text
gc pack outdated [<import-name>] [--refresh] [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- shows what `upgrade` could move
- does not mutate manifests, lockfiles, or cache
- with no target, reports all outdated imports in scope
- with a target, reports one imported pack
- respects version constraints already recorded on imports
- uses cached catalogs by default and warns when catalog metadata is stale
- with `--refresh`, refreshes relevant registry catalogs before comparing

Expected output:

```text
Name         Current  Latest allowed  Latest available  Status
lighthouse   1.1.0    1.2.0           2.0.1             upgrade available
```

### `gc pack upgrade`

```text
gc pack upgrade [<import-name>] [--refresh] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- with no target, upgrades all imports in scope
- with a target, upgrades one imported pack
- refreshes relevant catalogs when `--refresh` is passed
- respects manifest version constraints
- updates the lockfile to the newest allowed verified release
- fetches and verifies the new resolved result into cache
- does not loosen manifest constraints unless the user edits them through
  `gc pack add --version ...` or manual config changes

For v1, dependency resolution should fail on version conflicts rather than
trying to solve by silently selecting a lower or higher transitive version. The
error should name the conflicting imports and constraints and tell the user
which manifest entries to adjust.

Resolver ordering for registry-backed imports is:

1. reuse a locked version when it still satisfies the manifest constraint and
   the selected release is not withdrawn
2. otherwise choose the highest non-withdrawn semver release that satisfies the
   manifest constraint
3. exact version constraints beat ranges
4. `sha:<hex>` pins select the exact commit and do not participate in semver
   ordering

Direct git sources without `--version` resolve to the remote default branch
HEAD at sync time and lock that commit. Direct git sources with semver
constraints discover candidate versions from git tags. If tags cannot be read
and no usable lock entry exists, sync/upgrade fails with remediation text.

### `gc pack why`

```text
gc pack why <name-or-path> [--json] [--pack <path>] [--city <path>] [--rig <name-or-path>]
```

- explains why a direct or transitive pack appears in the resolved graph
- reads the lockfile and cached manifests
- does not fetch or mutate
- is the successor for `gc import why`

## Registry Commands

### `gc pack registry list`

```text
gc pack registry list [--json]
```

- lists configured registries from `$GC_HOME/registries.toml`
- table output is default; `--json` emits one JSON object on stdout and declares
  its result schema through `gc pack registry list --json-schema`

Expected output:

```text
Name   Source
main   https://registries.gascity.example/main/registry.toml
acme   https://registries.acme.example/catalog/registry.toml
```

### `gc pack registry add`

```text
gc pack registry add <registry-name> <source> [--no-validate]
```

- adds one configured registry entry
- edits `$GC_HOME/registries.toml`
- validates the source by fetching and parsing the catalog unless
  `--no-validate` is passed
- writes through an atomic temp-file replacement while holding the registry
  config lock
- `--no-validate` skips only the reachability and parse precheck; the trust
  decision is still the user's explicit choice to add that registry source

### `gc pack registry remove`

```text
gc pack registry remove <registry-name>
```

- removes one configured registry entry
- edits `$GC_HOME/registries.toml`
- does not delete cached catalog snapshots immediately
- warns when known lock metadata or diagnostics still reference the removed
  registry
- writes through an atomic temp-file replacement while holding the registry
  config lock

Catalog snapshots for removed registries are pruned by `gc pack registry
refresh` and by `gc doctor --fix`. Until then, they are ignored for resolution.

### `gc pack registry refresh`

```text
gc pack registry refresh [<registry-name>]
```

- refreshes cached `registry.toml` snapshots
- with no target, refreshes all configured registries
- with a target, refreshes one configured registry
- validates schema and immutable release metadata before replacing the cached
  snapshot

### `gc pack registry search`

```text
gc pack registry search [query] [--registry <name>] [--refresh] [--limit <n>] [--all] [--json]
```

- uses a plain text query, not regex
- with no query, returns available pack entries up to the default result limit
- searches across all configured registries by default
- `--registry <name>` narrows the search to one registry
- `--refresh` refreshes catalog snapshots before searching
- `--limit <n>` controls the result limit
- the default result limit is 50 across all searched registries
- `--all` disables the default result limit
- registry fetch failures are isolated per registry and reported in the output
- JSON output includes `truncated: true|false`

Expected output:

```text
Registry  Name         Latest  Description
main      lighthouse   1.2.0   Harbor-watch checks and incident response workflows.
acme      lighthouse   2.0.1   Acme-flavored harbor patrol and response tooling.
```

### `gc pack registry show`

```text
gc pack registry show <pack-name> [--refresh] [--json]
```

- exact-address lookup for one pack catalog entry
- accepts a qualified name like `main:lighthouse`
- accepts an unqualified name only when the same registry-resolution rules
  would resolve it unambiguously for `gc pack add`
- `--refresh` refreshes the owning registry before display
- cross-registry "show me every registry carrying this pack" behavior is
  covered by `gc pack registry search <pack-name>`

Expected output:

```text
Pack:         acme:lighthouse
Registry:     acme
Name:         lighthouse
Latest:       2.0.1
Description:  Acme-flavored harbor patrol and response tooling.
Source:       https://packages.example/acme/lighthouse.git
Source kind:  git

Releases:
- 2.0.1  v2.0.1  0123456789abcdef0123456789abcdef01234567  Adds patrol upgrades and doctor fixes.
- 1.9.0  v1.9.0  89abcdef0123456789abcdef0123456789abcdef  Stabilizes maintenance workflows.
```

### Registry Resolution Rules

- there is no `default` registry
- first-party registry name is `main`
- `main` is simply the conventional name seeded at installation time; no CLI
  behavior depends on its continued presence
- unqualified names only resolve when exactly one configured registry has a
  matching cached entry and all relevant registries were reachable at the last
  refresh used by the command
- if any relevant registry is unreachable and could affect unqualified
  resolution, the command fails and asks the user to qualify the name
- collisions fail with an actionable error listing qualified names such as
  `main:lighthouse` and `acme:lighthouse`

### Initial Registry Seeding

`gc init` seeds a `main` registry entry when `$GC_HOME/registries.toml` does not
already exist. Installers do not mutate user state. Tests that override
`GC_HOME` start with no registry entries and must add `main` explicitly.

The first-party registry URL is a release-blocking constant for this design. It
must be defined before implementation ships; examples use
`https://registries.gascity.example/main/registry.toml` as a placeholder.

## Lockfile and Cache Rules

PackV2 currently uses a city-root `packs.lock` for the full transitive graph.
Registry support requires a schema-2 extension of that file, not a second
parallel lockfile format. Existing schema-1 lockfiles migrate forward by
preserving their version/commit/fetched data and leaving registry-specific
fields empty.

Schema-2 writes are fenced. Commands keep writing schema 1 when the resolved
state contains only schema-1 fields, even if the binary understands schema 2.
They write schema 2 only when the lock needs schema-2-only information such as
registry origin, withdrawn state, graph-node scope namespace, or a non-legacy
source kind. This keeps ordinary legacy direct-git workflows from creating
gratuitous lockfile churn.

Lockfile location by scope:

- city-pack imports use the city root's `packs.lock`
- rig-scoped imports use the owning city root's `packs.lock`
- imported regular packs do not carry their own lockfiles
- standalone packs outside a city may edit their manifest, but `sync`,
  `outdated`, `upgrade`, and `list --transitive` require a city context; their
  resolution is deferred until a city consumes them
- `gc pack add` against a standalone pack therefore implies `--no-sync`

Schema-2 lock entries are graph nodes, not just pack names. The node identity is
the import path from the root scope, including rig namespace when present. This
allows `a.common` and `b.common` to resolve independently even when they point
at the same source and commit. Cache materialization may deduplicate content by
source, commit, and hash, but lock nodes remain distinct.

Registry-backed schema-2 lock entries record at least:

- graph node identity
- local import name
- parent graph node, or `(root)`
- scope namespace, such as city pack or `rig:<name>`
- registry name and registry source at resolution time
- pack name
- manifest constraint
- selected release version
- source and source kind
- release ref
- resolved commit
- content hash
- fetched/synced status
- withdrawn status at last refresh

Command behavior:

- `add` edits the manifest and then syncs by default
- `add --no-sync` edits the manifest and leaves the lock stale or missing
- `remove` edits the manifest and removes that import's lock entry
- `sync` repairs missing/stale lock entries after verified fetches
- `outdated` reads the lockfile for current versions
- `upgrade` writes new verified lock entries
- `list --transitive` reads the lockfile
- `show` reports both manifest and lockfile state

Lockfile updates must be deterministic and should avoid rewriting unrelated
entries. Merge conflicts are treated like other committed lockfile conflicts:
the user resolves the file and runs `gc pack sync` to validate the result.

The current on-disk cache layout remains an implementation detail for v1.
Logically, verification keys content by source kind, source, commit, and hash.
The implementation may continue materializing cache directories by graph node or
binding name while using the logical key to detect equivalent content and verify
that each node contains the expected tree. A content-addressed cache layout is
future work and must be designed as an explicit migration.

## Network and Offline Behavior

Registry catalogs are cached under `GC_HOME`. Cached snapshots include fetch
time, source, schema version, and immutable release metadata.

Default behavior:

- `registry search`, `registry show`, and `outdated` use cached snapshots when
  they are fresh
- `--refresh` forces a refresh before the command proceeds
- `registry refresh` refreshes snapshots explicitly
- stale snapshots may be used for read-only commands, but the command must warn
  that results may be stale
- registry-backed `add`, `sync`, and `upgrade` may use cached snapshots when
  offline only if they can still verify the locked or selected release
- commands must fail closed when an unreachable registry could change
  unqualified-name resolution

Freshness thresholds are part of the v1 contract:

- `registry search` and `registry show`: 24 hours
- `outdated`, `add`, `sync`, and `upgrade`: 6 hours

`GC_REGISTRY_FRESHNESS` may override these thresholds for tests and scripted
environments. A later config surface may make them user-tunable.

Private authenticated registries are out of scope for v1. The v1 implementation
should not invent bespoke credential storage. A later design may define auth
using standard credential helpers, `.netrc`, environment variables, or platform
keychains.

## File Formats

### `registries.toml`

This is the machine-known registry config.

- lives under `$GC_HOME/registries.toml`
- is edited by `gc pack registry add` / `gc pack registry remove`
- does not carry pack descriptions
- does not define a default registry
- is updated with a lock plus atomic replacement
- the lock is `$GC_HOME/registries.toml.lock`; Unix implementations use
  `flock(2)` and Windows implementations use `LockFileEx`
- the lock is held from read through temp-file write and final rename

Example:

```toml
schema = 1

[[registry]]
name = "main"
source = "https://registries.gascity.example/main/registry.toml"

[[registry]]
name = "acme"
source = "https://registries.acme.example/catalog/registry.toml"
```

### `registry.toml`

This is the published registry catalog file.

- each `[[pack]]` entry has a required `description`
- each `[[pack]]` entry has a required `source`
- each `[[pack]]` entry has a required `source_kind`
- each `[[pack.release]]` entry has a required `description`
- each `[[pack.release]]` entry has a required `version`
- each `[[pack.release]]` entry has a required `ref`
- each `[[pack.release]]` entry has a required `commit`
- each `[[pack.release]]` entry has a required `hash`
- each `[[pack.release]]` entry may set `withdrawn = true`
- each withdrawn release may include `withdrawn_reason`

CLIs reject schemas they do not understand with a clear upgrade message.

### `pack.toml`

- `pack.toml` does not need a required description field for this proposal
- registry selectors are command-time inputs only; durable imports continue to
  use concrete `source` values and optional `version`
- new CLI flags should map to existing import fields before new manifest fields
  are introduced
- if a manifest temporarily contains both legacy `[packs]` and `[imports]`,
  `[imports]` is authoritative and compatibility diagnostics must explain how
  to remove or rewrite `[packs]`

## Output Formats

JSON output is stable for tooling and must compose with the shared CLI JSON
contract introduced by PR 2222. Read commands use the root `--json` flag, write
one JSON object followed by a newline on stdout, and declare support by checking
in `schemas/<command path>/result.schema.json`.

Each command also supports the platform `--json-schema` discovery flag:

```text
gc pack list --json-schema
gc pack list --json-schema=result
gc pack list --json-schema=failure
```

`--json-schema` emits the platform manifest or role-specific JSON Schema
without executing the command. Commands without a result schema are treated as
not JSON-supported. Failures in JSON mode use the shared failure schema from
`schemas/failure.schema.json`.

Every checked-in pack/registry result schema must include `description` text for
each public field and nested public field. That text is part of the user-facing
contract: CLI reference generation and JSON-output help should use those schema
descriptions when presenting what `--json` emits, rather than duplicating field
explanations in handwritten prose.

Every command that declares JSON support must also have tests that run the
command with `--json` and validate the actual stdout object against the
command's `--json-schema=result` schema. Golden output tests are not sufficient
on their own.

Pack and registry result objects use a command-owned top-level
schema-version field:

```json
{
  "schema_version": "1"
}
```

Command-specific fields live beside `schema_version`. Schema versions are owned
per command: an incompatible JSON shape change bumps that command's result
schema and updates that command's golden tests without implying that unrelated
command schemas changed.

### Shared JSON Types

All timestamps are RFC3339 strings. Empty optional fields are omitted unless the
field is needed to distinguish an explicit empty result from "not evaluated".

```json
{
  "$defs": {
    "scope": {
      "type": "object",
      "required": ["kind"],
      "properties": {
        "kind": { "enum": ["city", "pack", "rig", "none"] },
        "city_path": { "type": "string" },
        "pack_path": { "type": "string" },
        "rig_name": { "type": "string" },
        "rig_path": { "type": "string" }
      },
      "additionalProperties": false
    },
    "diagnostic": {
      "type": "object",
      "required": ["severity", "code", "message"],
      "properties": {
        "severity": { "enum": ["info", "warning", "error"] },
        "code": { "type": "string" },
        "message": { "type": "string" },
        "remediation": { "type": "string" }
      },
      "additionalProperties": false
    },
    "import": {
      "type": "object",
      "required": ["name", "source", "scope"],
      "properties": {
        "name": { "type": "string" },
        "source": { "type": "string" },
        "origin": { "type": "string" },
        "source_kind": { "type": "string" },
        "constraint": { "type": "string" },
        "resolved_version": { "type": "string" },
        "ref": { "type": "string" },
        "commit": { "type": "string" },
        "hash": { "type": "string" },
        "synced": { "type": "boolean" },
        "withdrawn": { "type": "boolean" },
        "scope": { "$ref": "#/$defs/scope" },
        "diagnostics": {
          "type": "array",
          "items": { "$ref": "#/$defs/diagnostic" }
        }
      },
      "additionalProperties": false
    },
    "registry": {
      "type": "object",
      "required": ["name", "source"],
      "properties": {
        "name": { "type": "string" },
        "source": { "type": "string" },
        "cached_at": { "type": "string" },
        "stale": { "type": "boolean" },
        "reachable": { "type": "boolean" },
        "diagnostics": {
          "type": "array",
          "items": { "$ref": "#/$defs/diagnostic" }
        }
      },
      "additionalProperties": false
    },
    "release": {
      "type": "object",
      "required": ["version", "ref", "commit", "hash", "description"],
      "properties": {
        "version": { "type": "string" },
        "ref": { "type": "string" },
        "commit": { "type": "string" },
        "hash": { "type": "string" },
        "description": { "type": "string" },
        "withdrawn": { "type": "boolean" },
        "withdrawn_reason": { "type": "string" }
      },
      "additionalProperties": false
    },
    "registry_pack": {
      "type": "object",
      "required": ["registry", "name", "description", "source", "source_kind"],
      "properties": {
        "registry": { "type": "string" },
        "name": { "type": "string" },
        "qualified_name": { "type": "string" },
        "description": { "type": "string" },
        "source": { "type": "string" },
        "source_kind": { "type": "string" },
        "latest": { "type": "string" },
        "releases": {
          "type": "array",
          "items": { "$ref": "#/$defs/release" }
        }
      },
      "additionalProperties": false
    }
  }
}
```

### Command JSON Results

These are result-shape examples. The implementation source of truth is the
checked-in JSON Schema file for each command.

`gc pack list --json`:

```json
{
  "schema_version": "1",
  "scope": { "kind": "city", "city_path": "/path/to/city" },
  "transitive": false,
  "imports": [
    {
      "name": "lighthouse",
      "source": "https://packages.example/main/lighthouse.git",
      "origin": "main:lighthouse",
      "source_kind": "git",
      "constraint": "^1.2",
      "resolved_version": "1.2.0",
      "ref": "v1.2.0",
      "commit": "0123456789abcdef0123456789abcdef01234567",
      "hash": "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7",
      "synced": true,
      "scope": { "kind": "city", "city_path": "/path/to/city" }
    }
  ],
  "diagnostics": []
}
```

`gc pack show --json`:

```json
{
  "schema_version": "1",
  "scope": { "kind": "city", "city_path": "/path/to/city" },
  "import": {
    "name": "lighthouse",
    "source": "https://packages.example/main/lighthouse.git",
    "origin": "main:lighthouse",
    "source_kind": "git",
    "constraint": "^1.2",
    "resolved_version": "1.2.0",
    "ref": "v1.2.0",
    "commit": "0123456789abcdef0123456789abcdef01234567",
    "hash": "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7",
    "synced": true,
    "scope": { "kind": "city", "city_path": "/path/to/city" }
  },
  "diagnostics": []
}
```

`gc pack outdated --json`:

```json
{
  "schema_version": "1",
  "scope": { "kind": "city", "city_path": "/path/to/city" },
  "refreshed": false,
  "stale_catalogs": ["main"],
  "items": [
    {
      "name": "lighthouse",
      "current": "1.1.0",
      "latest_allowed": "1.2.0",
      "latest_available": "2.0.1",
      "status": "upgrade_available",
      "origin": "main:lighthouse",
      "withdrawn": false
    }
  ],
  "diagnostics": []
}
```

`gc pack why --json`:

```json
{
  "schema_version": "1",
  "scope": { "kind": "city", "city_path": "/path/to/city" },
  "query": "harborkit",
  "matches": [
    {
      "name": "harborkit",
      "path": ["lighthouse", "harborkit"],
      "reason": "transitive_import",
      "source": "https://packages.example/main/harborkit.git"
    }
  ],
  "diagnostics": []
}
```

`gc pack registry list --json`:

```json
{
  "schema_version": "1",
  "registries": [
    {
      "name": "main",
      "source": "https://registries.gascity.example/main/registry.toml",
      "cached_at": "2026-05-17T12:00:00Z",
      "stale": false,
      "reachable": true
    }
  ],
  "diagnostics": []
}
```

`gc pack registry search --json`:

```json
{
  "schema_version": "1",
  "query": "light",
  "registry": "",
  "refreshed": false,
  "limit": 50,
  "truncated": false,
  "results": [
    {
      "registry": "main",
      "name": "lighthouse",
      "qualified_name": "main:lighthouse",
      "description": "Harbor-watch checks and incident response workflows.",
      "source": "https://packages.example/main/lighthouse.git",
      "source_kind": "git",
      "latest": "1.2.0"
    }
  ],
  "diagnostics": []
}
```

`gc pack registry show --json`:

```json
{
  "schema_version": "1",
  "pack": {
    "registry": "main",
    "name": "lighthouse",
    "qualified_name": "main:lighthouse",
    "description": "Harbor-watch checks and incident response workflows.",
    "source": "https://packages.example/main/lighthouse.git",
    "source_kind": "git",
    "latest": "1.2.0",
    "releases": [
      {
        "version": "1.2.0",
        "ref": "v1.2.0",
        "commit": "0123456789abcdef0123456789abcdef01234567",
        "hash": "sha256:3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7",
        "description": "Adds dock patrol checks and improves incident triage.",
        "withdrawn": false
      }
    ]
  },
  "diagnostics": []
}
```

## Compatibility and Doctor

This proposal does not create a migration command surface. In particular,
`gc import migrate` is not carried forward.

Compatibility guidance:

| Existing command | New surface | Policy |
|---|---|---|
| `gc import add` | `gc pack add` | deprecated alias |
| `gc import remove` | `gc pack remove` | deprecated alias |
| `gc import check` | `gc pack sync --verify-only` | deprecated alias |
| `gc import install` | `gc pack sync` | deprecated alias |
| `gc import upgrade` | `gc pack upgrade` | deprecated alias |
| `gc import list` | `gc pack list` | deprecated alias |
| `gc import why` | `gc pack why` | deprecated alias |
| `gc import migrate` | no command replacement | removed after `gc doctor --fix` parity on the existing migration golden corpus |
| `gc pack fetch` | `gc pack sync` | deprecated alias |
| legacy `gc pack list` | `gc pack list` | preserved in the new surface |

Deprecated aliases, except `gc import migrate`, ship with this surface. The
planned window is two minor releases of warnings, one minor release of hard
errors with remediation text, then removal. `gc import migrate` has no
replacement command and is parity-gated rather than time-gated; before it starts
that window, `gc doctor` / `gc doctor --fix` must provide equivalent dry-run and
fix guidance for the migration cases it used to cover.

Current implementation checkpoint: `gc pack add/remove/sync/upgrade/why` share
handlers with the existing `gc import` commands while keeping `gc import` text
stable. `gc pack fetch` and legacy `gc pack list` remain compatibility commands
for the existing `[packs]` surface until the PackV2 deprecation train explicitly
advances them.

`gc doctor` owns migration and registry-health pressure. It should report
warnings or errors with complete remediation descriptions for:

- deprecated command references in parsed `pack.toml` / `city.toml` command or
  doctor hooks
- deprecated command references in `.gc`-managed generated files
- deprecated command references in pack/agent prompt templates controlled by
  Gas City
- registry config parse failures
- unreachable or stale registry catalogs
- manifests that reference removed registries
- invalid durable imports that still contain command-time registry selectors
- missing, stale, or schema-1 lock entries
- cache entries missing for lock nodes
- ref/commit/hash mismatch
- withdrawn locked releases

Free-form repository-wide scanning of arbitrary docs and scripts is future
work, not part of this proposal.

## Verification

Before implementation is considered complete, test coverage should include:

- unit tests for source/name classification, including bare-name versus local
  path, qualified names, HTTPS URLs, `file:` URLs, Windows paths, and invalid
  names
- registry catalog parsing tests for schema, immutable fields, source kind,
  full commit SHA validation, and hash format
- integration tests with a stub HTTPS registry and a local `file:` git registry
- catalog parse tests that reject `file:` pack sources in HTTPS catalogs
- lockfile tests for `add`, `add --no-sync`, `sync`, `remove`, `outdated`,
  `upgrade`, and `list --transitive`
- lockfile schema migration tests for existing schema-1 files
- scope tests for ambient pack, city pack, explicit pack, explicit city,
  explicit rig, ambiguous rig directory, and no ambient context
- integrity tests that fail on ref/commit mismatch and hash mismatch
- canonical tree-hash tests for ordering, modes, symlinks, exclusions, and
  rejected submodules
- concurrency tests for parallel registry config writes
- JSON golden tests for schema-versioned output
- doctor tests for old command remediation messages
