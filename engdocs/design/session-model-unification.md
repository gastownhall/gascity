---
title: "Session Model Unification"
---

| Field | Value |
|---|---|
| Status | Accepted |
| Date | 2026-04-10 |
| Author(s) | Codex |
| Issue | N/A |
| Supersedes | named-configured-sessions (partially); clarifies the post-pool session model layered over agent-pools |

## Summary

Gas City should expose one runtime model:

- `[[agent]]` is a session-producing config and policy object
- sessions are the only runtime identities
- `[[named_session]]` reserves canonical session identities backed by an
  agent config
- generic config demand creates ephemeral sessions
- manual and provider-compatibility entry points create ordinary
  config-backed sessions with their own session identities

The old "pool vs no-pool" split must disappear as a semantic category.
Capacity config answers "how many sessions may run for this config," not
"what kind of thing is this" or "what does this bare name mean."

This design keeps user-facing product language mostly unchanged in Phase
1, but it removes the internal footgun by unifying identity, routing,
demand, and lifecycle semantics behind a single session model.

## Problem

The named-session refactor moved Gas City in the right direction, but
the codebase still carries a surviving semantic split that is bigger
than a naming problem:

- helpers like `isMultiSessionCfgAgent` still let capacity settings
  control routing and identity behavior
- bare names sometimes mean a concrete session and sometimes mean
  "materialize from config"
- `gc.routed_to`, `assignee`, `work_query`, and status all partially
  encode the old pool/non-pool distinction
- controller-owned retirement still leaks through `closed` and reopen
  behavior
- prompts and hooks still infer semantics from overloaded variables like
  `GC_AGENT`

The result is a major footgun: users cannot tell whether a config name
is a session, a factory, a singleton, an elastic worker class, or all of
the above depending on `max_active_sessions`, wake paths, and legacy
fallbacks.

## Goals

- Make `[[agent]]` a pure config/factory concept.
- Make all runtime identity live on sessions.
- Allow multiple configured named sessions to share one backing agent
  config.
- Separate config routing from concrete session ownership.
- Make `scale_check` the only controller-side generic demand signal.
- Keep direct session continuity exact and inspectable.
- Preserve compatibility through read-side shims and phased rollout.
- Start with exhaustive red tests and a gap analysis before production
  code changes.

## Non-goals

- Immediate user-facing terminology cleanup in CLI, API, or docs.
- A mandatory one-shot metadata migration of historical beads.
- Static policing of arbitrary custom `work_query` or `scale_check`
  scripts.
- A full Phase 1 rollout of new user-facing `pin/unpin` commands.
- Preserving every legacy pool-specific identity primitive such as
  `pool_slot`.

## Core Model

### Agent Configs

`[[agent]]` defines reusable behavior and policy:

- prompt
- provider/start command
- workdir and environment
- dependencies
- wake mode
- idle policy
- `scale_check`
- `min_active_sessions` / `max_active_sessions`
- naming policy like `namepool`

An agent config does not itself own runtime identity.

Fully qualified config identity uses the same scope form:

- `<rig>/<name>` for rig-scoped configs
- `<name>` for city-scoped configs

### Sessions

A session bead is the only runtime identity. Every session has:

- a bead ID
- a backing config identity (`template`)
- a runtime `session_name`
- optional presentation alias
- mutable lifecycle state

Phase 1 writes exactly three `session_origin` values:

- `named`
- `ephemeral`
- `manual`

Origin is immutable and means "how this session came into existence."
External bindings, provider kind, attachments, pinning, and assigned
work are separate runtime facts, not origin values.

### Named Sessions

`[[named_session]]` declares canonical configured session identities.

New shape:

```toml
[[agent]]
name = "reviewer"

[[named_session]]
name = "mayor"
template = "reviewer"
mode = "on_demand"

[[named_session]]
name = "triage"
template = "reviewer"
mode = "always"
```

Rules:

- `name` is the public identity
- `template` is the backing config identity
- multiple named sessions may point at the same template
- `[[named_session]].name` must be unique across configured named
  sessions after qualification
- if `name` is omitted, compatibility means `name = template`
- identical `name` and `template` is a supported steady-state pattern;
  omitting `name` is compatibility syntax, not a separate semantic mode
- fully qualified identity is `<rig>/<name>` for rig-scoped identities
  and `<name>` for city-scoped identities

## Namespace Model

Gas City now has two separate namespaces:

### Session Namespace

Configured named identities reserve the session namespace immediately,
even before bead materialization.

Normal session-targeting surfaces resolve in strict order:

- bead ID
- configured named-session alias
- current alias
- current `session_name`
- historical alias only on explicitly allowed compatibility surfaces

Rules:

- if session-namespace resolution finds zero matches, the operation fails
  with an explicit session-targeting error
- if more than one concrete or reserved session exposes the same bare
  token, bare resolution fails and qualification is required
- no rig-local implicit precedence exists for bare tokens; cross-rig or
  city-vs-rig ambiguity always fails closed
- session-targeting resolution never falls through to config namespace
- session-targeting resolution must not fall back to `template` or
  `agent_name`
- a configured named-session alias in conflict remains authoritative for
  failure: bare targeting must fail with the conflict rather than hit the
  blocking session

#### Session-targeting token matrix

| Token class | Accepted on | Lookup scope | Success rule | Notes |
|---|---|---|---|---|
| bead ID | session-targeting and compatibility surfaces | global exact bead lookup | exactly one open bead | never falls through |
| fully qualified named identity `<rig>/<name>` | session-targeting and compatibility surfaces | exact `configured_named_identity` lookup | exactly one reserved or open canonical match | config-managed alias is only the mirrored presentation of this same identity, not a second lookup branch |
| unqualified named token `<name>` | session-targeting and compatibility surfaces | city-scoped identities plus identities in the current rig only | exactly one reserved/open session-side match across configured named alias, current alias, and current `session_name` | cross-rig bare lookup is never permitted |
| current alias | session-targeting and compatibility surfaces | same as unqualified named token | exactly one open bead | reserved configured named alias still wins conflicts by failing closed |
| current `session_name` | session-targeting and compatibility surfaces | same as unqualified named token | exactly one open bead | compatibility only if the surface explicitly allows historical forms |
| historical alias | compatibility-only surfaces | explicit compatibility lookup only | exactly one compatibility candidate | never used by normal target resolution |

Unqualified session-targeting lookup never searches other rigs. If a
token is not uniquely resolvable from city-scoped identities plus the
current rig, the operation fails and requires qualification.

#### Session-targeting precedence algorithm

For a bare session-targeting token within the visible lookup scope:

1. exact open bead ID wins if present
2. if the token matches any reserved configured named identity:
   - exactly one reserved named match and no competing open
     alias/`session_name` match: target that named identity
   - exactly one reserved named match and any competing open
     alias/`session_name` match: fail with configured-named conflict
   - more than one reserved named match: fail and require qualification
3. otherwise resolve against current alias
4. otherwise resolve against current `session_name`
5. consult historical aliases only on explicitly compatibility-only
   surfaces

No session-targeting surface may reinterpret a bare token as a config
token after a session-side miss or conflict.

Multiple matching identifiers on the same concrete bead count as one
candidate, not an ambiguity. Ambiguity exists only when the token maps
to different reserved identities or different beads.

Fully qualified session-side tokens bypass the bare-token algorithm and
resolve only by exact bead ID or exact qualified configured named
identity.

### Historical alias policy

Phase 1 keeps historical aliases as compatibility input only.

Rules:

- normal session-targeting CLI/API commands do not consult
  `alias_history`
- historical alias lookup is reserved for compatibility translation of
  persisted legacy ownership/session references and any explicit debug
  surfaces
- new surfaces default to "no historical alias resolution" unless they
  opt in deliberately
- no current user-facing targeting command relies on historical alias
  lookup

### Config Namespace

Factory targeting resolves only by agent-config identity in strict
order:

- explicit qualified config identity
- explicit `template:<name>` syntax
- bare `<name>` token resolved only against city scope plus current-rig
  scope, and only when exactly one visible config matches

Named-session aliases do not reverse-map into config namespace.

If config-namespace resolution is ambiguous, qualification is required.
City-scoped versus rig-scoped bare-name collisions are therefore
fail-closed; neither scope silently wins on a bare token.

#### Factory-targeting token matrix

| Token class | Accepted on | Lookup scope | Success rule | Notes |
|---|---|---|---|---|
| fully qualified config identity `<rig>/<name>` | factory-targeting surfaces | exact config lookup | exactly one configured agent | canonical stored form for rig-scoped config references |
| city-scoped config identity `<name>` | factory-targeting surfaces | exact city-scoped config lookup | exactly one city-scoped configured agent | canonical stored form for city-scoped config references |
| `template:<qualified>` | factory-targeting surfaces | exact config lookup after removing `template:` | exactly one configured agent | explicit family marker only |
| `template:<name>` | factory-targeting surfaces | city plus current-rig config namespace | exactly one visible config | no reverse mapping from named-session aliases |
| bare config name `<name>` | factory-targeting surfaces | city plus current-rig config namespace only | exactly one visible config | other rigs are never searched by bare lookup |

Cross-rig factory targeting always requires explicit qualification.
There is no "search every rig and pick the only one" fallback.

### Resolver pipeline

All target-bearing surfaces share the same pipeline:

1. classify the surface as `session-targeting`, `factory-targeting`, or
   `compatibility-only`
2. classify the token form as bead ID, fully qualified identity,
   `template:` token, or bare token
3. choose the namespace permitted by the surface class
4. apply the namespace-specific exact/bare resolution rules
5. canonicalize stored identity if the surface writes metadata, or fail
   closed if resolution is ambiguous

No implementation may merge parsing and resolution in a way that allows
post-miss reinterpretation across namespaces.

Top-level invariant: no bare token may ever denote both a session-family
target and a factory-family target on the same surface, including under
compatibility translation.

### Ambient rig resolution

Bare-token lookup that refers to "current rig" is only valid when the
surface has an unambiguous ambient rig:

- CLI commands use the caller's current rig/session context
- workflow/automation/API surfaces must provide explicit rig context if
  they want rig-scoped bare-token lookup
- if no ambient rig exists, bare rig-scoped lookup is forbidden and the
  caller must use a fully qualified token or a city-scoped identity

When ambient rig is absent, the canonical failure is an explicit
qualification-required error. The resolver must not attempt partial
lookup before failing.

Non-CLI entry points must not invent a current rig heuristically.

Ambient-rig source by surface family:

| Surface family | Ambient rig source |
|---|---|
| CLI commands | caller's current rig/session context |
| workflow/automation actions | explicit rig carried on the workflow object or dispatch context |
| API endpoints with explicit rig field/path | that explicit request scope |
| API endpoints without explicit rig scope | none; bare rig-scoped lookup is qualification-required |

### `template:` scope

`template:<name>` is valid only on factory-targeting surfaces:

- CLI arguments whose command family is factory-targeting
- metadata fields whose contract is config-targeted, such as
  `gc.routed_to` and `gc.execution_routed_to`

It is not a session-targeting token and it is not a separate identity
syntax inside config-managed session metadata.

### Shadowing

Configured named-session aliases intentionally shadow config names in
the session namespace. That means:

- bare `mayor` in a session-targeting command means the named session
- `template:mayor`, `gc session new mayor`, `gc.routed_to=mayor`, and
  `gc.execution_routed_to=mayor` mean the config

Manual session aliases may not collide with config names. Config-name
shadowing is reserved for configured named sessions.

Manual aliases also may not collide with configured named-session
identities, because both live in the session namespace.

Uniqueness rules:

- configured named identities are unique after qualification
- current aliases must be unique across open sessions after
  qualification
- `session_name` must be unique across open sessions within the city
- collisions are rejected at write time and surfaced diagnostically if
  discovered in historical data

Global collision policy:

- configured named identities are the only session-side identifiers
  allowed to intentionally shadow config names
- manual aliases may not collide with any current config identity,
  configured named identity, current alias, or current `session_name`
- generated or renamed `session_name` values may not collide with any
  configured named identity, current alias, current `session_name`, or
  current config identity after qualification; generation must retry or
  fail closed rather than create ambiguity
- historical aliases do not reserve namespace and never block new
  config or session creation on their own

If a newly added config name collides with an existing manual alias, the
config remains authoritative in config namespace and bare targeting of
the colliding manual alias must fail with an explicit conflict until the
alias is renamed or removed.

Manual alias collision checks happen both at alias-creation time and
again at config-load/reconciliation time so newly added configs cannot
silently inherit a squatted factory name.

## Command Classes

Commands are classified by target family rather than by token alone.

Every resolver entrypoint must declare its surface class at
registration-time or compile-time. Helper functions may not widen,
infer, or retry surface class dynamically.

### Session-targeting

Examples:

- `gc session attach`
- `gc session wake`
- `gc session suspend`
- `gc session close`
- `gc mail`
- `gc session nudge`
- bare `gc sling <target>`

These resolve through the session namespace. If they target a
configured named session, they may materialize its canonical bead.

### Factory-targeting

Examples:

- `gc session new <config>`
- `template:<name>`
- `gc sling template:<config> <work>`
- `gc.routed_to=<config>`
- `gc.execution_routed_to=<config>`

These resolve through the config namespace only.

Any new surface that accepts a target token must explicitly declare one
of:

- session-targeting
- factory-targeting
- compatibility-only

Unclassified bare-token resolution is not allowed.

### Surface matrix

| Surface class | Examples | Resolution family | Historical alias | Notes |
|---|---|---|---|---|
| session-targeting CLI | `gc session attach`, `gc session wake`, `gc session suspend`, `gc session close`, `gc mail`, `gc session nudge`, bare `gc sling` | session namespace | no | materialize named session if needed |
| factory-targeting CLI | `gc session new`, `gc sling template:<config>`, explicit `template:` args | config namespace | no | generic dispatch/config creation |
| session-targeting API/workflow | direct session-targeted workflow `assignee`, session action APIs mirroring attach/wake/close/suspend/mail/nudge | session namespace | no | concrete session delivery only |
| factory-targeting API/workflow | provider/agent create surfaces normalized to config identity, workflow `gc.routed_to`, `gc.execution_routed_to` | config namespace | no | config-backed execution only |
| stored metadata | `assignee`, `gc.routed_to`, `gc.execution_routed_to` | field-defined | restricted | `assignee` = session; routed fields = config |
| compatibility readers | legacy `assignee`, older stored references | compatibility-only | restricted | may resolve only through explicit compatibility rules |
| session-context execution | `gc hook` | non-target-bearing | n/a | may query `gc.routed_to=$GC_TEMPLATE` explicitly; this is not namespace fallback |
| inspection surfaces | `status`, `doctor` | non-target-bearing unless separately declared | n/a | render/diagnose, not target resolution |

Phase 0 tests must pin every currently shipped target-bearing surface to
one row in this matrix so the classification cannot drift silently in
implementation.

### Current public surface inventory

Phase 1 treats the following as the complete target-bearing surface set
that must be classified and tested:

| Surface | Class | Notes |
|---|---|---|
| `gc session attach` | session-targeting | may materialize named |
| `gc session wake` | session-targeting | may materialize named |
| `gc session suspend` | session-targeting | may materialize named into held state |
| `gc session close` | session-targeting | concrete session lifecycle only |
| `gc mail` | session-targeting | delivery-only; may materialize named |
| `gc session nudge` | session-targeting | delivery-only; may materialize named |
| bare `gc sling <target>` | session-targeting | concrete session delivery |
| `gc session new <config>` | factory-targeting | explicit config factory |
| `gc sling template:<config> <work>` | factory-targeting | explicit config routing |
| workflow/API direct `assignee` target | session-targeting | concrete session ownership |
| workflow/API `gc.routed_to` | factory-targeting | generic config execution |
| workflow/API `gc.execution_routed_to` | factory-targeting | control-dispatch preserved config lane |
| provider-create boundary (`kind=provider`, provider name) | factory-targeting compatibility shim | boundary-only sugar before factory resolution |
| stored legacy ownership/session-reference readers | compatibility-only | translation only |
| doctor/debug/migration identity readers | compatibility-only | inspection/repair only |

If any other public entrypoint accepts a free-form target token, it is a
bug against this design until it is added here with an explicit class.

### Phase 1 compatibility-only surfaces

Phase 1 keeps compatibility-only behavior on a narrow explicit list:

- stored metadata readers that normalize legacy `assignee` or older
  session-reference fields
- provider-entry request normalization at the boundary from
  `kind=provider`/provider-name input to canonical config targeting
- explicit debug/doctor/migration tooling that intentionally inspects
  historical aliases or ambiguous legacy references

No normal user-facing session-targeting or factory-targeting CLI/API
command remains compatibility-only in Phase 1.

Compatibility terminology is exact:

- `session-targeting` and `factory-targeting` are normal runtime
  surfaces
- `compatibility-only` means legacy translation/inspection surfaces that
  never reinterpret themselves as normal runtime targeters
- phrases like "compatibility readers" or "compatibility translation"
  refer only to that `compatibility-only` class

There are no surviving public CLI shorthands that may first try session
resolution and then reinterpret the same bare token as config targeting.
If any such surface still exists in implementation, it is a bug against
this design.

## Ownership and Routing

### `assignee`

New canonical rule:

- all new `assignee` writes use the concrete session bead ID

Compatibility:

- readers continue to accept legacy `session_name`, legacy alias,
  exact configured named-session identity tokens, and only those
  template-era tokens that exactly equal a current configured named
  identity under present qualification rules
- touched records should be opportunistically normalized to bead ID

Legacy ownership normalization is intentionally narrower than normal
session-targeting resolution:

- it may resolve through existing open-bead IDs, current alias, and
  current `session_name`
- it may also resolve through exact configured named identity for a
  config-managed named session
- template-era tokens are never looked up in factory namespace; they may
  normalize only by exact equality to a configured named identity under
  current qualification rules
- it must not resolve through historical aliases or generic
  config/template fallback
- if more than one candidate remains, normalization fails closed and
  diagnostics report the ambiguity

If a legacy token could match both a session-side candidate and a config
name, compatibility resolution must still stay in the session family or
fail; it must never reinterpret that token as a factory target.

If legacy ownership normalization resolves by exact configured named
identity and no canonical bead currently exists, the ownership reader
may materialize that canonical named bead solely to obtain a concrete
bead ID for canonical rewrite. This is the only Phase 1 compatibility
path that may materialize a reserved named identity.

Direct session-targeted writes set only:

- `assignee=<session-bead-id>`

They do not also stamp `gc.routed_to`.

### `gc.routed_to`

`gc.routed_to` is only for generic config-targeted execution and stores
the resolved qualified config identity.

It is not:

- a session identity
- a named-session alias
- a direct-session delivery hint

### `gc.execution_routed_to`

`gc.execution_routed_to` remains only as a narrow internal escape hatch
for control-dispatch flows that temporarily repoint `gc.routed_to` at
the control dispatcher while preserving the real config execution lane.
It must not become a second general-purpose routing channel.

### Claiming Generic Work

Generic config-routed work may keep `gc.routed_to` as provenance after a
session claims it, but once it is claimed:

- `assignee` becomes the concrete session bead ID
- it no longer counts as generic demand for `scale_check`
- continuity follows the owning session, not the generic route

### Canonical persisted identity forms

All new writes of config-backed identity fields persist canonical scope
forms, not raw user input:

- rig-scoped config identities are stored as `<rig>/<name>`
- city-scoped config identities are stored as `<name>`
- configured named identities are stored in the same qualified form
- `template`, `gc.routed_to`, and `gc.execution_routed_to` always carry
  backing config identity, never a session alias or user-entered token

Legacy unqualified rig-scoped tokens may be normalized only when the
current city snapshot makes the mapping unique. Otherwise they remain
legacy data and diagnostics surface the ambiguity instead of guessing.

Provider-era names, legacy `agent_name` references, and other historical
factory/session shims follow the same rule: they are compatibility input
only at explicitly listed boundary readers and never participate in
normal session-targeting or factory-targeting lookup.

Low-level raw assign surfaces may remain permissive in Phase 1 for
compatibility. `gc doctor` reports invalid or stale ownership instead of
making the controller guess.

## Demand Model

### `scale_check` is the only controller-side generic demand signal

`scale_check` answers only:

> How many generic config-backed sessions should exist for this config?

It does not encode:

- named-session wake semantics
- direct concrete session ownership
- prompt-side work pickup behavior

### `work_query` is session-local introspection

`work_query` remains useful for:

- `gc hook`
- prompt-side work pickup
- running-session introspection

It no longer drives controller-side materialization or desired-count
decisions.

### Default `work_query`

The synthesized default remains, but becomes origin-aware at runtime:

- all sessions check assigned `in_progress`
- all sessions check assigned ready work
- only `origin=ephemeral` checks unassigned `gc.routed_to=$GC_TEMPLATE`

Named and manual sessions stop at explicit ownership.

Custom `work_query` and `scale_check` remain escape hatches.

## Runtime Environment

Every config-backed session start should receive explicit env that
matches the unified model:

- `GC_SESSION_ID` = concrete session bead ID
- `GC_SESSION_NAME` = current runtime session handle
- `GC_ALIAS` = current public alias, if any
- `GC_TEMPLATE` = qualified backing agent-config identity
- `GC_SESSION_ORIGIN` = `named`, `ephemeral`, or `manual`
- `GC_AGENT` = temporary compatibility alias for the public handle only

New prompt and hook logic should key config semantics off `GC_TEMPLATE`
and lifecycle semantics off `GC_SESSION_ORIGIN`, not off `GC_AGENT`.

### Token relationships by origin

| Origin | `configured_named_identity` | `alias` | `session_name` | `GC_ALIAS` | `GC_AGENT` |
|---|---|---|---|---|---|
| `named` | present; immutable fully qualified named identity | always equals `configured_named_identity` while config-managed | deterministic runtime handle derived from the named identity and workspace naming policy | same as `alias` | same as `alias` |
| `ephemeral` | absent | optional, mutable if non-conflicting | opaque runtime handle | alias if present | alias if present, otherwise `session_name` |
| `manual` | absent | optional, mutable if non-conflicting | opaque runtime handle | alias if present | alias if present, otherwise `session_name` |

Configured named sessions do not carry a second mutable runtime alias
separate from their configured identity.

Configured named-session alias vocabulary:

- `[[named_session]].name` is the config-declared public identity
- `configured_named_identity` is that same identity after qualification
  and is stored on the bead
- bead `alias` mirrors `configured_named_identity` for config-managed
  named sessions

`GC_AGENT` remains compatibility-only. It must not be read by new logic
for routing, ownership, demand, or namespace resolution.

Phase 1 `GC_AGENT` contract is exact:

- `named`: identical to `GC_ALIAS`, which is the configured named
  identity
- `ephemeral` and `manual`: `GC_ALIAS` if present, otherwise
  `GC_SESSION_NAME`

No Phase 1 path may interpret `GC_AGENT` as backing config identity,
factory target, or durable ownership token.

## Materialization Rules

### Named Sessions

`mode = "always"`:

- canonical bead is always desired
- the controller rematerializes a fresh bead if needed

`mode = "on_demand"`:

- identity is reserved immediately
- canonical bead is created only when needed

Materialization causes for `on_demand` named sessions:

- direct session targeting
- direct concrete ownership writes that first materialize the canonical
  bead and then persist `assignee=<session-bead-id>`
- dependency wake
- active external binding continuity for that exact session
- explicit pinning

Reserved-but-unmaterialized named identities must be visible in
config-aware status surfaces, but bead-only listings remain bead-only.

Commands that need concrete ownership never persist an abstract named
identity token. They first materialize the canonical bead, then persist
its bead ID.

Delivery/ownership commands that materialize a named session, such as
mail or direct work assignment, continue their delivery against that
materialized bead. They do not require a pre-existing live runtime to
resolve the target identity.

### Non-named sessions

Non-named direct session-targeting must hit an existing concrete session.
Ordinary sessions are never implicitly created from a bare config name.
Creation remains explicit through factory-targeting actions.

Per-origin continuity rules:

- `named`: exact identity continuity is keyed by
  `configured_named_identity`; non-terminal beads for that identity may
  be resumed or re-adopted according to the named-session rules in this
  document
- `ephemeral`: generic config demand always creates fresh session
  identity; it never revives a prior `drained` or `archived` bead just
  because the config still has work
- `ephemeral`: only explicit concrete continuity, such as exact
  `assignee` ownership or active external binding continuity, may revive
  a non-closed ephemeral bead
- `manual`: manual sessions never satisfy generic config demand and are
  resumed only by direct targeting or exact continuity handles such as
  bindings

## Lifecycle State Machine

Concrete sessions and configured named identities also project a
controller desired state separate from their current bead state:

- `undesired`: no bead needs to exist or continue existing for this
  identity right now
- `desired-asleep`: the identity should exist as a bead, but no runtime
  start is currently required
- `desired-running`: the identity should have a live runtime now
- `desired-blocked`: the identity would otherwise be `desired-running`,
  but a hard blocker currently suppresses start

For configured named identities, `reserved-unmaterialized` and
`conflict` describe whether a bead exists. The desired-state values
describe whether that identity should be absent, materialized asleep,
materialized/running, or currently blocked.

Desired-state projection rules:

| Inputs | Projected desired state | Notes |
|---|---|---|
| no wake/materialization cause and no requirement to preserve a concrete bead | `undesired` | configured named identity may still project `reserved-unmaterialized` |
| concrete bead required for ownership/identity continuity, but no current wake cause | `desired-asleep` | typical for on-demand named materialized for ownership only |
| durable or one-shot wake cause present and no hard blocker applies | `desired-running` | runtime should become `creating`/`active` |
| durable or one-shot wake cause present, but a hard blocker applies | `desired-blocked` | health is degraded, not silently healthy |

`mode=always` plus per-session suspend is therefore a supported
`desired-blocked` steady state: the named identity remains desired by
policy, the suspend override blocks start, and status should show the
identity as degraded/blocked rather than healthy-running or undesired.

Two important projected states are not bead states:

- `reserved-unmaterialized`: a configured named identity exists but no
  bead currently exists for it
- `conflict`: config reserves a named identity, but the canonical bead
  cannot currently be materialized because of a namespace conflict or
  similar blocker

They are status projections only. They do not require placeholder bead
records.

`conflict` enters when config owns the identity but canonical
materialization is blocked by a namespace collision or similar
reservation failure. It clears only when the blocking condition is
removed, and bare targeting must fail while the conflict exists.

Projection rules:

- `reserved-unmaterialized` = configured named identity exists, no open
  canonical bead exists, and no conflict blocks materialization
- `conflict` = configured named identity exists, but canonical alias
  reservation or materialization is currently blocked
- pending wake or delivery intents affect desiredness, not the projected
  identity class itself

Canonical occupancy rule:

- for a configured named identity, any unique open bead whose
  `configured_named_identity` exactly matches that fully qualified
  identity and is `continuity_eligible=true` remains the canonical
  occupant regardless of whether its base state is `active`, `asleep`,
  `suspended`, `drained`, `archived`, or `orphaned`
- the identity is `reserved-unmaterialized` only when no such open bead
  exists
- an open bead with matching `configured_named_identity` but
  `continuity_eligible=false` is historical only and must not block
  fresh canonical rematerialization once reconciliation has removed it
  from the canonical uniqueness set
- `drained` therefore still occupies canonical identity until it is
  terminally closed or loses continuity eligibility by explicit
  controller action

Crash quarantine is also a blocker overlay, not a separate base
identity/state model.

Overlay rules:

- `conflict` applies only to configured named identities, not to generic
  ephemeral/manual sessions
- crash quarantine applies only to materialized non-terminal beads and
  normally leaves them in `archived` while blocked from restart
- `reserved-unmaterialized` identities do not carry runtime-only
  overlays like quarantine until a bead exists

### Base bead states

| State | Counts toward `max_active_sessions` | Meaning | Typical exits |
|---|---|---|---|
| `creating` | yes | bead exists and runtime start/rematerialization is in progress | `active`, `suspended`, `archived`, `closed` |
| `active` | yes | runtime is live | `creating`, `asleep`, `drained`, `suspended`, `archived`, `closed` |
| `asleep` | no | bead exists but runtime is not live | `creating`, `suspended`, `drained`, `archived`, `closed` |
| `suspended` | no | per-session hold suppresses wake | `asleep`, `creating`, `closed`, `orphaned`, `archived` |
| `drained` | no | non-terminal completed/retired bead that may later resume the same identity | `creating`, `archived`, `closed` |
| `archived` | no | controller-retired historical bead preserved for inspection, quarantine recovery, duplicate repair, or explicit continuity re-adoption | `creating`, `closed` |
| `orphaned` | no | backing config is missing, so the bead is not startable | `archived`, `creating`, `closed` |
| `closed` | no | terminal bead; never reopened or re-adopted | none |

### Continuity eligibility

Retired/open non-terminal beads also carry a controller-owned continuity
eligibility bit for exact-identity reuse:

- `continuity_eligible=true`: exact identity may later resume this same
  bead if other rules permit it
- `continuity_eligible=false`: the bead is historical only and must
  never become the continuity target again

Normative defaults:

- `drained` canonical beads are continuity-eligible
- `suspended` canonical beads are continuity-eligible
- `orphaned` canonical beads are continuity-eligible while the missing
  config condition is the only blocker
- `archived` beads are continuity-eligible only when the controller
  archived them from the canonical exact identity due to temporary
  blocker/recovery conditions such as crash quarantine or interrupted
  restart
- duplicate-bead losers, abandoned stray beads, and other historical
  non-canonical records must be `continuity_eligible=false`

`continuity_eligible=false` and canonical occupancy are mutually
exclusive after reconcile repair. If a bead still carries matching
`configured_named_identity` but is non-eligible, repair must first
remove it from the canonical uniqueness set before a replacement bead is
created.

### Normative transition rules

- `reserved-unmaterialized` -> `creating` when materialization also
  requires immediate liveness, such as `mode=always`, explicit wake,
  attach, active bound inbound event, or an unblocked `pin_awake`
- `reserved-unmaterialized` -> `asleep` when materialization is needed
  only to create concrete ownership or identity continuity
- `reserved-unmaterialized` -> `suspended` when suspend materializes the
  canonical bead directly into held state
- `creating` -> `active` when runtime becomes live
- `creating` -> `suspended`, `archived`, or `closed` if start intent is
  cancelled, controller retirement wins, or explicit close terminates
  the bead before activation
- `active` -> `creating` on live restart paths such as non-deferrable
  config drift while preserving the cap slot for that same session
- `active` -> `asleep` when no durable wake reason remains and normal
  idle policy allows sleep
- any non-terminal bead state -> `suspended` on per-session suspend
- `asleep`, `drained`, `archived`, and `orphaned` -> `creating` when
  the same exact identity becomes startable and desired again
- any non-terminal bead state -> `closed` on explicit close
- any non-terminal bead state -> `orphaned` when the backing config is
  removed or cannot be resolved
- controller retirement uses `drained`, `suspended`, `archived`, or
  `orphaned`; it never uses `closed`

`drained` versus `archived`:

- `drained` is normal non-terminal retirement of a previously healthy
  session whose exact identity may still matter
- `archived` is controller history retention for failed starts,
  quarantine, duplicate-bead losers, or other retired sessions that
  should not look like active job completion

Fresh rematerialization versus resume is exact:

- if a configured named identity already has one open canonical bead and
  that bead is `continuity_eligible=true`, wake/resume/start targets
  that same bead
- reconciliation mints a fresh canonical bead only when no open
  continuity-eligible canonical bead exists for the identity, or after
  the prior bead has crossed the terminal close barrier
- duplicate-bead losers and any bead with
  `continuity_eligible=false` are never resume/re-adoption candidates

### Origin-by-action summary

| Origin | direct session target | generic `scale_check` demand | exact assigned continuity | generic dependency satisfaction | bound inbound continuity | suspend | close |
|---|---|---|---|---|---|---|---|
| `named/on_demand` | materialize or resume exact canonical bead | never | resume exact canonical bead | may use implicit/explicit named satisfier rules | resume exact bound bead | may materialize exact bead into held state | terminal for current bead; config rematerializes only on the next real demand/explicit target |
| `named/always` | materialize or resume exact canonical bead | never | resume exact canonical bead | may use implicit/explicit named satisfier rules | resume exact bound bead | may materialize exact bead into held state | terminal for current bead; config immediately desires a fresh canonical bead once the close barrier completes |
| `ephemeral` | existing bead only | create fresh generic bead | resume exact bead only if non-closed and continuity-eligible | generic dependency may create fresh ephemeral bead | resume exact bound bead | existing bead only | terminal for current bead |
| `manual` | existing bead only | never | resume exact bead only if non-closed and continuity-eligible | never satisfies generic dependency by default | resume exact bound bead | existing bead only | terminal for current bead |

### Resume vs fresh rematerialization

| Identity/bead condition | Qualifying cause | Result |
|---|---|---|
| named identity, one open canonical bead, `continuity_eligible=true` | direct target, assigned continuity, dependency, binding, `pin_awake`, `mode=always` policy | resume/start that same bead |
| named identity, one open matching bead, `continuity_eligible=false` | any named-session cause | repair removes it from canonical uniqueness; then mint a fresh canonical bead if the identity remains desired |
| named identity, no open canonical bead | any named-session materialization cause | mint a fresh canonical bead |
| ephemeral/manual bead, exact bead exists and `continuity_eligible=true` | direct exact-bead continuity (`assignee`, bound inbound event, direct session target) | resume/start that same bead |
| ephemeral/manual bead, exact bead exists and `continuity_eligible=false` | direct exact-bead continuity | fail; do not invent successor identity |
| ephemeral, no exact continuity target | generic `scale_check` or generic dependency demand | mint a fresh ephemeral bead |
| manual, no exact continuity target | generic demand | fail; manual sessions are never generic capacity |

No policy or controller path may choose between "resume same bead" and
"mint fresh bead" heuristically once the row above is known.

`creating` is not an indefinite limbo state. A failed or abandoned start
attempt must leave `creating` for a non-counting state, normally
`archived` with quarantine/blocker metadata preserved on the bead, and
it must stop counting toward cap once the attempt has terminated.

`creating` carries a controller-owned start epoch/lease. If the runtime
does not become `active` before that lease expires, or the start owner
is lost, reconciliation terminates the attempt, records quarantine or
failure metadata, and moves the bead out of `creating`.

## Reconciler Contract

The controller computes two disjoint desired-state outputs:

- per-session desired materialization/liveness for concrete identities
- per-config generic ephemeral desired count

It never infers concrete session identity from generic config demand.

Normative reconciliation invariants:

- at most one open bead may exist per fully qualified
  `configured_named_identity`
- `closed` beads are never wake targets, never adoption targets, and
  never continuity targets
- repeated reconcile passes with unchanged inputs must be idempotent:
  they must not create extra beads or rewrite ownership differently
- direct commands and APIs may assert intent or materialize identity, but
  canonical bead creation/adoption must still flow through the same
  reconciliation/materialization guard path
- if multiple explicit continuity starts become newly eligible in the
  same pass, reconciliation emits start attempts in deterministic order:
  configured named identities sort by fully qualified identity, all
  other sessions sort by bead ID; this ordering affects attempt
  sequencing only, not desiredness

Canonical materialization for a configured named identity is
compare-and-swap on that fully qualified identity key. Command-side
materialization may create intent, but it must acquire the same
canonical guard before creating or adopting a bead.

If duplicate open canonical beads are ever observed for one configured
named identity, reconciliation must deterministically keep exactly one
winner and retire all losers non-terminally. The winner is selected by
the canonical materialization ordering metadata already on the beads
(generation, then creation order as a tiebreaker). Ownership and
bindings remain attached to exact bead IDs; the controller restores the
uniqueness invariant, and diagnostics report the anomaly.

`generation` is the monotonic canonical-materialization generation for a
configured named identity. It increments whenever that identity creates
a fresh canonical bead after terminal close or other fresh
rematerialization event.

Normative reconciliation order:

1. Validate config invariants and normalize compatibility inputs.
2. Repair metadata drift that does not require runtime liveness.
3. Project per-named-identity desired state from config mode, direct
   targeting, concrete ownership, dependency wake, active binding
   continuity, and `pin_awake`.
4. Project generic ephemeral demand from `scale_check` and
   `min_active_sessions`.
5. Apply hard blockers and wake-cause precedence.
6. Count `active` + `creating` sessions toward caps.
7. Materialize, start, sleep, retire, or refuse starts accordingly.

When a direct session-targeted operation needs a named session that does
not yet have a bead, the command materializes the canonical bead record
or start intent, then reconciliation performs any actual runtime start.
Commands do not bypass canonical duplicate-prevention by spawning
runtime directly.

If ownership repair, duplicate-canonical repair, and startability
projection all occur in one pass, the ordering is strict:

1. normalize and repair metadata that does not depend on runtime
2. restore canonical uniqueness for configured named identities
3. compute desired state and start/stop decisions against that repaired
   identity set

Later phases in the same pass must not target duplicate losers or stale
pre-repair ownership candidates.

When canonical fields are present, core lifecycle and identity logic
must not consult legacy pool-era markers such as `pool_slot`,
`pool_managed`, or `manual_session`. Those fields are compatibility-read
inputs only and may participate only in translation/diagnostic paths.

## Wake, Suspend, and Pin

### Explicit wake

`gc session wake` becomes a real liveness trigger:

- materializes the canonical bead for configured named sessions if
  needed
- sets the existing one-shot start request path
- starts the session immediately via reconciliation
- does not implicitly pin it

Implementation should reuse `pending_create_claim=true` rather than
introducing a separate transient wake flag.

### Attach

`attach` is a first-class liveness transition:

- it clears per-session suspend on the target bead or named identity
  before reconciliation evaluates startability
- it sets the same one-shot start intent as explicit wake
- for `reserved-unmaterialized` named identities it materializes the
  canonical bead and requests immediate start
- against a `closing` bead ID it fails
- against a `closing` named identity it is retained as successor demand
  on that identity and re-evaluated after the close barrier

`attach` is therefore stronger than passive inspection: it is explicit
operator intent to make the target live and attachable now.

### Suspend

Per-session suspend is a runtime override, not a config edit:

- may apply even to config-managed `mode=always` named sessions
- suppresses wake until explicit wake or attach clears it
- may materialize an unmaterialized named session directly into
  suspended/held state without starting runtime

Per-session suspend is a hard blocker. `pin_awake` may remain set while
the session is suspended, but it has no effect until the suspend hold is
cleared.

### Pin awake

`pin_awake` is a first-class per-session override:

- durable explicit wake reason
- suppresses idle sleep and no-wake-reason sleep
- may materialize and wake a reserved named session
- visible in normal status as a wake reason

`pin_awake` does not override hard blockers:

- backing config suspended
- backing config missing/orphaned
- per-session suspend
- explicit terminal close
- crash quarantine

While a temporary blocker exists, the pin remains set. If the blocker is
later cleared, the session becomes wake-eligible again without needing
to be re-pinned.

`unpin` removes only the durable pin wake reason. It does not force an
immediate stop.

`close` clears `pin_awake` because the bead itself is being terminated.
Non-terminal restarts preserve it.

### Wake-cause precedence

The reconciler should combine causes with one invariant order:

1. terminal close wins completely and clears bead-scoped overrides
2. hard blockers suppress start even if wake causes exist
3. durable wake causes remain recorded while temporarily blocked:
   `mode=always`, `pin_awake`, assigned-work continuity, and live
   dependency demand
4. one-shot wake causes request immediate start but do not pin:
   explicit wake, attach, bound inbound event, and newly assigned
   concrete work
5. lack of durable or one-shot causes allows normal sleep/drain policy

One-shot wake intents are retained only until one of three outcomes:

- they are consumed by a successful transition into `creating`
- they are invalidated by terminal close of the target bead/identity
- they are explicitly replaced by a stronger operator action

Hard blockers may defer a one-shot wake, but they do not create a second
independent wake queue.

Close/wake races follow one invariant:

- wake never reopens a closing or closed bead
- bead-ID-targeted wake against a closed/closing bead fails
- named-identity-targeted wake or continuity after close resolves
  against the post-close desired state and, if still valid, targets the
  fresh canonical bead rather than the terminating one

For configured named identities, once close is accepted for bead `B`,
`B` is permanently excluded from future wake, adoption, and continuity
targeting. Any replacement bead may only appear after `B` has crossed
the terminal close barrier in the canonical materialization guard path.

`closing`-window intents are handled exactly as follows:

- bead-ID-targeted wake/attach/delivery against a `closing` bead fails
  for every origin
- named-identity-targeted demand received while bead `B` is `closing` is
  retained against the named identity, not against `B`
- once `B` crosses the close barrier, retained named-identity demand is
  re-evaluated against the successor identity state and may materialize
  or wake a fresh canonical bead
- ephemeral/manual sessions have no successor-identity rewrite; exact
  bead-targeted callers must retry explicitly after close if they want a
  new session

For policy-desired `mode=always` named sessions, fresh rematerialization
is same-pass desired once the old bead has crossed that close barrier.
The controller must not leave the identity undesired waiting for a later
independent demand edge.

## Cap Accounting

### `max_active_sessions`

`max_active_sessions` is a config-wide concurrency bound.

It counts all active or creating sessions from that config, regardless
of origin:

- named
- ephemeral
- manual

Asleep, drained, archived, orphaned, and closed sessions do not count.

A config with a finite `max_active_sessions` may not declare more
`mode=always` named sessions than that cap at config-load time. That is
an invalid config, not a runtime guess.

Manual sessions are explicit user actions. They may soft exceed the cap
the same way other explicit continuity actions do; generic automation is
the only thing denied under sustained over-cap pressure.

### `min_active_sessions`

`min_active_sessions` applies only to generic ephemeral sessions. Named
and manual sessions have their own lifecycle contracts.

### Generic demand satisfaction

For generic config demand:

- only active + creating ephemerals satisfy desired count
- asleep, drained, and archived do not
- assigned-work continuity may revive a specific non-closed session, but
  that is not generic demand reuse

### Soft cap for explicit continuity

Explicit continuity-targeted actions may soft exceed the cap:

- explicit user-targeted wake/attach
- explicit `gc session new`
- inbound external event on an active binding for a concrete session
- durable `pin_awake`
- rematerialization of a policy-required `mode=always` named session if
  temporary runtime conditions have already consumed all headroom

While over cap:

- automation may not start more generic sessions
- the controller does not forcibly kill sessions just to get back under
- the system returns to normal only as sessions naturally sleep, drain,
  close, or unpin
- policy-required named sessions still outrank generic automation; they
  must not be left undesired just because generic load already filled
  headroom

Phase 1 treats simultaneous explicit continuity causes as intentionally
soft-unbounded. The controller does not invent a second refusal rule for
explicit user or continuity-directed actions; infrastructure/resource
limits outside this model remain the backstop.

The starvation rule is explicit: policy-required named sessions and
exact continuity-targeted sessions remain desired even under sustained
soft over-cap pressure, while generic automation stays blocked.

For named `mode=on_demand` continuity retention:

- a materialized non-running bead remains `desired-asleep` while it
  still carries concrete continuity anchors such as owned work, active
  binding continuity, `pin_awake`, or explicit per-session suspend
- once no such anchor remains, ordinary idle reconciliation may retire
  the bead non-terminally to `drained`
- `archived` is not the normal idle outcome for a healthy named
  `on_demand` bead; it remains reserved for failure/quarantine/history

## Named Sessions and Dependencies

Dependencies remain config-to-config relationships.

Generic dependency satisfaction rules:

- if a config has exactly one named session, it is the implicit default
  named satisfier
- if multiple named sessions exist and exactly one is explicitly marked
  as the default satisfier, that named session is used
- otherwise generic dependency satisfaction uses ephemeral sessions only
  and must not pick an arbitrary named session

Named sessions may satisfy generic dependencies. Dependency wake is its
own wake cause and does not require synthetic `assignee` or
`gc.routed_to`.

Phase 1 may defer the explicit "default dependency satisfier" config
field unless a real city requires it immediately. Until that field
exists, any config with multiple named sessions behaves as "no named
default available" and therefore uses ephemeral dependency
satisfaction only.

## Close and Retirement Semantics

### Explicit close

`gc session close` is terminal for the bead being closed.

It must:

- fail if the session still owns open or `in_progress` work
- require explicit requeue, migrate, or unassign before close succeeds
- avoid any implicit transfer of ownership to a fresh bead
- avoid any Phase 1 `--force` escape hatch

### Config-managed named sessions

Closing a config-managed named session closes the current bead only.

If the named identity remains desired by policy:

- `mode=always` rematerializes a fresh canonical bead immediately
- `mode=on_demand` rematerializes only on the next real demand or
  explicit targeting event

This keeps `close` terminal for the old conversation while keeping
desired-state semantics coherent for the configured identity.

`close` also terminates bead-scoped overrides such as suspend and
`pin_awake`. A fresh rematerialized bead starts from config defaults and
new runtime causes, not from the old bead's per-instance overrides.

This is deliberate for `suspend + close` as well: closing a suspended
named bead clears the hold with that bead. A replacement canonical bead,
if policy later rematerializes one, starts unsuspended unless a new
per-session suspend is applied to that replacement bead.

For `mode=always` named identities, the post-close projection is exact:

- if no remaining hard blocker applies after the close barrier, the
  identity projects `desired-running` and reconciliation may mint the
  fresh canonical bead in the same pass
- if some other hard blocker still applies, the identity projects
  `reserved-unmaterialized + desired-blocked` until that blocker is
  cleared or some other materialization rule requires a bead

`closing` is a transient controller barrier, not a stable bead state. It
means close has been accepted for a bead and the canonical-materialization
guard path is preventing any new continuity from targeting that bead
before terminal close completes.

`closing` inherits cap accounting from the bead's last base state until
terminal close commits:

- if the bead was `active` or `creating`, it continues to occupy that
  cap slot until the close barrier completes
- otherwise it does not count toward cap

`closing` is observable only as controller metadata/status, not as a new
base lifecycle state.

### Controller-owned retirement

Controller-owned retirement must stop using `closed` as a generic
inactive state. Use non-terminal states such as:

- `drained`
- `suspended`
- `orphaned`
- `archived`

This removes the need for "reopen closed bead" continuity semantics.

### Config removal and re-add

Removing a configured named session:

- releases the alias immediately
- tears down the session immediately
- retires it non-terminally

Re-adding the same identity may re-adopt that retired bead by exact
identity match only. Never use heuristics like template or historical
runtime name for re-adoption.

For configured named sessions, exact identity match means the fully
qualified `configured_named_identity` string only. Template, alias
history, `session_name`, and prior routing metadata must not participate
in re-adoption.

Re-adoption is allowed only from non-terminal retired states such as
`drained`, `archived`, or `orphaned`. `closed` beads and duplicate-bead
losers are never re-adoption sources.

Re-add adopts only the unique retained retired bead for that exact
identity, meaning a non-terminal retired bead with matching
`configured_named_identity` and `continuity_eligible=true`. If no such
retained retired bead exists, re-add mints a fresh canonical bead.

If more than one eligible retired bead exists for the same fully
qualified configured named identity, re-adoption uses deterministic
selection:

1. highest `generation`
2. latest canonical materialization ordering metadata within that
   generation
3. latest retirement timestamp as final tiebreaker

All non-winning candidates remain retired and diagnostics surface the
anomaly.

When config removal retires a named-session bead, the bead keeps
`configured_named_identity` only as exact-match historical metadata for
possible re-add adoption. That metadata no longer reserves namespace or
claims canonical occupancy once the config entry has been removed.

Config-removed named beads are outside the active canonical uniqueness
set until the config identity is reintroduced. Duplicate-canonical
repair and ordinary named-identity occupancy accounting ignore them
until re-add makes that qualified identity live again.

They remain only as historical retained bead records. They are not
ordinary active/open named-session occupants for wake, attach, or
generic status accounting while the config entry is absent.

For a configured named identity, the "current canonical bead" is the
unique open bead whose `configured_named_identity` exactly matches that
fully qualified identity. If no such open bead exists, the identity is
currently unmaterialized.

## Config Drift and Restart

Any config fingerprint mismatch is non-deferrable. The old
"attached-session deferral" should be removed.

Rules:

- active sessions hold their cap slot and transition through the unified
  live-restart path back into `creating`; they do not expose a temporary
  generic-cap vacancy mid-restart
- creating sessions that must restart stay within the same guarded
  start/restart path rather than creating a second bead
- non-live non-terminal sessions repair drift in place, mark
  continuation reset as needed, and use the fresh provider conversation
  on their next start
- reserved-but-unmaterialized identities update their projected config
  state without forcing materialization
- restart uses the unified continuation-reset path
- the next wake creates a fresh provider conversation

For named sessions:

- changing `name` means a new identity and is semantically remove-old
  plus add-new
- changing `template` preserves the named identity but resets provider
  conversation continuity

`active -> drained` is controller retirement, not ordinary idle sleep.
Idle/no-cause behavior is `active -> asleep`; controller scale-down or
job-complete retirement is `active -> drained`.

Configured named-session aliases are config-owned, not rename history:

- alias and `configured_named_identity` must match
- drift is repairable metadata drift
- configured aliases do not accumulate `alias_history`
- old configured aliases are released immediately on rename or removal

## External Bindings

External bindings are orthogonal to origin.

A session may be:

- `origin=ephemeral` plus a Discord binding
- `origin=named` plus a mailbox binding
- `origin=manual` with no binding

Rules:

- active binding continuity routes to the exact bound session
- non-terminal inactive bound sessions may revive on inbound events
- explicit `close` ends the binding path as well
- bound inbound continuity may soft exceed `max_active_sessions`
- suspend still blocks wake even for bound sessions

## Provider Compatibility

"Provider session" should not remain a separate ontology.

Provider-oriented entry points normalize to the equivalent explicit or
implicit agent config at the boundary. The created session is then an
ordinary config-backed session with whatever origin applies.

Phase 1 may preserve current provider-entry compatibility behavior:

- synchronous creation
- immediate option-default application
- immediate initial-message delivery
- `201 Created`

But those are compatibility details at the boundary, not a second
internal lifecycle model.

Provider names are resolution sugar, not automatic persisted aliases.

## Status and Diagnostics

### Status

User-facing status should move toward two top-level concepts:

- agent configs and their capacity/policy
- sessions and their state/origin

Phase 1 does not require terminology cleanup everywhere, but new logic
and new docs should stop relying on "pool vs non-pool" as ontology.

Config-aware status may synthesize:

- reserved but unmaterialized named sessions
- materialized inactive named sessions
- active named sessions

Bead-oriented listings remain bead-only.

### Diagnostics

`gc doctor` should report, without auto-fixing:

- work beads whose `assignee` points at a missing or closed session bead
- stale or unknown `gc.routed_to` identities
- alias/config conflicts that leave a reserved identity or manual alias
  unresolved
- other migration-visible identity drift introduced by permissive
  low-level compatibility paths

Config load and reconciliation should surface alias/config conflicts
proactively, not only when a later targeting operation fails.

### Public error taxonomy

Phase 1 surfaces should converge on this public error set:

| Error | Meaning | Typical trigger | Notes |
|---|---|---|---|
| `session_not_found` | no session-family target exists | session-targeting miss | no materialization attempted unless the surface allows named materialization |
| `factory_not_found` | no config target exists | factory-targeting miss | never consults session namespace |
| `ambiguous_session_target` | multiple session-family candidates remain | bare session token conflict | qualification required |
| `ambiguous_factory_target` | multiple config candidates remain | bare factory token conflict | qualification required |
| `configured_named_conflict` | configured named identity is reserved but blocked by another session-side claimant | named alias collision | must fail closed |
| `qualification_required` | bare token cannot be resolved safely under current scope rules | city/rig ambiguity or no ambient rig | caller must qualify |
| `target_closing` | concrete target bead is closing and cannot accept new bead-ID-targeted work | close race on exact bead | named-identity successor demand may still exist separately |
| `invalid_surface_class` | surface attempted an illegal resolver family or fallback | implementation bug / invalid API path | should never be silently coerced |

### Materialization contract

| Surface | Requires existing concrete session? | May materialize configured named session? | Must synchronously ensure liveness? |
|---|---|---|---|
| `gc session attach` | no for named; yes otherwise | yes | yes |
| `gc session wake` | no for named; yes otherwise | yes | yes |
| `gc session suspend` | no for named; yes otherwise | yes, into held state | no |
| `gc mail` | no for named; yes otherwise | yes | no |
| `gc session nudge` | no for named; yes otherwise | yes | no |
| bare `gc sling <target>` / direct session-targeted workflow ownership | no for named; yes otherwise | yes | no |
| `gc session close` | yes | no | n/a |
| `gc session new <config>` / factory create | no | n/a | follows create surface contract |

Compatibility-only readers are not normal operator surfaces. Their only
allowed materialization side effect is the exact configured named
identity rewrite path described in the compatibility appendix.

### Status contract

Config-aware status for each configured named identity should expose at
least:

- qualified named identity
- backing template/config identity
- mode
- projected desired state
- projected identity class (`reserved-unmaterialized`, `conflict`, or
  materialized)
- canonical bead ID if materialized
- base bead state
- continuity eligibility
- wake causes
- hard blockers
- degraded yes/no

### Doctor contract

Every `gc doctor` identity/routing finding should include at least:

- finding kind
- affected object ID (`work` bead or `session` bead)
- offending field/value
- relevant named identity or config identity
- rig/city scope context
- collision peer IDs or candidate IDs if any
- one concrete operator action suggestion

Required finding classes include at least:

- `missing-bead-owner`
- `closed-bead-owner`
- `ambiguous-legacy-session-token`
- `legacy-token-matches-config-only`
- `canonical-legacy-divergence`
- `stale-routed-config`
- `configured-named-conflict`

### Degraded health rule

City status is degraded whenever any `mode=always` named identity is not
currently satisfiable as running because it is:

- `desired-blocked`
- `conflict`
- `orphaned`
- repeatedly failing/quarantined in start paths
- under unresolved duplicate-canonical repair

## Persistence and Normalization Contract

Canonical behavior must follow field-specific rules:

| Field | Accepted legacy input | Canonical stored form | Normalization trigger | Must fail closed when |
|---|---|---|---|---|
| `assignee` | open bead ID, current alias, current `session_name`, exact configured named identity, limited template-era exact-to-named token | concrete session bead ID | any ownership mutation or compatibility rewrite | token is ambiguous or resolves only through config/factory namespace |
| `template` | legacy qualified/unqualified config identity | canonical qualified config identity | any session-bead rewrite or create | rig-scoped legacy token is not uniquely mappable |
| `configured_named_identity` | implicit `name=template` compatibility shape | canonical qualified named identity | named-session create/reconcile | multiple configured identities would result |
| `gc.routed_to` | older config tokens | canonical qualified config identity | any workflow/config-routing mutation | no unique config target exists |
| `gc.execution_routed_to` | older config tokens | canonical qualified config identity | control-dispatch mutation | no unique config target exists |
| `session_origin` | legacy marker combinations | `named`, `ephemeral`, or `manual` | session create or touched-bead normalization | canonical fields are absent and legacy inputs conflict |

Canonical fields always govern runtime behavior. When canonical and
legacy hints disagree on the same record:

- canonical fields drive behavior
- legacy fields survive only for diagnostics or compatibility rewrite
- `gc doctor` must report the divergence explicitly

Compatibility-triggered named-session materialization used for ownership
rewrite must be retry-safe. One normalized legacy owner token must
produce at most one canonical materialization attempt and one canonical
rewrite outcome under concurrent readers.

## Workflow Ownership Contract

Allowed ownership/routing states:

| Work state | `assignee` | `gc.routed_to` | Meaning |
|---|---|---|---|
| generic unclaimed | absent | config identity present | generic config demand |
| generic claimed | bead ID present | config identity may remain as provenance | continuity belongs to `assignee`; route is non-operative provenance |
| direct session-targeted | bead ID present | absent | direct concrete ownership |
| explicitly unassigned/requeued | absent | config identity present or re-added intentionally | generic demand again |

Atomic claim invariant:

- once a valid concrete `assignee` is present, generic-demand accounting
  must exclude that work item regardless of preserved provenance fields
- retry/continuity follows `assignee` only
- retained `gc.routed_to` must not re-activate generic routing unless an
  explicit unassign/requeue action clears ownership

Low-level permissive assignment APIs may still store malformed data in
Phase 1, but the controller must never derive ownership by reconciling
`assignee` and `gc.routed_to` together. Invalid combinations are
diagnostic problems, not alternate routing instructions.

## Provider Boundary Contract

Phase 1 provider-create compatibility normalizes to ordinary
config-backed session creation with these exact rules:

- resulting `session_origin` is `manual`
- provider-create is fresh-session factory creation only; it does not
  silently resume prior sessions by provider identity
- provider-create and `gc session new <config>` must converge on the
  same persisted session schema (`template`, `session_origin`,
  configured named metadata if applicable, lifecycle intent metadata,
  and option/override persistence)
- the compatibility difference is boundary behavior only: sync response,
  immediate option-default application, and optional immediate first
  message delivery

For synchronous provider-create plus initial message:

- bead/session creation must be durable before returning success
- if first-message delivery fails after session creation succeeds, the
  caller still receives the concrete session identity and may inspect or
  retry against that session explicitly
- partial success must never fabricate a second session on retry when
  idempotency keys or equivalent request identity is available

## Dependency and Capacity Edge Rules

- dependency-driven starts stay in the automation lane unless they are
  resuming an already exact continuity-anchored bead; they do not gain
  soft-cap exception merely because the satisfier is named
- repeated one-shot explicit wake/attach/inbound events must deduplicate
  by target identity per reconcile pass; they do not create multiple
  over-cap entitlements for the same target
- `min_active_sessions` is a generic floor input, not a stronger claim
  than named/manual continuity under over-cap pressure
- the implicit single-named-session dependency satisfier rule is a
  temporary compatibility shortcut; adding a second named session
  disables that implicit named satisfier until an explicit default is
  configured

## Compatibility and Migration

Phase 1 is incremental, not a bulk rewrite.

Rules:

- read legacy data
- write canonical data for new or mutated records
- opportunistically normalize touched records
- avoid one-shot mandatory store migration
- legacy ownership tokens may normalize to bead ID only through
  session-namespace rules, never through config/template fallback

Legacy fields such as `pool_slot`, `pool_managed`, and `manual_session`
may still be read during migration, but new writes should converge on:

- `session_origin`
- canonical `template`
- canonical `assignee=<bead-id>`
- `configured_named_identity` where applicable

### Compatibility appendix

Phase 1 permits legacy-token interpretation only on the following
surfaces:

| Surface | Examples | Class | Historical alias consults | May materialize reserved named session? |
|---|---|---|---|---|
| stored ownership/reference readers | legacy `assignee`, legacy stored session refs | compatibility-only | yes, only as explicit compatibility translation | yes, but only when exact configured named identity wins ownership normalization |
| provider-entry normalization boundary | `kind=provider`, provider-name create inputs | factory-targeting compatibility shim | no | no; normalize to config target first |
| debug/doctor/migration tools | doctor checks, explicit migration repair tools | compatibility-only | yes | no unless the tool explicitly invokes a session-targeting surface afterward |

No other Phase 1 surface may ingest historical alias, provider-era name,
template-era session token, or other legacy identifier form.

The accepted legacy token taxonomy is closed:

- open bead ID
- current alias
- current `session_name`
- exact configured named identity
- historical alias where the matrix permits it
- template-era token that exactly matches a configured named identity

Any other legacy string fails closed.

Stored ownership/reference readers are stricter still. They accept only:

- open bead ID
- current alias
- current `session_name`
- exact configured named identity
- historical alias only if no current reserved named identity or other
  current session-side candidate matches that token
- template-era token only if it exactly maps to configured named
  identity under current qualification rules

Compatibility precedence matrix:

| Surface | Open bead ID | Current alias | Current `session_name` | Exact configured named identity | Historical alias | Template-era exact-to-named translation | Config/factory lookup |
|---|---|---|---|---|---|---|---|
| stored ownership/reference readers | yes | yes | yes | yes | yes, only if still unambiguous after current session-side checks | yes | never |
| provider-entry normalization boundary | no | no | no | no | no | no | factory-only after sugar expansion |
| debug/doctor/migration tools | yes when inspecting current state | yes | yes | yes for reporting | yes | yes for reporting only | never unless the tool explicitly invokes a factory-targeting action |

The surviving public boundary sugar set is intentionally tiny:

- `template:` for explicit factory targeting
- provider-entry normalization on provider-create boundaries only

There are no other sanctioned public legacy shorthands that may resolve
config identity from a bare token in Phase 1.

The only Phase 1 provider-name sugar surfaces are provider-create
compatibility entrypoints, including the direct provider session-create
API/CLI boundary and equivalent request wrappers that normalize to the
same factory-targeting path. No other public endpoint may accept
provider-name sugar.

Provider compatibility normalization is boundary-contained and ordered:

1. classify the incoming surface
2. if and only if the surface is factory-targeting, expand provider-name
   sugar into explicit-or-implicit config identity
3. run ordinary factory-targeting resolution

Provider/name sugar must never run on session-targeting surfaces or on
compatibility-only session-reference readers.

Any provider-era name that maps to more than one factory candidate is
ordinary factory ambiguity and must fail with the same
qualification-required/error shape as any other factory-targeting bare
token.

No ambiguous or partially normalized provider result may be persisted.

Provider-create compatibility endpoints must register as
`factory-targeting` in the same resolver-class registry as other
target-bearing surfaces. They may not implement separate ad hoc fallback
logic.

Worked ambiguity cases:

- `gc sling mayor ...` where named session `mayor` shadows config
  `mayor`: session-targeting, resolves to the named session
- `gc sling template:mayor ...` in the same city: factory-targeting,
  resolves to config `mayor`
- stored legacy `assignee=mayor` while both named session `mayor` and
  config `mayor` exist: compatibility reader stays in the session
  family or fails closed; it never falls through to config
- stored legacy `assignee=mayor` where `mayor` is a configured named
  session but currently unmaterialized: ownership compatibility may
  materialize the canonical named bead, then rewrite `assignee` to that
  bead ID
- compatibility-only read of historical alias `mayor` after `mayor`
  becomes a reserved configured named identity: fail closed unless the
  token is the exact configured named identity form being translated
- provider-create input `name=mayor` while named session `mayor` exists:
  factory-targeting provider normalization runs only inside the
  provider/create boundary and resolves only to config identity; if
  config resolution is ambiguous or absent, it fails instead of stealing
  session-targeted traffic

Compatibility-only resolution algorithm:

1. the caller must already be a registered `compatibility-only` surface
2. accept only stored legacy session-reference tokens, not live
   user-command targets
3. if the token is an unqualified rig-scoped form and no ambient rig is
   defined, reject it; do not search the whole city heuristically
4. consult open bead ID, current alias, current `session_name`, exact
   configured named identity, and historical alias only if this appendix
   says that surface may consult that class
5. never consult generic `template`, config identity, or factory
   namespace; template-era compatibility is allowed only through exact
   configured named identity translation
6. on miss or ambiguity, fail closed and surface diagnostics rather than
   retrying another namespace

If a compatibility-only unqualified token could refer to both a
city-scoped identity and a current-rig identity, it is ambiguous and
must fail closed exactly as normal runtime resolution would.

Inspection helpers such as status/doctor must use the same declared
resolver class as command/API paths for the identifier they are
reporting. They must not silently widen lookup rules just because the
surface is read-only.

Compatibility-only translation helpers are a separate resolver entry
family. Normal session-targeting and factory-targeting commands must not
call them as fallback after ordinary lookup fails.

Compatibility-triggered materialization of a named session is allowed
only after the token has already resolved as an exact session-family
match under this compatibility matrix. It is never part of exploratory
lookup.

For ownership/reference readers specifically:

- exact configured named identity may trigger canonical-bead
  materialization for rewrite
- template-era exact-to-named translation may trigger that same
  materialization only after the exact configured named identity match is
  established
- current alias, current `session_name`, and historical alias matches do
  not create or reserve sessions; they only bind to existing concrete
  beads

Compatibility collision example:

- compatibility-only read of bare `mayor` when city-scoped named
  identity `mayor` and current-rig named identity `demo/mayor` both
  exist: fail closed and require qualification, even if the historical
  data predates qualification support

Historical aliases must never outrank or capture a token that is
currently reserved as a configured named identity. On a
compatibility-only surface, that collision fails closed unless the
surface is explicitly translating exact configured named identity.

If exact configured named identity and a different open bead alias both
match due to corruption, compatibility resolution fails closed and
diagnostics surface the corruption. Exact configured named identity does
not silently steal the write in that case.

## Phase Plan

### Phase 0: red test matrix and gap analysis

Before production code changes:

- write the full desired-behavior test matrix
- land it as the first deliberately red commit on the branch
- use the failing set as the formal gap analysis

The spec suite should be primarily deterministic and exercise semantic
boundaries:

- resolution and namespace behavior
- lifecycle and wake logic
- demand accounting and cap behavior
- metadata writes and compatibility reads
- workflow routing and retry behavior
- config evolution and re-adoption paths

Minimum Phase 0 red matrix dimensions:

- token class: bead ID, configured named identity, current alias,
  current `session_name`, historical alias, template-era exact-to-named
  token, provider-name sugar
- surface class: session-targeting, factory-targeting,
  compatibility-only
- scope condition: city-scoped, current-rig, cross-rig ambiguity, no
  ambient rig
- materialization state: open named bead, reserved-unmaterialized named
  identity, closed bead, duplicate/corrupt candidates

Mixed-era red cases must include at least:

- legacy `assignee` colliding with a current config name
- historical alias that is now a reserved configured named identity
- unqualified rig-scoped legacy token with no ambient rig
- provider-create sugar colliding with a same-named session identity
- touched-record rewrite that requires canonical bead materialization

### Phase 1: internal cleanup with compatibility shims

- no deliberate user-facing CLI/API/docs rename
- canonicalize new behavior and data model
- preserve compatibility on read paths and raw low-level surfaces where
  needed

Phase 1 sequencing is normative. These must land together or behind one
gate:

- resolver surface-class registry
- canonical write-path changes
- compatibility-reader narrowing
- doctor/status visibility for ambiguity and stale references
- the red spec suite covering the public surface inventory

### Phase 2: optional external cleanup

Possible later work:

- terminology cleanup in CLI, API, and docs
- explicit `pin/unpin` command surface
- stricter validation on raw assignment APIs
- further removal of legacy pool language from user-visible flows

## Exit Criteria

The old pool/no-pool ontology is behaviorally dead only when all of the
following are true:

- every public target-bearing surface is classified in the registry and
  covered by the Phase 0 matrix
- no bare session-facing token can create from config implicitly
- no compatibility reader can fall through into config/factory lookup
- new writes use only canonical ownership/routing/origin forms
- diagnostics surface every accepted mixed-era drift case without
  guessing intent
- there is no remaining runtime decision that depends on `pool_slot`,
  `pool_managed`, `manual_session`, or similar pool-era semantic flags

## Alternatives Rejected

### Keep `[[agent]]` as both config and identity

Rejected because capacity settings would continue to change routing and
resolution semantics.

### Use alias or `session_name` as canonical `assignee`

Rejected because ownership should be one exact concrete session token.
Human readability belongs in formatters and UI, not persistence.

### Keep `work_query` as a controller wake/materialization signal

Rejected because it double-counts demand and conflates session-local
introspection with controller desired-state logic.

### Preserve provider sessions as a separate internal kind

Rejected because provider-vs-agent is a config concern, not a lifecycle
identity concern.

## Implementation Notes

The existing code already contains partial seams this design can reuse:

- `pending_create_claim` for one-shot start requests
- continuation-reset metadata for fresh provider conversations after
  restart
- `held_until` and `sleep_intent` for suspend-style negative overrides
- config injection of implicit provider agents

Implementation should prefer consolidating on those seams rather than
adding parallel mechanisms unless the behavior truly differs.
