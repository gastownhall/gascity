---
title: Coming from Gas Town
description: The fastest way to translate Gas Town concepts into Gas City primitives.
---

Gas City is the SDK extracted from Gas Town. The fastest way to get
productive is to stop looking for a one-to-one port of Town's role tree and
instead map Town concepts onto Gas City's primitives:

- agents
- beads
- events
- config
- prompt templates
- derived mechanisms like orders, formulas, waits, mail, and sling

If you built systems in Gas Town, you already know the operational problems Gas
City is trying to solve. The main change is where the logic lives.

## The Core Shift

Gas Town is shaped around a role taxonomy and a filesystem layout. Gas City is
shaped around a small primitive set plus configuration.

In Gas Town, it is normal to think in terms of:

- mayor, deacon, witness, refinery, polecat, crew, dog
- `~/gt/...` directory layout
- plugins and convoys as named orchestration features
- role-specific managers and cwd-derived identity

In Gas City, the default mental model should be:

- everything is configured in `city.toml` plus packs
- every durable work item is a bead
- agents are generic; roles come from prompts, formulas, orders, and config
- the controller owns SDK infrastructure behavior
- directories are an implementation detail, not the architecture

That is the biggest onboarding difference. Gas City is not "Gas Town with
renamed commands". It is the lower-level orchestration toolkit that Gas Town
can be expressed in.

## Concept Map

| Gas Town concept | Gas City concept | What changes for you |
|---|---|---|
| Town config + rig config + role homes | `city.toml` plus packs and overrides | Most behavior is configured declaratively in one place instead of being spread across role-specific directories and managers. |
| Mayor, deacon, witness, refinery, polecat, crew, dog | Configured agents | Gas City has no baked-in role names in Go. These are pack conventions, not SDK primitives. |
| Plugin | Order | An exec order runs shell directly with no agent session. A formula order instantiates agent work. If you were thinking "plugin that runs a command", start with an exec order. |
| Convoy | Convoy bead plus sling/formulas | Convoys are still bead-backed work grouping, but there is no special convoy runtime layer you have to use to get orchestration. |
| Dog | Usually an order first, sometimes a pool agent | In Gas Town, dogs are named infrastructure helpers. In Gas City, a lot of that work is cleaner as exec orders because no LLM session is needed. |
| Deacon watchdog logic | Controller and supervisor | Health patrol, order dispatch, wisp GC, and reconciliation are controller concerns, not role-agent responsibilities. |
| Witness lifecycle logic | Pack behavior built on waits, formulas, pool config, and controller wake/sleep | The SDK gives you the mechanisms. A pack decides whether to model a witness role at all. |
| Crew and polecats as hard types | Persistent agents and pool agents | "Crew" and "polecat" are operating styles. Gas City only knows agent config and pool behavior. |
| Directory tree under `~/gt` | `dir` for identity scope and `work_dir` for session cwd | Do not encode architecture into paths. Keep identity in config and metadata. Use `work_dir` only when a role really needs filesystem isolation. |
| Role-specific startup files and local settings dirs | Prompt templates, overlays, provider hooks, `pre_start`, `session_setup`, `gc prime` | Startup shaping is explicit and provider-aware instead of being mostly inferred from where a role lives on disk. |
| Path-derived identity | Explicit agent identity, rig scope, env, bead metadata | Avoid porting code or prompts that assume cwd implies who the agent is. |
| Formula runner inside Town workflows | Formula resolution in Gas City plus backend-owned execution | Gas City resolves formulas and dispatches them, but real multi-step execution is still backend-dependent today. `bd` is the production path. |

## What Usually Maps Cleanly

### Roles Become Pack Agents

If you would have added a new role in Gas Town, the Gas City move is usually:

1. start in your local `city.toml`
2. include a pack if one already solves most of the problem
3. override the stamped agent if you just need local behavior changes
4. edit the pack only when you are changing the shared default for everyone
5. add formulas or orders around the agent if it needs workflow automation

That keeps role behavior in configuration instead of hardcoding more role
semantics into the SDK, while still making the common day-one workflow feel
local and incremental.

### Start In `city.toml`

This is the main day-one habit to adopt.

Most Gas Town users should begin in their local `city.toml`, not by editing a
pack. Packs are for reusable defaults. Your local city config is for:

- registering rigs
- including the Gastown pack
- adding named crew agents
- changing providers
- overriding pool sizes
- tweaking prompts, hooks, overlays, and timeouts for your own city

Reach for a pack edit when the change should become the new reusable default
for every consumer of that pack.

### Plugins Become Orders

This is the most important practical translation.

If the Gas Town idea is "something should run automatically on a schedule, on
an event, or when a condition is true", you probably want an order.

- Use an **exec order** when the work is just shell or controller-side logic.
- Use a **formula order** when the work should instantiate agent-driven
  workflow.

That is the clean replacement for many Town "plugin" instincts. Exec orders are
especially important because they can run non-agent commands with no prompt, no
session, and no extra role agent.

### Convoys Stay Bead-Shaped

Gas Town taught people to think in convoys. That mental model still transfers
well, but the implementation boundary is different.

In Gas City:

- convoys are still bead-backed grouping and lineage
- `gc sling` can create convoy structure as part of routing
- formulas, orders, and waits compose around that bead graph

So keep the convoy mental model for tracking work, but do not assume it needs a
special orchestration subsystem beyond beads plus dispatch.

### Crew and Polecats Are Operating Modes

In Gas Town, these feel like first-class worker types. In Gas City, they are
best thought of as conventions:

- **crew**: persistent named agents you expect humans to reason about
- **polecats**: pooled or transient agents, often with dedicated worktrees

That distinction is real and useful, but the SDK does not force it. A pack can
adopt the convention, relax it, or replace it.

## Where Gas City Deliberately Differs

### The Controller Owns Infrastructure Behavior

In Gas Town, some orchestration behavior is mediated through specific roles. In
Gas City, the controller is the canonical owner of infrastructure operations
like:

- reconcile desired sessions to running sessions
- pool scaling
- order evaluation
- health patrol
- wisp garbage collection

If something is fundamentally SDK infrastructure, prefer putting it in the
controller path instead of inventing another deacon-like role behavior.

### Filesystem Layout Is Not The Architecture

Gas Town uses directories as part of the system contract. Gas City tries not to.

The current rule of thumb is:

- use `dir` to carry the agent's scope and identity context
- use `work_dir` when the session must run somewhere else
- use bead metadata for durable handoff state

Good reasons to use a separate `work_dir`:

- the role mutates a repo and needs an isolated worktree
- provider scratch files would collide with another role
- the role needs a durable sandbox independent from the canonical rig root

Bad reason:

- "Gas Town had a separate folder for this role"

### Roles Are Examples, Not SDK Law

The Gastown pack still ships familiar roles, but that is an example operating
model, not a type system inside Gas City.

This matters when you change the system:

- adding a new behavior usually means editing a pack, formula, order, or prompt
- it usually does not mean adding a new hardcoded role to the SDK

That is a feature, not a missing abstraction.

It is also worth separating two kinds of changes:

- **local city change**: edit `city.toml`, add rig overrides, add patches, or
  add a city-specific agent
- **shared product change**: edit the pack because you want a better default
  for everyone

Most onboarding work should live in the first category.

## Common Translation Patterns

### "I need a new dog"

Ask this first:

- Can this be an exec order?

If yes, prefer the order. That gives you trigger logic, history, and controller
ownership without burning an agent slot.

Reach for a dog-like pool agent only if the task truly needs a long-lived
session, rich interactive context, or repeated agent judgment.

### "I need a witness-like lifecycle manager"

Ask which parts are:

- controller infrastructure
- bead state transitions
- formula logic
- prompt guidance

Only the first category belongs in Go SDK infrastructure. The rest usually live
better in the pack.

### "I need another special directory tree"

Usually you do not.

Start with:

- canonical repo root from the rig
- isolated `work_dir` only for roles that mutate repos or need provider-file
  isolation
- explicit env and metadata, not cwd inference

### "I need to run something without an agent"

Use an exec order before inventing a plugin, helper role, or hidden session.

That is the direct Gas City answer to many old Town automation tasks.

## Common Gastown Overrides In `city.toml`

If you are using the Gastown pack, these are the most common local changes.

### Register a rig

This activates the rig-scoped Gastown agents for one repo:

```toml
[[rigs]]
name = "myproject"
path = "/path/to/myproject"
includes = ["packs/gastown"]
```

### Increase or shrink the polecat pool

This is the cleanest answer to "I want more or fewer polecats for this rig."

```toml
[[rigs]]
name = "myproject"
path = "/path/to/myproject"
includes = ["packs/gastown"]

[[rigs.overrides]]
agent = "polecat"

[rigs.overrides.pool]
max = 10
```

### Change the provider for one rig's polecats

```toml
[[rigs]]
name = "myproject"
path = "/path/to/myproject"
includes = ["packs/gastown"]

[[rigs.overrides]]
agent = "polecat"
provider = "codex"
```

You can combine that with pool overrides, env, prompt changes, or hook changes
on the same override block.

### Change a city-scoped Gastown agent

City-scoped agents such as `mayor`, `deacon`, and `boot` are easiest to tweak
with patches:

```toml
[[patches.agent]]
dir = ""
name = "mayor"
provider = "codex"
idle_timeout = "2h"
```

Use patches when the target is already a concrete city-scoped agent. Use
`[[rigs.overrides]]` when the target is a pack agent stamped per rig.

### Add a named crew agent

Crew is usually city-specific, so it often belongs directly in `city.toml`
rather than in the shared Gastown pack:

```toml
[[agent]]
name = "wolf"
dir = "myproject"
prompt_template = "packs/gastown/prompts/crew.md.tmpl"
nudge = "Check your hook and mail, then act accordingly."
work_dir = ".gc/worktrees/myproject/crew/wolf"
idle_timeout = "4h"
```

That keeps the shared pack generic while still letting your city have named
long-lived workers.

### Change a prompt, overlay, or timeout without forking the pack

This is what rig overrides are for:

```toml
[[rigs]]
name = "myproject"
path = "/path/to/myproject"
includes = ["packs/gastown"]

[[rigs.overrides]]
agent = "refinery"
prompt_template = "local/prompts/refinery.md.tmpl"
overlay_dir = "local/overlays/refinery"
idle_timeout = "4h"
```

If that change turns out to be broadly useful across cities, that is when it
should move into the pack.

## `gt` -> `gc` Command Map

This is a closest-match map, not a claim that the two CLIs have identical
architecture.

Two rules help a lot:

- if the old `gt` command was about orchestration, sessions, routing, hooks, or
  runtime behavior, the closest home is usually `gc`
- if the old `gt` command was really about bead CRUD or bead content, the
  closest home is often still `bd`, not `gc`

### Workspace And Runtime

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt install` | `gc init` | Gas City uses `gc init` to create a city. |
| `gt init` | `gc rig add` or `gc init` | Town `init` and `install` split across city creation and rig registration in Gas City. |
| `gt rig` | `gc rig` | Near-direct mapping. |
| `gt start` | `gc start` | Starts the city under the machine-wide supervisor. |
| `gt up` | `gc start` | Same high-level intent. |
| `gt down` | `gc stop` | Stop sessions for the current city. |
| `gt shutdown` | `gc stop` | Same intent, different implementation model. |
| `gt daemon` | `gc supervisor` | Supervisor is the canonical long-running runtime in Gas City. |
| `gt status` | `gc status` | City-wide overview. |
| `gt dashboard` | `gc dashboard` | Same general purpose; `gc dashboard serve` still exists as the explicit form. |
| `gt doctor` | `gc doctor` | Near-direct mapping. |
| `gt config` | `gc config` plus editing `city.toml` | Gas City config is file-first; `gc config` is mostly inspect/explain. |
| `gt disable` | `gc suspend` | Closest operational match is per-city suspension, not a system-wide Town toggle. |
| `gt enable` | `gc resume` | Resumes a suspended city. |
| `gt uninstall` | no direct equivalent | Gas City has supervisor install/uninstall, but not a Town-style global uninstall command. |
| `gt version` | `gc version` | Direct mapping. |
| `gt completion` | no direct equivalent | Gas City does not currently expose a matching completion command. |
| `gt help` | `gc help` | Direct mapping. |
| `gt info` | `gc version`, `gc status`, docs | No single `gc info` command. |
| `gt stale` | no direct equivalent | Closest checks are `gc version` and `gc doctor`. |
| `gt town` | split across `gc start`, `gc status`, `gc stop`, `gc supervisor` | Gas City does not keep a separate Town namespace. |

### Configuration And Extension

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt git-init` | `git init` plus `gc rig add` | Git repo setup and city registration are separate concerns in Gas City. |
| `gt hooks` | config-driven hook install plus `gc doctor` | Gas City does not have Town's hook-management namespace; hook install is primarily config and lifecycle driven. |
| `gt plugin` | `gc order` | Plugin-like controller automation usually becomes an exec order or formula order. |
| `gt issue` | no direct equivalent | Usually replaced by bead metadata or session context, depending intent. |
| `gt account` | no direct equivalent | Provider account management is outside Gas City's core CLI. |
| `gt shell` | no direct equivalent | Gas City does not ship a Town-style shell integration namespace. |
| `gt theme` | no direct equivalent | Pack scripts or tmux config are the normal path. |

### Work Routing And Workflow

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt sling` | `gc sling` | Direct mapping in spirit and name. |
| `gt handoff` | `gc handoff` | Near-direct mapping. |
| `gt convoy` | `gc convoy` | Near-direct mapping for convoy creation and tracking. |
| `gt hook` | `gc hook` | Same name, narrower surface: `gc hook` is work-query and hook injection behavior, not the full Town hook manager. |
| `gt ready` | `bd ready` | This stays bead-centric more than city-centric. |
| `gt done` | no single direct equivalent | In Gas City this is usually a bead close, metadata transition, convoy action, or formula step. |
| `gt unsling` | no direct equivalent | Usually replaced by bead edits plus re-routing with `bd` and `gc sling`. |
| `gt formula` | `gc formula list/show/cook`, `gc sling --formula`, `gc order` | `gc formula` manages formulas (list, show, cook). `gc sling --formula` dispatches as a wisp. |
| `gt mol` | `gc formula cook`, `bd mol ...` | `gc formula cook` creates molecules; `bd` handles bead-level operations. |
| `gt mq` | no direct generic `gc` command | Gastown-style merge queue behavior lives in the pack and formulas, not a generic SDK namespace. |
| `gt gate` | `gc wait` | Durable waits are the closest SDK concept. |
| `gt park` | `gc wait` | Same underlying idea: stop and resume around a dependency or gate. |
| `gt resume` | `gc wait ready`, `gc session wake`, `gc mail check` | Depends on whether the old action was a parked wait, sleeping session, or handoff/mail resume. |
| `gt synthesis` | partial: `gc converge`, formulas, convoys | No one-command parity. |
| `gt orphans` | no direct generic command | In Gas City this is usually pack logic plus witness/refinery formulas and bead inspection. |
| `gt release` | mostly `bd` state edits | No single `gc release` command. |

### Sessions, Roles, And Agents

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt agents` | `gc session` plus `gc status` | Session management is generic in Gas City; not a Town-specific agent switcher. |
| `gt session` | `gc session` | Same broad idea, but not polecat-specific. |
| `gt crew` | `city.toml` agents plus `gc session` | Crew is a pack convention, not a first-class SDK command family. |
| `gt polecat` | Gastown pack `polecat` agent plus `gc status` / `gc session` / `gc sling` | No dedicated top-level SDK namespace. |
| `gt witness` | Gastown pack `witness` agent plus `gc session` / `gc status` | No dedicated top-level SDK namespace. |
| `gt refinery` | Gastown pack `refinery` agent plus `gc session` / `gc status` | No dedicated top-level SDK namespace. |
| `gt mayor` | Gastown pack `mayor` agent plus `gc session attach mayor` / `gc status` | Managed as a configured agent, not a baked-in command family. |
| `gt deacon` | Gastown pack `deacon` agent plus `gc session`, `gc status`, controller behavior | In Gas City, much of what deacon did lives in the controller/supervisor. |
| `gt boot` | Gastown pack `boot` agent | Same pattern as other role agents. |
| `gt dog` | usually `gc order`, sometimes a pooled agent in `city.toml` | Dog-like helpers are often better modeled as exec orders. |
| `gt role` | `gc config explain`, `gc session list`, prompt/config inspection | Role is not a first-class SDK concept. |
| `gt callbacks` | no direct equivalent | Callback behavior is folded into runtime, hooks, waits, and orders. |
| `gt cycle` | no direct generic command | Closest equivalents are tmux bindings or pack-specific session UX. |
| `gt namepool` | config-only today | Gas City supports namepool files in config, but does not expose a top-level `gc namepool` command. |
| `gt worktree` | `work_dir`, `pre_start`, `git worktree`, pack scripts | Worktree behavior is explicit config and script wiring, not a generic `gc worktree` namespace. |

### Communication And Nudges

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt mail` | `gc mail` | Near-direct mapping. |
| `gt nudge` | `gc session nudge` or `gc nudge` | Use `gc session nudge` for a specific live session, `gc nudge` for deferred delivery controls. |
| `gt peek` | `gc session peek` | Near-direct mapping. |
| `gt broadcast` | no single direct equivalent | Usually modeled as `gc mail send` to a group or multiple explicit targets. |
| `gt notify` | no direct equivalent | Notification policy is not a top-level SDK command family. |
| `gt dnd` | no direct equivalent | Closest behavior usually lives in mail or local workflow policy. |
| `gt escalate` | no direct equivalent | Model escalations with beads, mail, orders, or pack-specific workflow. |
| `gt whoami` | no direct equivalent | Identity is explicit in config, session metadata, and `GC_*` env rather than a dedicated CLI. |

### Beads, Events, And Diagnostics

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt bead` | mostly `bd` | Bead CRUD is still primarily the bead tool's job. |
| `gt cat` | mostly `bd` | Same rule: bead content inspection is bead-centric. |
| `gt show` | mostly `bd` | Use the bead tool for detailed bead state/content. |
| `gt close` | mostly `bd close` | Still bead-centric. |
| `gt commit` | `git commit` | Gas City does not wrap commit the way Town did. |
| `gt activity` | `gc event emit` and `gc events` | Same basic event/logging space. |
| `gt trail` | `gc events`, `gc session peek`, `gc session logs` | No one-command parity. |
| `gt feed` | `gc events` | Closest live system feed. |
| `gt log` | `gc events` or `gc supervisor logs` | Depends on whether you want event history or runtime logs. |
| `gt audit` | partial: `gc events`, `gc graph`, `bd` queries | No single audit namespace equivalent. |
| `gt checkpoint` | no direct equivalent | Session durability lives in the runtime and bead/session model rather than a user-facing checkpoint CLI. |
| `gt patrol` | no direct equivalent | Patrol behavior is generally modeled with orders plus formulas. |
| `gt migrate-agents` | `gc migration` | Same general migration/upgrade bucket. |
| `gt prime` | `gc prime` | Direct mapping. |
| `gt account` | no direct equivalent | Provider account management is outside Gas City's core CLI. |
| `gt shell` | no direct equivalent | Gas City does not ship a Town-style shell integration namespace. |
| `gt theme` | no direct equivalent | Pack scripts or tmux config are the normal path. |
| `gt costs` | no direct equivalent | No matching top-level cost accounting command today. |
| `gt seance` | no direct equivalent | Gas City has resume and session metadata, but not a seance command. |
| `gt thanks` | no direct equivalent | No matching command. |

### Practical Translation Rule

If you are unsure where a `gt` command went, ask this in order:

1. Is it now just `gc` with nearly the same name?
2. Is it really a bead operation that should stay in `bd`?
3. Is it no longer a special command because Gas City moved that behavior into
   config, orders, waits, formulas, or controller logic?

## What Not To Port Literally

These Gas Town habits usually create unnecessary complexity in Gas City:

- exact `~/gt/...` directory trees
- cwd-derived identity
- new hardcoded role names in SDK code
- plugin systems when an order is enough
- special helper agents for work that is really a shell command
- duplicating durable state outside beads when labels or metadata are enough

The most common architectural mistake is importing Town's surface area instead
of re-expressing the intent in Gas City's primitives.

## Fast Ramp Checklist

If you already know Gas Town, this is the shortest path to becoming effective
in Gas City:

1. Read the Nine Concepts Overview (`engdocs/architecture/nine-concepts`).
2. Read the Config System docs (`engdocs/architecture/config`).
3. Read Orders (`engdocs/architecture/orders`) and mentally remap "plugins" to
   "orders".
4. Read Formulas & Molecules (`engdocs/architecture/formulas`) and remember that
   formulas are resolved by Gas City but executed by the configured beads
   backend.
5. Look at [examples/gastown/city.toml](https://github.com/gastownhall/gascity/blob/main/examples/gastown/city.toml)
   first, then [examples/gastown/packs/gastown/pack.toml](https://github.com/gastownhall/gascity/blob/main/examples/gastown/packs/gastown/pack.toml).
   The city file is the normal starting point; the pack defines the reusable
   defaults behind it.

If you keep those five points straight, most of the Gas Town to Gas City ramp
goes quickly.
