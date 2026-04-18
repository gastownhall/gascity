# Custom Provider Inheritance

| Field | Value |
|---|---|
| Status | Draft — revised after design-review round 1 |
| Date | 2026-04-18 |
| Author(s) | Julian, Claude |
| Issue | — |
| Supersedes | — |

Design for first-class, opt-in inheritance between provider definitions in
`pack.toml` / `city.toml`, replacing today's silent name-match and
command-match auto-inheritance.

## Problem

[`internal/config/resolve.go`](../../internal/config/resolve.go) currently
has two implicit rules that merge a city-level provider over a built-in:

1. **Name match** — `[providers.codex]` at the city level auto-merges with
   the built-in named `codex`.
2. **Command match** — a custom provider whose `command` equals a built-in
   name (e.g. `command = "claude"`) auto-merges with that built-in.

Both rules exist to give custom provider definitions sensible defaults for
fields like `PromptMode`, `ReadyDelayMs`, `PermissionModes`,
`OptionsSchema`, and the pool-worker safety flags. The rules work for
simple aliases but fail silently for any provider that wraps a binary
through an intermediary launcher. The canonical failure mode — and the
one that motivated this design — is aimux-wrapped providers:

```toml
[providers.codex-mini]
command = "aimux"
args = ["run", "codex", "--", "-m", "gpt-5.3-codex-spark",
        "-c", "model_reasoning_effort=\"medium\""]
```

Neither rule matches here. `codex-mini` is not a built-in name; `aimux` is
not a built-in command. The provider loads without the built-in's
defaults, so:

- codex boots in its default `suggest` permission mode instead of
  `unrestricted` → every agent run prompts for approval on the first
  sandboxed command and hangs forever waiting for a non-existent human.
- `ReadyDelayMs` is unset → the pool worker is considered ready before
  the TUI has finished bootstrapping; the first prompt races the UI.
- `ResumeFlag` / `ResumeStyle` / `SessionIDFlag` are unset → crash
  recovery cannot reattach to the previous session; the agent restarts
  from a cold context.
- `SupportsHooks`, `SupportsACP`, `PrintArgs`, `InstructionsFile` are all
  empty → hooks aren't installed, headless mode is broken, the agent
  can't find its instructions file.

The code itself flags this as a deferred decision
([`resolve.go:273-278`](../../internal/config/resolve.go#L273)):

> Limitation: wrapper aliases that use an intermediary launcher (e.g.,
> `command = "aimux"`, `args = ["run", "gemini"]`) are not resolved to
> the underlying builtin provider. [...] Fixing this requires a deeper
> design decision about how to parse args for wrapped providers and is
> deferred.

This design is that deferred decision.

## Goals

1. Give users a way to opt a custom provider into inheriting from any
   other provider — built-in or custom — via a single explicit field.
2. Allow chaining, so users can build shared intermediate ancestors
   (e.g. `claude-reasoning` feeding `claude-max` and `claude-mid`)
   without copy-pasting fields across siblings.
3. Remove the existing silent auto-inheritance rules so behavior is
   always explicit and never depends on coincidental name collisions —
   **while providing a deprecation window that prevents the fix from
   reintroducing the same silent-failure mode at a different trigger.**
4. Surface inheritance misconfigurations at config load rather than at
   session spawn time.
5. Make inherited ancestry a first-class resolved property used
   consistently across every runtime surface that branches on provider
   family (hook install, settings injection, skill materialization,
   session kind metadata, HTTP `/v0/providers` view, `/v0/config/explain`).

### Non-goals

- Inheriting anything about an agent (`[[agent]]` entries) — this design
  is scoped to `[providers.*]`.
- Multiple inheritance / mixins — single-inheritance chain only.
- Introspection UI beyond extending `gc config show` / `gc config
  explain` to surface the resolved chain.

## Design

### TOML schema additions

Four new fields on `[providers.X]` blocks (up from two in round 1):

```toml
[providers.codex-max]
base = "codex"
args_append = [
  "-m", "gpt-5.4",
  "-c", "model_reasoning_effort=\"xhigh\"",
]
args_prepend = []                                 # optional; e.g., outer wrappers
supports_hooks = false                            # optional; tri-state override
```

| Field | Type | Required | Semantics |
|---|---|---|---|
| `base` | `string` | no | Name of the parent provider. When unset (or empty), this provider has no parent and uses only the fields it declares plus minimal framework defaults. Accepts `"<name>"` (looks up custom first, then built-in) or the namespaced form `"builtin:<name>"` to force the built-in lookup unconditionally. |
| `args_append` | `[]string` | no | String list appended to the effective `args` of the resolved chain. Applied after that layer's `args` replacement. |
| `args_prepend` | `[]string` | no | String list prepended to the effective `args` of the resolved chain. Applied before that layer's `args` replacement. Enables outer-wrapper composition (`timeout 30s …`, `env VAR=x …`, `nice -n 10 …`). |
| capability-bool overrides (`supports_hooks`, `supports_acp`, `emits_permission_warning`) | `*bool` | no | Tri-state: absent = inherit from parent; `true` = enable; `false` = explicitly disable. Represented internally as `*bool`. |

All existing fields retain their current types and TOML tags. `Args`,
`ProcessNames`, `PrintArgs`, and `OptionsSchema` get a pinned nil-vs-empty
contract (see Nil-vs-empty semantics below).

### Name resolution for `base`

Resolving `base = "X"` for a provider named `P`:

1. **Namespaced prefix:** if `X` starts with `builtin:` (`base = "builtin:codex"`),
   look up the suffix in `BuiltinProviders()` only. If not found → error.
   This is the unambiguous form for users who want to refer to a built-in
   even when a custom provider shadows the name.
2. **Namespaced prefix:** if `X` starts with `provider:` (`base = "provider:codex-wrap"`),
   look up the suffix in custom providers only. If not found → error.
3. **Bare name** (`base = "codex"`):
   - Look up `X` in the custom providers table, **excluding `P` itself**.
   - If not found, look up `X` in `BuiltinProviders()`.
   - If still not found → error: `unknown base "X" for provider "P"
     (no custom provider or built-in with that name)`.

Self-exclusion is the mechanism that lets a shadowing custom provider
inherit from the built-in it shares a name with. It only scopes to the
declaring hop, not the whole walk: `codex-max → codex → builtin codex`
resolves correctly because at each hop self-exclusion only hides the
current hop's declarer.

`base = "P"` inside `[providers.P]` when no built-in named `P` exists is
a **self-cycle error**, not "unknown base" — the user's clear intent was
to reference themselves.

`gc config show` output renders every resolution through the annotation:

```
# inherited chain: codex-max → codex → builtin:codex (via self-exclusion)
[providers.codex-max]
...
```

The self-exclusion annotation is required output so the resolution is
never invisible to someone reading the config dump. Users who want
zero-ambiguity authoring can use `base = "builtin:codex"` directly.

### Resolution semantics

Resolution happens **eagerly, post-compose, post-patch**. The full chain
is walked once; the fully merged `ProviderSpec` is cached on the `City`
struct alongside per-field / per-key provenance metadata (see
Provenance data model). Subsequent `lookupProvider` calls return the
cached spec by value (immutable from caller's perspective).

#### Chain walk

Walk `base` links leaf → root. Collect ancestors in a list. Terminate
when a provider has no `base` set (that provider is the root — either a
built-in or a user-declared standalone provider).

Cycle detection: maintain a `visited` set scoped to this walk only. If
a name reappears, emit an error:

```
config error: provider "A" has inheritance cycle: A → B → A
```

The error message names the chain as walked, so the user can see where
the walker turned around.

#### Merge direction

Merge **root first**. Starting with an empty `ProviderSpec`, apply each
ancestor in order from root to leaf. Each layer runs through the same
merge function (a rename of the existing
[`MergeProviderOverBuiltin`](../../internal/config/resolve.go#L164)).

### Cache, compose, and patch interaction

Chain resolution must see the fully composed and patched provider
table, not the raw TOML parse output. Order:

1. **Compose:** pack fragments + city overrides merged in
   [`compose.go`](../../internal/config/compose.go). `Base`,
   `ArgsAppend`, `ArgsPrepend`, and tri-state capability booleans
   participate in `deepMergeProvider` — they deep-copy and overlay like
   every other field.
2. **Patch:** `[[patches.providers]]` patches apply via
   [`patch.go`](../../internal/config/patch.go). Add `Base`,
   `ArgsAppend`, `ArgsPrepend`, tri-state booleans to `ProviderPatch`,
   to `applyProviderPatch`, and to deep-copy paths.
3. **Resolve:** walk each provider's `base` chain, build final
   `ProviderSpec`, record per-field provenance. Cache on `City`.
4. **Lookup:** `lookupProvider` returns the cached resolved spec **by
   value** — callers receive an independent copy so mutation cannot
   corrupt the cache.

On config reload, the full table is rebuilt atomically. The old cache
is retained until the new one is fully materialized (or errors); on
error the old cache stays in place and the reload is rejected.

Paths that use "quick" pre-compose parsing (like
[`cmd_config.go:77-85`](../../cmd/gc/cmd_config.go#L77)) **do not run
chain resolution**. They return raw parsed providers, flagged so
downstream callers know the cache is not populated.

### Field-level merge rules

Rules are applied at each merge layer (parent → accumulated) and again
when the leaf merges on top.

| Field | Merge rule | Change? |
|---|---|---|
| Scalar strings (`DisplayName`, `Command`, `PromptMode`, etc.) | Non-zero child replaces parent. | Unchanged |
| Scalar integers (`ReadyDelayMs`) | Non-zero child replaces parent. | Unchanged |
| Tri-state capability booleans (`SupportsHooks`, `SupportsACP`, `EmitsPermissionWarning`) | `*bool`: nil = inherit; non-nil replaces. Enables explicit disable. | **Changed** |
| `Args` | Non-nil child replaces parent entirely. Explicit empty list (`args = []`) clears. Absent inherits. | Nil-vs-empty pinned |
| `ArgsAppend` | Accumulated across chain: each layer's append extends the running list, applied after that layer's `args` replace. | **New** |
| `ArgsPrepend` | Accumulated across chain (outermost-first): each layer's prepend inserts before accumulated, applied before that layer's `args` replace. | **New** |
| `ProcessNames`, `PrintArgs` | Non-nil child replaces parent. Explicit empty clears. Absent inherits. | Nil-vs-empty pinned |
| `Env`, `PermissionModes`, `OptionDefaults` | Additive map merge; child keys win on collision. | Unchanged |
| `OptionsSchema` | **Merge by `Key`**: child entries with matching keys replace the parent's entry entirely; child entries with new keys append; `omit = true` on a key-only child entry removes the inherited entry. | **Changed** |

#### `args_prepend`, `args`, and `args_append` interaction

Resolution proceeds layer by layer, root first. For each layer:

1. Apply layer's `args_prepend` to accumulated args (insert before).
2. If layer declares `args`: replace accumulated with layer's args
   (but preserve already-applied prepends by restoring them at the front).
3. Else: keep accumulated args unchanged.
4. Apply layer's `args_append` to accumulated args.

**Same-layer ordering:** `args_prepend` + `args` + `args_append` on one
layer resolves as `args_prepend ++ args ++ args_append`. No layer-level
ambiguity, no rejection. (Round 1's "ambiguous" rationale was
self-contradictory once the cross-layer algorithm was defined; dropped.)

Worked example — outer wrapper case:

```toml
[providers.codex]                            # mid-tier wrapper
base = "builtin:codex"
command = "aimux"
args = ["run", "codex", "--"]

[providers.codex-max-timeout]                # leaf adds outer timeout
base = "codex"
args_prepend = ["timeout", "300s"]
args_append = ["-m", "gpt-5.4"]
```

Effective args: `["timeout", "300s", "run", "codex", "--", "-m", "gpt-5.4"]`.

Worked example — append-only:

```
builtin codex:        args=nil                  → acc = []
[providers.codex]:    args=["run","codex","--"] → acc = ["run","codex","--"]
[providers.codex-max]: args_append=["-m","gpt-5.4",...]
                      → acc = ["run","codex","--","-m","gpt-5.4",...]
```

#### `options_schema` merge with removal

Each `[[providers.X.options_schema]]` entry is identified by its `Key`
field. During merge:

- A leaf entry with `key` matching a parent entry replaces that entry
  entirely.
- A leaf entry with `omit = true` and a `key` matching a parent entry
  removes that entry.
- A leaf entry with a new `key` appends.
- Parent entries not mentioned by the leaf remain unchanged.
- Within a single layer: `Key` must be non-empty and unique. Empty or
  duplicate `Key` on the same layer is a config error.
- `options_schema = []` on a child explicitly clears inherited schema.

#### Nil-vs-empty semantics

Authoritative contract, pinned end-to-end through parse → compose →
patch → cache:

| TOML form | Meaning |
|---|---|
| Field absent | Inherit from parent / use built-in default |
| Field present, empty list (e.g. `args = []`) | Clear inherited value; final is empty list |
| Field present, non-empty list | Replace (slice fields) or merge (map fields) per field-level rule |

A per-field clear test is required for every slice-typed field: load a
child with `args = []` / `process_names = []` / `options_schema = []`,
assert the resolved spec has empty (not inherited) values.

### Provenance data model

The resolved-spec cache stores provenance alongside the merged values.
Data shape:

```go
type ResolvedProvider struct {
    ProviderSpec                          // final merged values
    Provenance ProviderProvenance          // source attribution
}

type ProviderProvenance struct {
    Chain          []string               // ["codex-max", "codex", "builtin:codex"]
    FieldLayer     map[string]string      // "Command" -> "providers.codex"
    MapKeyLayer    map[string]map[string]string
        // "OptionDefaults" -> {"permission_mode": "builtin:codex", ...}
    SchemaEntryLayer []SchemaProvenance   // per OptionsSchema entry: layer + action (inherited|replaced|appended|omitted)
    ArgsSegments   []ArgsSegment          // each arg string tagged with layer + origin (args|args_prepend|args_append)
}

type ArgsSegment struct {
    Layer  string                         // "providers.codex-max"
    Origin string                         // "args_prepend" | "args" | "args_append"
    Start  int                            // index into effective args
    End    int
}
```

Provenance is populated during chain resolution, not reconstructed by
`gc config explain`. The existing `Provenance` type in
[`compose.go:18-33`](../../internal/config/compose.go#L18) tracks only
`Agents`/`Rigs`/`Workspace` — add a `Providers` field.

### Kind / provider-family propagation

Gas City today branches on `provider_kind` (= resolved built-in name)
in many places. When inheritance is chained, the leaf must know what
built-in ancestor it derives from, not just what it literally declared.

Add to `ResolvedProvider`:

```go
BuiltinAncestor string   // nearest built-in in the chain, or "" if none
```

Definition: walk the chain from leaf to root; the first name resolvable
to `BuiltinProviders()` wins. If no built-in is in the chain, empty
string.

**All sites that currently branch on provider name/kind must consume
`BuiltinAncestor`, not the raw name:**

- `resolveProviderKind`
  ([`resolve.go:269-291`](../../internal/config/resolve.go#L269))
- hook install/enable logic
  ([`cmd/gc/build_desired_state.go:1061-1063`](../../cmd/gc/build_desired_state.go#L1061),
  [`internal/hooks/hooks.go:32-90`](../../internal/hooks/hooks.go#L32))
- Claude `--settings` injection
  ([`cmd/gc/cmd_start.go:699`](../../cmd/gc/cmd_start.go#L699))
- skill materialization
  ([`internal/materialize/skills.go:57`](../../internal/materialize/skills.go#L57))
- session submit/interrupt provider_kind branching
  ([`internal/session/submit.go:192`](../../internal/session/submit.go#L192))

Phase 4 includes an audit pass grepping for each of these patterns and
routing them through `BuiltinAncestor`.

### HTTP and API surface consistency

All provider-aware HTTP and API endpoints must consume the same
resolved cache, not re-derive provider behavior independently:

- `/v0/providers?view=public`
  ([`handler_providers.go:91-100`](../../internal/api/handler_providers.go#L91))
- `/v0/config/explain`
  ([`handler_config.go:124`](../../internal/api/handler_config.go#L124))
- Provider CRUD / patches
  ([`handler_provider_crud.go:10`](../../internal/api/handler_provider_crud.go#L10),
  [`configedit.go:647`](../../internal/configedit/configedit.go#L647))

Phase 3 updates each handler to read from the cache. The CRUD validation
is relaxed: a provider with `base` set is authorable without `command`
or `args` — those can be inherited.

### Migration & deprecation window

**Round 1 specified docs-only migration. Review blocked that** because
silent field loss reproduces the original bug at a different trigger.
Revised plan:

#### Phase A (this release) — load-time detector, no behavior change

A custom provider that meets ANY of these conditions generates a
**load-time warning** without changing resolution behavior:

- provider name equals a built-in name AND `base` is unset
- provider `command` equals a built-in name AND `base` is unset

Warning text names the exact line to add:

```
config warning: provider "codex" in pack.toml may be relying on legacy
  name-match auto-inheritance (matches built-in "codex").
  Add `base = "codex"` to make inheritance explicit. This warning
  becomes an error in the next release.
```

Legacy auto-inheritance continues to fire in Phase A so existing
configs keep working. The warning also surfaces in `gc doctor`, so
operators have a proactive check.

#### Phase B (next release) — auto-inheritance removed

Next release: legacy auto-inheritance is deleted. Warnings from Phase A
become hard load-time errors with the same "add `base = "X"`" message.

Users who migrated during Phase A experience no break. Users who ignored
the warnings get a clear actionable error at upgrade.

This two-phase approach is scoped to this design; it does not block
shipping Phase 1–6 in a single release other than gating legacy removal
to Phase B.

### Errors (all at config load)

```
config error: provider "codex-max" has inheritance cycle:
    codex-max → codex-mid → codex-max

config error: provider "codex-mini" has unknown base: "codex-foo"
    (no custom provider or built-in with that name)

config error: provider "codex-max" options_schema entry 2 has empty Key

config error: provider "codex-max" options_schema entry 2 duplicates
    Key "permission_mode" (also at entry 0)

config error: provider "codex-mini" base "builtin:aimux": no built-in
    with that name exists

config warning: provider "codex" in pack.toml may be relying on legacy
    name-match auto-inheritance. Add `base = "codex"` to make
    inheritance explicit. (Phase A — becomes error in next release)
```

### Observability

Extend `gc config show` to render, as a comment above each
`[providers.X]` block, the resolved chain:

```
# inherited chain: codex-max → codex → builtin:codex
[providers.codex-max]
...
```

Custom-rooted chains (no built-in in lineage):

```
# inherited chain: my-alias → my-base (no built-in ancestor)
[providers.my-alias]
...
```

No-base providers:

```
# no inheritance (stands alone)
[providers.my-standalone]
...
```

Round-trip: this output is **not** produced by `cfg.Marshal()` (which
strips comments). A dedicated annotated renderer — `cfg.MarshalShow()`
or similar — is required. Annotated output is intended for human
reading; re-parsing it discards the comments but is otherwise a valid
TOML round-trip.

`gc config explain` extends to show per-field / per-key / per-segment
provenance from the cache. Structured JSON output (`--json`) emits the
full `Provenance` struct for diffing or tooling:

```json
{
  "provider": "codex-max",
  "chain": ["codex-max", "codex", "builtin:codex"],
  "fields": {
    "Command":      {"layer": "providers.codex"},
    "PromptMode":   {"layer": "builtin:codex"},
    "ReadyDelayMs": {"layer": "builtin:codex"}
  },
  "option_defaults": {
    "permission_mode": {"layer": "builtin:codex"},
    "effort":          {"layer": "providers.codex-max"}
  },
  "args_effective":       ["run","codex","--","-m","gpt-5.4"],
  "args_segments": [
    {"layer":"providers.codex","origin":"args","start":0,"end":3},
    {"layer":"providers.codex-max","origin":"args_append","start":3,"end":5}
  ],
  "options_schema": [
    {"key":"permission_mode","action":"inherited","layer":"builtin:codex"},
    {"key":"effort","action":"replaced","layer":"providers.codex-max"}
  ]
}
```

## Built-in spec completeness

The built-in codex spec
([`provider.go:286-310`](../../internal/config/provider.go#L286))
currently does not define `ResumeFlag`, `ResumeStyle`, or
`SessionIDFlag`. Without adding them, `base = "codex"` does not restore
crash recovery for aimux-wrapped codex. Phase 1 adds these fields to
the built-in codex spec so the motivating use case works end-to-end.

## Implementation plan

### Phase 1 — data model + built-in spec gaps

- Add to `ProviderSpec` in
  [`internal/config/provider.go`](../../internal/config/provider.go):
  `Base string`, `ArgsAppend []string`, `ArgsPrepend []string`,
  `SupportsHooksPtr *bool` (and similar for other capability booleans),
  TOML tags `base`, `args_append`, `args_prepend`, `supports_hooks`, etc.
- **Simultaneously** add all new fields to `ProviderPatch`
  ([`patch.go:160`](../../internal/config/patch.go#L160)), patch apply
  functions, and deep-copy paths. Add `TestProviderFieldSync` analogous
  to `TestAgentFieldSync` to enforce parallel updates for future
  additions.
- Add missing built-in codex fields (`ResumeFlag`, `ResumeStyle`,
  `SessionIDFlag`) so the motivating example works end-to-end.
- Add top-level schema version discriminator (`pack_format = 1` or
  equivalent) to pack.toml / city.toml parsing.
- Unit tests: parse a provider with each new field, nil-vs-empty
  contract for each slice field, tri-state capability bool round-trip.

### Phase 2 — chain resolver

- Add `resolveProviderChain(name string, allProviders)
  (ResolvedProvider, error)` to
  [`internal/config/resolve.go`](../../internal/config/resolve.go).
- Implement namespaced prefixes (`builtin:`, `provider:`) + bare-name
  lookup with self-exclusion.
- Cycle detection with walk-scoped visited set. Self-cycle variant
  distinguished from unknown-base.
- Populate provenance during walk.
- Compute `BuiltinAncestor`.
- Emit error messages specified in Errors section.
- Unit tests: chain depth 1–3, self-exclusion to built-in via shadowing,
  `builtin:`/`provider:` prefixes, self-cycle (with and without built-in
  shadow), transitive cycle, unknown base, transitive unknown base,
  multiple descendants sharing an ancestor (shared-ancestor DAG).

### Phase 3 — remove legacy auto-inheritance (Phase A — warning only)

- Legacy auto-inheritance blocks at
  [`resolve.go:131-138`](../../internal/config/resolve.go#L131) remain
  in place but now emit load-time warnings.
- `gc doctor` runs the same check.
- Warning surfaces via `config.Warnings` — a new per-load warning
  channel that the loader returns alongside errors.
- No breaking behavior change in this release.

### Phase 4 — merge rule updates + runtime propagation

- Rename `MergeProviderOverBuiltin` to `mergeChainLayer`; extend for
  `ArgsAppend`, `ArgsPrepend`, tri-state capability booleans,
  `options_schema` merge-by-`Key` with `omit` sentinel.
- Refactor every site branching on provider name/kind to consume
  `ResolvedProvider.BuiltinAncestor` instead of the raw name. Audit via
  grep for hook install, settings injection, overlay selection, skill
  materialization, session provider_kind branches.
- Tests: golden-file resolved `ProviderSpec` per realistic chain;
  explicit negative test that legacy auto-inheritance does not fire
  when `base` is unset (in Phase B); 3-layer `args_append` /
  `args_prepend` accumulation; `options_schema` by-key merge with
  replace/append/omit; transitive cycle through shadowed built-in;
  end-to-end integration: pack.toml with aimux-wrapped codex →
  resolved provider has `PermissionModes["unrestricted"]`, correct
  `ReadyDelayMs`, `BuiltinAncestor="codex"`, hooks install.

### Phase 5 — eager cache + provenance

- After compose + patch, walk every `[providers.X]` and materialize
  `ResolvedProvider` with provenance.
- Store cache on `City`. `lookupProvider` returns by value.
- Cycle and missing-base errors fire here.
- Atomic reload: build new cache before swapping; on error keep old.
- Tests: cache contains expected providers; mutation of returned value
  does not affect subsequent lookups; reload with broken config
  preserves previous cache; Level 0 ("no agents, no providers") loads
  unchanged.

### Phase 6 — HTTP / API surface consistency

- Route `/v0/providers?view=public`, `/v0/config/explain`, `/v0/config`,
  and provider CRUD / patch handlers through the resolved cache.
- Relax CRUD validation to allow `base`-only descendants (no `command`
  / `args` required if inherited).
- Tests: `/v0/providers` returns the same resolved spec the runtime
  uses; `/v0/config/explain` returns per-field provenance; provider
  CRUD accepts `base`-only definitions.

### Phase 7 — observability

- Extend `gc config show` with the annotated chain comment. Render
  edge cases (no-base, custom-rooted, 4+ layer) from unit tests.
- Extend `gc config explain` with per-field / per-key / per-segment
  provenance annotation. Add `--json` structured output.
- Golden-file tests for each rendering case.

### Phase 8 — hard cutover (next release, Phase B)

- Delete the legacy auto-inheritance blocks at `resolve.go:131-138`.
- Promote Phase A warnings to hard load-time errors.
- Release notes name the cutover; migration instructions already
  surfaced in Phase A warnings.

### Phase 9 — docs and changelog

- Short user-facing doc under `engdocs/` summarising the TOML schema
  and behavior (separate from this design, which is for maintainers).
- Update README / pack.toml examples to use explicit `base` lines.
- Changelog entry naming this release's behavior (warning window) and
  the next release's cutover.
- Enumerate the `options_schema` merge semantic change as a breaking
  change with a worked migration example — distinct from the `base`
  migration.

Phases 1–2 can parallelize internally; Phase 3 gates Phase 4; Phases
5–7 gate on Phase 4; Phase 8 is scheduled in the next release.

## Test case inventory

Golden-file tests per chain depth asserting every field of the
resolved `ProviderSpec` + full `Provenance`. Named scenarios:

- **Built-in only lookup** (no `[providers.X]` shadowing): behavior
  unchanged.
- **Shadowing custom provider with `base = "<same-name>"`**: self-
  exclusion fires, resolves to built-in via bare-name path, inherits
  every built-in field.
- **Shadowing custom provider with `base = "builtin:<same-name>"`**:
  resolves identically to above via explicit namespaced form.
- **Self-cycle without shadow** (`[providers.foo] base = "foo"` with no
  built-in `foo`): error, cycle message.
- **Two-layer chain** (`codex-max → codex-custom → codex-builtin`):
  scalars, args, args_append, args_prepend, tri-state bools,
  options_schema merge each assert-by-field.
- **Three-layer chain**: same as above, deeper.
- **Shared-ancestor DAG** (`A → C`, `B → C`): both resolve
  independently; walks do not cross-contaminate visited sets.
- **Unknown base**: load fails with named error.
- **Transitive unknown base** (`A → B → <missing>`): error names both.
- **Transitive cycle through shadowed built-in** (`A → B → A` where A
  shadows a built-in): error, not resolved-to-built-in.
- **`args` + `args_append` + `args_prepend` on one layer**: resolves
  in the documented order.
- **Same-layer order across chain**: leaf prepend, middle replace,
  root append — exact expected final args.
- **options_schema replace**: leaf entry with matching key replaces.
- **options_schema append**: leaf entry with new key appends.
- **options_schema omit**: leaf entry with `omit = true` removes
  inherited entry.
- **options_schema = []**: leaf clears inherited schema.
- **options_schema empty / duplicate Key**: load error.
- **Nil-vs-empty per slice field**: absent vs `[]` vs non-empty for
  `Args`, `ProcessNames`, `PrintArgs`, `OptionsSchema`.
- **Tri-state capability bool**: absent vs `true` vs `false` for
  `SupportsHooks`, asserting runtime hook install fires/does-not-fire
  accordingly.
- **Negative auto-inheritance test (Phase B)**: `[providers.codex]
  command = "claude"` with no `base` → resolved spec has zero
  built-in-claude fields.
- **End-to-end aimux-wrapper integration**: pack.toml identical to the
  maintainer-city config → resolved provider for `codex-mini` has
  `PermissionModes["unrestricted"]`, `ReadyDelayMs=3000`,
  `ResumeFlag` set, `SupportsHooks=true`, `BuiltinAncestor="codex"`.
  Hook install actually fires for this provider.
- **Compose/patch coverage**: pack fragment defines parent; city-level
  child overrides; `[[patches.providers]]` patches the child → final
  cache reflects all three.
- **`TestProviderFieldSync`**: mirror of `TestAgentFieldSync`,
  asserting every `ProviderSpec` field has a corresponding entry in
  `ProviderPatch`, apply function, and deep-copy path.
- **Cache immutability**: mutate returned `ResolvedProvider`; re-lookup
  returns clean value.
- **Atomic reload**: reload with broken chain; cache unchanged.
- **Level 0 invariant**: city with no agents, no providers loads
  unchanged.
- **Phase A warning**: custom provider matching legacy name-match
  without `base` emits the expected warning.
- **Round-trip `gc config show`**: annotated output re-parses as valid
  TOML (comments stripped on re-parse is acceptable).

## Open questions

None blocking implementation. Surfaces to revisit if demand emerges:

- `_append` / `_prepend` variants for `ProcessNames`, `PrintArgs`.
- Multi-inheritance / mixins (deliberately excluded from v1).
- Further `omit = true` semantics for other keyed collections
  (currently `options_schema` only).
