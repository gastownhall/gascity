# Registry-Aware Version Resolution

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-25 |
| Author(s) | Emmanuel Sciara |
| Issue | [#3710](https://github.com/gastownhall/gascity/issues/3710) |
| Related | [#3659](https://github.com/gastownhall/gascity/issues/3659), [gascity-packs#137](https://github.com/gastownhall/gascity/issues/137), [#3644](https://github.com/gastownhall/gascity/issues/3644) |

## Summary

`gc import add <remote-pack> --version <constraint>` resolves the version
constraint against the **source repository's git tags**, never against the
**registry's published release→commit table**. In a monorepo registry such as
`gastownhall/gascity-packs` — where packs are released by pinning a commit on
`main` and git tags are sparse or stale — these are two independent version
namespaces. Conflating them makes the registry's own recommended command
(`gc import add … --version '>=0.2.1'`) resolve to the wrong commit (a stale
`v0.3.0` tag, hundreds of commits behind and predating the pack), whose tree
lacks the expected `pack.toml`. `validateCachedPackRoot` then fails *and deletes
the cache dir*, surfacing a misleading "missing pack.toml" error.

This design makes version constraints for **registry-known** sources resolve
against the registry's published release→commit mapping. Git-tag resolution
remains the fallback **only** for sources not known to any configured registry.

## Problem: two version namespaces

| Namespace | Source of truth | Today's resolver |
|---|---|---|
| Per-pack registry releases | `packregistry.CatalogRelease` (`Version`→`Commit`) | unused (presentation-only) |
| Repo-wide git tags | `git ls-remote --tags` | authoritative |

`internal/packman` is structurally registry-blind: `resolveSource` →
`ResolveVersion` resolves purely from git tags and never consults the
authoritative catalog table. For a monorepo registry the two namespaces diverge,
so the only correct answer for a registry-owned pack lives in a table the
resolver never reads.

## Contract

**Registry releases are authoritative for registry-known sources; git tags are
the fallback only for sources no configured registry owns.**

The single resolution call site is `syncState.resolveSource` →
`ResolveVersion(source, constraint)` (`internal/packman/install.go`), reached by
**every** import operation — add, install, remove, upgrade — through the
`syncImports = packman.SyncLock` seam, and by **transitive/nested** imports via
`walkImport`. Injecting a registry-aware resolver at this one site fixes all
parallel siblings and nested imports at once.

### Decision matrix

| Situation | Behavior |
|---|---|
| Source unknown to every configured registry | Resolver returns `ok=false`; fall back to git tags (unchanged behavior for plain git sources). |
| Source is registry-known, a release satisfies the constraint | Pin the release's commit; record the registry semver as the locked version. |
| Source is registry-known, **no** release satisfies the constraint | **Fail-closed**: error listing available versions. Never silently degrade to a git tag — silent fallback *is* the bug. |
| Constraint is `sha:<hex>` | Resolver returns `ok=false`; the existing end-to-end `sha:` pin path stays authoritative. |
| Catalog not cached / unreadable (cold cache, offline) | Resolver skips that registry (`ok=false`); the deep resolver never performs I/O. The interactive `gc import add` command refreshes a missing catalog *before* resolving (side effect confined to Layer 0). |

## Why injection, not import

`packman` must stay registry-blind — importing `packregistry` would create an
import cycle and bury registry assumptions in a generic SDK path. Instead:

1. **`packman` exposes a seam.** The data-source-agnostic selection guts are
   extracted into an exported `SelectVersion(candidates map[string]string,
   constraint)` so the same semver+constraint matching serves both tags and
   registry releases. A nil-by-default package var plus
   `SetVersionResolver(VersionResolver)` mirrors the existing `runGit` test-seam
   pattern.
2. **`resolveSource` consults the resolver first.** On `err` it propagates
   (fail-closed); on `ok` it stores the registry commit; otherwise it falls
   through to the unchanged `ResolveVersion` (tags) path.
3. **`cmd/gc` constructs and installs the resolver.** The orchestration layer
   already may import both `packman` and `packregistry`. The closure normalizes
   the import source via `remotesource.Parse` (CloneURL + Subpath), matches it
   against each configured registry's cached `CatalogPack.Source`, builds
   `version→commit` from non-withdrawn `Releases`, and calls
   `packman.SelectVersion`. It is installed once at process startup via
   `packman.SetVersionResolver`, so every `syncImports` caller picks it up.

### Threading: package seam var, not variadic `SyncLock`

The `syncImports = packman.SyncLock` seam is assigned by ~35 test stubs and 7
production callers as
`func(string, map[string]config.Import, packman.InstallMode)…`. A variadic
`SyncLock(..., opts...)` would change the var's type and break every stub and
the seam. The package var keeps all signatures and callers untouched. The var is
set **once** at process init and read-only thereafter, so the read in
`resolveSource` is race-free without synchronization; tests save/restore it via
`t.Cleanup` and must not run in parallel while it is set.

### Source matching: normalized, not exact

Catalog sources and user-typed sources differ in trailing slash, `.git` suffix,
and scheme handling. Matching uses a normalized key derived from
`remotesource.Parse` (CloneURL trimmed of `.git`, plus Subpath) — **not**
`packregistry.NormalizeSource`, which normalizes a registry-file location rather
than a pack source.

## Rejected alternatives

- **`packman` imports `packregistry`.** Creates an import cycle and violates the
  layering invariant that keeps provider-specific behavior out of generic SDK
  paths.
- **Variadic `SyncLock(..., opts...)`.** Breaks the ~35-stub `syncImports` seam
  for no behavioral gain over a package var.
- **Network in the deep resolver.** Would add network to `gc doctor`, `gc rig`,
  and install/upgrade paths. Refresh-on-miss is confined to the interactive
  `gc import add` command instead.

## Blast radius

- 7 production `SyncLock` callers all benefit; none change signature. The
  resolver must be installed at init before any can run.
- Upgrade / selective-upgrade reach the same resolver-first block — correct;
  `matchesExisting` still short-circuits unchanged entries.
- Lockfile shape unchanged: `ResolvedVersion{Version, Commit}` via the existing
  `storeChosen`→`LockedPack`; `Version` is now a registry semver string (still
  semver-parseable).
- `mergeConstraints` / `matchesExisting` and `sha:` pins are unaffected (the
  resolver returns `ok=false` for `sha:`).
