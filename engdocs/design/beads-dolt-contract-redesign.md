---
title: "Beads And Dolt Contract Redesign"
---

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-04-11 |
| Author(s) | Codex |
| Issue | Historical input: [#245](https://github.com/gastownhall/gascity/issues/245), [#506](https://github.com/gastownhall/gascity/issues/506), [#525](https://github.com/gastownhall/gascity/issues/525), [#541](https://github.com/gastownhall/gascity/issues/541), [#560](https://github.com/gastownhall/gascity/issues/560), [#561](https://github.com/gastownhall/gascity/issues/561) |
| Supersedes | N/A |

## Summary

Gas City's current `bd` plus Dolt integration has too many competing
authorities. Runtime state, `.beads/metadata.json`, `.beads/config.yaml`,
`.beads/dolt-server.port`, deprecated `city.toml` endpoint fields,
process-global env mutation, and K8s-specific file patching all
participate in observable behavior. The result is not one bug; it is a
contract drift problem.

This design narrows the contract to a small number of explicit canonical
surfaces, makes topology transitions owned operations instead of file
editing conventions, and separates server targeting from database
identity so one city can run one managed Dolt server while still serving
one logical database per scope.

The design also keeps non-`bd` providers first-class:

- `file` becomes multi-rig aware through one local store file per scope.
- `exec:` becomes multi-rig aware through an explicit GC-native
  store-target env contract.
- Dolt-specific concepts remain `bd`-only.

## Decision Log

The primary decisions frozen by this document are:

1. Gas City owns the Dolt lifecycle for managed-local `bd` cities.
2. The default `bd` topology is one Dolt server per city and one logical
   Dolt database per scope on that server.
3. City HQ defaults to database `hq`. New rig databases default from the
   rig's beads prefix. Existing tracked database identities are adopted
   and pinned instead of rewritten.
4. Endpoint ownership and database identity are separate concepts.
5. City endpoint origin is either `managed_city` or `city_canonical`.
6. Rig endpoint origin is either `inherited_city` or `explicit`.
7. Rigs inherit the city endpoint by default. A rig may explicitly point
   at a different external endpoint. No rig may independently become a
   second managed-local server.
8. `bd`-specific canonical state lives under `.beads/`:
   `.beads/metadata.json`, `.beads/config.yaml`, and local secrets in
   `.beads/.env`.
9. Provider-neutral scope identity stays in GC config:
   scope root plus pinned beads prefix.
10. Startup may only normalize verification status, deterministic
    GC-owned field backfills, and compatibility mirrors. It must not
    rewrite topology fan-out, authoritative endpoint declarations, or
    database identity.

## Problem Statement

### Current drift

Today the same `bd` plus Dolt target is reconstructed in several places:

- `cmd/gc/bd_env.go`
- `cmd/gc/template_resolve.go`
- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/cmd_bd.go`
- `cmd/gc/work_query_probe.go`
- `internal/api/convoy_sql.go`
- `internal/doctor/checks.go`
- `internal/runtime/k8s/provider.go`
- `cmd/gc/gc-beads-bd`

Those paths do not all read the same sources or apply the same
precedence rules. Several of them also mutate process-global env or
patch backend files in place.

### Historical failure input

This redesign is explicitly grounded in the current GitHub failure
history:

- [#245](https://github.com/gastownhall/gascity/issues/245):
  `GC_DOLT_PORT` versus `BEADS_DOLT_PORT` mismatch
- [#506](https://github.com/gastownhall/gascity/issues/506):
  `gc doctor` fails to propagate Dolt port to `bd` subprocesses
- [#525](https://github.com/gastownhall/gascity/issues/525):
  port drift and stale runtime state break `bd` connectivity
- [#541](https://github.com/gastownhall/gascity/issues/541):
  environment sanitization leaks stale `BEADS_*` state
- [#560](https://github.com/gastownhall/gascity/issues/560):
  duplicate lifecycle actions cause Dolt restart races
- [#561](https://github.com/gastownhall/gascity/issues/561):
  bootstrap sync leaves unusable `.beads` state in fresh worktrees

These are all manifestations of the same root problem: multiple
observable control planes for the same store and server target.

## Goals

- Define one explicit contract for `bd` plus Dolt scope resolution.
- Separate endpoint ownership from database identity.
- Make topology transitions explicit GC operations.
- Keep raw `bd` usable from each local scope.
- Make `file` and `exec:` providers multi-rig aware without forcing
  Dolt semantics onto them.
- Replace hidden fallback chains with typed validation and explicit
  fix paths.
- Tie historical bug classes to specific invariants and tests.

## Non-goals

- Supporting more than one managed-local Dolt server per city
- Giving `file` or `exec:` providers Dolt/database semantics
- Automatically renaming databases when prefixes change
- Treating manual file edits as first-class topology operations
- Keeping `.beads/dolt-server.port`, deprecated `city.toml` endpoint
  fields, or legacy metadata endpoint keys as authorities
- Redesigning the whole provider, session, or K8s architecture beyond
  the interfaces touched by this contract

## Upstream Alignment

This design intentionally aligns with upstream behavior in some places
and diverges in others.

### Aligned with upstream `gastown`

Gas City follows upstream Gastown's logical topology:

- one Dolt SQL server per town/city
- one logical database per scope
- fixed HQ database name `hq`
- rig database names derived from rig prefixes by default

Upstream references:

- `docs/design/architecture.md` in `gastown`
- Gastown's `doltserver.go` implementation

### Aligned with upstream `beads`

Gas City keeps `.beads/metadata.json`, `.beads/config.yaml`, and
`.beads/.env` as the local `bd`-facing contract surfaces and keeps raw
`bd` usable from a local scope.

### Intentional divergences

Gas City intentionally does not let upstream beads lifecycle artifacts
compete with its managed runtime publication:

- `.beads/dolt-server.port` is compatibility-only, not canonical
- managed runtime endpoint publication comes from GC runtime state
- topology transitions are GC-owned operations, not implicit file edits
- K8s and other adapters project env instead of patching canonical files

## Current Competing Authorities

The current state has too many independent or partially overlapping
authorities:

| Concern | Current competing authorities |
|---|---|
| Managed runtime endpoint | `.gc/runtime/.../dolt-state.json`, `.beads/dolt-server.port`, `GC_DOLT_PORT`, `BEADS_DOLT_SERVER_PORT`, reachability heuristics |
| Database identity | `.beads/metadata.json`, derived prefix defaults, historical metadata preservation logic |
| Endpoint ownership | city config, rig config, env overrides, K8s-specific file mutation |
| Raw `bd` compatibility | `.beads/config.yaml`, env projection, process-global mutation |
| Secrets and auth | process env, `.beads/.env`, beads credentials file, duplicated `BEADS_*` projection |

This design collapses each of those concerns onto one canonical source
plus an explicit compatibility layer.

## Proposed Contract

### Provider-neutral core

Every provider resolves a provider-neutral declared store target:

```go
type DeclaredStoreTarget struct {
    Provider      string // bd, file, exec:...
    ScopeRoot     string // city root or rig root
    ScopeKind     string // city or rig
    ScopeName     string // workspace name or rig name
    StoreIdentity DeclaredStoreIdentity
    Bootstrap     BootstrapState
    BD            *DeclaredBDTarget // nil for non-bd providers
}

type DeclaredStoreIdentity struct {
    ScopePrefix string
}
```

Provider-neutral identity is:

- scope kind
- scope root path
- pinned routing prefix

Provider-specific identity layers on top of that core.

### `bd`-specific extension

`bd` targets carry two separate identity layers:

```go
type DeclaredBDTarget struct {
    StoreRoot         string
    ServerTarget      ResolvedServerTarget
    DatabaseIdentity  ResolvedDatabaseIdentity
    EndpointOrigin    EndpointOrigin
    EndpointStatus    EndpointStatus
    CanonicalMetadata string // .beads/metadata.json
    CanonicalConfig   string // .beads/config.yaml
}

type ResolvedServerTarget struct {
    Mode         string // managed_local or external
    Host         string
    Port         int
    User         string
    AuthSource   string
    LifecycleGC  bool
}

type ResolvedDatabaseIdentity struct {
    Name     string
    Pinned   bool
    Source   string // metadata, adopted, default
}
```

The essential rule is:

- `ServerTarget` answers "which Dolt server do I talk to?"
- `DatabaseIdentity` answers "which logical database do I use after I
  connect?"

No consumer is allowed to collapse those two concerns again.

### Projection

Consumers do not rediscover topology or secrets. They receive a
projection from the declared target:

```go
type ProjectedConnectionTarget struct {
    StoreRoot string
    Env       map[string]string
}
```

Projection rules:

- all providers get GC-native store-target vars
- only `bd` targets get GC-native Dolt connection vars
- only direct `bd` compatibility adapters emit `BEADS_*`
- no projection may change `DatabaseIdentity`

`ScopePrefix` is provider-neutral identity metadata. For `bd` it is the
source of `issue_prefix` defaults and rig database-name defaults. For
`file` and `exec:` it is an opaque routing key, not a database or
topology authority.

### Resolver and lifecycle ownership invariants

The following are hard invariants of the redesign:

- every startup, doctor, bootstrap, repair, controller, adapter, and
  CLI flow must resolve endpoint, database, and env through one shared
  resolver and projection library
- no path may directly read `.beads/metadata.json`,
  `.beads/config.yaml`, `.beads/dolt-server.port`, deprecated
  `city.toml` Dolt fields, or managed runtime publication to rebuild
  authority outside that resolver
- one city-scoped lifecycle owner is responsible for managed-local Dolt
  `start`, `stop`, and `recover`; all other flows are verify-only or
  must delegate to that owner

Managed-local runtime publication must be atomically replaced and must
carry enough freshness data for readers to reject stale publications.
At minimum the publication contract includes:

- generation or epoch
- instance token
- pid
- host
- port
- started-at timestamp

Readers may treat managed runtime publication as authoritative only when
the generation and instance token are current for the active lifecycle
owner, the published server pid still exists, and the published pid
birth timestamp still matches that process. Otherwise the resolver must
return endpoint unavailable rather than guessing from compatibility
artifacts.

### Lifecycle-owner protocol

Managed-local lifecycle ownership is a first-class runtime contract,
separate from endpoint publication.

Canonical local owner record:

- directory: city-scoped GC runtime directory under
  `.gc/runtime/packs/dolt/`
- owner record path: `.gc/runtime/packs/dolt/dolt-owner.json`
- publication path: `.gc/runtime/packs/dolt/dolt-state.json`
- lifecycle lock path: `.gc/runtime/packs/dolt/dolt-owner.lock`
- lock is separate from the controller lock and separate from the
  topology journal lock
- record fields:
  - schema version
  - owner kind (`controller`, `supervisor-city-runtime`,
    `start-foreground`)
  - owner id
  - owner epoch
  - owner pid
  - current instance token
  - acquired-at timestamp
  - owner pid birth timestamp or equivalent monotonic process-start
    proof

Owner record example:

```json
{
  "version": 1,
  "owner_kind": "controller",
  "owner_id": "city-runtime:gascity",
  "owner_epoch": 42,
  "owner_pid": 81234,
  "owner_pid_birth": "2026-04-11T23:58:14.123456789Z",
  "instance_token": "tok-7f8a",
  "acquired_at": "2026-04-11T23:58:14.223456789Z"
}
```

Protocol:

- acquiring ownership increments the owner epoch and writes the owner
  record before any managed-local `start`, `stop`, or `recover`
- operations that touch both topology and managed-local lifecycle always
  acquire locks in one order only: topology lock first, lifecycle lock
  second
- managed runtime publication must embed the current owner epoch and
  current instance token from the owner record
- managed runtime publication must also embed the server pid birth
  timestamp or equivalent monotonic process-start proof for the Dolt
  server process
- readers validate runtime publication against the owner record, not
  against the publication alone; pid reuse is rejected by comparing the
  published pid birth proof with the live process
- on controller or supervisor restart, the new owner must either:
  - adopt the existing server and republish under the new owner epoch,
    or
  - stop and restart it under the new owner epoch
- non-owner flows such as `gc doctor` and `gc beads endpoint repair`
  must delegate lifecycle mutations through the active owner or fail
  closed; they do not become independent recovery actors
- ordinary startup and `gc doctor` may detect incomplete journals, but
  they do not become independent journal-completion writers; they must
  invoke the owning resume command or delegated owner path instead

Atomic relationship between owner record and publication:

1. acquire `dolt-owner.lock`
2. write `dolt-owner.json` with temp-write, fsync, and atomic rename
3. perform the managed-local lifecycle action
4. write `dolt-state.json` with matching owner epoch and instance token
   using temp-write, fsync, and atomic rename
5. if ownership is being retired or the server is stopped, remove or
   replace `dolt-state.json` before releasing the lifecycle lock

Readers always load `dolt-owner.json` first, then `dolt-state.json`, and
reject the publication unless owner epoch, instance token, pid, and pid
birth proof all agree.

## Canonical Sources Of Truth

### Provider-neutral

These are canonical for every provider:

- city/rig config for scope topology and pinned beads prefix
- scope root path from GC config and rig registry

### `bd`-specific canonical state

These are canonical only for `bd` scopes:

- tracked `.beads/metadata.json`
  - store identity
  - `dolt_database`
- tracked `.beads/config.yaml`
  - GC-owned endpoint topology marker
  - canonical external endpoint defaults
  - policy such as `dolt.auto-start`
- local `.beads/.env`
  - local per-scope secret surface
- beads credentials file
  - optional user-wide secret fallback
- GC runtime state
  - managed-local live endpoint publication only

### Compatibility-only surfaces

These remain observable but are not authoritative:

- deprecated city `dolt_host` / `dolt_port` fields in `city.toml`
- `BEADS_DIR`
- `.beads/dolt-server.port`
- legacy metadata endpoint/auth fields
- ambient process env for endpoint or database selection
- `issue-prefix` as a compatibility alias for `issue_prefix`

The only supported temporary overrides are auth-only execution-context
overrides:

- `GC_DOLT_USER`
- `GC_DOLT_PASSWORD`
- mirrored `BEADS_DOLT_SERVER_USER`
- mirrored `BEADS_DOLT_PASSWORD`
- `BEADS_CREDENTIALS_FILE`

Temporary overrides may affect auth material for the current process
only. They may not change `EndpointOrigin`, `ServerTarget` host, port,
or mode, `DatabaseIdentity`, or any persisted canonical state.

## Auth Resolution

Auth is resolved from the endpoint authority scope, not always from the
current store scope.

Auth scope rules:

- city scope resolves auth from city files and city-local secret inputs
- rig `inherited_city` resolves auth from the city endpoint authority
  scope, not from the rig as an independent auth authority
- rig `explicit` resolves auth from the rig scope

Effective username precedence is fixed:

1. temporary auth-only process override: `GC_DOLT_USER` or mirrored
   `BEADS_DOLT_SERVER_USER`
2. canonical `dolt.user` in the endpoint authority scope's
   `.beads/config.yaml`
3. implicit default `root`

Effective password precedence is fixed:

1. temporary auth-only process override: `GC_DOLT_PASSWORD` or mirrored
   `BEADS_DOLT_PASSWORD`
2. `.beads/.env` in the endpoint authority scope
3. beads credentials file selected by `BEADS_CREDENTIALS_FILE` or its
   default location
4. empty password

Rules:

- `dolt.user` is canonical external endpoint-default username for the
  endpoint authority scope; it is not a secret
- managed-local city scopes and rigs inheriting managed-local topology
  normally omit `dolt.user` and resolve to `root` unless a temporary
  auth-only override is active
- GC does not auto-mirror secrets across inherited rig scopes; raw `bd`
  on inherited rigs relies on the local compatibility files plus either
  the credentials file or explicit local operator-provided secret state
- `BEADS_CREDENTIALS_FILE` is an explicit part of this redesign's auth
  contract, not just an upstream reference; resolver, projection, and
  `bd` compatibility tests must cover custom credentials-file paths
- `AuthSource` in `ResolvedServerTarget` must identify both the source
  kind and the auth scope used to resolve it
- verification grouping and cache fingerprints use the resolved auth
  source kind, effective username, credentials-file path when used, and
  auth-scope root when `.beads/.env` is used
- projected `BEADS_DOLT_PASSWORD` for direct `bd` compatibility is
  derived once from this precedence order and not rediscovered ad hoc by
  downstream callers

## Provider-Specific Identity

### `bd`

`bd` store identity is:

- provider-neutral identity
- pinned `dolt_database`
- endpoint origin and verification state

### `file`

`file` provider has no Dolt/database semantics.

Per-scope local store artifact:

- `scope_root/.gc/beads.json`

The store file is persistence, not additional canonical topology.

### `exec:`

`exec:` provider has no Dolt/database semantics unless the external
implementation chooses to implement them privately.

GC guarantees only:

- scope root
- scope kind
- pinned beads prefix
- city/rig context

Provider-specific persistence layout remains entirely implementation
defined.

## `bd` Topology Model

### Physical topology

- one Dolt server per city
- city may be GC-managed (`managed_local`) or external
- rigs inherit the city endpoint by default
- a rig may explicitly override to its own external endpoint
- no rig may independently become a second managed-local server

### Logical topology

- one logical Dolt database per scope on the server target for that scope
- city HQ default database: `hq`
- new rig default database: derived from rig beads prefix
- tracked database identity is pinned after creation or adoption
- changing a prefix does not rename a database automatically

### Uniqueness and reserved names

- database identity is unique per resolved endpoint
- the same database name may exist on different external endpoints
- `hq` is reserved for the city scope
- system names such as `information_schema`, `mysql`,
  `performance_schema`, and `sys` are invalid as pinned scope databases
- names use the current `gc-beads-bd` SQL-safe subset:
  alphanumeric, hyphen, underscore

## `bd` Canonical State Machine

### City scope

Allowed city endpoint origins:

- `managed_city`
- `city_canonical`

### Rig scope

Allowed rig endpoint origins:

- `inherited_city`
- `explicit`

### Endpoint status

Canonical status values:

- `verified`
- `unverified`

Rules:

- `gc.endpoint_status` records last-known verification of the canonical
  endpoint declaration; it is not live runtime health
- `managed_city` is always `verified` because its topology is GC-owned
  and does not depend on external endpoint verification
- forced or adopted external topology may start as `unverified`
- ordinary validation may auto-promote `unverified -> verified`
- failed validation of an `unverified` external target does not invent a
  third state; it remains `unverified` and yields a typed startup or
  doctor failure
- no other status values are valid

Live health is reported separately:

- managed-local live health comes from runtime publication plus
  reachability and ownership checks
- external live health comes from endpoint verification results and
  `gc doctor` output
- no persisted canonical field doubles as live availability state

### Rig inheritance semantics

`inherited_city` means:

- if city origin is `managed_city`, the rig carries no tracked
  `dolt.host` / `dolt.port`
- if city origin is `city_canonical`, the rig mirrors the city's tracked
  external endpoint defaults locally so raw `bd` works from that rig

For GC resolution, the city remains the sole endpoint authority for an
`inherited_city` rig. Any rig-local mirrored `dolt.host` /
`dolt.port` fields are deterministic mirror fields for raw `bd`
interoperability and are never read by the GC resolver as independent
authority.

Those mirrored fields are canonical on-disk shape for raw `bd`
interoperability, but they are not canonical GC endpoint authority.
Only the canonical-file package and topology-migration code may parse or
rewrite them directly. Resolver, startup, doctor, controller, K8s,
convoy, work-query, and CLI helper code must obtain inherited rig
endpoint data only from the city-derived `ResolvedServerTarget` exposed
by the resolver. Validation code may compare rig-local mirrored values
against that derived city target, but it may not promote the rig-local
values to scope-local truth.

`explicit` means:

- the rig carries its own canonical external endpoint defaults
- city endpoint changes do not rewrite the rig

### Legacy origin derivation

When a legacy scope is missing `gc.endpoint_origin` but GC can still
derive topology deterministically, migration preflight derives origin in
this order:

1. if the city resolves to the managed-local city server, city origin is
   `managed_city` and rig origin is `inherited_city`
2. else if the scope is city and has a canonical external endpoint,
   origin is `city_canonical`
3. else if the scope is a rig with an explicit canonical external
   override, origin is `explicit`
4. else the rig inherits the city external endpoint and origin is
   `inherited_city`

If those rules still leave a scope ambiguous, migration preflight fails
and GC requires an explicit owning command rather than guessing.

This derivation logic is transitional. After the migration rollout is
complete, steady-state resolver code for canonical scopes must not keep
legacy derivation on the hot path.

## Canonical File Schemas

### `.beads/metadata.json`

Canonical keys owned by GC:

- `database`
- `backend`
- `dolt_mode`
- `dolt_database`

GC treats metadata as identity-only. Endpoint and auth keys are not
canonical and should be scrubbed during migration and repair.

### `.beads/config.yaml`

Canonical keys owned by GC:

- `issue_prefix`
- `dolt.auto-start`
- `gc.endpoint_origin`
- `gc.endpoint_status`
- external-only:
  - `dolt.host`
  - `dolt.port`
  - optional `dolt.user`

Compatibility-only mirror:

- `issue-prefix`

No other keys are canonical for GC's `bd` contract.

Writers must preserve unknown non-GC keys unless a key is explicitly in
the documented scrub list for deprecated endpoint or auth authority.
Round-tripping unknown upstream `bd` keys is required for
interoperability.

## Canonical File Examples

### City managed-local

`.beads/metadata.json`

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "hq"
}
```

`.beads/config.yaml`

```yaml
issue_prefix: gc
issue-prefix: gc
dolt.auto-start: false
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
```

### City external

`.beads/metadata.json`

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "hq"
}
```

`.beads/config.yaml`

```yaml
issue_prefix: gc
issue-prefix: gc
dolt.auto-start: false
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: root
```

### Rig inheriting managed city

`.beads/metadata.json`

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "fe"
}
```

`.beads/config.yaml`

```yaml
issue_prefix: fe
issue-prefix: fe
dolt.auto-start: false
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
```

### Rig inheriting external city

`.beads/metadata.json`

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "fe"
}
```

`.beads/config.yaml`

```yaml
issue_prefix: fe
issue-prefix: fe
dolt.auto-start: false
gc.endpoint_origin: inherited_city
gc.endpoint_status: verified
dolt.host: db.example.com
dolt.port: 3307
dolt.user: root
```

### Rig explicit external

`.beads/metadata.json`

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "fe"
}
```

`.beads/config.yaml`

```yaml
issue_prefix: fe
issue-prefix: fe
dolt.auto-start: false
gc.endpoint_origin: explicit
gc.endpoint_status: unverified
dolt.host: rig-db.example.com
dolt.port: 3307
dolt.user: agent
```

### Legacy adopted city with non-default database

```json
{
  "database": "dolt",
  "backend": "dolt",
  "dolt_mode": "server",
  "dolt_database": "gascity"
}
```

This remains valid for adopted legacy cities. `hq` is the default for
new cities, not a forced rewrite of existing tracked identity.

## Store-Target Projection

### Universal GC-native store vars

Every provider receives this finite, normative store-target contract:

| Variable | City scope | Rig scope | Meaning |
|---|---|---|---|
| `GC_STORE_ROOT` | required | required | canonical scope root for persistence |
| `GC_STORE_SCOPE` | `city` | `rig` | scope discriminator |
| `GC_BEADS_PREFIX` | required | required | stable routing prefix, treated as opaque by non-`bd` providers |
| `GC_CITY` | required | required | city name |
| `GC_RIG` | unset | required | rig name |
| `GC_RIG_ROOT` | unset | required | rig root path |
| `GC_PROVIDER` | required | required | resolved provider name |

No other provider-neutral vars are part of the contract.

Ambient process env is not authoritative for non-`bd` providers.
Provider adapters may pass through unrelated shell env, but the
provider-neutral contract above is the only guaranteed GC interface.

### GC-native connection vars

GC-native `bd` consumers use:

- `GC_DOLT_HOST`
- `GC_DOLT_PORT`
- `GC_DOLT_USER`
- `GC_DOLT_PASSWORD`

Non-`bd` providers receive only the universal store-target contract.

### `exec:` provider contract

`exec:` providers are invoked with a sanitized GC-native contract.

Guaranteed inputs:

- the finite provider-neutral vars listed above
- provider-specific invocation arguments defined by the exec provider
  protocol

Forbidden legacy inputs:

- `BEADS_*`
- `GC_DOLT_*`
- deprecated `city.toml` Dolt fields by env projection

Conformance rules:

- `exec:` scripts must isolate persistence by `GC_STORE_ROOT`
- `GC_BEADS_PREFIX` is routing metadata, not a persistence root
- ambient shell env may exist, but `exec:` correctness may not depend on
  any env outside the finite GC-native contract and the script's own
  documented inputs
- non-`bd` providers must never branch on `DeclaredBDTarget` or any
  Dolt-derived field once provider selection is complete

### `bd` compatibility vars

Only the `bd` adapter emits:

- `BEADS_DIR`
- `BEADS_DOLT_SERVER_HOST`
- `BEADS_DOLT_SERVER_PORT`
- `BEADS_DOLT_SERVER_USER`
- `BEADS_DOLT_PASSWORD`
- `BEADS_DOLT_AUTO_START=0`

The `bd` adapter derives those from the universal store-target contract
and the resolved connection target. GC core logic should no longer think
in terms of `BEADS_DIR`.

## Owning Operations

Topology state is owned by explicit GC operations, not by manual file
editing.

Canonical operations:

- city use-managed
- city use-external
- set rig inherit
- set rig external
- explicit database migration/rename
- explicit endpoint repair/create

Manual edits that attempt to simulate those operations are treated as
drift and should be rejected with the exact owning command needed to
reconcile.

### Command surface

The owning CLI must be explicit in this design, not deferred to
implementation:

| Operation | Command shape | Notes |
|---|---|---|
| migrate legacy contract | `gc beads migrate-contract [--city] [--rig <rig>] [--all] [--dry-run]` | materializes canonical files and records migration progress |
| city managed | `gc beads city use-managed [--dry-run]` | adopts or initializes managed-local city topology |
| city external | `gc beads city use-external --host <host> --port <port> [--user <user>] [--adopt-unverified] [--dry-run]` | validates before write unless `--adopt-unverified` |
| managed-local recovery | `gc beads city recover [--dry-run]` | the only operator-facing command that may request managed-local Dolt adopt or restart |
| rig inherit | `gc rig set-endpoint <rig> --inherit [--dry-run]` | rewrites the rig into derived inherited shape |
| rig explicit external | `gc rig set-endpoint <rig> --external --host <host> --port <port> [--user <user>] [--adopt-unverified] [--dry-run]` | validates before write unless `--adopt-unverified` |
| endpoint repair | `gc beads endpoint repair (--city | --rig <rig>) [--create-missing-databases] [--dry-run]` | verify-only by default; explicit scope required; may create only when explicitly requested |
| database migration | `gc beads database rename (--city | --rig <rig>) --to <database> [--dry-run]` | the only command that may rewrite `dolt_database`; explicit scope required |
| resume interrupted operation | `gc beads resume --op <operation-id> [--dry-run]` | resumes a journaled migration or topology operation using the recorded pre-state hashes |

CLI UX rules:

- owning commands are non-interactive by default and must fail with
  actionable errors instead of prompting
- rerunning an owning topology command against an already-canonical
  target state is idempotent verification, not an error:
  - if authoritative state already matches the requested target, the
    command exits cleanly after verification
  - if only derived fields or `unverified -> verified` status need
    normalization, the command may normalize them under the same lock and
    journal discipline
  - if authoritative state contradicts the requested target, the command
    fails with a drift error and prints the exact reconcile command
- `--dry-run` prints detected state, intended state, files to change,
  endpoint groups affected, and server-side provisioning actions
- `--adopt-unverified` must print a warning naming the
  unresolved endpoint and the exact follow-up command to verify or
  repair it
- every topology or validation error must include detected state,
  expected state, and one exact owning command to reconcile
- fanout operations such as `gc beads city use-external` must show in
  `--dry-run`:
  - affected inherited rigs and count
  - blocked scopes with local conflicting edits
  - files to be rewritten
  - database provisioning actions, if any
- `--adopt-unverified` warnings must include:
  - resolved endpoint
  - auth source kind
  - scopes affected
  - verification skipped or failed
  - persisted `gc.endpoint_status` value
  - exact follow-up verify or repair command
- successful `--adopt-unverified` writes must restate:
  - persisted endpoint origin and endpoint
  - affected scopes
  - persisted `gc.endpoint_status`
  - exact follow-up command to verify or repair
- interrupted journal states must always surface one exact resume
  command using the recorded operation id
- managed-local endpoint failures without an incomplete journal always
  surface `gc beads city recover` as the recovery command

Example forced-adoption success output:

```text
UPDATED: city endpoint recorded without live validation
  origin: city_canonical
  endpoint: db.example.com:3307 user=root
  scopes: city, rig fe, rig ml
  persisted endpoint_status: unverified
  next: gc beads endpoint repair --city
```

Managed-local recovery output contract:

- success output must include:
  - action: `adopted-existing`, `restarted`, `delegated-to-owner`, or
    `already-healthy`
  - owner id and owner epoch
  - resulting runtime publication endpoint
  - scopes restored
  - exact follow-up command when additional repair is still required
- failure output must include:
  - detected failure class
  - whether delegation was attempted
  - exact next command, which is either `gc beads resume --op <id>` when
    a journal blocks recovery or `gc beads city recover` when another
    retry is appropriate

Example managed-local recovery success output:

```text
RECOVERED: managed city runtime restored
  action: restarted
  owner: controller city-runtime:gascity epoch=43
  endpoint: 127.0.0.1:3311 user=root
  scopes: city, rig fe, rig ml
  next: none
```

## Transition Table

| Operation | Allowed pre-state | Post-state | Files changed | Validation / provisioning |
|---|---|---|---|---|
| city use-managed | uninitialized, managed legacy, or existing `city_canonical` | city `managed_city` + `verified` | city `.beads/metadata.json`, city `.beads/config.yaml`, inherited rig mirrors when converting from external | acquire lifecycle ownership, start or adopt the managed server, publish runtime state, create/verify required databases |
| city use-external | uninitialized, external legacy, or existing `managed_city` | city `city_canonical` + `verified|unverified` | city `.beads/metadata.json`, city `.beads/config.yaml`, inherited rig mirrors, managed runtime publication retirement when converting from managed | validate endpoint first unless `--adopt-unverified`; create databases only in explicit bootstrap flows |
| set rig inherit | rig `explicit` or legacy | rig `inherited_city` | rig `.beads/config.yaml` | derive local endpoint shape from current city origin |
| set rig external | rig `inherited_city` or legacy | rig `explicit` + `verified|unverified` | rig `.beads/config.yaml` | validate endpoint first unless `--adopt-unverified`; create/verify rig database if requested |
| database migration/rename | pinned DB exists | new pinned DB identity | `.beads/metadata.json`, optional backend files | explicit migration only |
| endpoint repair/create | canonical topology already valid | same topology, repaired server inventory | none by default | endpoint-scoped create/repair only |

Rules:

- topology operations may update all affected canonical files atomically
- topology operations never rewrite `dolt_database` unless the operation
  is explicitly a database migration
- external operations may support `--adopt-unverified`, which records
  canonical state with `gc.endpoint_status: unverified`

City-origin flip rules:

- `gc beads city use-external` is a real city-origin transition, not an
  init-only command. When converting from `managed_city`, it acquires the
  topology lock, then the lifecycle lock, validates or adopts the target
  external endpoint, retires the managed runtime publication, writes the
  new city canonical files, rewrites inherited rig mirrors, and then
  releases managed lifecycle ownership.
- `gc beads city use-managed` is also a real city-origin transition.
  When converting from `city_canonical`, it acquires the topology lock,
  then the lifecycle lock, writes the owner record, starts or adopts the
  managed server, publishes managed runtime state, writes the new city
  canonical files, strips inherited external endpoint mirrors from rigs,
  and verifies required databases on the managed server.
- if either city-origin flip is interrupted, `gc doctor` must surface
  the recorded `gc beads resume --op <id>` command as the single first
  command, and managed-local recovery remains suppressed until the
  journal is resolved.

## Crash-safe transition protocol

Topology and migration writes must be crash-safe and resumable.

Rules:

- every topology or migration operation acquires a city-scoped topology
  lock before mutating canonical files
- every write to canonical tracked files under `.beads/metadata.json`
  or `.beads/config.yaml`, including startup normalization and
  compatibility-alias maintenance inside those files, must acquire that
  same topology lock and must fail closed while an incomplete
  topology/migration journal exists for the affected city
- every canonical file write uses temp-write, fsync, and atomic rename
- multi-file operations record a local operation journal under `.gc/`
  before the first canonical write and clear it only after post-write
  verification succeeds
- the journal records operation id, command, target city, affected
  scopes, step ordering, and any pending inherited-rig mirror rewrites
- the journal snapshots hashes for all canonical files that will be
  touched and for every provider-neutral GC config input that affects
  scope identity or routing, including city config, rig registry entry,
  scope roots, and pinned prefixes
- startup and `gc doctor` must detect an incomplete journal and either:
  - stop with a named `migration_incomplete` or `topology_incomplete`
    error, or
  - delegate to the active owner only when that owner is already
    executing the same recorded operation
- interrupted journals are resumed by `gc beads resume --op <id>`
- the journal itself records the operation id, owning command, and
  resume command text so startup and `gc doctor` can print it verbatim

Write ordering:

1. validate pre-state and snapshot current canonical files
2. write city canonical files first
3. write affected rig canonical files second
4. run post-write verification
5. clear journal and record success

Inherited-rig mirror rewrites are not independent topology changes.
They are derived completion work for an already-authoritative city
transition. Only the owning topology command or `gc beads resume --op
<id>` may write those remaining rig mirrors. Startup and `gc doctor`
may report missing fan-out and print the exact resume or owning command,
but they never finish the writes themselves.

Migration cutoff rule:

- once a scope has canonical `.beads/config.yaml` with
  `gc.endpoint_origin` and `gc.endpoint_status`, deprecated endpoint
  authorities for that scope are ignored for normal resolution
- deprecated surfaces may still be read only for diagnostics and
  migration reporting
- on canonical scopes, leftover deprecated endpoint and auth fields are
  warning-only diagnostics, not blocking contradictions, even when their
  values differ from canonical state
- contradictory deprecated fields are hard errors only during migration
  preflight for non-canonical scopes, where they still participate in
  intent derivation
- mixed old and new binaries against migrated `bd` scopes are
  unsupported; rollout must upgrade binaries before running migration
- journal resume must verify pre-state hashes before writing any
  remaining steps; if tracked files changed since the journal was
  created, resume fails closed as a conflict rather than guessing
- if provider-neutral GC config changed after journal creation, such as
  scope root movement, rig registry changes, or pinned prefix edits,
  resume fails closed as a conflict rather than re-deriving a new fanout

## Validation Rules

### Provider-neutral

- every configured provider must support distinct city and rig stores
- prefixes are provider-neutral routing identities
- prefixes must be unique within the city

### `bd` topology

- city origin must be `managed_city` or `city_canonical`
- rig origin must be `inherited_city` or `explicit`
- `city_canonical` is valid only for city scope
- `inherited_city` is valid only for rig scope
- there may be at most one managed-local server per city

### `bd` file invariants

- `managed_city` must not track `dolt.host` / `dolt.port`
- `city_canonical` must track `dolt.host` / `dolt.port`
- `explicit` must track `dolt.host` / `dolt.port`
- `inherited_city` must match the derived local shape for the current
  city origin
- `issue_prefix` is the only canonical prefix spelling
- `issue-prefix` is compatibility-only

### Drift and contradiction

Examples of hard errors:

- manual topology flip by file editing alone
- `explicit` without endpoint defaults
- `managed_city` with tracked host/port
- rig database identity duplicate on the same resolved endpoint
- invalid reserved-name combination such as rig database `hq`
- manual `dolt_database` edit that changes pinned identity without an
  explicit database migration operation
- contradictory deprecated endpoint fields on a non-canonical legacy
  scope where migration preflight still needs them to derive intent

## Verification And Repair Model

### Endpoint grouping

Verification groups scopes by resolved endpoint identity:

- effective host
- effective port
- effective user
- auth-source fingerprint
- mode

The cache key must not contain secrets. It may include:

- auth source kind
- effective username
- credentials-file path
- scope-root path for `.beads/.env`

Different auth sources against the same host and port are different
endpoint identities for verification and caching purposes.

Auth-only temporary overrides participate in auth resolution and cache
keys, but they do not change endpoint selection.

### Verification semantics

For each endpoint group:

1. verify the endpoint is reachable with the resolved auth
2. verify each required pinned database by actual `USE <db>` success,
   not just catalog presence
3. report endpoint-level failures separately from scope-level missing
   database failures

### Creation and repair

- ordinary startup only verifies
- explicit bootstrap or repair flows may create missing databases
- external database creation requires canonical tracked external config,
  not transient env overrides
- repair refuses to proceed while topology or identity drift remains
  unresolved

## Startup Write Budget

Ordinary startup may auto-write only:

- `unverified -> verified` after successful validation
- missing GC-owned field backfills on already-canonical scopes when the
  canonical file already exists and the backfill does not constitute
  first-write canonicalization
- compatibility mirror maintenance when required

Ordinary startup may not auto-write:

- inherited-rig endpoint mirror fan-out
- topology transitions
- explicit endpoint declarations
- pinned database identity
- any ambiguous merge over existing unrelated local file changes

Allowed startup writes must be:

- serialized under the same city topology lock used by owning topology
  and migration commands
- preceded by a re-check that no incomplete topology or migration
  journal exists for the affected city
- logged explicitly
- surfaced in `gc doctor` when practical
- left uncommitted in git

## `gc doctor`

`gc doctor` should report:

- endpoint-origin drift
- endpoint reachability failures
- database-identity failures
- endpoint-grouped summaries
- migration warnings for deprecated compat fields
- recent or last-known normalization details when available

It should validate per-rig independent connection targets only for
explicit external rigs. Inherited rigs are checked against the city
endpoint plus their scope-local database identity.

`gc doctor --fix` is explicitly out of scope for the `bd` topology and
Dolt contract redesign. In the redesigned model it may remain only for
non-`bd` hygiene checks such as worktree cleanup or cache
re-materialization. It must never mutate:

- `.beads/metadata.json`
- `.beads/config.yaml`
- topology journals
- lifecycle-owner records
- managed runtime publication
- endpoint ownership
- pinned database identity

For any `bd`/Dolt topology, endpoint, lifecycle, or database finding,
`gc doctor` prints the exact owning command and does not mutate state
itself.

When `gc doctor --fix` is invoked while `bd`/Dolt findings are present,
it may still run unrelated non-`bd` hygiene fixes, but it must print
that all `bd`/Dolt findings were skipped and remain owned by their
explicit commands.

### Command selection precedence

When multiple findings coexist, `gc doctor`, startup, and owning command
preflight must select the same first command in this order:

| Priority | Condition | First command | Notes |
|---|---|---|---|
| 1 | `topology_incomplete` or `migration_incomplete` journal exists | `gc beads resume --op <id>` | blocks all lower-priority fixes for scopes covered by the journal |
| 2 | contradictory canonical topology or manual topology edit | owning topology command for the affected scope | examples: `gc beads city use-managed`, `gc beads city use-external ...`, `gc rig set-endpoint <rig> --inherit`, `gc rig set-endpoint <rig> --external ...` |
| 3 | managed city unavailable and no incomplete journal exists | `gc beads city recover` | highest-priority managed-local runtime fix |
| 4 | inherited-rig mirror drift with no journal and canonical city topology otherwise valid | `gc rig set-endpoint <rig> --inherit` | rewrites the rig back to derived inherited shape |
| 5 | external endpoint unreachable, auth failure, or canonical external endpoint still `unverified` | `gc beads endpoint repair [--city | --rig <rig>]` | verifies endpoint and may guide follow-on repair |
| 6 | endpoint reachable but pinned databases missing | `gc beads endpoint repair [--city | --rig <rig>] --create-missing-databases` | only after endpoint validation succeeds |
| 7 | compatibility-only warnings | no owning repair command required | cleanup hints only |

Precedence rules:

- incomplete journals always outrank managed-local recovery, endpoint
  repair, and mirror drift fixes on affected scopes
- city-scope topology contradictions outrank inherited-rig mirror drift
  under that city
- endpoint-level failures suppress missing-database repair suggestions
  for scopes on the same endpoint until the endpoint is reachable
- when multiple scopes share one failing endpoint, the grouped endpoint
  command is shown first and per-scope database commands are suppressed
  until endpoint verification succeeds
- if a city external transition is partially blocked by conflicting rig
  edits, the first command remains the original city command or journal
  resume when one exists; blocked rigs are listed as blockers, not as
  competing first-fix commands

### Output contract

`gc doctor` output must be structured, not ad hoc. At minimum it prints:

1. topology drift findings
2. endpoint-grouped reachability and auth findings
3. per-scope database identity findings
4. compatibility and migration warnings
5. last normalization summary when canonical files were auto-written

Default `gc doctor` output is a stable human-readable contract. This
design does not require scraping prose for automation; if machine-stable
output is later added, it must ship behind an explicit structured mode
such as `--json` rather than changing the human output contract.

Finding ordering and suppression rules are part of the contract:

1. incomplete journal findings
2. contradictory topology findings
3. managed-local recovery findings
4. endpoint-group findings
5. per-scope database findings for already-validated endpoints
6. compatibility warnings
7. last-normalization details

Suppression rules:

- an incomplete journal suppresses lower-priority fixes for the scopes it
  covers and must print one exact resume command
- a topology contradiction suppresses lower-priority endpoint and
  database fixes for the same scope until topology is reconciled
- an endpoint failure suppresses missing-database fixes for scopes on
  that endpoint
- inherited-rig mirror drift under a city topology contradiction is
  reported as blocked detail under the city finding, not as an
  additional first-fix command

Exit codes:

- `0`: healthy, no actionable findings
- `10`: warnings only, including compatibility warnings or reachable but
  still `unverified` external endpoints
- `20`: repairable failures such as endpoint unreachable, auth failure,
  missing database, or deterministic drift with an owning command
- `30`: hard-stop states such as `topology_incomplete`,
  `migration_incomplete`, contradictory canonical files, or impossible
  origin or status combinations

Every actionable failure line must include one exact command.

Example shape:

```text
TOPOLOGY: rig fe is explicit but missing dolt.host
  detected: explicit without external endpoint defaults
  expected: explicit with host, port, and optional user
  fix: gc rig set-endpoint fe --inherit

ENDPOINT: db.example.com:3307 user=root [external, verified]
  reachability: ok
  missing databases: fe
  repair: gc beads endpoint repair --rig fe --create-missing-databases

COMPAT: city.toml [dolt].host still present for city scope
  status: ignored because canonical .beads/config.yaml exists
  cleanup: remove deprecated city.toml fields

TOPOLOGY: migration_incomplete operation=op-1234
  detected: interrupted city external adoption before rig mirror sync completed
  resume: gc beads resume --op op-1234
  conflicts: none

ENDPOINT: managed_city unavailable
  owner: missing or stale
  publication: stale runtime publication rejected
  recover: gc beads city recover
```

`gc doctor --last-normalization` should print the last allowed startup
normalization writes so operators can tell when canonical files changed
without an explicit topology command.

### Additional output examples

Mixed managed-local failure with one exact first command:

```text
TOPOLOGY: city canonical files consistent

ENDPOINT: managed_city unavailable
  owner: missing or stale
  publication: stale runtime publication rejected
  blocked scopes: city hq, rig fe, rig ml
  recover: gc beads city recover

DATABASE: suppressed until managed city endpoint is recovered
```

City external fanout with blocked inherited rigs:

```text
TOPOLOGY: topology_incomplete operation=op-8821
  detected: city external adoption interrupted during inherited rig fan-out
  endpoint: db.example.com:3307 user=root
  blocked scopes:
    - rig fe has conflicting local edits in .beads/config.yaml
    - rig ml pending inherited mirror write
  resume: gc beads resume --op op-8821

ENDPOINT: blocked by topology_incomplete
DATABASE: blocked by topology_incomplete
```

City origin flip from managed to external, interrupted after managed
publication retirement:

```text
TOPOLOGY: topology_incomplete operation=op-9910
  detected: city use-external interrupted after managed publication was retired
  transition: managed_city -> city_canonical
  resume: gc beads resume --op op-9910

ENDPOINT: managed-local recovery suppressed by topology_incomplete
DATABASE: blocked by topology_incomplete
```

## Migration Strategy

Implementation rolls out in stages:

1. introduce contract types and canonical-file schemas
2. implement resolver and projection layer
3. switch all core callers to the new resolver
4. add crash-safe migration protocol and topology journal
5. add owning topology operations and explicit legacy migration command
6. materialize and track canonical `.beads/metadata.json` and
   `.beads/config.yaml` through that command
7. migrate `file` and `exec:` providers to real rig-store roots
8. stop writing deprecated metadata endpoint/auth fields
9. stop reading deprecated `city.toml` endpoint fields once canonical
   per-scope config exists
10. remove legacy fallback paths

Temporary coexistence is allowed only where this document explicitly
permits compatibility mirrors. Co-equal authorities are not allowed.

Migration rules:

- first-write canonicalization of an unmigrated legacy scope is an
  explicit operation, not an ordinary startup side effect
- transitional legacy derivation is used only by migration preflight and
  explicit migration flows, not as a permanent steady-state resolver
  path
- ordinary startup may detect an already-recorded migration journal and
  point to the exact resume command, but it does not become an
  independent journal-completion writer
- ordinary startup may still fill in a deterministically missing
  GC-owned field on an already-canonical scope when no migration journal
  is active, but it may not create the first canonical `.beads` files or
  start a new legacy migration implicitly
- once a scope is canonicalized, deprecated authority for that scope is
  ignored even if old fields remain on disk
- downgrade after canonical migration is unsupported
- successful migration stores a GC-local snapshot of pruned legacy
  authority fields for audit and manual rollback analysis; those
  snapshots are not live authority

## Test Strategy

The tests are part of the design contract, not a follow-on appendix.

### Resolver matrix

The resolver matrix must be explicit over these dimensions:

- scope: city, rig
- city origin: `managed_city`, `city_canonical`
- rig origin: `inherited_city`, `explicit`
- endpoint status: `verified`, `unverified`
- database identity source: default, adopted, migrated
- canonical inputs: present, missing, contradictory
- compatibility inputs: absent, present-and-matching,
  present-and-contradictory

Required valid-state coverage table:

| Scope | Origin | Status | Required direct test |
|---|---|---|---|
| city | `managed_city` | `verified` | yes |
| city | `city_canonical` | `verified` | yes |
| city | `city_canonical` | `unverified` | yes |
| rig inheriting managed city | `inherited_city` | `verified` | yes |
| rig inheriting external city | `inherited_city` | `verified` | yes |
| rig inheriting external city | `inherited_city` | `unverified` | yes |
| rig explicit external | `explicit` | `verified` | yes |
| rig explicit external | `explicit` | `unverified` | yes |

Each of those valid-state combinations must also be exercised across the
database identity sources `default`, `adopted`, and `migrated`, with at
least one contradictory-input variant and one compatibility-mirror
variant per family.

Minimum concrete fixtures:

| Fixture | Operation | Assertions |
|---|---|---|
| city `managed_city` | resolve + project | managed runtime publication is the only live endpoint source; `.beads/dolt-server.port` ignored for resolution; `GC_DOLT_*` and `BEADS_*` agree |
| city `city_canonical` verified | resolve + project | canonical external host, port, user emitted; no managed lifecycle owner |
| city `city_canonical` unverified | resolve + startup validation | canonical endpoint retained; startup reports typed unverified failure or promotes to verified after success |
| rig `inherited_city` under managed city | resolve + project | no tracked external endpoint defaults; database identity stays rig-local |
| rig `inherited_city` under external city | resolve + project | local mirrored host and port match city canonical external endpoint |
| rig `explicit` | resolve + project | rig endpoint overrides city endpoint without changing pinned database identity |
| legacy adopted city with non-default DB | migrate + resolve | `dolt_database` preserved exactly; default `hq` not written |
| invalid rig managed-local attempt | resolve | hard failure with owning command |

For every valid resolver fixture, assert:

- resolved server target
- resolved database identity
- projected GC-native env when provider is `bd`
- projected `bd` compatibility env when relevant
- absence of `GC_DOLT_*` and `BEADS_*` for non-`bd` providers

### Invalid-state matrix

At minimum the negative matrix must cover:

| Invalid state | Expected outcome |
|---|---|
| city origin `inherited_city` | hard failure with owning command |
| city origin `explicit` | hard failure with owning command |
| rig origin `managed_city` | hard failure with owning command |
| rig origin `city_canonical` | hard failure with owning command |
| `managed_city` plus tracked `dolt.host` or `dolt.port` | hard drift failure |
| `explicit` without host and port | hard drift failure |
| `inherited_city` with external mirror that does not match city canonical external endpoint | hard drift failure |
| pinned rig database `hq` | hard drift failure |
| duplicate pinned databases on the same endpoint | hard drift failure |
| contradictory canonical files plus incomplete topology journal | named `topology_incomplete` or `migration_incomplete` failure |

### Canonical-file migration tests

Cover:

- missing GC-owned fields derivable from canonical state
- startup backfill allowed only on already-canonical scopes
- contradictory origin markers
- deprecated endpoint fields scrubbed from metadata
- compatibility alias `issue-prefix` mirrored but not read canonically
- partially migrated city with incomplete topology journal
- legacy metadata endpoint and auth keys preserved until canonical write
  succeeds, then scrubbed
- dirty local edits blocking allowed startup normalization
- ambiguous legacy state that must fail into explicit migration

### Owning-operation tests

Cover:

- city managed/external transitions
- city managed -> external transition with managed publication retirement
- city external -> managed transition with owner record and publication creation
- rig inherit/external transitions
- `--adopt-unverified` adoption
- endpoint validation gating
- endpoint-scoped repair
- database migration/rename
- dry-run output with detected state, expected state, and exact command
- city external transition with inherited-rig mirror fan-out and resume
  after partial failure

### Crash-interruption matrix

Journaled operations must be interrupted and resumed at every boundary:

- after journal creation
- after city canonical file write
- after first inherited-rig mirror write
- after last rig mirror write but before verification
- after verification succeeds but before journal cleanup
- after lifecycle owner record write but before runtime publication
- after runtime publication write but before availability checks complete

### Provider conformance tests

Cover:

- `file` per-scope `.gc/beads.json`
- `exec:` multi-rig scope targeting via
  `GC_STORE_ROOT`, `GC_STORE_SCOPE`, `GC_BEADS_PREFIX`
- `bd` adapter deriving `BEADS_DIR` from the GC-native contract
- `ScopePrefix` treated as opaque routing metadata by non-`bd` providers
- no `GC_DOLT_*` or `BEADS_*` projected to non-`bd` providers

### Startup and doctor tests

Cover:

- endpoint-grouped verification and reporting
- `unverified -> verified` auto-promotion
- custom `BEADS_CREDENTIALS_FILE` path participates in auth resolution,
  cache grouping, and `bd` projection
- inherited rig mirror rewrites after city topology change
- refusal to auto-normalize ambiguous local edits
- `gc doctor` command suggestions and grouped output contract
- last-normalization reporting
- cache separation by auth-source fingerprint
- fanout dry-run previews and blocked-rig reporting
- delegated lifecycle-owner behavior for startup and doctor during
  incomplete journals

### Boundary-enforcement tests

The redesign requires a repo-level boundary guard analogous to existing
architectural boundary tests.

Cover:

- no direct reads of `.beads/metadata.json`, `.beads/config.yaml`,
  `.beads/dolt-server.port`, or deprecated `city.toml` Dolt fields
  outside the resolver and canonical-file packages
- no direct synthesis of `BEADS_*` outside the `bd` adapter
- no direct endpoint or database reconstruction in doctor, startup,
  K8s, convoy, work-query, or CLI helpers
- compatibility mirrors remain write-only and read-noncanonical across
  startup, doctor, bootstrap, and ancillary adapters
- non-`bd` providers consume only the finite provider-neutral env
  contract and do not branch on `DeclaredBDTarget`

## Regression Traceability Matrix

| Historical record | Caller classes | Required contract tests | Required assertion |
|---|---|---|---|
| [#245](https://github.com/gastownhall/gascity/issues/245) | `gc bd`, projected shells, K8s adapter | `TestProjectedEnvClearsAmbientDoltVars`, `TestGcBdUsesProjectionNotAmbientEnv`, `TestK8sProjectionUsesResolvedEnv` | one projection owner emits matching `GC_DOLT_*` and `BEADS_*`; stale parent env is cleared everywhere |
| [#506](https://github.com/gastownhall/gascity/issues/506) | `gc doctor`, doctor subprocess env | `TestDoctorUsesResolvedManagedPort`, `TestDoctorSubprocessEnvUsesProjection` | doctor reports runtime-publication port only and never shells out with stale port-file authority |
| [#525](https://github.com/gastownhall/gascity/issues/525) | resolver, startup validation, repair | `TestResolverRejectsStaleManagedPublication`, `TestManagedPortFileIgnoredForResolution`, `TestManagedUnavailablePointsToGCRepair` | resolver returns managed endpoint unavailable; no fallback to port file; repair path points to GC-owned recovery |
| [#541](https://github.com/gastownhall/gascity/issues/541) | session env projection, startup helpers, controller and runtime helpers | `TestSanitizeAndPopulateProjection`, `TestStartupHelpersDoNotLeakAmbientBeadsEnv`, `TestControllerRuntimeHelpersUseProjection` | sanitize-and-populate removes unsupported keys and emits only resolved target values across all caller classes |
| [#560](https://github.com/gastownhall/gascity/issues/560) | startup, doctor, repair, lifecycle owner | `TestSingleLifecycleOwnerFencesRecover`, `TestNonOwnerFlowsDelegateLifecycleMutation`, `TestOwnerEpochChangesAcrossAdoption` | only one lifecycle owner may start or recover Dolt; all other paths delegate or remain verify-only |
| [#561](https://github.com/gastownhall/gascity/issues/561) | migration, bootstrap, startup resume | `TestExplicitMigrationCreatesCanonicalFiles`, `TestIncompleteJournalFailsClosed`, `TestResumeFromEachCrashBoundary` | canonical files are created through explicit migration or bootstrap with crash-safe journal; startup refuses partial state rather than half-repairing it |

## Primary Implementation Seams

The first implementation plan should center on these code boundaries:

- resolver and projection layer
- canonical file readers and writers
- `cmd/gc/gc-beads-bd`
- provider bootstrap and init flows
- `gc doctor`
- owning CLI operations
- ancillary adapters:
  - `gc bd`
  - work-query and sling env projection
  - K8s adapters
  - convoy SQL and other bead consumers

## Consequences

### Positive

- one explicit contract replaces several hidden ones
- historical bug classes map to named invariants
- raw `bd` remains usable from each local scope
- providers stay extensible without inheriting `bd` internals
- topology changes become deliberate and reviewable

### Costs

- more tracked config for `bd` scopes
- more explicit validation and migration logic
- startup and doctor must understand endpoint grouping and inheritance
- some user-visible operations will replace formerly implicit file edits

## Implementation Plan

### Phase 1: Contract primitives and canonical file I/O

- Introduce a dedicated internal contract package for:
  - declared store target types
  - `bd`-specific target types
  - auth-resolution inputs and outputs
  - canonical file readers and writers for `.beads/metadata.json` and
    `.beads/config.yaml`
- Preserve unknown upstream keys on round-trip and explicitly scrub only
  the deprecated endpoint/auth fields named in this design.
- Implement the machine-checkable predicate for `already-canonical`
  versus `first-write canonicalization`.

Likely files:

- new internal contract package
- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/gc-beads-bd`
- boundary tests around canonical-file access

Verification:

- canonical-file schema tests pass
- startup-backfill versus migration-cutoff tests pass
- unknown-key round-trip tests pass

### Phase 2: Managed lifecycle owner and runtime publication

- Implement `dolt-owner.json`, `dolt-state.json`, and the lifecycle lock
  using the exact serialization contract in this design.
- Replace current managed-publication validation with owner-record plus
  pid-birth-proof validation.
- Ensure topology lock then lifecycle lock ordering is enforced for all
  mixed operations.

Likely files:

- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/gc-beads-bd`
- managed-state tests in `cmd/gc/beads_provider_lifecycle_test.go`

Verification:

- lifecycle-owner tests pass
- stale-publication and pid-reuse tests pass
- crash-boundary tests for owner-record and publication sequencing pass

### Phase 3: Resolver, auth, and projection layer

- Implement one resolver for:
  - provider-neutral store identity
  - `bd` endpoint origin and status
  - server target
  - database identity
  - auth resolution and cache fingerprinting
- Implement one projector for:
  - GC-native store-target env
  - GC-native Dolt env
  - `bd` compatibility env
- Sanitize ambient `GC_DOLT_*` and `BEADS_*` before projection.

Likely files:

- new resolver/projection package
- `cmd/gc/bd_env.go`
- `cmd/gc/template_resolve.go`
- `cmd/gc/cmd_bd.go`

Verification:

- resolver matrix passes for city, inherited rig, and explicit rig cases
- auth-resolution tests, including custom `BEADS_CREDENTIALS_FILE`, pass
- projection tests prove no direct ambient env leakage

### Phase 4: Owning commands, journals, and migration flows

- Implement or refactor owning commands so they are the only mutation
  surface for `bd` topology:
  - `gc beads migrate-contract`
  - `gc beads city use-managed`
  - `gc beads city use-external`
  - `gc rig set-endpoint --inherit`
  - `gc rig set-endpoint --external`
  - `gc beads city recover`
  - `gc beads endpoint repair`
  - `gc beads database rename`
  - `gc beads resume`
- Make reruns idempotent verification when the authoritative target
  already matches.
- Journal every multi-file or city fan-out operation and make `resume`
  the only completion path after interruption.

Likely files:

- new or expanded CLI command files under `cmd/gc/`
- `cmd/gc/cmd_rig.go`
- `cmd/gc/cmd_doctor.go`
- `cmd/gc/gc-beads-bd`

Verification:

- owning-operation tests pass for both city-origin directions
- interrupted city fan-out resumes cleanly from every crash boundary
- `gc beads city recover` success and failure output tests pass

### Phase 5: Caller migration and doctor contract

- Move all callers to the resolver/projector path and delete direct
  authority reconstruction.
- Rebuild `gc doctor` around the new precedence table, grouped endpoint
  reporting, and `bd`/Dolt `--fix` restrictions.
- Update ancillary adapters:
  - `gc bd`
  - work-query and sling env projection
  - convoy SQL
  - K8s provider and pod setup
  - startup helpers and store-open paths

Likely files:

- `cmd/gc/main.go`
- `cmd/gc/cmd_doctor.go`
- `internal/doctor/checks.go`
- `internal/api/convoy_sql.go`
- `internal/runtime/k8s/provider.go`
- `cmd/gc/work_query_probe.go`

Verification:

- boundary-enforcement tests prevent new direct reads of `.beads/*`
- doctor output tests pass for grouped failures and exact commands
- regression tests for #245, #506, #525, #541, #560, and #561 all pass

### Phase 6: Provider conformance, deprecation cleanup, and rollout

- Make `file` multi-rig aware with per-scope `.gc/beads.json`.
- Make `exec:` consume only the GC-native store-target contract.
- Stop consulting deprecated `city.toml` Dolt endpoint fields after
  per-scope canonicalization.
- Stop writing deprecated metadata endpoint/auth fields.
- Track canonical `.beads/metadata.json` and `.beads/config.yaml` and
  update `.gitignore` accordingly.

Likely files:

- `cmd/gc/main.go`
- `cmd/gc/api_state.go`
- `cmd/gc/city_runtime.go`
- `internal/beads/exec/*`
- `.gitignore`
- docs and troubleshooting references

Verification:

- provider conformance tests pass for `bd`, `file`, and `exec:`
- migration rehearsals work on legacy test fixtures
- no deprecated authority remains on canonicalized scopes except
  diagnostics-only residue

### Checkpoints

- After Phases 1-2: contract types, canonical file schemas, lifecycle
  owner, and publication validation land with tests before caller
  migration starts.
- After Phases 3-4: resolver/projection and owning commands are
  complete, and city/rig topology transitions are crash-safe.
- After Phases 5-6: all callers use the contract, provider conformance
  is enforced, and deprecated fallback paths are removed.

## Residual Implementation Questions

This document intentionally leaves only implementation-shaped questions
open, not architecture-shaped ones:

- whether the future architecture docs should be split or updated in
  place once the redesign lands
- the exact test file layout for the regression matrix and boundary
  enforcement suite
- whether the topology journal should live under `.gc/runtime/` or a
  sibling GC-owned local path
