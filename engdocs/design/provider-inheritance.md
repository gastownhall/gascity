# Custom Provider Inheritance

| Field | Value |
|---|---|
| Status | Draft — revised after design-review round 3 |
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

Neither rule matches. `codex-mini` is not a built-in name; `aimux` is not
a built-in command. The provider loads without the built-in's defaults,
so:

- codex boots in its default `suggest` permission mode instead of
  `unrestricted` → every agent run prompts for approval on the first
  sandboxed command and hangs forever.
- `ReadyDelayMs` is unset → pool workers marked ready before TUI
  bootstraps; first prompt races the UI.
- `ResumeFlag` / `ResumeStyle` / `SessionIDFlag` unset → crash recovery
  fails.
- `SupportsHooks`, `SupportsACP`, `PrintArgs`, `InstructionsFile` empty →
  hooks don't install, headless mode is broken, the agent can't find
  its instructions file.

The code flags this as deferred
([`resolve.go:273-278`](../../internal/config/resolve.go#L273)):
"wrapper aliases that use an intermediary launcher [...] Fixing this
requires a deeper design decision [...] and is deferred."

## Goals

1. Give users a way to opt a custom provider into inheriting from any
   other provider — built-in or custom — via a single explicit field.
2. Allow chaining so users can build shared intermediate ancestors.
3. Remove the silent auto-inheritance rules without reintroducing the
   same silent-failure mode at a different trigger (explicit
   deprecation window; hard error in phase B).
4. Surface inheritance misconfigurations at config load, not session
   spawn.
5. Make inherited ancestry a first-class resolved property used
   consistently across every runtime surface that branches on provider
   family.

### Non-goals

- Inheriting anything about an agent (`[[agent]]` entries).
- Multiple inheritance / mixins.
- **Outer-wrapper composition.** A child cannot insert tokens **before**
  its inherited `Command`. Cases like `timeout 300s ...`, `env VAR=x ...`,
  `nice -n 10 ...` around an inherited invocation require mechanics this
  design does not supply. Users who need that MUST set `command` and
  `args` explicitly in the child and may use `base` solely to inherit
  non-argv fields. (Round-2 reviewers correctly observed that a naive
  `args_prepend` design would land tokens between the child's inherited
  `Command` and inherited `Args`, producing silently wrong command
  lines. The cleanest resolution is to forbid outer wrapping in v1.
  See "Deferred: outer-wrapper composition" at the bottom.)

## Design

### TOML schema additions

Three new fields on `[providers.X]` blocks:

```toml
[providers.codex-max]
base = "builtin:codex"
args_append = ["-m", "gpt-5.4",
               "-c", "model_reasoning_effort=\"xhigh\""]
supports_hooks = false          # optional tri-state override
```

| Field | Type | Required | Semantics |
|---|---|---|---|
| `base` | `*string` (presence-aware) | no | Name of the parent provider. Stored as a pointer so parse/compose/patch can distinguish *omitted* from *explicit empty*. Absent = no declaration (inherits any pack-level `base` during compose; triggers Phase A warning if legacy auto-inheritance matches). `""` (explicit empty) = standalone opt-out — no inheritance at all, silences Phase A warning, bypasses Phase A legacy-merge synthesis. `"<name>"` looks up custom first, then built-in (self-exclusion applies). `"builtin:<name>"` forces built-in lookup (recommended form). `"provider:<name>"` forces custom lookup. |
| `args_append` | `[]string` | no | String list appended to the effective `args` of the resolved chain. Applied after that layer's `args` replacement. Inner-argv composition only — cannot wrap `Command`. |
| capability-bool overrides (`supports_hooks`, `supports_acp`, `emits_permission_warning`) | `*bool` | no | Tri-state: absent = inherit; `true` = enable; `false` = explicitly disable. Serialized as optional TOML bool; internal representation is `*bool`. |

Plus one changed field:

| Field | Change |
|---|---|
| `options_schema` | Merge mode controlled by new `options_schema_merge` field (see below). Defaults to **replace** (unchanged from today) for backward compat. |

New opt-in:

| Field | Type | Required | Semantics |
|---|---|---|---|
| `options_schema_merge` | `string` | no | `"replace"` (default, today's semantics) or `"by_key"`. When `"by_key"`, child entries with matching `Key` replace parent entries; new keys append; `omit = true` removes inherited entries. |

**`resume_command` is an existing field** ([`provider.go:73-77`](../../internal/config/provider.go#L73)). This design does not change its syntax. Existing `{{.SessionKey}}` template variable is preserved — no new placeholder is introduced. What changes: wrapper descendants of subcommand-style resume providers become **required** to declare `resume_command` (previously optional; silently broken for wrappers).

### Name resolution for `base`

Resolving `base = "X"` for a provider named `P`:

1. **Namespaced built-in** (`base = "builtin:X"`): look up `X` in
   `BuiltinProviders()` only. Miss → error `unknown builtin "X" for
   provider "P"`.
2. **Namespaced custom** (`base = "provider:X"`): look up `X` in
   custom providers. `X == P` → self-cycle error. Miss → error
   `unknown custom provider "X" for provider "P"`.
3. **Bare name** (`base = "X"`):
   - Look up `X` in custom providers, excluding `P` itself.
   - If not found, look up `X` in `BuiltinProviders()`.
   - Both miss → error `unknown base "X" for provider "P" (no custom
     provider or built-in with that name)`.
4. **Presence-aware empty / absent**:
   - **Absent** (`base` field omitted in this layer): defer to parent layer during compose. If no layer sets `base`, the provider has no declared parent → Phase A legacy-merge synthesis applies when name/command matches a built-in; warning is emitted.
   - **Explicit empty** (`base = ""`): standalone opt-out. No inheritance. Phase A legacy-merge synthesis is **bypassed**. Phase A warning is **silenced**. `base = ""` set at ANY layer (pack fragment, city override, patch) sticks — subsequent absent layers do not re-enable legacy merge because the explicit empty is an explicit declaration, not "no declaration."
   - `Base` is internally `*string` so compose/patch can distinguish the two.
   - `base = ""` on a descendant layer overrides a pack-declared non-empty `base` from an earlier layer (consistent with "explicit wins").

Self-exclusion scopes to the declaring hop only, not the whole walk.
Colons (`:`) are reserved in `base` values: a custom provider name
containing `:` is rejected at parse time. Built-in provider names
cannot contain `:`. The `builtin:` and `provider:` prefixes are
reserved.

`base = "P"` inside `[providers.P]` when no built-in named `P` exists
is a self-cycle error.

### Resolution semantics

Resolution happens **eagerly, post-compose, post-patch**. The full chain
is walked once; the fully merged `ResolvedProvider` is cached on the
`City` struct alongside provenance metadata. Subsequent lookups return
a **deep-copied** `ResolvedProvider` — all slice and map fields are
cloned on return so caller mutation cannot corrupt the cache. Mutation-
isolation tests are required per reference field.

#### Chain walk + hop identity

Walk `base` links leaf → root. At each hop, record:

- **Identity kind**: `builtin` or `custom` (determined by which lookup
  path found the hop — `builtin:` prefix / fallthrough-to-builtin → `builtin`;
  `provider:` prefix / bare-name-match-in-custom → `custom`).
- **Identity name**: the canonical name (with prefix stripped).

Cycle detection uses this **identity tuple** `(kind, name)` as the
visited-set key — not the bare string `base` value. This prevents
false positives between a custom `codex` and built-in `codex` with the
same bare name.

Chain terminates when a provider has no `base` set.

#### `BuiltinAncestor` derivation

`ResolvedProvider.BuiltinAncestor` is computed during the walk: the
first hop whose **identity kind is `builtin`**. Not name-matching — a
fully custom chain that happens to contain a hop named `codex` but
which resolved through `provider:` or bare-name-matched-a-custom does
**not** set `BuiltinAncestor`. If no hop in the chain is a built-in,
`BuiltinAncestor = ""`.

Test (required): `alias → custom-codex → provider:wrapper` chain, where
the middle hop is literally named `codex` but is a custom provider.
Assertion: `resolved.BuiltinAncestor == ""`.

#### Merge direction

Merge **root first**. Starting with an empty `ProviderSpec`, apply each
ancestor root→leaf through the same merge function.

### Cache, compose, and patch interaction

1. **Compose** (pack fragments + city overrides in
   [`compose.go`](../../internal/config/compose.go)): `Base`,
   `ArgsAppend`, tri-state capability bools, `ResumeCommand`,
   `OptionsSchemaMerge` participate in `deepMergeProvider`.
2. **Patch** ([`patch.go`](../../internal/config/patch.go)): all new
   fields added to `ProviderPatch`, `applyProviderPatch`, deep-copy.
3. **Resolve**: walk chains, build merged specs + provenance, cache on
   `City`.
4. **Lookup**: `lookupProvider(name)` returns a deep-copied
   `ResolvedProvider`.

On reload, the full table is rebuilt atomically. Old cache retained
until new one materializes (or reload fails). Reload rejection leaves
old cache intact.

**Quick-parse paths** that pre-compose
([`cmd_config.go:77-85`](../../cmd/gc/cmd_config.go#L77)) must NOT run
chain resolution and must NOT expose their output to runtime spawn
paths. A separate Go type (`RawProviderSpec`) is introduced for
pre-compose representations — runtime code paths only accept
`*ResolvedProvider`, enforced at the type level. A test enumerates
every caller of the quick-parse path and asserts none feed reconciler
spawn, crash recovery, readiness probes, or session creation.

#### Phase A: cache reproduces legacy behavior

During Phase A (warning window — legacy auto-inheritance still fires),
the resolved cache must produce the **same merged spec** it would have
produced under the legacy rules. Concretely: when materializing a
provider whose `base` is unset, if its name or command matches a
built-in, the cache layer **synthesizes** the equivalent `base =
"builtin:<name>"` merge. The warning is emitted separately on the
config-load channel; the resolution result is unchanged.

Phase B removes the synthesis. Any previously-quiet provider now fails
loudly.

### Field-level merge rules

| Field | Merge rule | Change? |
|---|---|---|
| Scalar strings | Non-zero child replaces parent. | Unchanged |
| Scalar integers (`ReadyDelayMs`) | Non-zero child replaces parent. | Unchanged |
| Tri-state capability booleans | `*bool`: nil = inherit; non-nil replaces. | **Changed (new `*bool`)** |
| `Args` | Non-nil child replaces parent. `[] = clear`. Absent inherits. | Nil-vs-empty pinned |
| `ArgsAppend` | Accumulated across chain: each layer's `args_append` extends the running list, applied after that layer's `args` replace. `[] = append nothing` (not a clear). | **New** |
| `ProcessNames`, `PrintArgs` | Non-nil child replaces. `[]` clears. Absent inherits. | Nil-vs-empty pinned |
| `Env`, `PermissionModes`, `OptionDefaults` | Additive map merge; child keys win on collision. | Unchanged |
| `OptionsSchema` | Merge mode per `options_schema_merge`: `"replace"` (default) = current slice-replace; `"by_key"` = merge by `Key` with `omit = true` removal. | **New opt-in** |
| `ResumeCommand` | Non-zero child replaces. Inherited by default. | Unchanged (field semantic new) |

#### `args` + `args_append` interaction

Same-layer order: `args ++ args_append`. Per-layer accumulation across
the chain:

1. If layer declares `args`: accumulated = layer.args (replace).
2. If layer declares `args_append`: accumulated ++= layer.args_append.

No mutual-exclusion rejection. Both on the same layer resolve as
`args ++ args_append` in declared order.

Worked example:

```
builtin codex:         args = nil,    args_append = nil     → []
[providers.codex]:     args = ["run","codex","--"]           → ["run","codex","--"]
                       args_append = nil
[providers.codex-max]: args = nil,    args_append = ["-m","gpt-5.4"]
                                                             → ["run","codex","--","-m","gpt-5.4"]
```

#### `options_schema` merge modes

Default: `options_schema_merge = "replace"` — today's behavior. Setting
a child's `options_schema` replaces the parent's entirely. No migration
required for any existing config.

Opt-in: `options_schema_merge = "by_key"`. Each
`[[providers.X.options_schema]]` entry is identified by its non-empty
`Key`. Rules:

- Child entry with matching `Key` replaces parent entry entirely.
- Child entry with new `Key` appends.
- Child entry with `omit = true` and matching `Key` removes parent
  entry. `OptionDefaults[Key]` is also pruned.
- Child entry with `omit = true` and no matching parent entry: **no-op**
  (not an error — permits forward-compatible config where a parent
  might or might not declare the key).
- Child entry with `omit = true` alongside any other non-`Key` fields:
  **load error** (omit is key-only).
- Empty `Key` or duplicate `Key` within one layer: load error.
- `options_schema = []` under `by_key` mode: clear inherited schema.

Opt-in model avoids the round-2 "silent semantic drift" blocker — no
existing config's resolution changes unless the user explicitly sets
`options_schema_merge = "by_key"`.

#### Tri-state capability booleans

TOML form:

```toml
supports_hooks = false   # explicit disable
supports_hooks = true    # explicit enable (or inherit-if-parent-enabled)
# omitted                 # inherit from parent
```

Internal representation: `*bool` (`nil` = inherit). The existing
non-pointer form in older configs must continue to work — `true` and
`false` decode into `*bool` identically. Regression test required:
pre-existing `supports_hooks = false` config continues to disable hooks
after the `*bool` migration.

Compose-order test required: fragment sets `supports_hooks = false`,
override omits the field → final `*bool == &false`.

#### `ResumeCommand` — wrapper-aware resume

Built-in codex uses `ResumeStyle = "subcommand"`, which today inserts
`resume <id>` after the first token of the invocation. For a bare
`codex` invocation this works; for the aimux-wrapped form
(`aimux run codex -- ...`) it produces `aimux resume <id> run codex --
...`, which is not a valid resume command.

Solution: `ResumeCommand string` field on `ProviderSpec`. When set, it
overrides `ResumeFlag`/`ResumeStyle`/`SessionIDFlag` heuristics. Uses
`{{session_id}}` as the substitution token.

**Required for wrapper descendants**: a provider whose inherited
`ResumeStyle == "subcommand"` and whose `command` differs from its
inherited `command` (i.e., a wrapper) MUST declare `resume_command`.
If not, config load fails with:

```
config error: provider "codex-mini" wraps a subcommand-style resume
  provider (codex) but does not declare `resume_command`. Wrapper
  providers must specify their own resume invocation.
```

For the aimux-codex case:

```toml
[providers.codex-mini]
base = "builtin:codex"
command = "aimux"
args = ["run", "codex", "--", ...]
resume_command = "aimux run codex -- resume {{.SessionKey}}"
process_names = ["aimux", "codex"]
```

End-to-end test required: spawn wrapped codex → kill → reconcile →
assert actual executed resume command matches the declared template.

The wrapper-resume check is **data-driven**, not predicated on the
literal string `"subcommand"` — any `ResumeStyle` value that depends on
the leaf's `Command` being an invocation of the inherited binary
(today: `"subcommand"`; future styles may require the same) triggers
the requirement for descendants whose `command` differs.

#### Wrapper `process_names` requirement (parity with `resume_command`)

When a provider's resolved `Command` differs from its inherited
`Command` (wrapper descendant) AND `process_names` was not
overridden, config load emits a **warning** (not error — in some cases
the inherited process names are still valid):

```
config warning: provider "codex-mini" wraps a different command
  ("aimux") than its inherited parent ("codex") but does not override
  `process_names`. Supervision and PID tracking may fail. Set
  `process_names = ["aimux", ...]` explicitly to include wrapper
  processes.
```

Warning becomes a hard error in a future release if experience shows
it is always wrong; starts as warning because there are legitimate
cases where the wrapper exec-replaces itself with the wrapped binary.
An integration test asserts the maintainer-city aimux config overrides
`process_names` and the reconciler can supervise the wrapped process.

### Kind / provider-family propagation

Every site that branches on provider name/kind MUST consume
`ResolvedProvider.BuiltinAncestor`, not the raw name. Phase 4 audits
and updates every listed call site; Phase 4 tests cover each.

- `resolveProviderKind` ([`resolve.go:269-291`](../../internal/config/resolve.go#L269))
- Hook install/enable ([`build_desired_state.go:1061-1063`](../../cmd/gc/build_desired_state.go#L1061), [`hooks.go:32-90`](../../internal/hooks/hooks.go#L32))
- Claude `--settings` injection ([`cmd_start.go:699`](../../cmd/gc/cmd_start.go#L699))
- Skill materialization ([`skills.go:57`](../../internal/materialize/skills.go#L57))
- Session submit/interrupt ([`submit.go:192`](../../internal/session/submit.go#L192))
- Named session creation ([`session_template_start.go:292`](../../cmd/gc/session_template_start.go#L292))
- API session creation ([`session_resolution.go:215`](../../internal/api/session_resolution.go#L215))
- Session chat handlers ([`handler_session_chat.go:381,521`](../../internal/api/handler_session_chat.go#L381))
- Provider readiness init ([`init_provider_readiness.go:338`](../../cmd/gc/init_provider_readiness.go#L338))
- Template resolve ([`template_resolve.go:251`](../../cmd/gc/template_resolve.go#L251))
- Skill integration ([`skill_integration.go:172`](../../cmd/gc/skill_integration.go#L172))
- Reconciler session bead creation / backfill
  ([`session_beads.go:617-665`](../../cmd/gc/session_beads.go#L617),
  [`session_lifecycle_parallel.go:406-409`](../../cmd/gc/session_lifecycle_parallel.go#L406))
- Crash adoption and `GC_PROVIDER` env propagation
  ([`manager.go:373-376`](../../internal/session/manager.go#L373))

Per-site regression tests: Claude `--settings` injection for
`claude-max base="builtin:claude"`; skill materialization for same;
session submit/interrupt; readiness probe; hook install; named-session
creation. Each test asserts the behavior matches what the built-in
would have gotten, not what a raw-name match would give.

Session beads stamp `provider_kind = BuiltinAncestor` at creation;
downstream consumers read it from the bead, not re-derive.

### HTTP / API surface consistency

All provider-aware HTTP / API / CRUD paths must consume the same
resolved cache:

- `/v0/providers?view=public`
  ([`handler_providers.go:91-100`](../../internal/api/handler_providers.go#L91))
- `/v0/config/explain`
  ([`handler_config.go:124`](../../internal/api/handler_config.go#L124))
- Provider CRUD
  ([`handler_provider_crud.go:10`](../../internal/api/handler_provider_crud.go#L10),
  [`configedit.go:647`](../../internal/configedit/configedit.go#L647))
- `/v0/config/explain` per-provider form (new: `--provider <name>`
  query parameter)

CRUD validation relaxed: a provider with `base` set is authorable
without `command` / `args` — those may be inherited. CRUD round-trip
test for base-only descendants is required.

**CRUD JSON authoring DTO** — explicit contract for the new fields:

```json
{
  "name": "codex-max",
  "base": "builtin:codex",
  "args_append": ["-m", "gpt-5.4", "-c", "model_reasoning_effort=\"xhigh\""],
  "supports_hooks": true,
  "options_schema_merge": "by_key",
  "options_schema": [
    {"key": "permission_mode", "omit": true},
    {"key": "detail", "label": "Detail", "type": "select", "choices": [...]}
  ],
  "resume_command": "..."
}
```

- `base` (`null`/omitted | `""` | `"<name>"` | `"builtin:<name>"` | `"provider:<name>"`):
  null/omitted = no declaration; `""` = explicit standalone opt-out; other
  values are lookup forms.
- `supports_hooks` / `supports_acp` / `emits_permission_warning`:
  `null`/omitted = inherit; `true`/`false` = explicit.
- `options_schema_merge`: `"replace"` (default) or `"by_key"`. Unknown
  values → HTTP 400.
- `omit = true` IS authorable via CRUD (the round-2 decision to strip
  it from public DTOs is reversed — users must be able to author the
  removal sentinel). The public *read* DTO still renders `omit`
  entries as resolved absences rather than raw structs; the CRUD round
  trip preserves the user's raw input.

**Response DTO key naming**: `/v0/config/explain --json` maps
provenance keys to **TOML/API names**, not Go struct identifiers.
`Command` → `command`, `ReadyDelayMs` → `ready_delay_ms`,
`OptionDefaults` → `option_defaults`, etc. A test asserts no
Go-identifier leakage in any serialized response.

### Migration & deprecation window

#### Phase A (this release) — load-time detector

A custom provider meeting ANY of these without explicit `base` set
(including `base = ""` opt-out):

- Provider name equals a built-in name.
- Provider `command` equals a built-in name.

emits a **load-time warning**. Resolution behavior unchanged (cache
synthesizes legacy merge per the Phase A cache rule above).

Warning text primarily recommends the unambiguous `builtin:` form:

```
config warning: provider "codex" in pack.toml is relying on legacy
  name-match auto-inheritance (matches built-in "codex"). This becomes
  a hard error in the next release.

  Fix: add `base = "builtin:codex"` to the provider block.

  If this provider should NOT inherit from the built-in, add
  `base = ""` to explicitly opt out.
```

`base = "<name>"` (bare, resolving via self-exclusion) is a valid but
secondary recommendation — the `builtin:` form is preferred because it
reads unambiguously without knowing the self-exclusion rule.

`base = ""` is the documented **opt-out path** for standalone
providers that happen to collide with a built-in name. Silences the
warning; cache does not synthesize legacy merge; the provider stands
alone with only its declared fields.

Warnings surface on three channels:

- Config load returns a structured warnings list alongside errors.
- `gc doctor` renders them for operator-initiated checks.
- `gc config explain <provider>` includes them in its output.

#### Phase B (next release) — auto-inheritance removed

Legacy auto-inheritance deleted. Phase A warnings become hard errors
with the same text. Cache synthesis of legacy merge is also removed
(since there's nothing to preserve).

### Errors (all at config load)

```
config error: provider "codex-max" has inheritance cycle:
    codex-max → codex-mid → codex-max

config error: provider "codex-mini" has unknown base: "codex-foo"
    (no custom provider or built-in with that name)

config error: provider "codex-mini" base "builtin:aimux": no built-in
    with that name exists

config error: provider "codex-mini" wraps a subcommand-style resume
    provider (codex) but does not declare `resume_command`. Wrapper
    providers must specify their own resume invocation.

config error: provider "codex-max" options_schema entry 2 has empty Key

config error: provider "codex-max" options_schema entry 2 duplicates
    Key "permission_mode" (also at entry 0)

config error: provider "codex-max" options_schema entry 2 declares both
    `omit = true` and other fields; omit entries must be key-only

config error: provider "codex-max" has `omit = true` on entry 0 but
    options_schema_merge is "replace" (or unset, defaults to "replace");
    omit sentinel requires options_schema_merge = "by_key"

config error: provider "codex-max" options_schema_merge = "layer"; valid
    values are "replace" or "by_key"

config error: custom provider name "codex:foo" contains reserved
    character ":" — reserved for namespace prefixes
```

### Observability

`gc config show` renders, as a comment above each `[providers.X]`
block:

```
# inherited chain: codex-max → codex → builtin:codex (via self-exclusion)
[providers.codex-max]
...

# no inheritance (stands alone)
[providers.my-standalone]
...

# inherited chain: my-alias → my-base (no built-in ancestor)
[providers.my-alias]
...
```

The annotation is produced by a dedicated annotated renderer
(`cfg.MarshalShow()`) — `cfg.Marshal()` (plain TOML encoding) cannot
produce comments.

`gc config explain` (and `/v0/config/explain`):

- Default view: per-agent resolved view including provider chain.
- `--provider <name>`: focused view on one provider's resolved spec
  and full provenance.
- `--json`: structured output. Provenance includes:
  - `chain`: ordered hop list with identity kind + name.
  - `fields`: per-field source layer.
  - `option_defaults` / `permission_modes` / `env`: per-map-key source
    layer (`MapKeyLayer`).
  - `options_schema`: per-entry `{key, action, layer}` where `action` ∈
    `{inherited, replaced, appended, omitted, cleared}`.
  - `args_effective` + `args_segments`: half-open `[start, end)` ranges
    tagged with `{layer, origin}` where `origin` ∈ `{args, args_append}`.
- Phase A warnings surface on `gc config explain`, not just
  `gc doctor`.

### Provenance data model

```go
type ResolvedProvider struct {
    ProviderSpec
    BuiltinAncestor string
    Provenance      ProviderProvenance
}

type ProviderProvenance struct {
    Chain            []HopIdentity            // ordered, root → leaf
    FieldLayer       map[string]FieldProv     // "command" → {"layer": "providers.codex", "action": "set"}
    MapKeyLayer      map[string]map[string]string
                                              // "option_defaults" → {"permission_mode": "builtin:codex", ...}
    SchemaEntryLayer []SchemaProvenance
    ArgsSegments     []ArgsSegment
    Warnings         []string                 // Phase A warnings
    ResumeSuperseded bool                      // true when resume_command
                                              // set and inherited
                                              // ResumeFlag/ResumeStyle/
                                              // SessionIDFlag are
                                              // shadowed
}

type FieldProv struct {
    Layer  string   // source layer
    Action string   // "set" | "inherited" | "cleared" (for [] clear of slice fields)
}

type HopIdentity struct {
    Kind string   // "builtin" | "custom"
    Name string   // canonical name
}

type SchemaProvenance struct {
    Key    string
    Action string   // "inherited" | "replaced" | "appended" | "omitted" | "cleared"
    Layer  string
}

type ArgsSegment struct {
    Layer  string   // e.g. "providers.codex"
    Origin string   // "args" | "args_append"
    Start  int      // half-open [Start, End)
    End    int
}
```

`MapKeyLayer` covers `Env`, `PermissionModes`, `OptionDefaults`.
`SchemaProvenance.Action = "cleared"` applies when a layer set
`options_schema = []` under `by_key` mode.

### `pack_format` — decision

**Dropped.** Round-2 review flagged this as underspecified scope creep.
This design does not introduce a new schema discriminator. The existing
`[pack].schema` contract
([`config.go:551`](../../internal/config/config.go#L551),
[`pack.go:22`](../../internal/config/pack.go#L22)) is unchanged; if a
future breaking change needs a discriminator, that's a separate design.

### Built-in codex fields

Add `ResumeFlag`, `ResumeStyle`, `SessionIDFlag` to the built-in codex
spec ([`provider.go:286`](../../internal/config/provider.go#L286)) so
that `base = "builtin:codex"` restores the resume capability for
non-wrapper descendants. Wrapper descendants still must declare
`resume_command` per the Resume section.

## Deferred: outer-wrapper composition

Inserting tokens before the inherited `Command` is deliberately
**out of scope for v1**. Users who need outer wrapping (`timeout 300s
aimux run codex ...`, `env FOO=bar ...`, `nice -n 10 ...`) must
declare their own `command` and `args`. They can still use `base` to
inherit non-argv fields (permission modes, ready delay, hooks,
settings).

This restriction exists because the runtime's `sh -c` line concatenates
`Command + Args`. A naive "args_prepend" would insert tokens between
the child's inherited `Command` and inherited `Args`, producing
silently-wrong invocations. The correct model is to treat the wrapper
itself as a new provider identity (its own `command` + `args` + `base`
for field inheritance) — which is what users already write for such
cases and what this design leaves unchanged.

Future extension (not in this design): a `command_wrap` field or
placeholder-based args (`args = ["timeout", "300s", "@inherit"]`) that
substitutes the resolved parent argv. Requires design work on
ergonomics and runtime-layer changes beyond the config package.

## Implementation plan

Phases 1–7 ship in the **same release**. Phase 8 (hard cutover) ships
in the next release. Phase 9 docs update ships alongside Phase 1–7.

### Phase 1 — data model + built-in spec gaps

- Add to `ProviderSpec` in
  [`provider.go`](../../internal/config/provider.go): `Base string`,
  `ArgsAppend []string`, `ResumeCommand string`, `OptionsSchemaMerge
  string`, capability `*bool` overrides, TOML tags.
- **Simultaneously** add all new fields to `ProviderPatch`
  ([`patch.go:160`](../../internal/config/patch.go#L160)),
  `applyProviderPatch`, deep-copy paths. Patch-side presence-awareness
  for `[]` preservation.
- `TestProviderFieldSync` analogous to `TestAgentFieldSync`.
- Add `ResumeFlag`, `ResumeStyle`, `SessionIDFlag` to built-in codex.
- Introduce `RawProviderSpec` type for pre-compose quick-parse paths;
  refactor quick-parse callers.
- Parser rejects `:` in custom provider names and rejects `builtin:` /
  `provider:` reserved-prefix misuse.
- Unit tests: parse each new field; nil-vs-empty contract per slice
  field; tri-state bool round-trip with old-form back-compat
  (`supports_hooks = false` on old schema still disables hooks);
  `RawProviderSpec` / `ResolvedProvider` type isolation.

### Phase 2 — chain resolver + hop identity

- Add `resolveProviderChain(name, allProviders) (ResolvedProvider,
  error)` to
  [`resolve.go`](../../internal/config/resolve.go).
- Implement namespaced prefixes (`builtin:`, `provider:`) +
  self-exclusion bare-name lookup.
- Cycle detection with walk-scoped visited set keyed on `(kind, name)`.
- Populate `BuiltinAncestor` from hop identity.
- Emit all error messages in Errors.
- Wrapper-resume check: detect subcommand-style inherited `ResumeStyle`
  with differing `command` and demand `resume_command`.
- Unit tests (per Test inventory below).

### Phase 3 — legacy auto-inheritance detector (Phase A)

- Legacy auto-inheritance blocks at `resolve.go:131-138` stay; now
  emit warnings through `config.Warnings` return channel.
- Cache materialization synthesizes `base = "builtin:<name>"` merge for
  each same-name / command-match provider lacking `base`.
- `gc doctor` runs the same check.

### Phase 4 — merge rule updates + runtime propagation

- Rename `MergeProviderOverBuiltin` → `mergeChainLayer`; extend for
  `ArgsAppend`, tri-state capabilities, `options_schema` by-key +
  `omit`.
- Audit every site branching on provider name/kind; route through
  `BuiltinAncestor`. Sites listed in Kind/provider-family section.
- Per-site regression tests.

### Phase 5 — eager cache + provenance

- Post-compose + post-patch resolution; cache on `City`.
- `lookupProvider` returns deep-copied `ResolvedProvider`. Mutation-
  isolation tests per reference field.
- Atomic reload; failed reload preserves old cache.
- Quick-parse path test: enumerate callers, assert none feed runtime.

### Phase 6 — HTTP / API / CRUD consistency

- Route `/v0/providers`, `/v0/config/explain`, provider CRUD handlers
  through the cache.
- Relax CRUD validation for `base`-only descendants.
- `/v0/config/explain` `--provider <name>` form.
- `omit` sentinel stripped from public DTOs (`json:"-"`).

### Phase 7 — observability

- `gc config show` annotated renderer (`cfg.MarshalShow()`) with
  comment line per provider. Cover no-base / custom-rooted / deep
  chains as explicit test cases.
- `gc config explain` provenance annotation, per-map-key resolution,
  `--json` output with full `ProviderProvenance`.
- Phase A warnings surface on explain path.
- Golden-file tests for both text and JSON outputs.

### Phase 8 — hard cutover (next release)

- Delete legacy auto-inheritance blocks.
- Delete cache legacy-merge synthesis.
- Promote warnings to errors.
- **Removal-assertion tests**: explicit coverage that every surface
  rejects an unmigrated provider. For each of: config load,
  `lookupProvider`, `/v0/providers`, `/v0/config/explain`, provider
  CRUD create+read, `gc doctor`, assert that a provider matching
  legacy name-match or command-match without `base` (or with
  `base = ""` explicitly opting out) is handled with the correct
  Phase B behavior (hard error vs clean standalone).

### Phase 9 — docs and changelog (ships with 1–7)

- User-facing doc under `engdocs/` for the TOML schema.
- Pair `args_append` wrapper guidance with `process_names` override
  guidance (a wrapper provider needs to override `process_names` for
  supervision / PID tracking).
- Changelog entry covering this release's detector window + next
  release's cutover.
- `options_schema` merge mode is documented as opt-in; no migration
  needed unless users explicitly enable.

## Test case inventory

Organized by phase; every check asserts specific field values, not
category coverage.

### Chain resolution

- Built-in only lookup: behavior unchanged.
- Shadowing custom with `base = "<same-name>"` (self-exclusion → built-in).
- Shadowing custom with `base = "builtin:<same-name>"` — same result.
- Shadowing custom with `base = "provider:<same-name>"` → self-cycle error.
- Self-cycle without shadow (`base = "foo"` in `[providers.foo]`, no
  built-in `foo`) → cycle error.
- Transitive 3-node cycle `A → B → C → A` with error message naming
  the full chain.
- Unknown base; transitive unknown base (A → B → missing) with error
  naming both.
- Shared-ancestor DAG (`A → C`, `B → C`) — both walks independent.

### `BuiltinAncestor` correctness

- Direct chain to built-in → `BuiltinAncestor = <builtin>`.
- Two-layer chain with built-in root → `BuiltinAncestor = <builtin>`.
- Fully-custom chain whose hop happens to be named `codex` but
  resolves through `provider:` → `BuiltinAncestor = ""`.
- Bare-name-matched-custom with same name as a built-in, chain
  continues to built-in → `BuiltinAncestor` is the built-in (because
  the chain reaches it).
- Chain passing through `custom codex (base="builtin:codex")` → leaf
  `BuiltinAncestor = "codex"`.

### Merge rules

- Scalars: 3-layer override (root sets, mid inherits, leaf overrides).
- `args` replace + `args_append` accumulate across 3 layers.
- Same-layer `args` + `args_append`: `args ++ args_append`.
- `args = []` on leaf clears inherited.
- `args_append = []` on leaf: appends nothing (distinct from `args = []`).
- Tri-state `*bool`:
  - absent → inherits parent `true`.
  - `false` → explicit disable (parent `true` overridden).
  - `true` → explicit enable.
  - Pre-existing old-schema `supports_hooks = false` config decodes
    correctly into `*bool`.
  - Compose-order: fragment `false`, override absent → final `false`.
- `options_schema` replace mode: child slice replaces.
- `options_schema` by-key mode: child replace by key / append new /
  omit existing.
- `options_schema` by-key + omit-nonexistent: no-op.
- `options_schema` by-key + `omit` with siblings: load error.
- `options_schema` by-key + empty/duplicate Key: load error.
- `options_schema = []` (by-key mode): clears inherited; schema entry
  provenance marked `cleared`.
- `OptionDefaults[omitted_key]` is pruned after `omit`.

### Resume

- Non-wrapper `base = "builtin:codex"`: resume uses inherited
  subcommand style.
- Wrapper (`command = "aimux"`, inherits subcommand-style resume)
  without `resume_command` → load error.
- Wrapper with `resume_command` → end-to-end resume succeeds
  (integration test: spawn → kill → reconcile → actual executed
  command matches template).

### Cache & provenance

- Deep-copy: mutate returned `ResolvedProvider.Args` → subsequent
  `lookupProvider` unaffected (same for each reference field).
- Atomic reload: second load with broken chain keeps first cache.
- Level 0 ("no agents, no providers") loads unchanged.
- Quick-parse path: enumerate callers; assert none feed reconciler
  spawn, crash recovery, readiness, or session creation.

### Phase A detector

- Name-match without `base`: warning fires; resolution unchanged.
- Command-match without `base`: warning fires; resolution unchanged.
- `base = ""` on same provider: warning silenced; resolution bypasses
  legacy merge.
- `base = "builtin:<name>"`: warning silenced; explicit merge.
- `base = "<name>"` (bare self-exclusion): warning silenced; equivalent
  to builtin form.
- Cache during Phase A: for a name-matching provider without `base`,
  resolved spec equals the Phase B `base = "builtin:<name>"` spec.

### HTTP / API

- `/v0/providers` returns the same `ResolvedProvider` the runtime uses.
- `/v0/config/explain --provider <name>` returns full provenance.
- `/v0/config/explain --json` round-trips provenance.
- CRUD accepts `base`-only provider, round-trip reads it back.
- Public DTOs do not expose `omit` sentinel.

### Observability

- `gc config show` annotation for: no-base, custom-rooted, 4+ layer
  chain. Round-trip as valid TOML (comments stripped on re-parse OK).
- `gc config explain` golden-file text and JSON outputs.
- Phase A warning shows up in explain output.

### End-to-end integration

- **Aimux-wrapped codex regression (golden fixture)**: checked-in
  `testdata/pack_aimux_codex.toml` matching the maintainer-city config
  → resolved `codex-mini` has `PermissionModes["unrestricted"]`,
  `ReadyDelayMs = 3000`, `ResumeCommand` with `{{.SessionKey}}`,
  `SupportsHooks = true`, `BuiltinAncestor = "codex"`,
  `ProcessNames = ["aimux", "codex"]` (if overridden) or triggers
  wrapper `process_names` warning (if not). Snapshot the full
  resolved `ResolvedProvider` struct to a golden file; changes must
  be intentional and visible in diff. Spawn an agent; assert it does
  not hang on first sandboxed command, hooks install, `--settings`
  injection works for wrapper-derived Claude, resume round-trip
  substitutes `{{.SessionKey}}` correctly.
- **Aimux-wrapped claude regression (golden fixture)**: similar
  snapshot for `claude-max base="builtin:claude"` — covers claude
  wrapper-resume (`--resume` flag style, not subcommand) and claude
  settings injection.

### Sync enforcement

- `TestProviderFieldSync` — new fields present in `ProviderSpec` also
  present in `ProviderPatch`, `applyProviderPatch`, and deep-copy
  paths.

### Namespace / parse

- Custom provider name containing `:` → parse error.
- `base = "builtin:"` (empty suffix) → error.
- `base = "provider:"` (empty suffix) → error.

## Open questions

None blocking implementation. Surfaces to revisit if demand emerges:

- Multi-inheritance / mixins.
- `_append` variants for `ProcessNames`, `PrintArgs`.
- `command_wrap` / placeholder-based args for outer-wrapper
  composition.
- Schema discriminator for future provider-schema migrations.
- `_append` for other keyed collections beyond `options_schema`.
