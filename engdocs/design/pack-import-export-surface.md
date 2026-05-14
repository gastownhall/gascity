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

## Local Definitions

Current recommendation:

- local definitions remain public by default

That keeps the first version simple.

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

If two imported packs both export `worker` into the same resulting public
namespace, that should fail loudly unless and until we design an explicit
override mechanism.

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

- the current `transitive` / `export` surface is too clever
- explicit imports plus explicit exports is the cleaner model
- we believe we are on the right track with `[[exports]]`
