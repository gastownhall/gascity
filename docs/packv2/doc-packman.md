# City/Pack Import Management

**GitHub Issue:** TBD

Title: `feat: gc import — URL-based package manager for Gas City packs`

Companion to [doc-pack-v2.md](doc-pack-v2.md) ([gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360)), which defines the pack/city model and import semantics that this package manager operates on.

> **Keeping in sync:** This file is the source of truth. When a GitHub issue is created, edit here, then update the issue body with `gh issue edit <N> --repo gastownhall/gascity --body-file <(sed -n '/^---BEGIN ISSUE---$/,/^---END ISSUE---$/{ /^---/d; p; }' issues/doc-packman.md)`.

## Status update — 2026-04-10

The merge-wave design decisions now settled around `packs.lock` are:

- `packs.lock` is part of config/import management; the loader should
  assume composed config is correct rather than acting as a first-class
  lock orchestrator.
- The permanent cache/source contract is the Track 2 model:
  - `RepoCacheKey = sha256(normalizedCloneURL + commit)`
  - `packs.lock` keys remain the verbatim declared source string
- Remote git sources without a discernible semver tag are treated as
  path-like leaves:
  - no `packs.lock` entry
  - no version-lock / upgrade semantics
  - `gc import install` should fail if the city depends on them, because
    install is the reproducibility gate
- `pack.version` remains descriptive metadata for now. It does not
  become the managed remote version source in this wave.
- The remaining Track 6 work should be a narrow loader-read-path harvest
  that fits this contract, not a wholesale merge of the old branch.

---BEGIN ISSUE---

> **This proposal is part of a three-part design** that seeks to:
>
> - **Provide clean state separation for cities and packs** (a.k.a., have `city.toml` not do three different jobs) — [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360) (Pack/City v.next).
> - **Provide a more robust model for importing, identifying, and versioning packages** — [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360) (defines the `[imports]` schema and the city-as-pack refactor) plus **this issue** (`gc import`, which implements semver constraints, transitive resolution, and the lock file on top of that schema).
> - **Provide an initial package management mechanism for both on-machine and internet registries** — **this issue** (`gc import`) and [gastownhall/gascity#447](https://github.com/gastownhall/gascity/issues/447) (`gc registry`, the local pack store + discovery surface).
>
> The three issues are designed to land in sequence. `gc import` (this issue) ships first against the v1 schema. `gc registry` (#447) ships next, paired with `gc import` and exposing the local pack store and discovery surface. Pack/City v.next (#360) lands when the gascity loader gains the small set of capabilities the new schema requires. None of the three blocks the others — but they make more sense read together than in isolation.

## Problem

Gas City packs today are wired by hand. To use a remote pack, you copy a git URL into city.toml, pick a ref, run `gc pack fetch`, and hope the version you chose is compatible. There's no discovery, no version constraints, no way to add/remove/update packs without editing TOML by hand, and no concept of transitive resolution — if `gastown` depends on `polecat`, you have to know that and wire `polecat` yourself.

This proposal introduces **`gc import`**: an "in the small" package manager that handles URL-based pack identity, semver constraints, transitive resolution, lock files, and vendoring. It ships as a Gas City pack — pure Python, no Go changes required for v1 — and migrates to a cleaner v2 schema when the gascity loader gains a few small capabilities.

### What we need

1. **One verb to add a pack from a git URL.** Resolve the constraint, fetch the repo, recurse through the dep graph, write everything to the lock file, populate the cache. Done.
2. **Reproducibility.** A teammate clones the city repo and runs one command to get an identical working tree.
3. **Upgrade in place.** Bump locked versions within the user's existing constraints without hand-editing TOML.
4. **Transitive resolution.** Importing a pack pulls in its dependencies automatically. The user only thinks about what they directly want.
5. **Implicit baseline imports.** A small, hardcoded list of packs that every city implicitly imports unless it opts out. v1 has exactly one: `maintenance`. The point is that essential infrastructure agents are always present without each city having to know to wire them up.
6. **No new gascity Go work for v1.** The package manager runs against the current schema. v2 is a clean migration when the loader catches up.

### What we don't need (yet)

- An "in the large" centralized registry server *as part of `gc import`*. URLs remain the identity at resolve time. (A separate `gc registry` surface is in scope as a companion design — to be posted soon. It MAY influence `gc import add` if we decide to support `gc import add <name>` resolving through the local registry; that's an open question owned by the registry doc.)
- **Vendoring.** Snapshotting a pack into the city's source tree (the "seal in amber" model — we called it `gc import freeze` in earlier drafts) is **out of scope for v1**. The decision was to cut it; if users scream we'll bring it back. Hermetic builds, airgap, and "must-build-in-10-years" use cases will be reframed in terms of the local registry once that's designed. The `./packs/` directory and the `freeze` verb are gone.
- A SAT solver. Conflicts are errors with clear remediation.
- Package signing or provenance beyond commit hashes.

### Discovery is a separate concern

Discovery — "what packs exist that I might want?" — is the responsibility of the yet-to-be-implemented `gc registry`, not `gc import`. The two are designed in tandem but they're orthogonal pieces of work. `gc import` is fully URL-driven at resolve time and never touches an internet service. `gc registry` is where browsing, search, curation, and pre-warming live.

The one place `gc registry` might influence `gc import` is whether `gc import add <name>` (bare name) resolves through the local registry. That's an open question owned by the registry doc; the v1 of `gc import` is URL-and-path only, and bare-name resolution is deferred to phase 1.5 at the earliest.

## Design

### Core concepts

The minimum set of design principles. Read this first; the details below all rest on these.

**A pack's identity is its source.** Most packs are git repositories, identified by their clone URL. A pack can also be a local directory on disk, identified by its path. The two forms are different shapes of the same idea: an `[imports.X]` block points at *somewhere a pack lives*, and the resolver materializes it into the city.

#### Import source taxonomy (POR)

Imports have one public locator field: `source`.

The importer's access method and the target's behavior are related, but they are
not the same thing. The resolver first uses the source string to reach the
target, then classifies the target kind.

**Access methods.** A source may arrive as:

- a filesystem path such as `./packs/foo`, `../foo`, or `/abs/path/foo`
- `file://...`
- `https://...`
- `ssh://...`
- `git@...`
- bare `github.com/org/repo`

**Target kinds.** After resolution, an import target falls into one of four
behavioral buckets:

- a plain directory target
- a git target with discernible semver tags
- a git target without discernible semver tags
- an invalid pack target

**Pack validity is explicit.** A reachable target is only a valid import target
if the resolver can read `pack.toml` and the pack is schema-compatible. A URL
that is syntactically valid but does not yield a compatible pack is still an
invalid pack target.

**Plain directory targets stay directory-backed.** If the resolved target is a
plain directory, the import stays local and direct: no semver resolution, no
git-backed lock semantics, and no synthesized version field. This remains the
escape hatch for pack-author iteration.

**Git targets are git-backed no matter how they were reached.** `https://...`,
`file://...`, bare `github.com/...`, and even a local path that resolves to a
git repository all use the same git-backed import model. For local filesystem
paths, `gc import add` canonicalizes git-backed targets to `file://...` (with a
`//subpath` suffix when needed) so the stored `source` carries strong identity
and the lock/cache path stays stable.

**Semver-managed vs. SHA-managed imports.** Once a target is known to be git:

- if semver tags are present and `version` is absent, `gc import add` resolves
  the latest semver tag and writes the default caret constraint
- if semver tags are present and `version` is a semver constraint, the resolver
  picks the highest matching tag
- if `version` is `sha:<commit>`, the resolver pins that exact commit and tags
  are irrelevant
- if semver tags are absent and `version` is absent, `gc import add` resolves
  HEAD at import-creation time and writes `sha:<resolved-commit>`
- if semver tags are absent and `version` is a semver constraint, resolution is
  an error

**Responsibility split.** Semver tags are supplied by the repo owner. `sha:`
pins are supplied by the importer.

**Identity split.** The stored `source` string is the public import identity.
Cache identity is computed from the normalized clone URL plus commit, so
different access forms that reach the same git repo share cache entries once
they resolve to the same clone URL and commit.

**The local handle is the namespace key.** Each `[imports.X]` block introduces a local handle `X` that becomes the pack's namespace inside the importing city — what appears in the city's cache directory at `.gc/cache/packs/X/`, what agents are qualified by (`X.mayor`), what the loader uses to look up the pack at startup. The URL is identity for *resolution* (deduplication, conflict detection); the handle is identity for *consumption*.

**Transitive resolution is automatic.** When `gc import add gastown` runs, the resolver fetches gastown's repo, reads its `pack.toml`, sees `[imports.polecat]`, fetches polecat's repo, reads *its* `pack.toml`, and so on until everything is materialized. Every node in the closure ends up in `pack.lock` with a `parent` field marking transitive entries.

**The hidden download accelerator** at `~/.gc/cache/repos/<sha256(url+commit)>/` stores git clones, keyed by URL+commit. Two cities with different commits get separate clones; two cities with the same commit share. Never user-visible — no commands inspect or manipulate it; wiping it just makes the next fetch slower. This is the Go modules model.

**`pack.lock` is the source of truth.** It records the exact resolved transitive closure: URL, commit SHA, content hash, version, constraint, and parent (when transitive). It's committed. `gc import install` reproduces a city's state exactly from the lock without any other input.

#### Implicit imports

Every city gets a small set of **implicit imports** — packs that are pulled in automatically, regardless of whether the user has ever invoked `gc import` directly. **In v1, the implicit list contains exactly one entry: `maintenance`.** Every city gets a maintenance pack the moment its imports are resolved, the same way every Linux system gets `cron` without you asking for it.

The point: essential infrastructure agents are always present without each city having to know to wire them up.

#### How it works: a lexical splice of `[imports]`

The mechanism is deliberately small. There's a TOML file at `~/.gc/implicit-import.toml` containing **the same `[imports.X]` blocks you'd write in `city.toml`**:

```toml
# ~/.gc/implicit-import.toml — undocumented in v1; hand-edited if needed

[imports.maintenance]
source = "https://github.com/gastownhall/maintenance"
version = "^1"
```

When the resolver computes a city's effective import set, it reads `[imports]` from `~/.gc/implicit-import.toml` and merges those entries into the city's own `[imports]` table. The merge is **`[imports]`-only** — anything else in the implicit file (e.g. a stray `[beads]` or `[workspace]` section) is ignored, with a warning. The implicit file's contract is "contribute imports, nothing else."

The mental model: **the entries in `~/.gc/implicit-import.toml` behave as if they were lexically prepended to the city's `[imports]` section**. That's not literally how TOML parsing works (TOML has no `#include`), but it's the user-visible result. The merge happens after parse, on dicts, and the rule is simple: the city's own `[imports]` wins on any handle collision.

This file format is deliberately **identical to a fragment of `pack.toml`/`city.toml`**. It's not a new schema. Users who can write `[imports]` blocks already know how to edit `~/.gc/implicit-import.toml`. The same parser, the same validation, the same conflict-detection rules — there's just one extra source of import entries to merge before resolution begins.

#### Who owns the file

**`~/.gc/implicit-import.toml` is set by us, not by users.** It is not a configuration file. It's not documented in the user-facing docs, there are no commands to manage it, and the expected state is "the file exists with whatever we shipped, and nobody touches it." Think of it less like `~/.bashrc` and more like a vendor-installed config under `/etc/` that ships with the OS — present, predictable, hands-off.

The reason it lives in a file rather than in source code is purely implementation hygiene:

- The file format is identical to a `[imports]` fragment, so the splice can use the same parser and the same merge logic that the resolver already uses for the city's own imports.
- If we ever need to update the implicit list — bump the maintenance pack version, add a second entry — that's a `gc-import` (or gascity loader) update that ships a new default file. Users get the new default the next time they upgrade.
- If we ever need a per-machine escape hatch (e.g. an internal team needs to point at their fork), we have one available without inventing a new mechanism: edit the file. That's an emergency exit, not a documented user-facing capability.

The package manager writes a default `~/.gc/implicit-import.toml` on first run if it doesn't already exist. The default contains exactly one entry: `maintenance`, pointing at the canonical URL. Once written, the package manager doesn't touch the file again unless we ship a new default and the user runs `gc import` after the upgrade.

#### When the splice happens

The splice runs at **resolution time**: any time the city's import closure needs to be computed, the implicit file is read and merged in.

In **v1**, that means `gc-import` (the pack) does the splice in Python whenever `gc import add` / `install` / `upgrade` runs. The resolver reads `~/.gc/implicit-import.toml`, merges its `[imports]` into the city's `[imports]`, and then resolves the union as if all the entries had come from `city.toml`. The merged closure is written to `pack.lock`; entries that came from the implicit file are tagged with `parent = "(implicit)"` so `gc import list` can show them.

In **v1.5+**, after the gascity loader patches land (which already include "recognize `[imports]` blocks in `city.toml`"), the loader picks up the same splice. It reads `~/.gc/implicit-import.toml`, merges `[imports]` into the city's `[imports]`, hands the merged dict to whatever does resolution. **The file format is identical between v1 and v1.5+; only the consumer changes.**

#### Known v1 limitation

In v1, the splice only happens when `gc-import` runs. That means a user who creates a city via `gc init` and starts it via `gc start` — without ever invoking `gc-import` directly — would not get the maintenance pack until they ran `gc import install` once.

Two mitigations:

1. **`gc init` runs `gc import install` once at the end of city creation** (when `gc-import` is installed on the machine). This makes the typical flow zero-touch: `gc init my-city` produces a city that already has the maintenance pack materialized in `pack.lock` and `.gc/cache/packs/`.
2. **In v1.5+, the loader does the splice at startup**, removing the dependency on running `gc-import` at all. The implicit list is honored regardless of whether the package manager has been invoked. This is the long-term answer; the `gc init` workaround is just a v1 bridge.

This is documented as a known v1 wrinkle and called out in Open Questions.

#### Visibility, opt-out, and override

All three of these knobs live **in the city's own `city.toml`** — never in `~/.gc/implicit-import.toml`. The implicit file is not a user-config layer; the city is.

- **Visibility.** Implicit imports do **not** appear in the `[imports]` section of `city.toml` (or `pack.toml` in v2). They're external inputs to the resolver, recorded only in `pack.lock` and the cache. `[imports]` in city.toml shows the user's *direct intent*; `pack.lock` shows what's *actually installed*. `gc import list` shows implicit entries with an `(implicit)` marker so users can see the maintenance pack appearing in their city without having to know that `~/.gc/implicit-import.toml` exists.
- **Opt-out.** A city can disable implicit imports entirely by setting `implicit_imports = false` at the top level of `city.toml` (v1) or `pack.toml` (v2). When that flag is false, the resolver skips `~/.gc/implicit-import.toml` entirely; the maintenance pack is not fetched, locked, or materialized. This is the per-city escape hatch for embedded / minimal / specialized cities that don't want the baseline.
- **Override.** A city that wants a *different* maintenance pack — a fork, an internal version, a pinned older version — can add an explicit `[imports.maintenance]` block to its own `city.toml`. The merge rule (city wins on collision) means the explicit version wins and the implicit one is silently dropped. No auto-suffixing, no parallel installs, no special-case code — it falls out of normal collision handling.

### What `gc import` solves over editing TOML by hand

The value proposition of `gc import` is best seen against the world we live in *today* — Gas City as it shipped before any package manager existed. The table below is "hand-editing today" vs "with `gc import`". Whether the city is using v1 or v2 schema doesn't change the answer; the v1/v2 distinction is a Pack/City schema concern (see the [appendix](#appendix-v1-vs-v2-schema) and [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360)), separate from the package manager itself.

| Pain | Hand-editing today | With `gc import` |
|---|---|---|
| Wiring a new remote pack | Hand-edit two TOML sections (`[packs.X]` and `[workspace].includes`) | `gc import add <url>` |
| Knowing which version you'll get | Whatever the git ref points at right now — could be a moving branch | A specific tag matching your semver constraint, recorded in `pack.lock` |
| Reproducing on another machine | Hope the ref hasn't moved; clone everything by hand | `gc import install` from `pack.lock` (same commit, hash-verified) |
| Bumping versions | Edit ref by hand, hope nothing breaks | `gc import upgrade [<name>]` with constraint-respecting re-resolution |
| Picking up a pack's dependencies | You have to know about them and wire each one yourself | Transitive resolution does it for you |
| Baseline infrastructure (maintenance pack) | Each city has to know to wire it | Implicit; every city gets it automatically (opt-out via `implicit_imports = false`) |
| Knowing where a pack came from | Inferred from `[packs.X].source`; no commit pinning | URL + commit + content hash recorded in `pack.lock` |
| Constraint visibility | n/a — there are no constraints | `[imports.<name>] version = "..."` block, hand-editable |

The runtime behavior of the loader is **unchanged** by `gc import`. After running `gc import add foo`, the city's `[packs]` and `[workspace].includes` sections look exactly like what you'd write by hand — the package manager is just the tool that wrote them, with versioning and a lock file to back up its choices.

### Verbs

#### `gc import add <source> [--version <constraint>] [--name <handle>]`

Add a pack to the city's imports.

- **Plain directory target** (`gc import add ../my-local-pack` where the target is
  not a git repo): writes a direct local import. No version is synthesized, no
  git-backed lock entry is created, and recursion stays local-directory style.
- **Git-backed target** (`gc import add https://github.com/example/gastown`,
  `gc import add file:///Users/me/src/gastown`, or even `gc import add ../gastown`
  when `../gastown` is itself a git repo): resolves the target as a git-backed
  import, materializes it in the shared repo cache, writes lock state, and
  records the user's direct intent in `[imports]`.

The `--version` flag accepts semver constraints such as `^1.2`, `~1.2.3`,
`>=1.0,<2.0`, or an exact `sha:<commit>`. If `--version` is omitted:

- semver-tagged git targets synthesize the default caret constraint from the
  latest semver tag
- untagged git targets synthesize `sha:<resolved-HEAD-commit>`
- plain directory targets omit `version`

The recorded `version` string lives in `[imports.<name>] version = "..."`; the
resolved version/commit lives separately in `pack.lock`. Subsequent
`gc import upgrade` re-runs semver-managed imports against fresh tags but never
modifies the declared constraint itself.

**Resolving handle collisions with `--name`.** The local handle (the key in `[imports.<handle>]`) is derived from the URL or path's last segment by default — `https://github.com/example/gastown` becomes `gastown`. If that handle already exists in the city's `[imports]` (e.g., the user already imported a different pack that resolved to the same default name, or two URLs both resolve to a handle the user wants to use), `gc import add` errors and tells the user to retry with `--name <alias>` to pick a different local handle. This is the same shape as `gc rig add` and `gc city register` — the default name is offered, the user overrides on collision. The resolver never auto-suffixes.

```
$ gc import add https://github.com/example/gastown                     # default constraint, default handle
$ gc import add https://github.com/example/gastown --version "^1.5"     # explicit constraint
$ gc import add https://github.com/example/gastown --version "~1.5.3"   # patch-level only
$ gc import add https://github.com/example/gastown --version "1.5.3"    # exact pin
$ gc import add https://github.com/other-org/gastown --name other-gtwn  # alias to dodge a collision
```

```
$ gc import add https://github.com/example/gastown
Resolving https://github.com/example/gastown...
  Available versions: 1.0.0, 1.1.0, 1.2.3
  Selected: 1.2.3 (latest, default constraint ^1.2)
  Cloned → ~/.gc/cache/repos/<hash>/  (hidden)
  Recursing into [imports]:
    polecat → https://github.com/example/polecat
      Selected: 0.4.1 (constraint ^0.4)
      Cloned → ~/.gc/cache/repos/<hash>/  (hidden)
  Materialized → .gc/cache/packs/gastown/, .gc/cache/packs/polecat/
  Updated city.toml ([imports], [packs], includes) and pack.lock (2 entries)
```

#### `gc import remove <name>`

Remove a pack from the city's imports.

- Drops the entry from `[imports]` in `city.toml` (v1) or `pack.toml` (v2).
- Removes the corresponding `[packs.X]` block from city.toml and the entry from `[workspace].includes` (v1).
- **Garbage-collects transitive deps** that are no longer needed. If `polecat` was in the lock only because `gastown` imported it, and `gastown` is being removed, `polecat` is removed too. **A transitive dep can have multiple parents** (e.g., both `gastown` and `maintenance` may pull in `polecat`). The lock file's `parent` field is therefore a *set* of parent handles, not a single value, and a transitive dep is GC'd only when all of its parents have been removed.
- Prunes the city cache directories for everything that was removed. (Once a registry is in scope, the registry retains a reference-counted copy and only the city cache is pruned. For v1 with no registry the two are the same operation.)
- Implicit imports (like `maintenance`) cannot be removed via `gc import remove`. They're not in `[imports]` to drop. To stop fetching the maintenance pack, set `implicit_imports = false` in `city.toml`.

```
$ gc import remove gastown
Removing gastown...
  Dropped [imports.gastown] from city.toml
  Removed [packs.gastown] from city.toml
  Removed "gastown" from [workspace].includes
  Garbage-collecting transitive deps no longer needed:
    polecat (was a dep of gastown only)
  Removed [packs.polecat], "polecat" from includes
  Deleted .gc/cache/packs/gastown/, .gc/cache/packs/polecat/
  Updated pack.lock
```

#### `gc import install`

Restore the city to the exact state recorded in `pack.lock`. This is the cold-clone / CI / teammate-onboarding command.

- Reads `pack.lock`.
- For each entry, fetches the URL at the recorded commit (using the hidden accelerator if a copy already exists for that commit hash).
- Materializes each pack into `.gc/cache/packs/<name>/`.
- Verifies the content hash matches the lock entry; errors on mismatch.
- Does **not** modify `city.toml`, `pack.toml`, or `pack.lock`. Pure restore.

```
$ gc import install
Installing from pack.lock...
  gastown v1.2.3 ✓
  polecat v0.4.1 ✓ (transitive: gastown)
  maintenance v2.0.1 ✓
```

#### `gc import upgrade [<name>]`

Re-resolve the constraints in `[imports]` (in `city.toml` for v1, in `pack.toml` for v2) against the latest available tags, pick higher versions where the constraint allows, and rewrite `pack.lock`.

**How constraints get set.** A constraint is written to `[imports.<name>] version = "..."` exactly once — when the user runs `gc import add <url> [--version <c>]`. From that point on, the constraint can be changed in two ways:

1. **Hand-edit `[imports.<name>] version = "..."`** to bump (e.g., `^1.2` → `^2.0`). This is the expected flow for major-version bumps and other intentional constraint changes. After editing, run `gc import upgrade <name>` to re-resolve.
2. **Run `gc import add <url> --version <new>`** with the same handle. The "already exists" check would currently error; a future `--force` flag could be added if hand-editing proves clunky. For v1, hand-edit is the intended path.

`gc import upgrade` itself **never modifies the constraint** — only the resolved version in the lock. This separation is deliberate: the constraint expresses user intent ("I am OK with anything compatible with 1.2"); the resolved version is the resolver's answer ("the highest such thing was 1.5.0"). Conflating them would mean every upgrade silently widens the user's risk surface.

- With no argument: every pack in the closure (subject to its constraint).
- With a name: just that pack and everything transitively under it.
- Re-recurses into transitive imports because a newer version of a pack may have changed *its* dependency constraints.

```
$ gc import upgrade gastown
Fetching tags for https://github.com/example/gastown...
  Constraint: ^1.2
  1.2.3 → 1.3.0
  Re-reading [imports]: polecat ^0.4 (unchanged)
  Materialized → .gc/cache/packs/gastown/
  Updated city.toml [packs.gastown].ref, pack.lock
```

#### `gc import list [--tree]`

Show what this city imports.

- Default: a flat table of every pack in `pack.lock` (direct + transitive), one row per pack, with the constraint, resolved version, URL, and parent (for transitive).
- `--tree`: an indented tree showing the import graph.

**Name collisions in the transitive closure.** Two transitive imports can resolve to the same local handle from different parents — for example, both `gastown` and `maintenance` may pull in something they each call `polecat`. There are three cases:

1. **Same handle, same URL, compatible versions** → unify (single entry in the closure with a multi-parent set).
2. **Same handle, same URL, incompatible majors** → cross-major conflict; resolver errors with the disambiguation hint described under "Side-by-side versions" below.
3. **Same handle, different URLs** → hard conflict. The resolver errors with: "two parents claim the local handle 'polecat' but they refer to different repos: gastown wants `https://github.com/example/polecat`, maintenance wants `https://github.com/other/polecat`. Add an alias in your `[imports]` to disambiguate." The user resolves it by adding an explicit `[imports.<alias>]` block in `city.toml`/`pack.toml` that re-binds one of them to a non-colliding handle.

```
$ gc import list
NAME         VERSION  CONSTRAINT  URL                                       PARENT
gastown      1.2.3    ^1.2        https://github.com/example/gastown        —
polecat      0.4.1    ^0.4        https://github.com/example/polecat        gastown
maintenance  2.0.1    ^2.0        https://github.com/example/maintenance    —

$ gc import list --tree
gastown 1.2.3 (^1.2) — https://github.com/example/gastown
└── polecat 0.4.1 (^0.4) — https://github.com/example/polecat
maintenance 2.0.1 (^2.0) — https://github.com/example/maintenance
```
#### Declarative: `[imports.foo] source = "..."` (no verb)

Path imports are introduced in Core concepts above. Listed here for completeness because they're the third way to bring a pack into a city, alongside `gc import add <url>` and the implicit-imports list. Edit `[imports]` in `city.toml` (v1) or in `pack.toml` (v2) by hand to add a path import:

```toml
[imports.foo]
source = "../foo"
```

`gc import add <path>` is sugar for editing this by hand. Both forms write the same TOML.

### Side-by-side versions

Three distinct cases, all resolved by treating the local handle as the namespace key and the URL as the resolution identity.

**Case 1: Different versions in different cities on the same machine.** Trivial. The hidden accelerator is keyed by URL+commit; each city has its own `pack.lock` and its own checkout. They never share state at the city level.

**Case 2: Within-city transitive conflict.** When two transitive constraints on the same URL meet:

- **Same major** → unify to the highest version satisfying both. polecat 1.2.3 and 1.5.0 → 1.5.0.
- **Different majors** → the resolver errors with a remediation hint, asking the user to add explicit `[imports.X_v1]` and `[imports.X_v2]` blocks with different local handles. The resolver never auto-suffixes.

```
Conflict: polecat is required at incompatible majors.
  - gastown wants polecat ^1.2 (would resolve 1.5.0)
  - maintenance wants polecat ^2.0 (would resolve 2.0.1)

Add explicit imports to disambiguate. In your city.toml (v1) or pack.toml (v2):

  [imports.polecat_v1]
  source = "https://github.com/example/polecat"
  version = "^1.2"

  [imports.polecat_v2]
  source = "https://github.com/example/polecat"
  version = "^2.0"
```

**Case 3: Intentional dual-import.** The user *wants* two versions (migration, A/B testing). Just write two `[imports]` blocks with different local handles pointing at the same URL. Each gets its own cache directory and lock entry; agents become `polecat_v1.scout` and `polecat_v2.scout`.

### Storage layout

```
~/.gc/
└── cache/
    └── repos/                  # hidden download accelerator (hash-keyed)
        ├── a1b2c3.../          # clone of one repo at one commit
        ├── d4e5f6.../
        └── 789abc.../

<city>/
├── city.toml                   # committed; user-managed deployment config + machine-managed [packs]/includes (v1 only)
├── pack.toml                   # committed; v2 only — replaces v1's [imports]/[packs]/includes in city.toml
├── pack.lock                   # committed; full resolved transitive closure
└── .gc/
    └── cache/
        └── packs/              # gitignored, derived from pack.lock
            ├── gastown/
            ├── polecat/
            └── maintenance/    # the implicit import
```

### Lock file format

```toml
# Auto-generated by gc import. Commit for reproducibility.
schema = 1

[packs.gastown]
url = "https://github.com/example/gastown"
version = "1.2.3"
constraint = "^1.2"
commit = "a1b2c3d4e5f6..."
hash = "sha256:9f8e7d6c5b4a..."

[packs.polecat]
url = "https://github.com/example/polecat"
version = "0.4.1"
constraint = "^0.4"
commit = "789abc012..."
hash = "sha256:..."
parent = "gastown"

[packs.maintenance]
url = "https://github.com/gastownhall/maintenance"
version = "1.5.0"
constraint = "^1.5"
commit = "deadbeef..."
hash = "sha256:..."
parent = "(implicit)"

# Multi-pack monorepo example: URL has a subpath inline AND a
# separate subpath field for fast access without re-parsing.
[packs.foo]
url = "https://github.com/example/multi-pack/foo"
subpath = "foo"
version = "1.4.0"
constraint = "^1.4"
commit = "4d320fd6f054..."
hash = "sha256:..."
```

Field reference:

- **`url`** — the repo URL the user (or upstream) provided. For multi-pack monorepos, the URL includes the subpath inline (e.g., `https://github.com/example/multi-pack/foo`). Identity for resolution.
- **`subpath`** — present iff the URL points at a pack inside a multi-pack monorepo. Holds the subpath portion of the URL as a separate field, redundant with what's in `url` but available without re-parsing. The download accelerator clones the *repo* (URL minus subpath); the loader reads `pack.toml` from the *subpath* inside that clone.
- **`version`** — the resolved semver, parsed from a git tag.
- **`constraint`** — the semver constraint that resolved to this version. Used by `upgrade` to re-resolve.
- **`commit`** — the git commit SHA at the resolved tag. Identity for the download accelerator.
- **`hash`** — content hash of the materialized pack directory. Used by `install` to verify integrity.
- **`parent`** — present iff this entry was *not* a direct import. Either names the importing pack's local handle (transitive dep) or has the special value `"(implicit)"` for entries from the implicit-imports list (see Core concepts). Used by `gc import why <name>` (phase 2) and by `remove` for transitive garbage collection. Implicit entries are never GC'd by `remove`; they're only GC'd if the city sets `implicit_imports = false`.

The lock file's keys are **local handles**, not URLs. URLs may repeat across entries (different versions of the same pack via dual-import; cross-major coexistence in the closure).

## Implementation

`gc import` is itself a Gas City pack — pure Python, no Go changes for v1. Under V2 the commands and doctor check are convention-discovered: each command lives in its own `commands/<name>/` directory with a `command.toml` sidecar pointing at the Python entrypoint (V2 discovery defaults to `run.sh`, so non-shell entrypoints need an explicit `run = "<name>.py"` override). The pack is published as a git repo (the conventional name is `gc-import`) and installed by users via the existing `gc pack` mechanism (or, after v1 ships, via `gc import add` itself, which is a fun bootstrap).

### Repo layout

```
gc-import/
├── pack.toml                  # [pack] schema = 2 — no [[commands]] inventory
├── README.md                  # user guide (the canonical entry point)
├── doctor/
│   └── python3.11/
│       ├── doctor.toml        # description only; run.sh is the V2 default
│       └── run.sh             # verifies Python 3.11+ is available
├── commands/
│   ├── add/
│   │   ├── command.toml       # description + run = "add.py"
│   │   └── add.py
│   ├── remove/
│   │   ├── command.toml
│   │   └── remove.py
│   ├── install/
│   │   ├── command.toml
│   │   └── install.py
│   ├── upgrade/
│   │   ├── command.toml
│   │   └── upgrade.py
│   └── list/
│       ├── command.toml
│       └── list.py
├── lib/
│   ├── __init__.py
│   ├── semver.py              # constraint parsing and matching
│   ├── git.py                 # subprocess wrappers around git
│   ├── lockfile.py            # pack.lock read/write
│   ├── manifest.py            # [imports] section read from city.toml
│   ├── citytoml.py            # surgical edits to city.toml ([packs] + includes)
│   ├── implicit.py            # hardcoded implicit-imports list
│   ├── resolver.py            # transitive resolution + conflict detection
│   ├── cache.py               # ~/.gc/cache/repos/ + .gc/cache/packs/ management
│   └── ui.py                  # consistent output formatting
└── tests/
    └── ...                    # integration tests using a known test repo
```

### Dependencies

**Python 3.11+, stdlib only.** The reader uses `tomllib` (stdlib in 3.11). Writers are hand-rolled — they generate small, well-formed TOML for `pack.lock` (which the package manager fully owns), and do surgical text edits to `city.toml` (which the user partly owns) using bracketed-section finding rather than full TOML parsing. Three sections in `city.toml` are managed by `gc import`: `[imports]` (user-facing), `[packs.X]` blocks (machine-managed view of the resolved closure), and `[workspace].includes` (the loader's pack list). All three get rewritten on every `add`/`remove`/`upgrade`.

No `tomlkit`, no `tomli_w`, no `packaging`, no `gitpython`. Git operations are subprocess calls. Semver is a small custom module (~100 lines) — Gas City's needs are simple and well-bounded.

The motivation for stdlib-only: zero install friction. Users running `gc import add` for the first time should not need to `pip install` anything.

### Resolution algorithm (transitive)

```
resolve(direct_imports):
    queue = direct_imports.copy()
    closure = {}                    # local_handle → ResolvedPack
    while queue:
        import_spec = queue.pop()
        if import_spec.url:
            tags = git_ls_remote_tags(import_spec.url)
            version = pick_highest_matching(tags, import_spec.constraint)
            commit = tag_to_commit(import_spec.url, version)
            local_handle = import_spec.local_handle
            if local_handle in closure:
                # Same handle imported twice — must be the same URL+version
                if (closure[local_handle].url != import_spec.url or
                    closure[local_handle].version != version):
                    error("local handle '%s' conflicts" % local_handle)
                continue
            # Cross-major coexistence check
            for existing in closure.values():
                if (existing.url == import_spec.url and
                    same_major(existing.version, version)):
                    if existing.version != version:
                        # Same major, different version — should have unified
                        # Pick the higher one and try again
                        ...
                    else:
                        # Already resolved, deduplicate
                        continue
                if (existing.url == import_spec.url and
                    different_major(existing.version, version)):
                    error("cross-major conflict for %s" % import_spec.url)
            # Fetch and recurse
            fetch_to_accelerator(import_spec.url, commit)
            inner_pack_toml = read_pack_toml_from_clone(...)
            for inner_import in inner_pack_toml.imports:
                inner_import.parent = local_handle
                queue.append(inner_import)
            closure[local_handle] = ResolvedPack(...)
        elif import_spec.path:
            # Path imports don't recurse and don't get lock entries
            closure[import_spec.local_handle] = PathPack(...)
    return closure
```

The same-major unification is done in a small fixed point loop (re-resolve any URL whose constraint set changes when a new transitive dep is added). Cross-major conflicts are reported with the parent chain so the user knows where each constraint came from.

## Open questions (still need answers before v1 ships)

These are the questions where we don't yet have a settled answer. Each one needs a decision before v1 of `gc import` ships.

1. **Rig-level imports.** `gc import add` operates on city-level imports. Should there be a `--rig <name>` flag for rig-scoped imports in `city.toml`? **Yes, by analogy with how agents have rig-scoping with a different default.** Need to decide before phase 1 ships because adding `--rig` later changes the mental model of what the bare `gc import add` does. Likely shape: bare `gc import add` continues to write to the city's `[imports]`; `--rig <name>` writes to `[rigs.<name>.imports]` (or similar) and the loader scopes the import to that rig only. **Owner: needs a decision in this design pass.**
2. **Hash verification scope.** Hash over the git tree at the locked commit, the materialized pack directory, or both? The latter is more robust but requires deterministic file ordering. Phase 1 implementation question. **Owner: ask Julian.**

## Explicit design decisions worth calling out

These aren't open questions — they're settled, but they're decisions worth being able to point at when someone asks "why."

1. **Side-by-side versions in the local cache.** The cache at `~/.gc/cache/repos/` is keyed by `sha256(url + commit)`, which means each `(URL, commit)` pair is a separate entry. Two cities pinning the same commit share one clone; two cities pinning different commits get two clones. This is not a deferred decision — it's the intended behavior, and it matches the local-registry design ([gastownhall/gascity#447](https://github.com/gastownhall/gascity/issues/447)). If disk pressure ever becomes a real problem, we could revisit a worktree-based scheme (one clone, multiple worktrees), but that's not on the v1 roadmap.
2. **Within-city version conflicts: error message gallery is its own thing.** The remediation hint sketched above is the v1 message; tuning with real examples lives in a follow-on subsection (or a small "error message gallery" doc). Not a v1 blocker.
3. **`gc pack` retirement path: integrate with this work.** `gc pack fetch` and `gc pack list` are deprecated but not yet aliased to `gc import install` / `gc import list`. Make the deprecation part of the `gc import` rollout: leave the old verbs in with a deprecation notice that points at the new ones. How aggressive the deprecation is depends on our tolerance for breaking changes.
4. **Vendoring (`./packs/` and `gc import freeze`) is cut.** Earlier drafts shipped a `freeze` verb that vendored a resolved pack into `./packs/<name>/` and a hand-authored sub-pack convention in the same directory. Both are gone for v1. Use the local registry (when it ships) or pin a specific commit in your import constraint if you need stability. If users scream about the loss, we'll bring vendoring back — probably reframed in terms of registry pinning rather than tree-side directories.

## Phasing

**Phase 1: Verbs against v1 schema.** Everything described above, running against the current `[packs]`+`includes` schema with a new `[imports]` section added inline in `city.toml`. Ships as the `gc-import` pack with no gascity Go changes — `[imports]` is a section the v1 loader doesn't recognize, so it's silently ignored by gascity until v2 lands. The package manager owns three sections in `city.toml`: `[imports]` (user-facing), `[packs.X]` (machine-managed view of the resolved closure), and `[workspace].includes` (machine-managed pack list for the loader). Yes, this writes the imports view twice — once as `[imports]` for users, once as `[packs]`/`includes` for the loader. This is not DRY-ideal, but it's the price of making v1 ship without loader changes; v2 collapses the two views into one. The duplication is mechanical (the `[packs]`/`includes` view is fully derived from `[imports]` + `pack.lock`), so there's no risk of the two going out of sync as long as users don't hand-edit `[packs]`.

**Phase 1.5: Loader patches.** Two small additions to `internal/config/` in gascity: read pack.toml at city root, recognize `[imports]` blocks. Independent track from the package manager. See [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360) for the Pack/City v.next design that defines what these patches need to do.

**Phase 2:  v2 migration — TBD whether we even build it.** When the v2 loader patches land, we *could* ship `gc import migrate` to convert v1 cities to the v2 schema. But it's not yet decided whether we support migration at all; we might just say "v1 cities keep using the v1 schema until the user manually rewrites them" if migration tooling proves more trouble than it's worth. The lock file is identical between v1 and v2, so there's nothing to lose if a user wants to do the migration by hand. **Defer the migration-tooling decision until v2 loader work is closer.**

**Phase 3: Graph awareness.** `gc import outdated`, `info`, `why`, and improved `list --tree` once transitive imports are battle-tested.

**Later:** `gc import publish`, `gc import downgrade`, drift detection UX, and discovery surface integration via the registry.

## Alternatives considered

- **Built into gascity.** Would violate the "packs are a sufficient extension mechanism" thesis. If the package manager needs Go changes that's a signal the pack command system needs work, not that the package manager belongs in Go.
- **Centralized registry server *as part of `gc import`*.** Requires hosting infrastructure, auth, upload pipeline. Overkill for an ecosystem this size and contradicts the URL-as-identity model. (Note: a separate `gc registry` surface for *discovery* — pure pointer service, never load-bearing for builds — IS in scope as a companion design. The thing rejected here is a registry that participates in resolution, not a registry that helps with discovery.)
- **Tap-based model (brew style).** Considered and rejected during the design session. Tap monoliths force authors into multi-pack repos and add a discovery surface that's load-bearing for builds. URL-as-identity is simpler.
- **User-level registry of named packs as a `gc import` dependency** (`~/.gc/registered.toml` + register/unregister verbs as part of the package manager). Considered and rejected. `gc import` deduplicates clones invisibly via the hidden accelerator. The user-level *catalog* of named packs is real but it belongs to `gc registry`, not to `gc import`.
- **Vendoring (`gc import freeze` and `./packs/`).** Earlier drafts shipped this as experimental. Cut for v1 — see "What we don't need (yet)". The five-verb surface (`add`/`remove`/`install`/`upgrade`/`list`) is the v1 deliverable. If users scream we'll bring vendoring back, probably reframed in terms of the local registry rather than the tree-side `./packs/` directory.
- **`tomlkit` for TOML editing.** More capable but adds an install dependency. Hand-rolled writers + surgical text edits are sufficient for the small set of files we touch.

## Appendix: v1 vs v2 schema

This appendix is here for reference. **The package manager's behavior is the same under v1 and v2.** What changes between schemas is *where* the data lives — which file has the `[imports]` section and what the gascity loader reads. Those changes are owned by the Pack/City v.next design at [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360); `gc import` is congruent with both schemas and migrates between them.

The information in this appendix is not load-bearing for understanding `gc import`. Skip it unless you specifically want to know how the package manager interacts with the schema work.

### What gc import writes today (v1 schema)

In v1, everything lives in `city.toml`. There is no sidecar file. The user-facing surface is a new `[imports]` section that `gc import` reads and writes; the loader-facing surface is the existing `[packs]` and `[workspace].includes` constructs that `gc import` also writes (derived from `[imports]` + the lock). Both sections live in the same file:

```toml
# city.toml — v1 with gc import
[workspace]
name = "my-city"
includes = ["my-helper", "gastown", "polecat", "maintenance"]

# ─── user-facing: direct imports and version constraints ───
# This is what you edit (or let gc import write). gc import reads and rewrites this section.

[imports.gastown]
source = "https://github.com/example/gastown"
version = "^1.2"

[imports.maintenance]
source = "https://github.com/example/maintenance"
version = "^2.0"

[imports.my-helper]
source = "../my-helper"

# ─── machine-managed: resolved [packs] for the gascity loader ───
# gc import rewrites these from [imports] + pack.lock. Treat as outputs, not inputs.

[packs.gastown]
source = "https://github.com/example/gastown"
ref = "v1.2.3"

[packs.polecat]
source = "https://github.com/example/polecat"
ref = "v0.4.1"

[packs.maintenance]
source = "https://github.com/example/maintenance"
ref = "v2.0.1"

[beads]
provider = "bd"
```

```
my-city/
├── city.toml                 ← contains [imports], [packs], [workspace], [beads] — one file
├── pack.lock                 ← committed; full transitive closure with commit + hash
└── .gc/
    └── cache/
        └── packs/            ← gitignored, derived from pack.lock
            ├── gastown/
            ├── polecat/      ← transitive dep of gastown
            └── maintenance/
```

Three things to notice about the v1 shape:

1. **`[imports]` is the new user-facing surface that `gc import` introduces.** Today, without `gc import`, the user-facing surface for declaring packs is `[workspace].includes` — the user lists pack names there and the loader picks them up. With `gc import`, `[imports]` becomes the place where users declare what they want (with version constraints), and `gc import` re-emits the includes list and `[packs]` blocks as a derived view. `[workspace].includes` remains user-facing in the no-gc-import world; it just becomes machine-managed once `gc import` is in play.

2. **`[packs]` and `[workspace].includes` are machine-managed.** `gc import` rewrites these every time `[imports]` or the lock changes. They're a derived view of `[imports]` + `pack.lock` — treat them as outputs, not inputs.

3. **Transitive deps appear in `[packs]` and `pack.lock` but NOT in `[imports]`.** In the example above, `polecat` is a transitive dep that the user never directly asked for. The user only ever sees `polecat` in the lock file (and in `gc import list`, which reads the lock). This is the boundary between "what the user asked for" (`[imports]`) and "what the resolver figured out" (`[packs]` + lock).

### What gc import will write after the gascity loader patches (v2 schema)

```toml
# pack.toml at the city root (cities are packs in v2)
[pack]
name = "my-city"
version = "0.1.0"

[imports.gastown]
source = "https://github.com/example/gastown"
version = "^1.2"

[imports.maintenance]
source = "https://github.com/example/maintenance"
version = "^2.0"

[imports.my-helper]
source = "../my-helper"
```

```toml
# city.toml — much smaller, deployment config only
[beads]
provider = "bd"
```

```
my-city/
├── pack.toml                 ← imports + city pack identity
├── city.toml                 ← deployment config only
├── pack.lock                 ← committed; full transitive closure
└── .gc/
    └── cache/
        └── packs/            ← gitignored, derived from pack.lock
            ├── gastown/
            ├── polecat/
            └── maintenance/
```

The v2 shape collapses the v1 `[imports]` + `[packs]` + `includes` constructs in city.toml into one place: a top-level `[imports]` block in `pack.toml`. The resolver no longer has to maintain two views of the same data.

### What's actually different between v1 and v2

Beyond file locations and TOML syntax, the differences are smaller than you'd think. The package manager's behavior is the same; what changes is where the loader looks for things and which file the user (or `gc import`) edits.

| Concern | v1 | v2 |
|---|---|---|
| User-facing imports live in | `[imports]` in `city.toml` | `[imports]` in `pack.toml` (city root) |
| Loader-facing pack list | `[packs.X]` + `[workspace].includes` in `city.toml` (machine-managed) | The same `[imports]` block — no separate machine-managed view |
| Number of TOML sections `gc import` writes per add | Three (`[imports]`, `[packs]`, `[workspace].includes`) plus `pack.lock` | One (`[imports]`) plus `pack.lock` |
| Loader behavior on unrecognized sections | Ignores `[imports]` (it's a section the loader doesn't know) | Reads `[imports]` directly |
| Transitive resolution in pack manager | Same | Same |
| Hidden download accelerator | Same | Same |
| Reproducibility | `gc import install` reads `pack.lock` and rebuilds the city cache + the [packs] view | `gc import install` reads `pack.lock` and rebuilds the city cache |
| Hand-editable user surface | `[imports]` (constraints) | `[imports]` (constraints) |

The take-away: **the user-facing experience is identical**. The same `[imports]` syntax, the same verbs, the same `pack.lock`, the same constraints, the same transitive resolution. v2 is a refactor of *where* the data lives, not *what* the data is. The migration is a one-shot file move, not a behavior change.

### Migration

Per the loader investigation in [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360), the v2 schema needs two localized gascity Go changes before it can land:

1. Read `pack.toml` at the city root (treat the city as a pack).
2. Recognize `[imports]` blocks in city/pack TOML.

The package manager doesn't have to wait. It ships against the v1 schema with one compromise (the `[imports]` section is added to `city.toml` instead of living in a top-level `pack.toml`), then migrates cleanly with a one-shot `gc import migrate` when the loader patches land — *if* we decide to ship migration tooling at all (see Phasing).

The migration command, if built, would: read `[imports]` out of `city.toml`, write it to a new top-level `pack.toml`, and strip `[packs]` and `includes` from `city.toml`. Idempotent and reversible.

---END ISSUE---
