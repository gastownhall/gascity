# Custom Provider Inheritance

| Field | Value |
|---|---|
| Status | Draft |
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
   always explicit and never depends on coincidental name collisions.
4. Surface inheritance misconfigurations at config load rather than at
   session spawn time.

### Non-goals

- Inheriting anything about an agent (`[[agent]]` entries) — this design
  is scoped to `[providers.*]`.
- Multiple inheritance / mixins — single-inheritance chain only.
- Introspection UI beyond extending `gc config show` / `gc config
  explain` to surface the resolved chain.
- Backward compatibility with existing pack.toml content that relied on
  auto-inheritance. Migration is a one-line manual addition per affected
  provider, documented in the changelog.

## Design

### TOML schema additions

Two new fields on `[providers.X]` blocks:

```toml
[providers.codex-max]
base = "codex"
args_append = [
  "-m", "gpt-5.4",
  "-c", "model_reasoning_effort=\"xhigh\"",
]
```

| Field | Type | Required | Semantics |
|---|---|---|---|
| `base` | `string` | no | Name of the parent provider. When unset (or empty), this provider has no parent and uses only the fields it declares plus minimal framework defaults. |
| `args_append` | `[]string` | no | String list appended to the effective `args` of the resolved chain. Applied after all merge layers. |

All existing fields retain their current types and TOML tags.

### Resolution semantics

Resolution happens **eagerly, at config load**. When the TOML is parsed,
each `[providers.X]` has its `base` chain walked, cycles checked, and
final merged `ProviderSpec` cached. Subsequent `lookupProvider` calls are
O(1) reads against the cache.

#### `base` name resolution (self-exclusion)

To resolve `base = "X"` for a provider named `P`:

1. Look up `X` in the custom providers table, **excluding `P` itself**.
2. If not found, look up `X` in `BuiltinProviders()`.
3. If still not found, error: `unknown base "X" for provider "P"
   (no custom provider or built-in with that name)`.

Self-exclusion is the mechanism that lets a shadowing custom provider
inherit from the built-in it shares a name with:

```toml
[providers.codex]          # custom provider shadowing built-in "codex"
base = "codex"             # self-exclusion → resolves to built-in codex
command = "aimux"
args = ["run", "codex", "--"]
```

Chain resolution for `codex-max base = "codex"`:

```
[providers.codex-max]  →  [providers.codex] (custom aimux wrapper)  →  built-in codex
```

The leaf inherits everything the intermediate layer declared, plus
everything the built-in declared, minus the fields the leaf overrides.

#### Chain walk

Walk `base` links leaf → root. Collect ancestors in a list. Terminate
when a provider has no `base` set (that provider is the root — either a
built-in or a user-declared standalone provider).

Cycle detection: maintain a `visited` set during the walk. If a name
reappears, emit an error:

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

### Removal of legacy auto-inheritance

Delete the two auto-inheritance blocks in `lookupProvider`:

- [`resolve.go:131-134`](../../internal/config/resolve.go#L131) — the
  name-match block.
- [`resolve.go:135-138`](../../internal/config/resolve.go#L135) — the
  command-match block.

After these are removed, a custom provider with no `base` is resolved
exactly as it appears in TOML, plus the minimal framework defaults
([`resolve.go:89-92`](../../internal/config/resolve.go#L89)):
`PromptMode` defaults to `"arg"` when unset.

### Field-level merge rules

Rules are applied at each merge layer (parent → accumulated) and again
when the leaf merges on top.

| Field | Merge rule | Change? |
|---|---|---|
| Scalar strings (`DisplayName`, `Command`, `PromptMode`, etc.) | Non-zero child replaces parent. | Unchanged |
| Scalar integers (`ReadyDelayMs`) | Non-zero child replaces parent. | Unchanged |
| Booleans (`SupportsHooks`, `SupportsACP`, `EmitsPermissionWarning`) | One-directional: child can enable, cannot disable. | Unchanged |
| `Args` | Non-nil child replaces parent entirely. | Unchanged |
| `ArgsAppend` | Concatenated in chain order: each layer's `args_append` is applied after that layer's `args` resolution. | **New** |
| `ProcessNames`, `PrintArgs` | Non-nil child replaces parent. | Unchanged |
| `Env`, `PermissionModes`, `OptionDefaults` | Additive map merge; child keys win on collision. | Unchanged |
| `OptionsSchema` | **Merge by `Key`**: child entries with matching keys replace the parent's entry entirely; child entries with new keys append. | **Changed** |

#### `args` and `args_append` interaction

Resolution proceeds layer by layer (root first):

1. Compute that layer's effective args:
   - If the layer declares `args`: replace accumulated with layer's args.
   - Else: keep accumulated args unchanged.
2. Append the layer's `args_append` (if any) to accumulated args.

Example chain `codex-max → codex (aimux wrapper) → built-in codex`:

```
built-in codex:
  args = nil       →  accumulated = []
  args_append = -

[providers.codex]:
  args = ["run", "codex", "--"]   →  accumulated = ["run", "codex", "--"]
  args_append = -

[providers.codex-max]:
  args = -         →  accumulated unchanged
  args_append = ["-m", "gpt-5.4", "-c", "model_reasoning_effort=\"xhigh\""]
  →  accumulated = ["run", "codex", "--", "-m", "gpt-5.4",
                    "-c", "model_reasoning_effort=\"xhigh\""]
```

Declaring both `args` and `args_append` on the same layer is a config
error: the intent is ambiguous (does the append apply to the replacement
or to the pre-replacement value?).

#### `options_schema` merge by key

Today, setting `options_schema` on a custom provider replaces the base's
schema entirely. That forces a leaf that wants to add one option to
redeclare all four.

Under this design, each `[[providers.X.options_schema]]` entry is merged
by its `Key` field:

- A leaf entry with `key` matching a parent entry replaces that parent
  entry's whole struct.
- A leaf entry with a new `key` appends to the schema.
- Parent entries not mentioned by the leaf remain unchanged.

Removal of a parent entry is deferred to a future sentinel (`omit =
true` on a key-only entry) — not in v1.

### Errors

All errors fire at config load:

```
config error: provider "codex-max" has inheritance cycle:
    codex-max → codex-mid → codex-max

config error: provider "codex-mini" has unknown base: "codex-foo"
    (no custom provider or built-in with that name)

config error: provider "codex-mini" declares both `args` and
    `args_append`; they are mutually exclusive
```

### Observability

`gc config show` already renders resolved providers. Extend the renderer
to annotate each custom provider with its resolved chain:

```
# inherited chain: codex-max → codex → builtin codex
[providers.codex-max]
command = "aimux"
args = ["run", "codex", "--", "-m", "gpt-5.4",
        "-c", "model_reasoning_effort=\"xhigh\""]
...
```

`gc config explain` (which shows per-field provenance) extends its
provenance annotation to include the chain layer that contributed the
value:

```
Command        = aimux          [providers.codex]
Args           = [run codex --] [providers.codex]
PromptMode     = arg            built-in codex
ReadyDelayMs   = 3000           built-in codex
PermissionModes = {unrestricted=..., suggest=..., auto-edit=...}
                                built-in codex
OptionDefaults = {permission_mode=unrestricted, effort=max}
                                [providers.codex-max] (merged)
                                built-in codex     (merged)
```

## Migration

Breaking change. Custom providers shadowing a built-in name (e.g.
`[providers.codex]`) or whose `command` matches a built-in binary no
longer auto-inherit from that built-in. The fix is one line per affected
provider:

```diff
 [providers.codex]
+base = "codex"
 command = "aimux"
 args = ["run", "codex", "--"]
```

Changelog entry names this as a breaking change. No CLI migration tool
ships with the change; users migrate on their own timeline as they hit
behavior issues.

## Implementation plan

### Phase 1 — data model

- Add `Base string` and `ArgsAppend []string` to `ProviderSpec` in
  [`internal/config/provider.go`](../../internal/config/provider.go).
- TOML tags: `base`, `args_append`.
- Unit tests: parse a provider with `base` set, with `args_append` set,
  with both declaring `args` and `args_append` (error).

### Phase 2 — chain resolver

- Add `resolveProviderChain(name string, allProviders)
  (ProviderSpec, error)` to
  [`internal/config/resolve.go`](../../internal/config/resolve.go).
- Implement cycle detection and self-exclusion lookup.
- Emit the error messages specified in the Errors section.
- Unit tests: chain depth 1–3, self-exclusion to built-in, cycle,
  unknown base, double-args error.

### Phase 3 — remove legacy auto-inheritance

- Delete the name-match and command-match blocks at
  [`resolve.go:131-138`](../../internal/config/resolve.go#L131).
- Update tests that exercised those paths to declare `base` explicitly.
- This phase will break CI on tests and example configs that relied on
  auto-inheritance; update all of them.

### Phase 4 — merge rule updates

- Rename `MergeProviderOverBuiltin` to `mergeChainLayer` and make it
  handle an arbitrary number of layers (same pairwise semantics, new
  caller).
- Add `ArgsAppend` threading per the interaction rule above.
- Rewrite `OptionsSchema` merge from slice-replace to merge-by-`Key`.
- Unit tests for each new semantic.

### Phase 5 — eager cache at config load

- After config parse and compose, walk every `[providers.X]` and
  materialize the fully-resolved `ProviderSpec`. Store it on the `City`
  struct.
- `lookupProvider` returns the cached spec directly; no per-call chain
  walk.
- Cycle and missing-base errors fire here.

### Phase 6 — observability

- `gc config show` renders the inherited-chain comment line above each
  provider block.
- `gc config explain` annotates per-field provenance with the chain
  layer that supplied the value.

### Phase 7 — docs and changelog

- Add a short doc under `engdocs/` (beyond this design) summarising the
  user-facing TOML schema and behavior.
- Update any README/guide that shows an example `[providers.*]` block.
- Changelog entry naming the breaking change and one-line migration.

## Test case inventory

Core scenarios that must pass before merge:

- **Built-in only lookup** (no `[providers.X]` shadowing): behavior
  unchanged.
- **Shadowing custom provider with `base = "<same-name>"`**: self-
  exclusion fires, resolves to built-in, inherits cleanly.
- **Two-layer chain** (`codex-max → codex-custom → codex-builtin`):
  args/args_append accumulate correctly, scalars override correctly.
- **Three-layer chain**: same as above, deeper.
- **Unknown base**: config load fails with the named error.
- **Self-cycle** (`[providers.a] base = "a"`) and transitive cycle
  (`a → b → a`): config load fails with chain-named error.
- **Both `args` and `args_append` on one layer**: config load fails.
- **`options_schema` by-key merge**: child entry with matching `key`
  replaces parent's entry; new `key` appends; unmentioned parent entries
  survive.
- **Legacy auto-inheritance removed**: a custom provider that previously
  relied on name-match with no `base` now loads with only its declared
  fields.
- **Existing tests for `MergeProviderOverBuiltin`-equivalent semantics**
  still pass under the renamed function.

## Open questions

None blocking implementation. Surfaces to revisit if demand emerges:

- `_append` variants for `ProcessNames`, `PrintArgs`.
- Explicit removal sentinel for `options_schema` entries
  (`omit = true`).
- Restoring the ability to disable inherited booleans (currently
  one-directional).
- Multiple-inheritance / mixins (deliberately excluded from v1).
