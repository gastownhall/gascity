---
title: "Named Configured Sessions"
---

| Field         | Value                                 |
| ------------- | ------------------------------------- |
| Status        | Accepted                              |
| Date          | 2026-03-23                            |
| Author(s)     | Codex                                 |
| Issue         | N/A                                   |
| Supersedes    | N/A                                   |
| Superseded by | session-model-unification (partially) |

> Note
>
> This document introduced `[[named_session]]`. The accepted follow-on
> design in [`session-model-unification`](session-model-unification.md)
> keeps that concept but replaces the remaining pool/non-pool split with
> one unified session model.

## Summary

Gas City now treats `[[agent]]` primarily as a template catalog. That is
the right direction, but some cities still need one canonical persistent
session for a template identity such as `mayor`, `deacon`, or
`repo/refinery`. The missing feature is not "managed singleton agents"
that are always awake. It is a first-class configured session identity:
a stable alias-backed session that is declared in config, can be
materialized on demand, and can optionally be kept always on.

This design adds `[[named_session]]` as a first-class config object.
Named sessions reference a non-pool agent template, reserve a stable
public target, and define whether the controller should keep them
continuously alive or only materialize them when referenced or when work
requires them.

The resulting model is:

- `[[agent]]` defines reusable templates
- `[[named_session]]` declares canonical persistent sessions built from
  those templates
- pool agents remain elastic workers
- ad-hoc `gc session new` chats remain ordinary sessions, not
  config-managed singletons

## Problem

The old model conflated template existence with desired uptime. Any
non-pool configured agent implicitly behaved like an always-on singleton,
which caused several problems:

1. saved sessions derived from the same template were mistaken for the
   canonical config-managed session
2. configured singleton names were referenced by mail, nudge, and sling,
   but there was no explicit session object behind them
3. idle-kill or idle-sleep behavior interacted badly with unconditional
   config wake
4. status and routing code carried template-name fallbacks instead of
   resolving through one canonical session identity

After the recent "templates only" change, the opposite problem appeared:
the implicit always-on behavior was gone, but the product still lacked an
explicit way to say "this template has one canonical session target."

We want to keep sessions first class while still supporting configured
canonical sessions where they make sense.

## Goals

- Keep `[[agent]]` as a reusable template definition, not an implicit
  runtime singleton.
- Add an explicit config object for canonical persistent sessions.
- Give named sessions a stable alias so `gc sling`, `gc mail`, `gc
nudge`, attach, and workflow routing can target the same identity.
- Support both:
  - `always`: controller-kept sessions like `deacon`
  - `on_demand`: lazy sessions like `mayor` or `refinery`
- Ensure work-driven wake can create the canonical session when no bead
  exists yet.
- Prevent ad-hoc sessions created from the same template from being
  treated as the configured singleton.
- Preserve pool behavior and keep pool configuration as the right tool
  for elastic workers, not named singletons.

## Non-goals

- Replacing pools with named sessions.
- Adding arbitrary per-session prompt/provider/workdir overrides in this
  first version.
- Supporting custom public aliases that differ from the routed template
  identity in the first version.
- Reintroducing "all non-pool agents are managed singletons."

## Current Behavior

Current `origin/main` behavior has three important properties:

1. non-pool configured agents are templates only; `gc start` does not
   auto-create their canonical sessions
2. several ingress paths still carry configured-singleton fallbacks
   based on the template name rather than a real canonical session bead
3. on-demand session creation for fixed targets mints ordinary sessions,
   but not via a canonical alias-backed path

That means the system currently lacks one central answer to:

> "What is the single canonical session for template `mayor`?"

## Proposed Design

### 1) Add `[[named_session]]`

Introduce a new first-class config object at city level and in packs:

```toml
[[named_session]]
template = "mayor"
scope = "city"
mode = "on_demand"

[[named_session]]
template = "deacon"
scope = "city"
mode = "always"

[[named_session]]
template = "refinery"
scope = "rig"
mode = "on_demand"
```

Fields:

- `template` (required): template name from `[[agent]]`
- `scope` (optional): `city` or `rig`, matching pack stamping semantics
- `mode` (optional): `on_demand` or `always`, default `on_demand`
- `dir` (runtime/stamped): populated during rig pack expansion exactly
  like agent `dir`

The canonical public alias of a named session is its qualified identity
after stamping:

- city scope: `mayor`
- rig scope: `repo/refinery`

More generally, the qualified identity for a rig-scoped named session is
`<rig-identity>/<template>`, where `<rig-identity>` is the stamped rig
identity after pack expansion.

In v1, the alias is intentionally fixed to the qualified identity. This
keeps the public target aligned with the template's default mail/work
identity and avoids splitting alias routing from `GC_AGENT`,
`work_query`, and `sling_query`.

The stored canonical alias never includes a trailing slash. Commands
that already accept mailbox-style targets such as `mayor/` normalize one
trailing slash away before resolution, so `mayor` and `mayor/` refer to
the same configured named-session identity.

The deterministic runtime `session_name` for a named session is:

```text
agent.SessionNameFor(city.name, <qualified-identity>, city.session_template)
```

In other words, named sessions use the same runtime naming function as
other GC sessions, but with the canonical qualified identity as input.
That rule is part of the contract, not an implementation detail. The
public identity is still the alias; `session_name` is the deterministic
runtime slot name derived from the current city naming policy. Renaming
the city or changing `session_template` is therefore treated as runtime
identity drift, not as transparent continuity. In v1, GC does not
auto-adopt across that drift.

### 2) Named sessions reference templates, they do not replace them

The referenced `[[agent]]` remains the source of:

- prompt
- provider/start command
- work directory
- work/sling query
- wake mode
- idle timeout / idle sleep policy
- dependencies

`[[named_session]]` only adds runtime identity and controller policy.

This keeps templates reusable:

- `gc session new mayor` still creates an ordinary ad-hoc session from
  the `mayor` template
- `mayor/` as a target refers to the canonical configured named session
  if one exists
- `template:mayor` explicitly creates a fresh ordinary session from the
  `mayor` template even when a canonical configured named session also
  exists

In v1, fresh-template targeting is available through both
`gc session new <template>` and `template:<template>` on
session-targeting command surfaces. Bare configured names resolve in
priority order: existing session/session_name first, then existing live
alias, then template fallback.

Ad-hoc sessions created from a named-session-backed template always use
the ordinary session namespace, never the configured canonical one. In
v1 that means:

- they get the normal auto-generated runtime `session_name`, not the
  reserved deterministic named-session `session_name`
- they do not inherit the configured alias automatically
- any user-supplied alias must still pass the configured-identity
  reservation checks
- they never publish as, consume work for, or otherwise resolve as the
  configured canonical identity
- their mailbox/work routing identity is their own ordinary session
  identity (explicit alias if any, otherwise session-specific identity),
  not the configured qualified template identity

When a canonical named session is materialized, the opposite is true:
its runtime and template-scoped routing context stays bound to the
configured qualified identity. `GC_AGENT`, the template's `work_query`,
and the template's `sling_query` all execute in that configured
qualified-identity context. Ordinary sessions never consume
configured-target queue items even if they later own the same
human-visible alias.

### 3) Materialization modes

Named sessions support two controller modes.

#### `mode = "on_demand"`

The alias is reserved immediately, but the canonical session bead is
created only when needed.

Creation triggers are:

- explicit reference from `gc sling`, `gc mail`, `gc nudge`, attach, or
  similar command paths
- controller work detection for the qualified named-session identity
  (`work_query` is evaluated in that identity's stamped runtime context
  and attributed only to that identity)
- dependency wake when another realized session needs this template awake

Once created, the bead persists as the canonical session history record.
It is not auto-closed just because the process is asleep. If the open
canonical bead is later manually removed or otherwise lost, GC does not
recreate it merely because it once existed. The identity falls back to
`reserved` until a fresh on-demand root appears again.

`on_demand` sessions may use existing `sleep_after_idle` behavior.

An `on_demand` named session whose canonical bead exists but whose
process is asleep and has no active wake root is a stable no-op in the
patrol loop. The controller continues to reconcile the bead as the
canonical record, but it does not treat "bead exists" as a wake reason.
This is the key distinction between "desired record exists" and "process
must be running."

`gc session close` on a canonical `on_demand` session is the operator's
explicit path back to `reserved`. The closed bead remains normal history
and may retain the configured-session discriminator metadata for audit,
but closed beads never satisfy live configured-name resolution.

`wake_mode` still applies to the runtime process once the canonical bead
exists. `mode = "on_demand"` with `wake_mode = "fresh"` is valid: first
materialization still creates the canonical bead once, and later wakes
of that canonical session follow the template's normal fresh/resume
provider semantics.

#### `mode = "always"`

The canonical session is controller-managed. The controller ensures the
session bead exists and includes it in config wake evaluation every
patrol tick.

This is the replacement for the old implicit singleton behavior, but it
is explicit and session-scoped instead of being derived from template
existence.

`always` sessions are not compatible with "sleep when idle" semantics in
practice because config wake will keep restoring them. In v1:

- configs that combine `mode = "always"` with non-`off`
  `sleep_after_idle` are invalid and fail validation
- configs that combine `mode = "always"` with `wake_mode = "fresh"`
  should emit a warning unless the operator is deliberately modeling a
  watchdog or other restart-per-cycle actor

This matches the operator-level mental model: this session is supposed
to be always available.

### 4) Canonical identity and reservation

Named sessions need a single canonical identity before any bead exists.

For each configured named session, GC reserves:

- the public alias: qualified identity such as `mayor`
- the deterministic runtime `session_name`, derived from
  `city.session_template` and the qualified identity

This prevents ad-hoc sessions from squatting on the canonical singleton
name before the configured session is ever materialized.

These reservations are authoritative. If a user-supplied alias or
explicit `session_name` collides with a configured named-session
identifier, session creation or rename is rejected even if no canonical
bead exists yet.

The reservation covers every routable form of the configured public
identity:

- current alias
- live `alias_history` on any non-closed session bead
- deterministic runtime `session_name`

Registry ownership and resolver ownership are the same contract. Any
session-targeting path that accepts alias-like or session-name-like
input must consult the configured named-session registry before ordinary
bead lookup when the token matches any reserved form above. Only exact
session bead IDs bypass this registry-first rule.

Deterministic `session_name` lookups are therefore registry-mediated,
not a bypass namespace. In v1, only exact bead IDs bypass configured
named-session routing.

Closed ordinary history remains readable only by explicit session ID or
explicit closed-session lookup. It must not satisfy the configured
named-session public identity once that identity is reserved.
Configured-identity uniqueness is therefore scoped to live/open
resolution. Closed historical beads do not poison future canonical
materialization of the reserved identity.

When the canonical bead exists, it carries explicit discriminator
metadata:

- `configured_named_session = "true"`
- `configured_named_identity = "<qualified-identity>"`
- `configured_named_mode = "always" | "on_demand"`

These keys distinguish the canonical bead from ordinary sessions created
from the same template.

While the `[[named_session]]` entry exists, the canonical bead's public
identity is immutable in v1:

- its configured alias cannot be manually changed
- its deterministic `session_name` cannot be manually changed
- ordinary rename flows may only change the human title, not the routing
  identity

Any future identity migration must be an explicit dedicated flow, not an
incidental side effect of ordinary session-edit commands.

In v1, GC does not automatically adopt historical ordinary sessions into
configured named sessions. If a live ordinary session already occupies a
reserved alias or deterministic `session_name`, the configured named
session enters `conflict` state and that public target becomes
unroutable until the operator renames, closes, or otherwise resolves the
conflict. Ordinary sessions must never win by accident over a reserved
configured identity.

### 5) Controller mechanics

The desired-state builder changes from "all non-pool agents imply a
singleton" to "only configured named sessions imply a canonical
singleton."

The patrol loop iterates over configured named-session entries on every
tick, not only over existing beads. That is what makes `mode = "always"`
re-creation work even if the bead was lost or manually removed, and it
is also how status can distinguish "configured but not yet materialized"
from "not configured."

Each patrol tick runs against an immutable config snapshot captured at
tick start. Mid-tick config reloads do not partially affect that tick;
they take effect on the next reconciliation pass.

There are three distinct concepts:

- configured registry state: the `[[named_session]]` entry exists
- canonical bead state: the session history/identity record exists
- process state: the runtime session is currently running

The patrol loop is explicitly two-phase:

1. bead reconciliation decides whether the canonical identity should
   exist as a bead-level record in desired state
2. wake evaluation decides whether a realized canonical bead should have
   a running process this tick

Phase 1 inclusion never implies phase 2 wake. This is a hard invariant
of the feature, not a side effect of implementation.

For `on_demand`, bead reconciliation can require the canonical bead to
exist without requiring the process to run. Wake roots still control
process liveness.

Controller work detection runs before Phase 1 bead reconciliation. Its
results feed both phases and use the same immutable config snapshot
captured at patrol-tick start:

- Phase 1, so an `on_demand` named session with pending work can be
  materialized even when no bead exists yet
- Phase 2, so a realized session receives `WakeWork` for runtime
  liveness

If that same config snapshot no longer contains the `[[named_session]]`
entry, discovered work does not re-materialize the removed configured
identity.

#### Phase 1: bead reconciliation inputs

Pools continue to work exactly as they do now.

Named sessions add a second desired-state source:

- `always` named sessions are always part of desired state
- `on_demand` named sessions join bead desired state when:
  - an open canonical bead already exists, or
  - the template has pending work, or
  - dependency wake requires the named-session identity

An `always` named session is visible on a fresh city as a canonical session
bead in `creating` state before the runtime process is confirmed. This is the
same controller-owned creation intent used for any desired session; it is not a
separate operator action.

Dependency wake is evaluated over the validated graph of fully qualified
template identities after pack expansion, not over ambiguous bare
template strings. Each configured named session maps 1:1 to one
qualified identity. If a dependency points at a qualified identity with
a configured named session, that canonical session is the dependency
wake target. If it points at a pool or an ordinary template, existing
pool/template lifecycle rules continue to apply.

Dependency propagation roots are intentionally narrow. They begin only
from:

- `mode = "always"` named sessions
- sessions with a hard wake root in this tick: work, wait, create,
  attach/reference intent, or pending interaction

Reservation alone, open-bead existence alone, and keep-warm/session
wake after prior activity never start new dependency propagation.

Config validation already rejects dependency cycles after expansion. The
runtime propagation logic still keeps a visited set as a safety belt; if
an unexpected cycle is encountered, GC logs a warning and stops
propagating the repeated edge for that tick rather than looping.

#### Bead creation

When the desired builder includes a named session that does not yet have
an open canonical bead, `syncSessionBeads` creates one with:

- the configured alias
- the deterministic configured `session_name`
- `state=creating`
- `pending_create_claim=true`
- metadata marking it as a configured named session

GC never auto-adopts an ordinary historical bead into configured named
session ownership in v1. If a colliding live bead exists, the named
session enters `conflict` state and materialization is blocked until the
operator resolves the collision manually. This is stricter than the old
implicit-singleton migration story, but it preserves the core invariant:
ad-hoc history is never mistaken for the configured canonical session.

Publication of the canonical identity is atomic from GC's perspective.
The initial bead create publishes the alias, deterministic
`session_name`, `state=creating`, `pending_create_claim=true`, and the
configured-session discriminator metadata together under city-scoped
locks for both the alias and the deterministic `session_name`. There is
no "create anonymous bead, then patch it into canonical identity later"
path for named sessions.

The lock contract is:

- city-scoped identifier locks are acquired in deterministic lexical
  order over the alias and deterministic `session_name`
- store-side uniqueness checks run while those locks are held
- if another caller wins the race, the loser re-reads the store and
  resolves the published canonical bead instead of creating a duplicate

If the controller or an ingress command crashes after publishing the
canonical bead but before runtime start completes, the next patrol tick
adopts that open bead in `creating` / `pending_create_claim=true` state
and either completes startup or clears the claim as a failed start. It
must never create a second canonical bead for the same configured
identity.

#### Phase 2: wake behavior

Only canonical named sessions with `mode = "always"` receive
`WakeConfig`.

Ad-hoc sessions created from the same template never receive `WakeConfig`
just because the template exists.

Canonical `on_demand` named sessions wake only for real wake roots:

- create
- work
- wait
- attach/reference intent
- pending interaction
- dependency wake
- keep-warm/session wake after they are already active

If dependency wake or work wake becomes false again, the session simply
returns to ordinary idle-sleep / asleep eligibility. Dependency wake is
an additive wake root, not a sticky "must stay alive forever" marker.
For `on_demand`, `WakeSession` and `WakeKeepWarm` are non-originating
reasons: they may preserve warmth for an already-running process, but
they never create a bead, never cause `asleep -> awake`, and never
originate dependency propagation.
If dependency wake targets a configured named session that is currently
in `conflict`, the dependent session is treated as blocked on conflict
for that tick; GC logs the condition, surfaces the blocked dependency in
status/diagnostics, and does not silently route around the canonical
identity.

Dependency wake is a hard originating wake root for `on_demand` targets.
If it is the reason a target joins Phase 1 desired state, it also
participates in Phase 2 wake evaluation for that target in the same
tick.

If a configured named session with `mode = "always"` is in `conflict`,
GC treats the city as degraded:

- `gc start` reports the conflict loudly in startup diagnostics
- every patrol tick logs a warning until the conflict is resolved
- dependent wakes remain blocked rather than silently targeting an
  ordinary session

Operator lifecycle semantics for canonical `mode = "always"` sessions
are explicit:

- `gc session kill` is a transient process action only; the controller
  may restart the same canonical bead on the next tick
- `gc session suspend` is allowed and acts as an explicit user hold that
  suppresses all automated wake roots until the operator wakes it again
- `gc session close` against the canonical bead is rejected while the
  `[[named_session]]` entry still exists, because permanent destruction
  conflicts with controller ownership

To permanently decommission an `always` named session, the operator
removes or changes the `[[named_session]]` entry first, waits for
downgrade, then closes the now-ordinary session if desired.

### 6) Centralize first-reference materialization

All first-reference ingress paths should resolve through one helper:

1. resolve exact session IDs directly
2. resolve exact live session handles in this order:
   current `session_name`, then current alias, then live alias history
3. normalize alias-like targets (`mayor/` -> `mayor`)
4. if the target is `template:<name>`, create a fresh ordinary session
   from that template
5. otherwise, if the normalized token resolves to a template:
   if the template has a configured named session, materialize or reuse
   the canonical aliased session
   otherwise create a fresh ordinary session from that template
6. otherwise return not found

Configured named identities are authoritative only through their live
canonical alias once materialized. An already-live ordinary session with
alias `mayor` resolves first; `template:mayor` is the explicit escape
hatch when a caller wants a fresh session regardless of alias shadowing.

Trailing-slash normalization is internal to this helper, not a caller
responsibility. `mayor` and `mayor/` normalize to the same target token
before template fallback runs.

This helper replaces scattered configured-singleton fallbacks in:

- `gc mail`
- `gc nudge`
- `gc sling`
- workflow fixed-target routing
- `gc session attach`
- `gc session wake`
- `gc session peek`
- `gc session kill`
- `gc session close`
- `gc session suspend`
- `gc session logs`
- equivalent API session-targeting paths

Explicit session IDs still bypass template fallback entirely.

CLI and API session targeting intentionally differ on ambient context:

- CLI resolution may apply current-rig shorthand first, so a bare target
  like `maya` can resolve as `corp/maya` when the operator is already in
  the `corp` rig context.
- API resolution has no ambient rig shortcut. Bare names only resolve
  when city-unique; otherwise callers must send the fully qualified
  identity or use `template:<qualified-name>`.
- real-world app and other API clients should normalize user-selected
  targets to fully qualified identities before calling GC so rig-scoped
  templates and aliases are always representable.

Centralized resolution does not mean every command materializes a
reserved `on_demand` named session. First-reference behavior is
command-class specific:

- `gc mail`, sling/workflow routing with a configured-session identity,
  `gc nudge`, `gc session attach`, and `gc session wake` may materialize
  the canonical session on first reference
- `gc session wait` does not materialize; it only targets an already
  existing session bead
- `gc session logs` does not materialize; if no canonical bead exists it
  may fall back to the configured template/workdir context for read-only
  log discovery
- `gc session peek`, `gc session kill`, `gc session close`, and
  `gc session suspend` do not materialize a merely reserved identity; if
  no canonical bead exists they return not-found or conflict
- `gc session new <template>` remains its own explicit template-creation
  command and bypasses configured-session resolution entirely
- explicit session IDs always target the existing bead only; they never
  create a new canonical session

The canonical bead is created with the aliased manager path, not as an
anonymous session. That makes alias resolution stable and removes the
need for synthetic "pretend there is a singleton here" logic.

Materialization is serialized by city-scoped reservation locks on both
the reserved alias and the deterministic `session_name`. The
correctness invariant is durable and global from the controller's point
of view: at most one open canonical bead may exist for a configured
named identity. If two callers race, the winner creates the canonical
bead and the loser resolves that winner's result instead of creating a
duplicate.

When the controller is running, first-reference materialization uses the
bead-only create path: GC creates the aliased canonical bead with
`state=creating`, `pending_create_claim=true`, and the configured named
metadata, then pokes the patrol loop for an immediate tick. That maps to
`WakeCreate` on the next evaluation and makes the startup cause visible
in bead metadata. When the controller is not running, the same helper
may create and start the canonical session directly, but it still
persists the same canonical metadata contract.

Ingress materialization performs the same conflict checks as the patrol
loop before attempting creation. If it finds `conflict`, it surfaces
that condition immediately to the caller; it does not create an
anonymous fallback session.

Ingress persistence binds to the reserved configured identity, not to an
incidental session bead. Mail records, work assignees, and other queued
targets are written against a structured configured-target namespace,
not a plain ordinary alias string. Conceptually that target is the
configured identity itself, for example `configured:mayor`, even if the
user-facing command spelled it as `mayor`. Later materialization resolves
that same configured target to the canonical bead; it does not rewrite
queued work to whatever session happened to exist at publish time.

If the `[[named_session]]` entry is later removed, queued mail/work that
is still addressed to the formerly reserved identity does not
automatically transfer to the downgraded ordinary session. Those queued
configured-target records remain unroutable until an operator retargets
them or reintroduces the named-session config entry. Fresh commands
issued after downgrade may still target an ordinary session if that
session now publicly owns the alias; persisted configured-target records
do not.

Failure semantics are ingress-specific:

- `gc mail send` and sling/workflow routing persist the message or work
  assignment first, then best-effort materialize or poke the canonical
  session. If wake fails, the queued work/message remains authoritative.
- `gc nudge` and attach-like flows require a concrete session target. If
  canonical materialization or wake fails, the command returns an error.
- Failed runtime start never creates a second competing bead. At most
  one canonical bead exists for the named session identity.

### 7) Status semantics

Status commands should report configured named sessions as the canonical
singleton runtime entries.

Templates without a corresponding `[[named_session]]` are not shown as
managed singleton runtime entries. They still exist as templates and are
visible in config- or UI-level template listings, but they are not
implicitly listed as always-there sessions.

Named-session status needs to expose at least:

- `reserved` (configured, no canonical bead yet)
- `conflict` (reserved identity occupied by a non-canonical live bead)
- `asleep`
- `awake`
- `creating`
- `draining`
- whether the entry is `always` or `on_demand`

This lets operators distinguish "correctly dormant" from "broken
materialization."

For `mode = "always"`, `conflict` is not a quiet dormant state. Status
and health surfaces should flag it as degraded until an operator fixes
the reservation collision.

### 8) Validation rules

Config validation should enforce:

- `named_session.template` is required
- the referenced template exists after pack expansion
- the referenced template is not a pool
- duplicate named session qualified identities are rejected
- the full reserved-token set is unique after pack expansion and
  deterministic `session_name` derivation:
  - no configured alias may equal another configured alias
  - no configured alias may equal another configured deterministic
    `session_name`
  - no configured deterministic `session_name` may equal another
    configured deterministic `session_name`
- `mode` must be `on_demand`, `always`, or empty
- if `mode = "always"` and the referenced template resolves to
  non-`off` `sleep_after_idle`, reject config
- duplicate-identity validation runs after pack expansion and rig
  stamping, not on raw pre-expansion config
- runtime creation and rename paths reject reserved configured aliases
  and deterministic named-session `session_name` values even before the
  canonical bead exists

In v1, custom alias override is not supported. If later needed, it
should be a deliberate extension because it changes mail/work identity
semantics.

## Gastown and Tutorial Migration

This feature is how Gastown regains canonical persistent roles without
reintroducing implicit singleton sessions.

Recommended Gastown mapping:

```toml
[[named_session]]
template = "mayor"
scope = "city"
mode = "on_demand"

[[named_session]]
template = "deacon"
scope = "city"
mode = "always"

[[named_session]]
template = "boot"
scope = "city"
mode = "always"

[[named_session]]
template = "witness"
scope = "rig"
mode = "always"

[[named_session]]
template = "refinery"
scope = "rig"
mode = "on_demand"
```

Rationale:

- `mayor`: stable endpoint, but no need to burn tokens when idle
- `deacon`: proactive patrol loop, intentionally controller-kept
- `boot`: controller-owned watchdog loop, intentionally controller-kept
- `witness`: proactive patrol loop per rig
- `refinery`: canonical merge queue endpoint, wake on work

For the lifecycle and minimal packs, `refinery` should also be expressed as
an `on_demand` named session instead of an implicit singleton.

### Refinery does not need a new "check" feature

`refinery` already has the right model if it is a named session backed by
its template's work query:

- polecats assign merge-ready work to `rig/refinery`
- controller work detection sees that work
- if the canonical `rig/refinery` bead does not exist yet, GC
  materializes it
- the session wakes and processes the queue

That means `refinery` does not need to be a pool with `max = 1`, and it
does not need a separate first-class singleton `check` field in v1.

## Alternatives Considered

### Keep using non-pool `[[agent]]` as implicit singletons

Rejected. This is the model that caused the original confusion and wake
loops. Template existence is not the same as configured runtime identity.

### Encode singletons as `pool max = 1`

Rejected. Pool semantics are label-based and worker-oriented:

- pool work routing defaults differ
- configured mailbox fallback intentionally skips pools
- pool hooks and slot semantics are not the right abstraction for a
  canonical alias-backed singleton

### Support custom aliases in v1

Rejected for the first implementation. Alias divergence from template
identity introduces work-query, mail, and `GC_AGENT` questions that are
better handled deliberately in a follow-up.

## Lifecycle Semantics

The canonical lifecycle for a named session is:

1. `reserved`
   The config entry exists. Alias and deterministic `session_name` are
   reserved. No bead is required yet.
2. `materialized`
   A canonical bead exists and is marked with configured-session
   metadata. The process may be `awake`, `asleep`, `creating`, or
   `draining`.
3. `conflict`
   The config entry exists, but a non-canonical live bead occupies the
   reserved alias or deterministic `session_name`. The identity is
   blocked and unroutable until the operator resolves the conflict.
4. `downgraded`
   If the `[[named_session]]` entry is removed from config, the existing
   canonical bead is downgraded to an ordinary session by clearing the
   configured-session metadata. History remains, but controller
   management stops.

GC stop/start semantics:

- `gc stop` stops the patrol loop/controller first, then stops or drains
  processes using the normal stop lifecycle; because the patrol is no
  longer running, `mode = "always"` does not immediately reassert on the
  next tick
- `gc stop` leaves canonical beads and reservations intact
- after a later `gc start`, `always` sessions are re-materialized and
  re-woken by config
- after a later `gc start`, `on_demand` sessions remain reserved or
  materialized-asleep until a wake root appears

Cold start uses the same downgrade and recovery rules as hot reload.
During the initial reconciliation pass, if GC finds an open bead marked
as a configured named session but no longer finds a matching
`[[named_session]]` entry in the loaded config snapshot, it downgrades
that bead before wake evaluation. Likewise, if it finds an open
canonical bead stranded in `creating` / `pending_create_claim=true`, it
adopts or clears that in-flight create rather than minting a second
canonical bead.

`pending_create_claim` does not require a separate TTL in v1. Single-city
controller exclusivity means no second controller is racing to complete
the same create. On restart, GC adopts or clears the stale in-flight
claim before attempting any replacement materialization.

Changing a named session's template or removing/re-adding the config
entry is treated as identity change. Existing canonical beads are either
downgraded to ordinary sessions or left in conflict if the reserved
identity is still occupied.

Downgrade ordering is explicit:

1. clear configured named-session discriminator metadata from the bead
2. stop treating its alias and deterministic `session_name` as
   controller-owned reservations
3. leave the ordinary session process alone; from the next patrol tick
   onward it is just an unmanaged session

Config removal takes effect before the next desired-state and wake
evaluation pass. A downgraded bead is therefore not eligible for
`WakeConfig` in the same tick that removes the `[[named_session]]`
entry.

If config is later re-added and that ordinary session still occupies the
reserved identifiers, the named session reappears in `conflict` state
until the operator resolves it.

Because live `alias_history` participates in configured-identity
reservation, ordinary rename alone may not be enough to resolve a
conflict if the conflicting session still carries the reserved token in
live history. In v1, supported conflict-resolution paths are:

- close the conflicting live session
- rename it and explicitly prune the routing-relevant historical alias
  entry as part of the same repair flow
- remove or change the `[[named_session]]` entry

If a later config change modifies `city.session_template` or otherwise
changes the deterministic runtime `session_name`, the old canonical bead
does not transparently migrate in v1. On the first reconciliation pass
after drift is observed:

- GC downgrades the old canonical bead to an ordinary session by
  clearing configured-session ownership metadata
- the reservation derived from the current config becomes authoritative
  immediately
- if the downgraded bead still occupies the reserved alias or the newly
  reserved deterministic `session_name`, the named session enters
  `conflict` until the operator resolves the collision

GC never silently re-adopts the old bead under the new deterministic
identity.

## Implementation Plan

1. Add `NamedSession` config support to city loading, pack expansion,
   validation, schema, and docs.
2. Replace non-pool singleton config assumptions with named-session
   helpers in desired-state and wake evaluation.
3. Centralize configured-session materialization through one aliased
   helper and switch mail/nudge/sling/workflow to use it.
4. Update Gastown and the minimal/lifecycle packs to declare explicit named
   sessions.
5. Update status output and tests so canonical named sessions, not plain
   templates, define singleton runtime presence.

## Risks

- The feature touches config composition, pack expansion, routing, and
  reconciliation together, so partial implementation would create new
  inconsistencies.
- Status and alias/name reservation logic are easy places for hidden
  regressions because existing code still assumes singleton templates in
  a few places.
- `mode = "always"` plus `wake_mode = "fresh"` intentionally recreates a
  fresh loop on every drain. That is correct for `boot`, but operators
  can still burn tokens if they misconfigure a chatty role this way.

## Acceptance Criteria

- A template without `[[named_session]]` never auto-creates a canonical
  session and never gets `WakeConfig`.
- A configured `on_demand` named session reserves its public target and
  materializes the canonical aliased session on first reference.
- A configured `on_demand` named session with pending work is
  auto-materialized by the controller even if no bead exists yet.
- A configured `always` named session is controller-kept alive without
  depending on implicit non-pool template behavior.
- Ad-hoc sessions created from the same template are never mistaken for
  the canonical configured named session.
- `gc session new <template>` continues to create fresh ordinary
  sessions even when a configured named session reserves the same
  qualified identity.
- Gastown and lifecycle/minimal configs work through explicit named
  sessions instead of implicit singleton templates.
