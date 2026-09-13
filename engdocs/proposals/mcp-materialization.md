---
title: "MCP Materialization and Provider Projection"
---

## Context

Pack V2 introduced a provider-agnostic `mcp/` directory convention and a
list-only CLI (`gc mcp list`), but Gas City still does not project MCP servers
into the provider-native runtime config that Claude, Codex, and Gemini
actually read. The result is the same gap skills had before v0.15.1:
catalogued content exists in the pack model, but agents never receive the
runtime effect.

This proposal ships the active MCP slice:

- parse and validate neutral MCP TOML definitions
- resolve a precedence-ordered effective catalog per target
- project that catalog into the provider's native MCP config surface
- reconcile stale entries and restart sessions when projected MCP changes

Unlike skills, MCP delivery is not a directory of symlinks. Each provider owns
its own config file shape, merge semantics, and scope rules. The core design
problem is therefore not just discovery, but deterministic projection and
ownership.

## What Exists Today

- `mcp/` and `agents/<name>/mcp/` are already recognized as pack/agent
  attachment roots during config composition.
- `gc mcp list` exists, but it is still a raw visibility view and still claims
  MCP is list-only.
- Skills already established the relevant runtime shape:
  - a two-stage materialization flow
  - scope-root reconciliation at supervisor time
  - session/worktree delivery via a hidden internal command
  - fingerprint-driven restart on content drift
- The loader already composes ordered city and rig pack graphs, explicit
  imports, and implicit bootstrap imports.
- Claude, Codex, and Gemini all have provider-native project-local MCP
  surfaces, but Gas City does not yet write to them.

## Provider Surface Evidence

This proposal follows the providers' native MCP surfaces instead of inventing a
GC-specific sidecar format.

- Claude Code documents project MCP in `.mcp.json`.
- Gemini CLI documents project MCP in `.gemini/settings.json` under
  `mcpServers`.
- Codex's published docs still emphasize user-global
  `~/.codex/config.toml` under `[mcp_servers]`, but current upstream behavior
  appears to honor repo-local `.codex/config.toml` as well.

Implementation must carry provider acceptance coverage for all three families:

- Claude: project-local `.mcp.json` is read, reconciled, and cleaned up as
  designed
- Gemini: project-local `.gemini/settings.json` `mcpServers` subtree is read,
  preserved outside the managed subtree, and cleaned up as designed
- Codex: repo-local `.codex/config.toml` is honored in the session workdir

Where provider docs and runtime behavior diverge, the tested runtime behavior
is the release gate.

Transport support is also provider-gated at projection time. If a provider
family's current tested build does not support a neutral transport shape
(for example, HTTP remote MCP on a given provider), projection for that target
must hard-error instead of emitting a config block the provider will ignore.

The Codex project-local target is therefore a design requirement with an
explicit merge gate: implementation must land with an acceptance test against
the current Codex build proving repo-local `.codex/config.toml` is honored in
the session workdir. If that proof fails, the feature does not merge in its v1
form because user-global projection would violate workdir-scoped ownership.

## Goals

1. Every agent with an effective MCP catalog receives that catalog in the
   provider-native MCP config file for the exact workdir the provider runs in.
2. MCP source of truth remains provider-agnostic and pack-composable:
   `mcp/*.toml` and `agents/<name>/mcp/*.toml`.
3. Shared MCP can be shipped from city packs, rig packs, explicit imports,
   implicit imports, and bootstrap packs with deterministic precedence.
4. Gas City fully owns the projected MCP target it manages and reconciles it on
   every tick/startup instead of appending best-effort entries.
5. Config drift in projected MCP restarts affected sessions.
6. Users can inspect the realizable projected result through `gc mcp list` and
   diagnose configuration problems through `gc doctor`.

## Non-Goals (v1)

- Provider-specific extension knobs such as trust policies, OAuth blocks,
  allowlists, approval defaults, or Codex parallel-tool-call hints.
- SSE remote MCP servers. v1 remote transport is streamable HTTP only.
- Live preflight of MCP commands, credentials, or HTTP endpoints during
  `gc init` or provider readiness checks.
- Support for providers outside the Claude, Codex, and Gemini families.
- Partial merge semantics between same-name MCP definitions. Same-name override
  is whole-definition replacement.

## Durable Source Model

### File identity

An MCP server is one TOML file:

- shared: `mcp/<server>.toml` or `mcp/<server>.template.toml`
- agent-local: `agents/<name>/mcp/<server>.toml` or
  `agents/<name>/mcp/<server>.template.toml`

Identity is the filename stem. `foo.toml` and `foo.template.toml` are both the
same logical server `foo`.

Rules:

- `name` is required and must exactly match the filename stem
- server names are restricted to lowercase `[a-z0-9-]+`
- if one directory contains both `foo.toml` and `foo.template.toml`, that is a
  hard error
- if two definitions with the same logical name meet through precedence, the
  higher-precedence definition replaces the lower one entirely

### Schema

The neutral MCP schema stays intentionally small:

- required:
  - `name`
- optional metadata:
  - `description`
- stdio transport:
  - `command`
  - `args`
  - `[env]`
- remote transport:
  - `url`
  - `[headers]`

Rules:

- exactly one of `command` or `url` must be set
- stdio definitions may not set `url` or `headers`
- HTTP definitions may not set `command`, `args`, or `env`
- `description` is metadata only; it is excluded from runtime equality and
  restart fingerprints

### Templates

`.template.toml` uses the same deterministic template context as prompt
templates. There is no implicit access to the controller host environment.

Expansion rules:

- expansion is per effective target (agent/session/workdir), not once globally
- missing template keys are a hard error
- template expansion happens before provider mapping
- expanded values are written literally into the projected runtime config in v1

### Relative command resolution

For stdio definitions:

- bare commands without `/` are preserved verbatim and resolved by the
  provider/runtime `PATH`
- relative commands containing `/` are resolved against the owning source
  directory and projected as absolute paths

This keeps pack-relative scripts stable across pooled worktrees while avoiding
controller-host-specific absolute resolution for generic commands such as
`uvx`, `node`, or `python`.

## Catalog Discovery and Precedence

### Shared catalog layers

Shared MCP is not limited to the root city pack. It follows the pack graph and
import graph, because sharing MCP definitions from packs is a primary use case.

For an agent, the effective shared catalog is layered as:

1. city pack graph
2. explicit city imports
3. non-bootstrap implicit imports
4. bootstrap implicit imports

For an agent inside a rig, a rig-shared layer overlays on top of the city
shared layer:

1. city shared layers
2. rig pack graph
3. rig explicit imports

The full effective precedence is therefore:

1. agent-local
2. rig-shared
3. city pack graph
4. explicit city imports
5. non-bootstrap implicit imports
6. bootstrap implicit imports

### Ordering inside each layer

- City and rig pack graphs follow their existing ordered pack directory stacks.
  Nearer packs win over their transitive dependencies.
- `workspace.includes` and other legacy include stacks keep their current
  ordered override model: later includes win over earlier ones.
- Sibling explicit imports keep deterministic sorted root binding order with
  first-wins precedence.
- This is intentionally different from legacy include stacks. MCP preserves
  each source surface's existing precedence contract instead of inventing one
  new winner rule for every layer.
- Shared imported-pack MCP is transitive by default; `transitive = false`
  limits visibility to the directly imported pack's own `mcp/`.
- `transitive = false` only limits visibility through that root import. Hidden
  transitive packs do not participate in shadowing or diagnostics through that
  route, but can still appear through any other visible import path.
- `export` has no effect on shared MCP because shared MCP has no namespaced
  binding surface to flatten.
- The same pack directory reached multiple times through a graph is deduplicated
  to its highest-precedence occurrence.

Whole-definition replacement is transport-agnostic: a higher-precedence layer
may replace a stdio server with an HTTP server or vice versa. That is
intentional. Partial field merges and metadata-only patching are out of scope
for v1.

### Shadow diagnostics

Same-name shared MCP collisions resolve by precedence, not hard error.

Diagnostics:

- explicit import shadows warn by default
- `[imports.<binding>].shadow = "silent"` suppresses shadow warnings for that
  root import and its transitive closure
- rig imports mirror the same behavior, but diagnostics identify the rig and
  root rig import binding
- legacy include-driven shadows warn by default and have no silent knob
- bootstrap shadows remain diagnostic-only and are not controlled by import
  flags

## Effective Runtime Model

Projection is built from a normalized in-memory model:

- one effective server record per logical server name
- transport already validated
- templates already expanded
- relative command paths already resolved
- metadata separated from behavioral fields

This normalized model is the single source for:

- provider emitters
- `gc mcp list`
- doctor checks
- same-target equality checks
- restart fingerprints

### Canonical normalization

Shared-target equality and restart fingerprints operate on the normalized
effective model, not on emitted JSON or TOML bytes.

Canonical rules:

- `args` are order-sensitive and preserved exactly
- `env` and `headers` are maps and are compared in stable key-sorted order
- empty optional maps and slices canonicalize to absent in the normalized model
- provider emitters consume that canonical model but equality does not depend on
  provider-specific serialization details such as TOML formatting, JSON
  whitespace, or map iteration order
- `description` is excluded from the canonical behavioral model

This keeps shared-target equality deterministic across runs and across
providers even though Claude and Gemini emit JSON while Codex emits TOML.

## Provider Projection

Projection targets the provider's native MCP surface directly.

### Claude-family providers

- target: `<workdir>/.mcp.json`
- Gas City owns the whole file

### Gemini-family providers

- target: `<workdir>/.gemini/settings.json`
- Gas City owns only the top-level `mcpServers` object
- unrelated settings are preserved
- preservation is semantic, not formatting-preserving; sibling JSON may be
  reserialized canonically on write

### Codex-family providers

- target: `<workdir>/.codex/config.toml`
- Gas City owns only the `[mcp_servers]` subtree
- unrelated settings are preserved
- preservation is semantic, not formatting-preserving; sibling TOML may be
  reserialized canonically on write

Because Codex's published docs lag the observed project-local behavior, the
project-local target is conditioned on the acceptance-test gate described in
"Provider Surface Evidence." The design does not fall back to user-global
projection.

Provider selection keys off `ResolvedProvider.Kind`, not just the configured
provider name, so aliases such as `fast-codex` or `my-claude` still map to the
correct native MCP surface.

### Ownership and cleanup

If an effective MCP catalog exists for a target, Gas City owns the managed
projection surface for that target.

Ownership rules:

- first projection overwrites preexisting provider-native MCP content
- any effective `mcp/` catalog is treated as an explicit opt-in to GC-owned MCP
  projection for that target
- for Gemini and Codex, "preserve unrelated settings" means preserve sibling
  config outside the GC-owned MCP subtree; Gas City owns every server entry
  inside the managed subtree
- stale projected servers are removed on reconcile
- empty effective set removes the managed projection surface:
  - delete `.mcp.json`
  - remove `mcpServers` from Gemini settings and delete the file if empty
  - remove `[mcp_servers]` from Codex config and delete the file if empty

Adoption and migration rules:

- if a target already contains provider-native MCP content, the first
  reconcile that sees effective MCP takes ownership and replaces that content
- first adoption is non-interactive but not destructive: before overwrite, Gas
  City snapshots the exact provider-native MCP content it is about to replace
  into a GC-owned adoption backup location under `.gc/`
- after that one-time snapshot, a recorded adoption marker prevents repeated
  backup churn on every reconcile
- first adoption also emits a one-time high-signal warning naming the target
  path and the backup location before the overwrite lands
- users who want to preserve existing provider-native MCP entries must port
  them into neutral `mcp/*.toml` before enabling GC-owned MCP
- `gc doctor` should surface preexisting provider-native MCP content as
  "will be adopted and replaced" before first projection so the transition is
  visible, but the runtime behavior remains deterministic and non-interactive
- once a target is GC-owned, out-of-band edits to the managed MCP surface are
  treated as drift: reconcile restores the canonical payload, and doctor/warn
  paths should identify that the managed surface was edited after adoption
- provider-specific server fields inside the adopted managed subtree that do not
  exist in the neutral v1 model are preserved only in the adoption backup; they
  do not survive active GC ownership and should be called out in the adoption
  warning text

### Security and file permissions

Projected runtime files may contain expanded secrets from `env` or `headers`.

Rules:

- created and rewritten managed MCP files are normalized to `0600`
- writes use atomic temp-file creation with `0600` before rename; there is no
  write-then-chmod window on the final path
- failure to write or tighten permissions is a hard error
- if the managed target path already exists as a symlink, projection hard-errors
  instead of following it
- `.gitignore` updates for `.mcp.json`, `.gemini/settings.json`, and
  `.codex/config.toml` are best-effort and local-only
- failed `.gitignore` updates surface as doctor warnings; they are not silent
- diagnostics, doctor output, equality failures, and drift logs never print
  expanded secret values; they identify field names and source paths only

## Runtime Delivery Model

MCP follows the same two-stage structure as skills, but writes provider-native
config instead of skill symlinks.

### Stage 1: scope-root reconcile

At `gc start` and supervisor ticks, Gas City reconciles MCP for every eligible
scope root even if no session is currently running.

This handles:

- initial projection
- ongoing drift correction
- cleanup after catalog removal

Stage-1 and stage-2 both flow through one serialized writer keyed by
`(provider-kind, target path)`. That writer is responsible for:

- shared-target equality validation for that concrete target
- acquiring an OS-level per-target lock so the supervisor and
  `gc internal project-mcp` cannot write the same target concurrently
- atomic temp-write-and-rename
- last-good-state preservation when validation fails
- avoiding concurrent stage-1 vs stage-2 writes to the same target

Each target also carries an unhealthy-state record keyed by the target path and
current failure signature. Repeated ticks that hit the same unresolved conflict
or delivery error do not trigger repeated drains or churn; they preserve the
last good state and continue surfacing the target as unhealthy until the error
changes or clears.

### Stage 2: per-session workdir projection

When the real provider workdir differs from the scope root, Gas City injects a
hidden internal pre-start command:

`gc internal project-mcp --agent <qualified-name> --workdir <path>`

This command resolves the target workdir and projects provider-native MCP there
before the provider starts.

Stage-2 ordering is strict:

1. final session workdir is resolved
2. any user-provided pre-start steps that create that workdir run
3. `gc internal project-mcp` runs against that resolved workdir
4. the provider process starts

Any projection failure aborts the launch before the provider starts.

### Runtime support in v1

MCP v1 hard-errors when effective MCP cannot be delivered.

Supported:

- `tmux`
- `subprocess` only when the provider runs in the scope root and no stage-2
  delivery is required

Unsupported with non-empty effective MCP:

- `subprocess` sessions whose real workdir differs from scope root
- `k8s`
- `acp`
- `hybrid`
- any unresolved runtime topology that cannot receive the provider-native file

This `subprocess` limitation is an implementation reality, not a conceptual MCP
constraint: current subprocess runtime paths do not offer the same host-side
stage-2 delivery hook as tmux. Extending subprocess stage-2 support is valid
follow-up work, but v1 does not pretend it exists.

## Shared Target Validation

Some agents can converge on the same provider-native target file. In that case
Gas City must avoid last-writer-wins behavior.

Rule:

- if two agents would project to the same `(provider-kind, target path)`, their
  fully expanded projected behavioral payloads must be identical
- otherwise startup/reconcile fails with a hard error

Equality is checked after:

1. precedence resolution
2. template expansion
3. relative path resolution
4. provider mapping

`description` is excluded from this equality check.

Lifecycle semantics:

- equality is evaluated for the concrete target context every time that target
  is reconciled, including stage-2 workdir-specific expansion
- if a running target becomes conflicted, Gas City does not mutate or clean up
  the last good projected file; it marks the target unhealthy, blocks affected
  new session starts, and leaves existing sessions on the last good payload
  until the conflict is resolved
- cleanup for an empty effective set happens only when no remaining claimant
  still owns that target

The claimant set for a target is derived from current desired state plus live
session records for concrete workdir targets. Session exit does not immediately
delete provider-native MCP files; cleanup happens only through the serialized
reconcile path after the target is observed to have zero remaining claimants.

## Drift and Restart Semantics

Projected MCP changes participate in session config drift.

Restart-triggering changes include:

- server add/remove/replace
- changed `command`, `args`, `env`, `url`, or `headers`
- changed template expansion result
- changed resolved absolute command path

Fingerprint scope:

- Claude: normalized canonical MCP payload for the `.mcp.json` target
- Gemini: normalized canonical MCP payload for the managed `mcpServers`
  subtree only
- Codex: normalized canonical MCP payload for the managed `mcp_servers`
  subtree only

Unrelated preserved Gemini or Codex settings do not participate in MCP drift.
Drift publication and restart decisions happen only after a target write has
committed successfully and the new canonical payload has been recorded as that
target's last good state.

## CLI and Doctor

### `gc mcp list`

`gc mcp list` becomes projected-only and target-specific.

Rules:

- bare `gc mcp list` is an error
- the error text should be explicit:
  `gc mcp list: projected MCP is target-specific; pass --agent <name> or --session <id>`
- `gc mcp list --agent <name>` works only when the target is fully
  deterministic without a live session
- otherwise users must pass `--session <id>`
- unsupported or undeliverable targets return the same hard error reason the
  runtime would use

`--agent` is non-deterministic and therefore rejected when the effective target
depends on a live session workdir, such as pooled or stage-2-projected
sessions.

Output is a human-readable summary, not a raw config dump. It shows:

- provider family
- target path
- server name
- transport
- command or URL
- args
- header/env key names only

Expanded secret values are redacted by default.

### `gc doctor`

MCP-specific doctor checks are report-only in v1.

Checks cover:

- filename/name mismatches
- duplicate same-layer logical names
- template expansion failures
- invalid transport field combinations
- unsupported provider/runtime delivery
- conflicting projected payloads for a shared target

Doctor and runtime diagnostics must include:

- the offending source file path
- the effective target path
- the winning and losing source layers for shadow/conflict cases
- the relevant import binding or rig binding when applicable
- concrete remediation guidance

They must not include expanded secret values.

## Failure Semantics

Projection is fail-closed.

- config validation failures are hard errors
- provider resolution failures for agents with effective MCP are hard errors
- unsupported provider families with effective MCP are hard errors
- undeliverable runtime/workdir topologies with effective MCP are hard errors
- projection write or permission failures are hard errors

There is no best-effort warning mode once effective MCP exists.

## Implementation Shape

The implementation should land as one coherent vertical slice:

1. neutral parser, validator, and effective model
2. discovery and precedence across city, rig, import, implicit, and bootstrap
   layers
3. provider emitters for Claude, Codex, and Gemini
4. stage-1 and stage-2 projection hooks
5. shared-target validation, doctor checks, drift fingerprints, and CLI updates
6. docs updates replacing the old "list-only / later slice" language

The detailed build order lives in the companion implementation plan.
