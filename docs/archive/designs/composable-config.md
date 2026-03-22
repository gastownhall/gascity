---
title: "Composable Config"
---

**Status:** Draft v4 — final synthesis (7 reviewers)
**Author:** Claude (with Steve)
**Date:** 2025-02-25
**Updated:** 2025-02-25

## Problem

Gas City configs are monolithic. A single `city.toml` defines every agent,
rig, provider, and formula. This creates three escalating problems:

**P1 — Unwieldy configs.** A Gas Town deployment has 8+ agents, 5+ rigs,
providers, and formulas. One file becomes hard to manage and review.

**P2 — Copy-paste per rig.** You want witness + refinery + polecat on
every rig. Today you duplicate agent blocks per rig, changing only `dir`.

**P3 — No reusable packs.** You can't say "run Gas Town" or "run
CCAT" and have it work. Each city hand-assembles its agent list. There's
no way to package, share, or version a pack.

### When these problems actually bite

These problems are **real but not yet urgent.** The pain threshold is
approximately 4-5 rigs with duplicate agent patterns. Tutorial 01 has one
agent and one rig. The current Gas Town example config is ~100 lines even
fully expanded.

**This document is a design, not an implementation plan.** It captures the
architecture for when the pain arrives (Tutorial 04-05 timeframe). Per
CLAUDE.md: "We do not build ahead of the current tutorial." The only
immediate deliverable is `gc config show` (useful regardless of
composition approach).

## Agent Identity

Before discussing composition, we must define the canonical identity key
for agents, since it governs merge targeting, validation, provenance, and
error messages.

**The key is `(dir, name)`**, already implemented in `ValidateAgents()`:

```go
type agentKey struct{ dir, name string }
```

The canonical string form is `QualifiedName()`:
- City-wide: `"mayor"` (dir is empty)
- Rig-scoped: `"hello-world/polecat"` (dir/name)

This identity flows through the entire system:

```
Config: Agent{Dir, Name}
  → QualifiedName() = "dir/name"
    → SessionNameFor() = "gc-{city}-dir--name"
      → Reconciliation matching
        → Fingerprint comparison
```

All composition operations (patch, suspend, override) target agents by
this `(dir, name)` key. Validation rejects duplicate keys. Error messages
reference qualified names, not array indices.

## Composition Operations

Any reusable config system needs three fundamental operations. Kustomize's
success comes from supporting all three without templates:

| Operation | Mechanism | Layer | Example |
|-----------|-----------|-------|---------|
| **Add** | Array concatenation | 1 | Fragment adds new agents/rigs |
| **Patch** | Keyed patch blocks | 1 | Override pool.max on one agent |
| **Suspend** | `suspended = true` | 1 | Skip refinery on small rigs |

All three operations are available at Layer 1. Without patch/suspend at
Layer 1, CLI file layering (`gc start -f base -f prod`) can only add
resources — it can't override a rig path for CI or disable an agent for
dev. That makes layering nearly useless and guarantees fork sprawl.

### Why suspend (not enable)

The codebase already has `Suspended bool` on agents (default false =
active). The controller already skips suspended agents. Overrides use
`suspended = true` rather than introducing a competing `enabled` field.
Go's zero value (`false` = not suspended = active) works correctly here
— no `*bool` needed.

## Kubernetes Parallel

The mapping between K8s and Gas City is surprisingly tight:

| Kubernetes | Gas City | Notes |
|-----------|----------|-------|
| Pod | Agent session | Smallest schedulable unit |
| Deployment | Agent + pool config | Declares desired replicas |
| ReplicaSet | Pool instances | Maintains N copies |
| Service | Session name | How agents address each other |
| ConfigMap | Prompt template | Injected config that shapes behavior |
| Namespace | Rig | Scoping / isolation boundary |
| Node | Rig path | Physical location where work runs |
| Controller loop | `gc supervisor run` | Reconcile desired → actual |
| etcd | Beads store | Persistent state |
| kube-apiserver | controller.sock + city.toml | Declared desired state |

K8s solved config composition three times, each learning from the last:

1. **Multi-file apply** (`kubectl apply -f dir/`) — split YAML into files
2. **Kustomize** — base + overlay patching, no templates, explicit patches
3. **Helm** — templated packages with values.yaml parameterization

Helm is powerful but widely criticized for Go-template-in-YAML debugging
pain. Kustomize is simpler and covers 80% of cases. The lesson: **start
with the simplest useful mechanism.**

**Important nuance:** Kustomize patches *replace* fields; they don't merge
them. The lesson is that explicit, predictable override semantics beat
clever merging. Our design should favor explicitness over magic.

**The K8s ConfigMap lesson:** K8s famously does NOT automatically roll
pods when a ConfigMap changes — many teams add content hash annotations to
force rollouts. Gas City's prompt templates are our ConfigMaps. The
fingerprint must include all resolved config, not just command + env
(see Fingerprinting section).

## Design: Three Layers

### Layer 0: Config Visibility (`gc config show`) — Build Now

Before any composition machinery, provide the debugging tool that makes
composition debuggable:

```bash
# Dump the fully-resolved config as TOML
gc config show

# Show where each field originated (when composition exists)
gc config show --provenance

# Validate without starting
gc config show --validate

# Explain a specific agent's resolved config with origins
gc config explain --rig big-project --agent polecat
```

This is useful today (validates city.toml) and becomes essential once
fragments and packs exist. **This is the only thing we build now.**

The `explain` subcommand (built with Layer 2) shows final values plus
which file set each one — the equivalent of `kustomize build` or
`helm template`.

### Layer 1: Config Fragments + Patches — Build at ~Tutorial 04

city.toml gains `include` for file splitting and `[[patches]]` for
targeted modifications.

#### Adding resources (concatenation)

```toml
# city.toml — the root
include = [
    "agents/mayor.toml",
    "agents/oversight.toml",
    "rigs/hello-world.toml",
]

[workspace]
name = "bright-lights"
provider = "claude"
```

```toml
# agents/mayor.toml — a fragment
[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md.tmpl"
```

```toml
# rigs/hello-world.toml — rig + its agents in one fragment
[[rigs]]
name = "hello-world"
path = "/home/user/hello-world"

[[agent]]
name = "witness"
dir = "hello-world"
prompt_template = "prompts/witness.md.tmpl"

[[agent]]
name = "polecat"
dir = "hello-world"
prompt_template = "prompts/polecat.md.tmpl"
[agent.pool]
min = 0
max = 5
```

#### Patching resources (keyed by identity)

Patches target existing resources by their identity key and modify
specific fields. They are the Kustomize equivalent — explicit, reviewable
modifications without editing the source.

```toml
# overrides/production.toml — patches for prod

# Change a rig's path for the CI environment
[[patches.rigs]]
name = "hello-world"
path = "/opt/deploy/hello-world"

# Tune pool size for an agent
[[patches.agent]]
dir = "hello-world"
name = "polecat"
pool = { max = 10 }

# Suspend an agent in dev
[[patches.agent]]
dir = "hello-world"
name = "refinery"
suspended = true

# Remove inherited env var
[[patches.agent]]
dir = "hello-world"
name = "polecat"
env_remove = ["VERBOSE_LOGGING"]

# Override provider model
[[patches.providers]]
name = "claude"
model = "opus"
```

**Patches are explicit — no warnings.** The warning system only fires on
accidental collisions (two fragments both adding the same agent). Explicit
patches are intentional by definition and produce no noise.

**Patch targeting:** Patches match by identity key:
- Agents: `(dir, name)` — both fields required
- Rigs: `name`
- Providers: `name`

If a patch targets a nonexistent resource, it's an error:

```
gc start: patch "hello-world/refinery": agent not found in merged config
```

#### Merge rules

- `agents` arrays **concatenate** (root first, then includes in order)
- `rigs` arrays **concatenate**
- `providers` maps **deep-merge per-field** (not whole-block replacement;
  see Provider Merge section). Warns on accidental per-field collision.
- `workspace` fields **merge** (include field overrides root per-field)
- `patches` **apply after merge** — they modify the concatenated result
- Validation runs **after** patches applied
- **Includes are NOT recursive** — fragments cannot include other
  fragments. Prevents cycles, keeps debugging simple.

#### Path resolution

All relative paths (prompt_template, include paths, pack refs) resolve
**relative to the file that declared them.** The merge converts all paths
to absolute/canonical form in the `City` struct. This matches Terraform
modules, Bazel, CSS imports, and Go module paths.

**Root-relative escape hatch:** Paths prefixed with `//` resolve relative
to the city root directory, regardless of which file declared them:

```toml
# overrides/production.toml
[[patches.agent]]
dir = "hello-world"
name = "witness"
prompt_template = "//prompts/witness-prod.md.tmpl"  # city root
```

Without `//`, this file would need `../prompts/witness-prod.md.tmpl` —
brittle and confusing. The `//` prefix prevents path spaghetti in
override files. (Borrowed from Bazel's `//` workspace-root convention.)

`gc config show --provenance` displays both the original path string and
the resolved absolute path for debugging.

#### CLI-level file layering (stolen from Docker Compose)

```bash
# Layer additional config from the CLI — great for dev/prod splits
gc start -f city.toml -f overrides/production.toml

# Equivalent to adding production.toml as the last include
# Patches in production.toml apply after all fragments merge
```

This is orthogonal to in-file includes and handles the CI/CD pipeline
use case where environment-specific overrides are injected externally.

#### Error provenance

Every error from a fragment includes the source file:

```
gc start: loading config: fragment "agents/bad.toml": agent[0]: name is required
gc start: patch "hello-world/refinery": agent not found in merged config
gc start: patch "hello-world/polecat": field "pool.max" conflicts with
    fragment "rigs/hw.toml" (was 5, patched to 10)
```

### Layer 2: Rig Packs (per-rig agent stamps) — Build When P2 Bites

**Trigger:** Build this when the same agent pattern appears on 3+ rigs.

A **pack** is a directory containing a config fragment + prompts +
metadata. It defines a reusable set of agents that can be stamped onto
any rig.

```
packs/gastown/
    pack.toml       # metadata + agent definitions (no dir — comes from rig)
    prompts/
        witness.md.tmpl
        refinery.md.tmpl
        polecat.md.tmpl
```

```toml
# packs/gastown/pack.toml

[pack]
name = "gastown"
version = "1.0.0"
schema = 1                  # pack schema version
# requires_gc = ">=0.9.0"  # optional: minimum gc version

[[agent]]
name = "witness"
prompt_template = "prompts/witness.md.tmpl"

[[agent]]
name = "refinery"
isolation = "worktree"
prompt_template = "prompts/refinery.md.tmpl"

[[agent]]
name = "polecat"
isolation = "worktree"
prompt_template = "prompts/polecat.md.tmpl"
[agent.pool]
min = 0
max = 3
```

The `[pack]` metadata header is intentionally lightweight. It
enables version compatibility checks and becomes the canonical
identifier structure when Layer 3 (published packs) arrives.
The `schema` field allows future pack format evolution without
breaking existing packs.

The city imports it **per rig**:

```toml
# city.toml
[workspace]
name = "bright-lights"

[[rigs]]
name = "hello-world"
path = "/home/user/hello-world"
pack = "packs/gastown"

[[rigs]]
name = "another-project"
path = "/home/user/another-project"
pack = "packs/gastown"

[[rigs]]
name = "simple-thing"
path = "/home/user/simple"
# no pack — just a rig with beads, no agents
```

**Resolution:** When a rig has `pack`, config loading:

1. Loads `pack.toml` from that directory
2. Checks `[pack]` metadata (version compatibility)
3. Sets `dir = <rig-name>` on every agent in the pack (overridable)
4. Resolves `prompt_template` paths relative to the pack directory
5. Merges the agents into the city's agent list
6. All happens before validation — downstream sees a flat `City` struct

**Per-rig overrides** (Kustomize-style patches, not templates):

```toml
[[rigs]]
name = "big-project"
path = "/home/user/big"
pack = "packs/gastown"

# Patch: change polecat's pool size
[[rigs.overrides]]
agent = "polecat"
pool = { max = 10 }

# Patch: add env to witness
[[rigs.overrides]]
agent = "witness"
env = { EXTRA_CONTEXT = "security-critical" }

# Suspend: skip refinery on this rig entirely
[[rigs.overrides]]
agent = "refinery"
suspended = true

# Override dir for monorepo subdirectory
[[rigs.overrides]]
agent = "polecat"
dir = "services/api"

# Remove inherited env var
[[rigs.overrides]]
agent = "polecat"
env_remove = ["VERBOSE_LOGGING"]

# Override prompt template (// = city root)
[[rigs.overrides]]
agent = "witness"
prompt_template = "//prompts/witness-secure.md.tmpl"
```

**Override granularity:** Sub-field patching. `pool = { max = 10 }` changes
only `pool.max`; `pool.min` retains the pack's value. This is achieved
using pointer types in the `AgentOverride` struct (see TOML Mechanics
section).

**Dir override:** By default, pack stamping sets `dir = <rig-name>`.
The `dir` field in overrides replaces this. For monorepos where agents
work in subdirectories, set `dir = "services/api"` — the override
replaces the stamped dir entirely, giving full control.

**Suspend semantics:** `suspended = true` on an override sets the agent's
`Suspended` field. The controller already skips suspended agents — no
sessions started, existing sessions stopped gracefully. The agent still
appears in `gc config show` so the configuration is inspectable and
reversible. This is the same pattern as Kubernetes `replicas: 0`.

**env removal:** `env_remove = ["KEY1", "KEY2"]` explicitly unsets
inherited env vars. Necessary because TOML has no null value. The removal
list is applied after env merging.

No Go-template-in-TOML. Explicit patches. You can always answer "what
config does polecat on big-project get?" by reading two files (or running
`gc config explain --rig big-project --agent polecat`).

### Layer 3: Published Packs (future, not designed now)

Once packs are directories with metadata, they can live anywhere:

- Local: `packs/gastown/`
- Git: `pack = "github.com/steveyegge/gastown-pack@v1"`
- Shared: `pack = "../shared-packs/ccat"`

This is Helm charts. We don't design the details until actual demand
from multiple independent Gas City users materializes.

**Forward-compatibility:** Layer 2's `[pack]` metadata and canonical
directory structure are designed to support Layer 3 without breaking
changes. The `pack` field on rigs is a string today (local path) and
can accept URLs later. When Layer 3 arrives, it will also need:

- Immutable refs (commit SHAs or content hashes, not just tags)
- Lockfiles for reproducibility
- Integrity checks (checksums)
- Trust boundaries (packs can set env/commands — security surface)

These are all solved problems (Terraform modules, Helm charts, npm), but
the solutions are substantial. The `[pack]` metadata header reserves
the namespace for these fields.

**Pack content hash:** Even for local packs, `gc config show`
displays a content hash (SHA256 of all files in the pack directory).
This is cheap to compute, enables reproducibility checks, and lays the
groundwork for integrity verification in Layer 3.

## TOML Mechanics (Verified by Testing)

### The `IsDefined()` Problem

The BurntSushi TOML library's `MetaData.IsDefined()` **does not work
inside arrays-of-tables** (`[[...]]`). Verified empirically:

```go
// For [workspace] (regular table):
md.IsDefined("workspace", "provider")  // → true  ✓
md.IsDefined("workspace", "name")      // → false ✓ (not in TOML)

// For [[agent]] (array-of-tables):
md.IsDefined("agent", "pool", "max")  // → false ✗ (WRONG — it IS defined)
md.IsDefined("agent", "name")         // → false ✗ (WRONG — it IS defined)
```

`Keys()` returns a correct flat list of all defined keys, but indexing
into arrays-of-tables is ambiguous (which `[[agent]]` entry?).

**Implications for our design:**

1. **Workspace merge can use `IsDefined()`** — it's a regular table.
   This solves zero-value ambiguity for workspace fields.

2. **Patch/override structs use pointer types** — since patches target
   agents (arrays-of-tables) where `IsDefined()` fails, pointer types
   distinguish "not set" from "set to zero":

```go
// AgentOverride uses pointers — nil means "don't override this field"
type AgentOverride struct {
    Agent          string            `toml:"agent"`
    Dir            *string           `toml:"dir,omitempty"`
    Suspended      *bool             `toml:"suspended,omitempty"`
    Pool           *PoolOverride     `toml:"pool,omitempty"`
    Env            map[string]string `toml:"env,omitempty"`
    EnvRemove      []string          `toml:"env_remove,omitempty"`
    Isolation      *string           `toml:"isolation,omitempty"`
    PromptTemplate *string           `toml:"prompt_template,omitempty"`
}

type PoolOverride struct {
    Min   *int    `toml:"min,omitempty"`
    Max   *int    `toml:"max,omitempty"`
    Check *string `toml:"check,omitempty"`
}

// AgentPatch is the same shape, used in [[patches.agent]]
type AgentPatch struct {
    Dir            string            `toml:"dir"`   // targeting key (required)
    Name           string            `toml:"name"`  // targeting key (required)
    Suspended      *bool             `toml:"suspended,omitempty"`
    Pool           *PoolOverride     `toml:"pool,omitempty"`
    Env            map[string]string `toml:"env,omitempty"`
    EnvRemove      []string          `toml:"env_remove,omitempty"`
    Isolation      *string           `toml:"isolation,omitempty"`
    PromptTemplate *string           `toml:"prompt_template,omitempty"`
}

// Agent struct — existing, minimal changes
type Agent struct {
    Name           string            `toml:"name"`
    Dir            string            `toml:"dir"`
    Suspended      bool              `toml:"suspended"`  // already exists
    Pool           *PoolConfig       `toml:"pool"`        // already a pointer
    // ...
}
```

3. **Fragment merge uses concatenation** for agents/rigs (arrays), so the
   zero-value problem doesn't apply there — we're appending, not merging
   fields.

4. **`Suspended` uses Go's correct zero value.** `bool` defaults to
   `false` = not suspended = active. No `*bool` needed in the Agent
   struct. Only the patch/override structs need `*bool` to distinguish
   "don't change" from "set to false."

### TOML `include` Placement

`include` must be **top-level** (before any `[table]` header) per TOML
spec. Bare keys before tables are valid TOML:

```toml
include = ["agents/mayor.toml", "rigs/hw.toml"]

[workspace]
name = "bright-lights"
```

This is actually good UX: the include list is the first thing you read,
which is the first thing you need to know about a composed config.

### Conflict Warnings (accidental collisions only)

When two fragments accidentally define the same resource or scalar field,
the design uses last-writer-wins but **logs a warning**:

```
gc start: config: provider "claude".model redefined by fragment "overrides.toml"
  (was: "sonnet", now: "opus")
```

**Explicit patches never warn** — they are intentional modifications by
definition. Warnings only fire on unintentional collisions between
fragments that both add the same thing.

`--strict` flag promotes accidental-collision warnings to errors for
CI/CD pipelines.

## Merge Semantics (Detailed)

### Processing order

1. Load root city.toml
2. Load and concatenate each included fragment (in order)
3. Load and concatenate each `-f` CLI file (in order)
4. Detect accidental collisions → warn (or error with `--strict`)
5. Apply `[[patches]]` blocks (keyed by identity)
6. Expand packs (Layer 2)
7. Apply `[[rigs.overrides]]` (Layer 2)
8. Canonicalize all paths to absolute
9. Validate the fully-resolved config

### Array fields (concatenation)

```
root.Agents = [A1, A2]
fragment1.Agents = [A3]
fragment2.Agents = [A4, A5]
result.Agents = [A1, A2, A3, A4, A5]
```

### Provider deep merge (not shallow replacement)

**Critical distinction:** Providers are **deep-merged per-field**, not
replaced as a whole block. This prevents the "shallow override nukes
secrets" problem:

```
root.Providers = { claude: { api_key_env: "KEY", model: "sonnet" } }
fragment.Providers = { claude: { model: "opus" } }

# WRONG (shallow replace — drops api_key_env):
result.Providers = { claude: { model: "opus" } }

# CORRECT (deep merge — only model changes):
result.Providers = { claude: { api_key_env: "KEY", model: "opus" } }
⚠ warning: provider "claude".model collision (fragment wins)
```

If you genuinely need to replace an entire provider block, use an
explicit marker:

```toml
[providers.claude]
_replace = true    # signals: replace entire block, don't deep-merge
model = "opus"
args = ["--verbose"]
```

The `_replace = true` escape hatch is opt-in. The default (deep merge)
is safe.

### Workspace (per-field override via IsDefined)

```
root.Workspace = { name: "city", provider: "claude" }
fragment.Workspace = { provider: "gemini" }
result.Workspace = { name: "city", provider: "gemini" }
```

Uses `md.IsDefined()` (works for regular tables) to distinguish "not set"
from "set to empty." Only explicitly-set fields from the fragment override.

### Pack agent expansion

```
rig = { name: "hw", path: "/hw", pack: "topo/gt" }
pack.agents = [
    { name: "polecat", pool: { min: 0, max: 3 } },
    { name: "refinery" },
    { name: "witness" },
]
rig.overrides = [
    { agent: "polecat", pool: { max: 10 } },
    { agent: "refinery", suspended: true },
    { agent: "witness", dir: "hw/frontend" },
]

expanded = [
    { name: "polecat", dir: "hw", pool: { min: 0, max: 10 } },
    { name: "refinery", dir: "hw", suspended: true },
    { name: "witness", dir: "hw/frontend" },
]
```

## Interaction with Existing Systems

### Controller hot-reload

The controller already watches city.toml via fsnotify. With includes:

- **Watch directories** containing config files and pack dirs, not
  individual files. This handles rename-swap saves (vim/emacs) where
  watching a specific file path fails after the rename.
- **Debounce reloads** with a 200ms coalesce window. Many editors write
  via temp file + rename, producing multiple events for a single save.
  Git checkouts touch many files quickly. Without debounce, the
  controller sees event storms and flapping reloads.
- **Last-known-good on failure:** If reload fails validation, keep the
  previous config running and log the error. Do not tear down agents
  because of a transient parse error during an editor save. This matches
  K8s controller behavior.
- **Multi-file snapshot consistency:** After debounce, read all config
  files, stat them, and if any mtime changed during reading, retry once.
  This reduces half-old/half-new snapshots when multiple files change
  in quick succession.
- **Config revision tracking:** Compute a bundle hash (SHA256 of all
  resolved input file contents). Store as `config_revision`. Surface in
  `gc status`:
  ```
  Config revision: a3f7b2c...
  Last reload: 2025-02-25T14:30:00Z (success)
  ```
  When running last-known-good after a failed reload, surface prominently:
  ```
  ⚠ Running stale config (revision a3f7b2c from 14:30:00)
    Last reload failed at 14:35:12: fragment "bad.toml": parse error line 7
  ```

### Config fingerprinting

**The fingerprint must be a full spec hash**, not hand-picked fields.

The current fingerprint (`SHA256(command + env)`) misses changes to pool
sizing, isolation mode, provider selection, prompt template content, and
other fields that should trigger agent restarts.

**New approach:**

```
fingerprint = SHA256(canonical_resolved_agent_spec)
```

Where `canonical_resolved_agent_spec` is a stable serialization of the
resolved agent config struct: sorted map keys, stable field order,
including the content hash of the resolved prompt template (and its
transitive dependencies if templates can include partials).

Fields explicitly excluded from fingerprint (observation-only hints):
- `ReadyDelayMs`, `ReadyPromptPrefix`, `ProcessNames`,
  `EmitsPermissionWarning` — these are startup detection hints that
  don't change agent behavior.

Everything else — command, args, env, pool config, isolation, provider,
prompt content — triggers a restart on change. This matches K8s
Deployments, which roll pods on any change to the Pod template spec.

**Note:** This is the K8s ConfigMap lesson. K8s doesn't auto-roll pods on
ConfigMap changes. We learn from that and include content hashes from the
start.

### Validation

All validation (ValidateAgents, ValidateRigs, prefix collision detection)
runs on the fully-resolved config. Patches that target nonexistent
resources are errors. Invalid fragments produce clear errors with source
file attribution:

```
gc start: loading config: fragment "agents/bad.toml": agent[0]: name is required
gc start: patch targets "hello-world/refinery" but no such agent in merged config
```

Pack `[pack]` metadata is validated early:
- `schema` must be a supported version
- `requires_gc` (if present) must be satisfied by current gc version

### Provenance

Provenance is **built into the merge API from the start**, not bolted
on later. The merge function returns provenance alongside the config:

```go
func LoadWithIncludes(fs FS, path string) (*City, *Provenance, error)
```

The `Provenance` struct tracks, per field and per resource:
- Source file path and line number
- Whether the value came from root, fragment, patch, or pack
- For patches: what the value was before patching

This enables:
- `gc config show --provenance` — annotate every field with its source
- `gc config explain --rig X --agent Y` — show resolved config + origins
- Error messages that reference source files, not array indices
- Review tooling that can diff "what changed and why"

Designing provenance into the merge API is cheap now and extremely
expensive to retrofit later.

### Doctor checks

`gc doctor` validates the merged config. A new `config-fragments` check
verifies all included files exist and parse.

### Progressive capability model

Include/pack are config-level features. They don't change which
primitives are active — that's still determined by section presence in
the merged config. A Tutorial 01 city with includes just has a split
config; it doesn't unlock formulas or messaging.

## What Changes

### Immediate (build now)

- `cmd/gc/cmd_config.go`: new `gc config show` command
- Dumps loaded city.toml as resolved TOML
- Add `--validate` flag (parse + validate, exit 0/1)

### Layer 1 (build at Tutorial 04)

**Config package (`internal/config/`):**

- Add `Include []string` to top-level City struct (TOML: `include`)
- Add `Patches` struct (agents, rigs, providers patch lists)
- New `AgentPatch` struct with pointer types, keyed by `(dir, name)`
- New `LoadWithIncludes(fs, path) (*City, *Provenance, error)`
- New `MergeCity(base, fragment *City) *City` — concatenation merge
- New `ApplyPatches(cfg *City, patches Patches) error`
- New `DeepMergeProvider(base, overlay Provider) Provider` — per-field
- Path canonicalization: `//` → city root, relative → declaring file
- `Provenance` struct tracking source file + line per field/resource

**CLI (`cmd/gc/`):**

- `cmd_start.go`: call `LoadWithIncludes` instead of `Load`
- `cmd_start.go`: add `-f` flag for CLI-level file layering
- `cmd_start.go`: add `--strict` flag (promote collision warnings to errors)
- `controller.go`: watch directories (not files), debounce 200ms,
  last-known-good, snapshot consistency check, config revision tracking
- `cmd_doctor.go`: add fragment existence check
- `cmd_config.go`: add `--provenance` flag

### Layer 2 (build when P2 bites)

**Config package (`internal/config/`):**

- Add `Pack string` to `Rig` struct
- Add `Overrides []AgentOverride` to `Rig` struct
- New `AgentOverride` struct with pointer types + `Dir`, `Suspended`,
  `EnvRemove`, `PromptTemplate`
- New `PoolOverride` struct with pointer types
- New `ExpandPacks(cfg *City, fs) error` — resolve pack refs
- New `PackMeta` struct (name, version, schema, requires_gc)
- Expand fingerprint to full canonical spec hash + prompt content hashes
- Pack content hash in `gc config show` output

**CLI (`cmd/gc/`):**

- `controller.go`: watch pack dirs
- `cmd_config.go`: add `explain --rig X --agent Y` subcommand

### No changes to

- Session provider, beads, events, formulas, agent package
- Reconciler, crash tracker, pool manager
- CLI commands other than start/controller/doctor/config

Both layers are invisible to everything downstream of config loading.
The rest of the system sees the same flat `City` struct it always has.

## Design Principles Applied

**ZFC (Zero Framework Cognition):** Config merging is pure data
transformation. No judgment calls. The merge function is deterministic
given the same inputs. Conflict warnings are informational, not
decision-making. Patches are explicit data operations, not intelligence.

**Bitter Lesson:** The real test is: does this become MORE useful as
models improve? Modular configs are easier for models to generate and
modify — a model can create a fragment without understanding the entire
city. But models also handle large single files well. The honest answer:
composition benefits humans more than models at current scale. It becomes
model-relevant when configs exceed context windows (unlikely soon).

**Primitive Test:** Not a new primitive. Enhancement to Config
(primitive #4). No new irreducible concept. The merge function is a
pure data transformation on existing structs.

**GUPP:** Unaffected. Agents see the same hooks and beads regardless
of config source.

**NDI:** Config resolution is deterministic and idempotent. Same files
→ same merged config → same reconciliation outcome. Debounce and
last-known-good don't affect determinism — they affect timing.

**Tutorial-Driven Development:** Only `gc config show` is built now.
Layers 1-2 wait until the tutorial needs them. This document is the
design, not the implementation plan.

## Resolved Questions

1. **`include` placement:** Top-level, before any table. TOML requires
   bare keys before tables. Good UX — includes are the first thing you
   read.

2. **Zero-value ambiguity:** Solved differently per context.
   `IsDefined()` works for `[workspace]` (regular table). Pointer types
   work for patches/overrides (inside arrays-of-tables where `IsDefined()`
   fails). Agent struct stays with value types — `Suspended bool` defaults
   to false (active), which is correct.

3. **Path resolution:** Relative to the file that declared the agent.
   `//` prefix resolves relative to city root (Bazel convention).
   All paths canonicalized to absolute at load time.

4. **Override granularity:** Sub-field patching via pointer types.
   `pool = { max = 10 }` changes only max; min retains the original.
   `PoolOverride` uses `*int` so nil = "don't touch this field."

5. **Fragment conflicts:** Warn on accidental collisions only (two
   fragments adding the same scalar). Explicit patches never warn.
   `--strict` promotes warnings to errors for CI/CD.

6. **Controller watch scope:** Watch directories, not individual files.
   Debounce 200ms. Last-known-good on failure. Snapshot consistency
   check. Config revision tracking for observability.

7. **Provider merge depth:** Deep-merge per-field (not whole-block
   replace). Opt-in `_replace = true` for full replacement when needed.

8. **Fingerprint scope:** Full canonical spec hash of resolved agent
   config + prompt content hashes. Not hand-picked fields. Excludes
   only observation hints (ready delay, process names). Matches K8s
   Pod template spec hashing.

9. **Suspend semantics:** Uses existing `Suspended bool` field, not a
   new `enabled` field. Go zero value (false = active) is correct.
   Override/patch structs use `*bool` to distinguish "don't change"
   from "set to false."

10. **Pack metadata:** `[pack]` header with name, version,
    schema, optional requires_gc. Pack content hash in config show.
    Reserves namespace for Layer 3 supply chain fields.

11. **Agent identity:** `(dir, name)` — already implemented in
    `ValidateAgents()`. `QualifiedName()` is the canonical string form.
    All patches/overrides target by this key.

12. **Dir override:** `Dir *string` in overrides. Default is rig-name
    stamping; override replaces entirely. Enables monorepo subdirectories.

13. **env removal:** `env_remove = [...]` for explicit removal of
    inherited env vars. Applied after env merging. TOML has no null, so
    explicit removal lists are the only mechanism.

14. **Provenance:** Built into the merge API from the start.
    `LoadWithIncludes` returns `(*City, *Provenance, error)`. Tracks
    source file, line, and transform type per field/resource.

## Remaining Open Questions

1. **Naming: `pack` vs `extends`?** Docker Compose uses `extends`;
   K8s uses concepts (Deployment, Service). `pack` names the concept
   (what orchestration shape); `extends` names the mechanism (inheritance).
   Leaning toward `pack` but open to feedback.

2. **`agent_templates` alternative.** A simpler mechanism: rigs reference
   named agent templates rather than full pack directories. Solves P2
   (copy-paste) without the pack directory structure. May be sufficient
   if P3 (reusable packs) doesn't materialize:

   ```toml
   [agent_templates.rig-workers]
   agents = ["witness", "refinery", "polecat"]

   [[rigs]]
   name = "hello-world"
   path = "/home/user/hello-world"
   template = "rig-workers"
   ```

   This is simpler but less powerful. Packs bundle prompts +
   config; templates only reference existing agents.

## Implementation Order

1. **`gc config show` now.** Useful immediately, no composition needed.
   Validates config, dumps resolved TOML.
2. **Layer 1 at Tutorial 04.** When city.toml exceeds ~150 lines with
   multiple rigs and agent types.
3. **Layer 2 when P2 bites.** When the same agent pattern appears on 3+
   rigs. Evaluate `agent_templates` vs full packs at that point.
4. **Layer 3 never (until needed).** Remote pack resolution is
   future work driven by actual demand from multiple independent users.

## Rejected Alternatives

### Auto-discovery (scan directory for *.toml)

Like `kubectl apply -f .` — load all TOML files in the city directory.
Rejected because:
- Ordering is implicit (alphabetical? modification time?)
- Hard to know which files contribute to the config
- Accidental inclusion of unrelated TOML files
- Explicit includes are easier to reason about

### TOML templating (Go templates in TOML)

Like Helm — `max = {{.Values.maxPolecats}}`. Rejected because:
- Go templates in TOML are ugly and fragile
- Syntax errors are confusing (TOML parse error? Template error?)
- Helm's biggest pain point is exactly this
- Kustomize-style patches achieve the same result without templates

### Deep nesting / recursive includes

Fragments that include other fragments. Rejected because:
- Cycle detection needed
- Hard to debug ("where did this agent come from?")
- Transitive dependency resolution is complex
- One level of includes covers the real use cases

### Inheritance-based config (extends/inherits)

Like CSS or OOP inheritance. Rejected because:
- "Which field came from which ancestor?" is notoriously hard to debug
- Kustomize proved that explicit patches beat inheritance
- Gas City's override cascade (workspace → agent inline) is already
  simple and working

### Config as code (Go/Lua/Starlark/CUE)

Programmatic config generation. Rejected because:
- Violates "config is data, not code"
- Makes validation, linting, and tooling much harder
- Models work better with structured data than with programs
- Kubernetes CRDs + Kustomize proved declarative config scales
- CUE/Dhall solve merge ambiguity but at enormous adoption cost
- TOML merge ambiguities can be solved with stricter semantics

### Rig-local config files

Each rig directory contains its own agent definitions. Rejected because:
- Violates city-as-single-source-of-truth (controller needs centralized
  desired state)
- Coupling to rig filesystem availability breaks config loading
- Doesn't solve pack reuse — just moves duplication to N rig.toml files
- Prompt template path confusion (relative to rig or city?)

### Pack-owns-the-city (inversion)

Pack is the primary artifact; city is just runtime rig bindings.
Interesting conceptual separation (WHAT agents vs WHERE they run) but
rejected because:
- Requires two files mandatory instead of one for simple cases
- City-wide agents (no `dir`) don't fit cleanly
- Conflicts with city-as-directory model (settled decision)
- The current design already captures the valuable parts via `pack`
  on rigs

### Shallow provider replacement

Replace entire provider block on key conflict. Rejected because:
- Silently drops fields like `api_key_env` when fragment only wants to
  change `model`
- Deep merge per-field with opt-in `_replace = true` is safer

### Concat-only Layer 1 (no patches)

Fragments can only add resources, not modify existing ones. Rejected
because:
- CLI file layering (`-f base -f prod`) becomes useless for overlays
- Can't change a rig path, disable an agent, or tune a pool via overlay
- Forces forks for any environment-specific customization
- Kustomize's entire value proposition is patching; concat alone is
  just `cat *.toml`

## Review Attribution

This design was reviewed across 7 independent perspectives:

1. **Gas City principles** — checked against ZFC, Bitter Lesson, GUPP,
   NDI, Primitive Test, tutorial-driven development
2. **Kubernetes lessons** — compared to K8s/Kustomize/Helm patterns and
   common pitfalls
3. **TOML mechanics** — empirically tested BurntSushi library behavior
   with actual Go programs
4. **User experience** — evaluated DX, error messages, migration path,
   debugging workflow
5. **Alternative approaches** — evaluated 7 alternatives (rig-local,
   convention-over-config, config generation, functional language,
   Docker Compose, do-nothing, pack-inversion)
6. **Operations stress-test** — failure scenarios, hot-reload edge cases,
   missing composition operations, supply chain forward-compatibility
7. **Codebase cross-reference** — verified design against existing Agent
   struct, ValidateAgents identity key, fingerprint implementation,
   Suspended field, and controller reconciliation
