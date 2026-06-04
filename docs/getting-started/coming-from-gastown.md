---
title: Coming from Gas Town
description: The fastest way to translate Gas Town roles, mechanisms, layout, commands, and workflows into Gas City primitives.
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

This page is laid out as a deliberate sequence. First it recaps what each Gas
Town role actually *did*, so the mappings make sense. Then it maps Town to Gas
City one domain at a time — roles, then mechanisms, then filesystem/state, then
commands, then workflows — so you are never untangling four kinds of mapping at
once. The prose sections after the tables go deeper on the patterns that matter
most.

If you want the system-level mental model before any of this, read the
[Architecture Overview](/concepts/architecture-overview) and the
[Primitives Reference](/concepts/primitives) first.

## The Core Shift

Gas Town is shaped around a role taxonomy and a filesystem layout. Gas City is
shaped around a small primitive set plus configuration.

In Gas Town, it is normal to think in terms of:

- mayor, deacon, witness, refinery, polecat, crew, dog
- `~/gt/...` directory layout
- plugins and convoys as named orchestration features
- role-specific managers and cwd-derived identity

In Gas City, the default mental model should be:

- reusable behavior lives in `pack.toml` plus pack directories
- deployment choices live in `city.toml`
- machine-local bindings and runtime state live in `.gc/`
- every durable work item is a bead
- agents are generic; roles come from prompts, formulas, orders, and config
- the controller owns SDK infrastructure behavior
- directories are an implementation detail, not the architecture

That is the biggest onboarding difference. Gas City is not "Gas Town with
renamed commands". It is the lower-level orchestration toolkit that Gas Town
can be expressed in.

The single most important consequence: **Gas City has zero hardcoded roles.**
No `mayor`, `deacon`, `polecat`, or any other role name appears in the SDK's
Go code. Every role you knew in Gas Town is reproduced in Gas City as
configuration — a generic agent plus a prompt template, optionally wrapped in
formulas and orders. The role recap and mapping tables below should be read
with that in mind: the left column is a Gas Town role; the right column is a
configuration pattern, not a built-in type.

## Gas Town Role Recap

Before mapping anything, here is what each Gas Town role did operationally. If
you have not touched Gas Town recently — or you are still deciding whether to
migrate — this is the meaning you need to recover before the mapping tables make
sense.

Each paragraph ends with the same reminder, because it is the whole point:
**Gas City has no equivalent named role baked in.** The behavior is reproduced
through configuration, not a built-in type.

- **Mayor** — the planner and coordinator. The mayor took high-level intent,
  broke it into work, assigned it, and monitored overall progress across the
  town. It was the human's primary point of contact. *In Gas City this is just
  a configured agent with a coordinating prompt — there is no Mayor type in the
  SDK.*

- **Deacon** — the watchdog. The deacon detected stalled or dead agents,
  restarted them, and enforced SLAs and health thresholds so work kept moving
  without human babysitting. *In Gas City this is controller/supervisor
  infrastructure plus config, not a role — there is no Deacon type in the SDK.*

- **Witness** — the lifecycle observer. The witness tracked session health and
  lifecycle transitions and published events about what was happening, giving
  the rest of the system something to react to. *In Gas City this is the event
  bus plus waits, formulas, and session config — there is no Witness type in
  the SDK.*

- **Refinery** — the post-processor. The refinery took raw agent output and
  transformed it into structured, usable results — cleaning, summarizing, or
  reshaping work products before they moved on. *In Gas City this is a
  configured agent plus a formula or order step — there is no Refinery type in
  the SDK.*

- **Polecat** — the ephemeral, on-demand worker. A polecat was spawned for a
  specific task and exited when done; they scaled up and down with demand and
  often ran in isolated worktrees. *In Gas City this is a scalable/transient
  session config (a pool) — there is no Polecat type in the SDK.*

- **Crew** — the persistent worker pool. Crew were long-lived agents that
  claimed work from a queue and stayed around between tasks, the standing
  workforce of the town. *In Gas City this is a persistent named agent config —
  there is no Crew type in the SDK.*

- **Dog** — the integration / external-messaging relay. Dogs connected the town
  to the outside world, relaying messages and bridging external systems. *In
  Gas City this is usually an exec order (no LLM session needed), sometimes a
  scalable session config — there is no Dog type in the SDK.*

## Mapping Tables

The next five tables map Gas Town to Gas City one domain at a time: roles,
mechanisms, filesystem/state, commands, then workflows. Each table is preceded
by a one-sentence scope statement so you always know which domain you are in.

### Roles → Gas City Equivalents

*Scope: how each Gas Town role name maps to Gas City. Every entry on the right
is configuration — a user-configured agent plus a prompt template — not a
built-in SDK type.*

| Gas Town role | Gas City equivalent | What changes for you |
|---|---|---|
| Mayor | Configured agent + coordinating prompt template (e.g. the Gastown pack's `mayor`) | A role name in a pack, not an SDK primitive. Reachable with `gc session attach mayor`. |
| Deacon | Controller / supervisor infrastructure + config; optionally a configured agent | Watchdog behavior moves into the controller. You tune thresholds in config rather than running a role. |
| Witness | Event bus + waits, formulas, and session scale config | The SDK gives you the mechanisms; a pack decides whether to model a "witness" role at all. |
| Refinery | Configured agent + a formula or order post-processing step | Post-processing is a workflow step, not a standing role type. |
| Polecat | Scalable / transient session config (a pool) | "Polecat" is an operating style — on-demand sessions, often with worktrees — expressed as session config. |
| Crew | Persistent named agent config | "Crew" is an operating style — long-lived named agents — expressed as agent config. |
| Dog | Usually an exec order; sometimes a scalable session config | Most relay work needs no LLM session, so an order is cleaner than a role. |

### Mechanisms / Behaviors → Gas City Equivalents

*Scope: what those Gas Town roles and features actually *did* — the behaviors —
and where that logic lives in Gas City.*

| Gas Town behavior | Gas City equivalent | Notes |
|---|---|---|
| Deacon watchdog logic | Controller health patrol + reconciliation | Stall detection, restart-with-backoff, and reconcile-to-desired-state are controller concerns, not a role agent. |
| Witness lifecycle tracking | Waits, formulas, session scale config, controller wake/sleep, event bus | The mechanisms are first-class; modeling a "witness" on top of them is optional pack behavior. |
| Plugin (scheduled / event / conditional automation) | Order — exec order or formula order | Use an **exec order** for shell or controller-side logic; a **formula order** to instantiate agent-driven work. |
| Convoy as an orchestration runtime | Convoy beads + `gc sling` + formulas | Convoys stay bead-backed grouping and lineage; there is no special convoy runtime layer you must use. |
| Formula runner inside Town workflows | Formula resolution + backend-owned execution | Gas City resolves and dispatches formulas; multi-step execution is backend-dependent today. `bd` is the production path. |
| Path / cwd-derived identity | Explicit agent identity, rig scope, env, bead metadata | Do not port code or prompts that assume cwd implies who the agent is. |

### Filesystem / State Layout → Gas City Equivalents

*Scope: where state lives. Gas Town encodes architecture into directories; Gas
City treats directories as an implementation detail.*

| Gas Town location | Gas City equivalent | Notes |
|---|---|---|
| `~/gt/...` directory tree | City directory + `.gc/` runtime state | A city is a directory containing `city.toml`, `.gc/`, and `rigs/`. State is discovered by querying, not from a fixed home tree. |
| Town config + rig config + role homes | `pack.toml`, `city.toml`, `agents/<name>/`, `.gc/` | Definition, deployment, and machine-local state are separated instead of spread across role-specific directories. |
| Role home directories | `dir` (identity scope) + `work_dir` (session cwd, only when needed) | Use `dir` to carry scope/identity; use `work_dir` only when a role truly needs filesystem isolation. |
| Role-specific startup files and local settings dirs | Prompt templates, overlays, provider hooks, `pre_start`, `session_setup`, `gc prime` | Startup shaping is explicit and provider-aware, not inferred from where a role lives on disk. |

### Commands → Gas City Equivalents

*Scope: the `gt` CLI mapped to its closest `gc` (or `bd`) home.* See the
[CLI reference](/reference/cli) for the full surface. This is a closest-match
map, not a claim that the two CLIs have identical architecture.

Two rules help a lot:

- if the old `gt` command was about orchestration, sessions, routing, hooks, or
  runtime behavior, the closest home is usually `gc`
- if the old `gt` command was really about bead CRUD or bead content, the
  closest home is often still `bd`, not `gc`

#### Workspace And Runtime

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

#### Configuration And Extension

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt git-init` | `git init` plus `gc rig add` | Git repo setup and city registration are separate concerns in Gas City. |
| `gt hooks` | config-driven hook install plus `gc doctor` | Gas City does not have Town's hook-management namespace; hook install is primarily config and lifecycle driven. |
| `gt plugin` | `gc order` | Plugin-like controller automation usually becomes an exec order or formula order. |
| `gt issue` | no direct equivalent | Usually replaced by bead metadata or session context, depending intent. |
| `gt account` | no direct equivalent | Provider account management is outside Gas City's core CLI. |
| `gt shell` | no direct equivalent | Gas City does not ship a Town-style shell integration namespace. |
| `gt theme` | no direct equivalent | Pack scripts or tmux config are the normal path. |

#### Work Routing And Workflow

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

#### Sessions, Roles, And Agents

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
| `gt dog` | usually `gc order`, sometimes a scalable session config in `city.toml` | Dog-like helpers are often better modeled as exec orders. |
| `gt role` | `gc config explain`, `gc session list`, prompt/config inspection | Role is not a first-class SDK concept. |
| `gt callbacks` | no direct equivalent | Callback behavior is folded into runtime, hooks, waits, and orders. |
| `gt cycle` | no direct generic command | Closest equivalents are tmux bindings or pack-specific session UX. |
| `gt namepool` | config-only today | Gas City supports namepool files in config, but does not expose a top-level `gc namepool` command. |
| `gt worktree` | `work_dir`, `pre_start`, `git worktree`, pack scripts | Worktree behavior is explicit config and script wiring, not a generic `gc worktree` namespace. |

#### Communication And Nudges

| `gt` | Closest in Gas City | Notes |
|---|---|---|
| `gt mail` | `gc mail` | Near-direct mapping. |
| `gt nudge` | `gc session nudge` | Use `gc session nudge <target> "msg"` to send messages to a live session. The `gc nudge` subcommand only exposes deferred-delivery controls (`drain`, `status`, `poll`); it does not accept a positional `<target> "msg"` form. |
| `gt peek` | `gc session peek` | Near-direct mapping. |
| `gt broadcast` | no single direct equivalent | Usually modeled as `gc mail send` to a group or multiple explicit targets. |
| `gt notify` | no direct equivalent | Notification policy is not a top-level SDK command family. |
| `gt dnd` | no direct equivalent | Closest behavior usually lives in mail or local workflow policy. |
| `gt escalate` | no direct equivalent | Model escalations with beads, mail, orders, or pack-specific workflow. |
| `gt whoami` | no direct equivalent | Identity is explicit in config, session metadata, and `GC_*` env rather than a dedicated CLI. |

#### Beads, Events, And Diagnostics

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
| `gt costs` | no direct equivalent | No matching top-level cost accounting command today. |
| `gt seance` | no direct equivalent | Gas City has resume and session metadata, but not a seance command. |
| `gt thanks` | no direct equivalent | No matching command. |

##### Practical Translation Rule

If you are unsure where a `gt` command went, ask this in order:

1. Is it now just `gc` with nearly the same name?
2. Is it really a bead operation that should stay in `bd`?
3. Is it no longer a special command because Gas City moved that behavior into
   config, orders, waits, formulas, or controller logic?

### Workflows → Gas City Equivalents

*Scope: how you actually *do* things — the verbs. If you used to perform a task
in Gas Town, this is the Gas City way to do it now.*

| Gas Town workflow | Gas City equivalent | Where to go deeper |
|---|---|---|
| Spin up a worker | `gc start` + a persistent agent config (`agents/<name>/`) | [Tutorial 02 — Agents](/tutorials/02-agents), [Shareable Packs](/guides/shareable-packs) |
| Send a task to the mayor | `gc sling "<description>"` (or `bd create` + a bead hook) | [`gc sling`](/reference/cli#gc-sling), [Tutorial 06 — Beads](/tutorials/06-beads) |
| Inspect what's stuck | `gc session list`, then `gc session peek <name>` | [`gc session list`](/reference/cli#gc-session-list), [`gc session peek`](/reference/cli#gc-session-peek) |
| Restart a stalled agent | `gc session reset <name>` (or let health patrol auto-restart it) | [`gc session reset`](/reference/cli#gc-session-reset) |
| Share a config across teams | A shareable pack: `pack.toml` + `agents/<name>/`, imported by each city | [Shareable Packs](/guides/shareable-packs) |
| Run a one-shot job | A formula or exec order dispatched on demand (`gc sling --formula <name>`) | [Tutorial 07 — Orders](/tutorials/07-orders), [Tutorial 05 — Formulas](/tutorials/05-formulas) |
| Watch live agent output | `gc session attach <name>` for an interactive live view, or `gc session peek <name>` for a non-attaching snapshot | [`gc session attach`](/reference/cli#gc-session-attach), [`gc session peek`](/reference/cli#gc-session-peek) |

<Note>
`gc session peek` takes a `--lines` count for a point-in-time snapshot; there
is no `--follow` flag. For a continuously updating live view, attach to the
session with `gc session attach`. For the system-wide live feed, use
`gc events --follow`.
</Note>

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

### Start With The City Pack And `city.toml`

This is the main day-one habit to adopt.

Most Gas Town users should begin with the root city pack plus `city.toml`, not
by editing an imported shared pack. The split is:

- `pack.toml` imports reusable packs and defines city-specific behavior
- `agents/<name>/` defines city-owned named agents
- `city.toml` declares deployment choices such as rigs, substrates, and scale
- `.gc/` stores site bindings such as local rig paths

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
- **polecats**: scalable or transient sessions, often with dedicated worktrees

That distinction is real and useful, but the SDK does not force it. A pack can
adopt the convention, relax it, or replace it.

## Where Gas City Deliberately Differs

### The Controller Owns Infrastructure Behavior

In Gas Town, some orchestration behavior is mediated through specific roles. In
Gas City, the controller is the canonical owner of infrastructure operations
like:

- reconcile desired sessions to running sessions
- session scaling
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

Reach for a dog-like scalable session config only if the task truly needs a long-lived
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

### "How do I get to my mayor?"

```bash
gc session attach mayor
```

The Mayor session is the primary Gas Town experience — an interactive Claude
session with full city context that coordinates everything. The CLI is
plumbing; this is the product.

City-scoped agents from the Gastown pack — `mayor`, `deacon`, `boot` — are all
accessible the same way. Use `gc session list` to see what is running.

This replaces `gt session at mayor/` or `tmux attach -t gt-mayor` from Gas Town.

## Common Gastown Overrides

If you are using the Gastown pack, these are the most common local changes.

### Register a rig

Import the Gastown pack in the root pack, then bind rigs in `city.toml` and
with `gc rig add`:

```toml
# pack.toml
[pack]
name = "my-city"
schema = 2

[imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

```bash
gc rig add /path/to/myproject --name myproject
```

### Increase or shrink scalable polecat sessions

This is the cleanest answer to "I want more or fewer polecats for this rig."

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

[[rigs.patches]]
agent = "gastown.polecat"

[rigs.patches.pool]
max = 10
```

### Change the provider for one rig's polecats

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

[[rigs.patches]]
agent = "gastown.polecat"
provider = "codex"
```

You can combine that with session scale overrides, env, prompt changes, or hook changes
on the same override block.

### Change a city-scoped Gastown agent

City-scoped agents such as `mayor`, `deacon`, and `boot` are easiest to tweak
with patches:

```toml
[[patches.agent]]
name = "gastown.mayor"
provider = "codex"
idle_timeout = "2h"
```

Use patches when the target is already a concrete city-scoped agent. Use
`[[rigs.patches]]` when the target is a pack agent stamped per rig.

### Add a named crew agent

Crew is usually city-specific, so it often belongs in the root city pack rather
than in the shared Gastown pack:

```text
agents/wolf/
├── agent.toml
└── prompt.template.md
```

```toml
# agents/wolf/agent.toml
scope = "rig"
nudge = "Check your hook and mail, then act accordingly."
work_dir = ".gc/worktrees/myproject/crew/wolf"
idle_timeout = "4h"
```

That keeps the shared pack generic while still letting your city have named
long-lived workers.

### Change a prompt, overlay, or timeout without forking the pack

This is what rig overrides are for:

```toml
# city.toml
[[rigs]]
name = "myproject"

[rigs.imports.gastown]
source = "https://github.com/gastownhall/gascity-packs/tree/main/gastown"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"

[[rigs.patches]]
agent = "gastown.refinery"
idle_timeout = "4h"
```

For prompt or overlay replacement, patch the imported agent from your root city
pack rather than editing the shared pack in place.

If that change turns out to be broadly useful across cities, that is when it
should move into the pack.

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

1. Read the [Architecture Overview](/concepts/architecture-overview) for the
   top-down mental model, then the [Primitives Reference](/concepts/primitives)
   for the nine building blocks in user terms.
2. Skim the [CLI reference](/reference/cli) alongside the command table above so
   the `gt` → `gc` muscle memory transfers.
3. Read [Tutorial 07 — Orders](/tutorials/07-orders) and mentally remap
   "plugins" to "orders".
4. Read [Tutorial 05 — Formulas](/tutorials/05-formulas) and remember that
   formulas are resolved by Gas City but executed by the configured beads
   backend.
5. Work through [Tutorial 02 — Agents](/tutorials/02-agents) and
   [Shareable Packs](/guides/shareable-packs) to see the PackV2
   `agents/<name>/` layout end to end.

If you keep those five points straight, most of the Gas Town to Gas City ramp
goes quickly.
