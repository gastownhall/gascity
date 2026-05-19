# Pack Import / Export Surface v0

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-05-14 |
| Author(s) | Codex |
| Issue | [#2120](https://github.com/gastownhall/gascity/issues/2120) |
| Related | [PR #2119](https://github.com/gastownhall/gascity/pull/2119) |
| Supersedes | Current `transitive` / `export` import surface |

Design note for a simpler pack import/export model that replaces the current
`transitive = ...` and `export = ...` behavior with explicit imports plus
explicit exports.

This is a proposed replacement for the current PackV2 contract, not a change to
the active user-facing syntax. The current contract remains documented in
`docs/packv2/doc-pack-v2.md`, `docs/packv2/skew-analysis.md`,
`docs/reference/config.md`, and `docs/guides/shareable-packs.md`. PR #2119 is
the antecedent implementation model this note is trying to make easier to
explain, migrate, and eventually supersede.

## Summary

We believe the current PackV2 import surface is trying to express a useful
idea through the wrong abstraction.

The useful idea is:

1. a pack can use another pack internally
2. a pack can choose whether that imported surface becomes part of its public API
3. if it does, that public API can either stay namespaced or appear as part of
   the importing pack's own top-level surface

The current `transitive` plus `export` encoding makes those ideas too hard to
see and too easy to misuse.

The direction proposed here is:

- imports are private/internal by default
- exports define the public API
- public exposure is explicit and intentional

## Problem

### Three product modes are hidden inside two booleans

As-built, the current system is effectively trying to represent three modes:

1. **shallow**
   - only the directly imported pack's own surface is visible upstream

2. **deep**
   - imported subordinate surfaces are visible upstream under subordinate bindings

3. **facade**
   - imported subordinate surfaces are visible upstream as part of the parent
     pack's own surface

Those are real product modes, but the user has to reverse-engineer them from
`transitive` and `export`.

### Import bindings leak into public API

A stronger design smell is that subordinate import binding names can become
part of a pack's public surface.

That means:

- a local binding choice inside pack `B`
- can become visible to consumer `A`
- even though `A` never chose that name

This makes internal structure leak outward, and can make internal renames into
public breaking changes.

### The model changes by layer

Pack-to-pack composition already feels tricky, and then the root city/rig
import layer rebinding makes it feel trickier still.

Users are forced to reason about:

- what the inner pack graph means
- what the importing pack exposes
- how the root layer rewrites that again

That is too much indirection for a feature that is supposed to make pack reuse
easier.

## Proposed Direction

### Core rule

Separate these concepts cleanly:

1. internal composition
2. public surface exposure

In other words:

- `[imports.*]` is internal wiring
- `[[exports]]` defines the public API

### Imports are private by default

If pack `B` imports pack `C` as `c`, then inside `B` we can refer to:

- `c.*`

But consumers of `B` should not automatically see `c.*`.

That import binding is local to `B` unless `B` deliberately exports it.

### Exports are explicit

If `B` wants to expose something from its imported packs, it does so
explicitly through `[[exports]]`.

This gives a clean rule:

- imports do not leak
- exports make things public

## Proposed Surface

```toml
[imports.c]
source = "<path-to-c>"

[imports.d]
source = "<path-to-d>"

[imports.e]
source = "<path-to-e>"

[imports.f]
source = "<path-to-f>"

[imports.g]
source = "<path-to-g>"

[[exports]]
from = "c"
as = "c"

[[exports]]
from = "d"
as = "c"

[[exports]]
from = "e"
as = "."

[[exports]]
from = "f"
as = "."
```

The repeated `as = "c"` entries are deliberate: `as` is the public namespace
label chosen by the exporting pack, not necessarily the local import binding.
Several imports may feed one public namespace only when the exported leaf names
under that namespace are disjoint.

### Meaning

- `g` is private/internal because it is imported but not exported
- `c` is exported under public namespace `c.*`
- `d` is also exported under public namespace `c.*`
- `e` and `f` are exported into the importing pack's own top-level public surface

### Meaning of `as`

- `as = "name"`
  - export under namespace `name.*`

- `as = "."`
  - facade export into this pack's own public top level

No `[[exports]]` entry means:

- internal-only import

## Public Naming Rule

If pack `A` imports pack `B` as `b`, then `A` should see `B`'s public surface
under `b.*`.

That gives a clean invariant:

- the importing pack chooses the public anchor it uses for the imported pack

Examples:

- if `B` defines local public `bee`
  - `A` sees `b.bee`

- if `B` exports `C` under `c`
  - `A` sees `b.c.*`

- if `B` facade-exports `E`
  - `A` sees those definitions as `b.*`

This is much cleaner than letting subordinate binding names leak upward as
unexpected peers.

## What Happens to `transitive`

Current recommendation:

- remove `transitive` from the user-facing syntax

Instead, recursive visibility should come from public APIs composing normally.

In this model:

- a pack imports another pack's public surface
- not its hidden internal wiring

If:

- `C` exports `D`
- `B` exports `C`
- `A` imports `B` as `b`

then `A` sees that nested structure because the chain is explicitly public all
the way up, not because transitive leakage happened by default.

## Migration And Deprecation

The implementation should preserve the current `transitive` / `export` syntax
for a deprecation window while also accepting `[[exports]]`. The old syntax and
the new syntax should not be mixed for the same imported binding; if an import
uses `export` or `transitive`, that binding is in legacy mode, and if an import
is named by `[[exports]]`, that binding is in explicit-export mode.

That mixing rule is a validation contract, not a best-effort warning. The
loader should return a typed configuration error naming the import binding and
the conflicting fields, for example: `import "tools" cannot use legacy field
"export" and [[exports]] at the same time`.

The migration is not always a mechanical one-line rewrite. The current default
transitive behavior and `export = true` can expose subordinate public surfaces
that a naive `[[exports]] from = "name" as = "."` entry would no longer expose.
Migration tooling must choose and report one of two policies:

1. **Behavior-preserving migration:** generate explicit `[[exports]]` entries
   for every public namespace currently leaked through the resolved legacy import
   graph, including surfaces exposed through `export = true` chains.
2. **Intentional narrowing:** generate only the exports named by the pack author
   and report every formerly public namespace that is no longer exported.

The default automated migration should be behavior-preserving. Authors can then
delete generated exports when they intentionally want to narrow their public
surface. Packs that rely on `export = true` across package boundaries need an
inter-pack coordination step, because the parent pack can preserve facade
behavior only for public surfaces that the imported pack itself continues to
publish.

The migration mapping is:

| Current import form | Current behavior | Explicit-export form |
|---|---|---|
| no `transitive`, no `export` | default transitive import; subordinate public surfaces remain reachable through the import chain | import the pack and, for a behavior-preserving migration, generate explicit exports for every currently reachable public namespace |
| `transitive = false` | imported pack is usable locally, but subordinate surfaces do not leak upstream | plain `[imports.name]` with no `[[exports]]` entry, unless the parent wants to expose selected public namespaces |
| `export = true` | imported surface is flattened into the parent public surface | `[[exports]]` with `from = "name"` and `as = "."`, plus generated explicit exports for any subordinate public surfaces needed to preserve legacy behavior |
| `transitive = false`, `export = true` | reachable legacy combination that behaves like a shallow facade and must not be lost during migration | `[[exports]]` with `from = "name"` and `as = "."`; no transitive leakage beyond the imported pack's public API |

The deprecation path should be:

1. accept both surfaces, but warn when a pack uses legacy `transitive` or
   `export`
2. teach `gc pack` or `gc doctor --fix` to rewrite straightforward legacy
   imports into explicit `[[exports]]`
3. keep the old syntax until the PackV2 deprecation wave, `gc pack`, and pack
   registry migration have all shipped
4. update `docs/packv2/skew-analysis.md` in the same implementation wave so
   `export`, `transitive`, and `shadow` have an explicit migration disposition
5. remove the old syntax only after the repository fixtures and user-facing
   reference docs have been updated to the explicit-export contract

During the deprecation window, `export` and `transitive` remain accepted legacy
fields and produce warnings. After the window, they should be removed from the
user-facing import syntax. `shadow` is not replaced by `[[exports]]`; it remains
accepted during this proposal's migration window and must be either preserved as
part of a separately designed override mechanism or deprecated in its own
documented wave. It must not disappear as a side effect of the import/export
rewrite.

## Local Definitions

Current recommendation:

- local definitions remain public by default

That keeps the first version simple.

The rationale is that authoring a top-level definition in a pack is already an
exposure decision for that pack's own public API. The asymmetry is intentional:
local authorship publishes a local definition, while importing another pack is
only permission to use that dependency internally. Re-publishing imported
definitions still requires `[[exports]]`.

Longer-term, we may want a per-definition visibility control, but we do not
think it is required to validate the import/export redesign.

Possible later addition:

- `visibility = "private"`

Important note:

- imported definitions should still stay private by default unless explicitly
  exported

## Collisions

Current recommendation:

- collisions in the same public slot should be hard errors

This is a deliberate breaking change from the accepted PackV2 contract in
`docs/packv2/doc-pack-v2.md`, where two imported packs defining the same bare
name both load and only ambiguous referring sites must qualify the name. If this
proposal is accepted, the implementation plan must call out that inversion,
ship it through the deprecation path above, and provide migration diagnostics
that list the colliding public names and the legacy qualified names that remain
available before the hard-error phase.

If two imported packs both export `worker` into the same resulting public
namespace, that should fail loudly unless and until we design an explicit
override mechanism.

Multiple imports may intentionally feed the same public namespace only when the
exported leaf names are disjoint. For example, `from = "c", as = "tools"` and
`from = "d", as = "tools"` are valid only if the public leaf names under
`tools.*` do not overlap. The slot is the resolved public leaf name, not merely
the namespace prefix.

Validation should happen when the pack graph is loaded, in the same
configuration validation path that resolves imports and exposes pack surfaces.
An unknown `from` binding, duplicate resolved public leaf, or invalid facade
target should be a typed configuration error that names the importing pack, the
`[[exports]]` entry, and the conflicting public name.

This preserves one of the most important properties:

- public API aggregation is intentional
- accidental ambiguity is not silently tolerated

## Why This Is Better

This direction gives us a much cleaner story:

- import bindings are local/private
- exported surface is explicit
- namespace export and facade export are both supported
- imported packs can stay internal unless deliberately made public
- pack reuse/customization becomes easier to teach
- future derived-pack / sibling-pack work has a cleaner foundation

Most importantly, it aligns with the mental model people tend to expect:

- internal wiring stays internal
- public API is what the pack author chooses to expose

The import graph must still preserve provenance for tooling. Even when a pack
facade-exports another pack with `as = "."` or converges several imports into
one public namespace, `gc why` and related inspection commands should still be
able to report the origin chain from the surfaced public name back to the
source pack and definition.

## Implementation Considerations

The config model needs a first-class `Export` table-array alongside
`[imports.*]`, with at least `from` and `as` fields. The loader should resolve
`from` against local import bindings, then project only that imported pack's
public API into the requested public namespace or facade surface.

The generated schema, TOML reference docs, PackV2 guide, and
`docs/packv2/skew-analysis.md` should be updated in the same implementation
wave so users see one coherent contract. Validation should reject unknown
`from` bindings, malformed `as` values, duplicate public leaf names, and
legacy/new syntax conflicts with context-rich messages such as `export from
"tools": unknown import binding`, `export as "tools": duplicate public name
"tools.worker"`, or `import "tools" cannot use legacy field "export" and
[[exports]] at the same time`.

Resolved config should retain export provenance as typed projection metadata:
source import binding, source pack identity, source definition name, resolved
public name, and whether the projection came from a namespace export or facade
export. `gc why` and related inspection commands should read that projection
metadata instead of inferring provenance from the public name after namespaces
have been collapsed.

`packs.lock` should remain the lock for the resolved transitive import graph:
repository, version, commit, and cache location. It should not become an export
projection lock. Export projections are derived from the locked pack graph and
the current `[[exports]]` declarations, so `gc import install` can keep the same
responsibility boundary while `gc import check` and config loading validate the
public surface projection.

## Open Questions

1. Should local definitions stay public by default forever, or eventually move
   to explicit export lists?
2. Is `as = "."` the right facade spelling, or do we want a different special
   value?
3. Do we want multiple imports feeding the same public namespace from day one?
   - current recommendation: yes
4. Do we want to expose only an imported pack's public API, or ever allow
   drilling into its private internals?
   - current recommendation: public API only
5. When the newer `gc pack` / pack registry work lands, how much should CLI
   authoring help generate or validate `[[exports]]`?

## Recommendation

The recommended next step is:

1. socialize this note and gather product feedback
2. confirm the explicit `[[exports]]` direction
3. queue the implementation work behind the current PackV2 deprecation,
   `gc pack`, and pack registry waves

Short version:

- explicit imports plus explicit exports is the cleaner model
- we believe we are on the right track with `[[exports]]`
