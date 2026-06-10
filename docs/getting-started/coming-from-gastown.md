---
title: Coming from Gas Town
description: Recap what Gas Town gives you, see how Gas City works, then map Gas Town roles, mechanisms, layout, commands, and workflows onto Gas City primitives.
---

## Why this page

If you have run Gas Town, you already know its operational machinery — the roles, the directory layout, the `gt` commands, and the day-to-day moves you make to get work done.

This page carries that knowledge across to Gas City.

Gas City is the SDK that operational machinery was extracted into, and that is the reason to care about it. Because Gas City is an SDK, a feature added to it lifts *every* orchestrator built on top of it — Gas Town included.

Gas Town and Gas City produce the same kind of system; the change is where the logic lives. Instead of a fixed role tree baked into the binary, Gas City gives you a small set of primitives plus configuration, and _you express Gas Town (or any other orchestration) on top of them_.

## Page Layout

This page is laid out as a deliberate sequence so you are never untangling several kinds of mapping at once:

1. [Gas Town Recap](#gas-town-recap) recaps what Gas Town gives you, one domain at a time, so the rest has something to anchor to
2. [How Gas City Works](#how-gas-city-works) does just that — presenting the small set of building blocks it offers in place of Gas Town's machinery
3. [Mapping Tables](#mapping-tables) maps Gas Town onto Gas City one domain at a time:
   1. [roles](#roles-→-gas-city-equivalents)
   2. [mechanisms](#mechanisms-/-behaviors-→-gas-city-equivalents)
   3. [filesystem/state](#filesystem-/-state-layout-→-gas-city-equivalents)
   4. [workflows](#workflows-→-gas-city-equivalents)
   5. [commands](#commands-→-gas-city-equivalents).

The prose sections after the tables ([What Usually Maps Cleanly](#what-usually-maps-cleanly) and so on...) go deeper on the patterns that matter most, before a short pointer to the [config recipes](/guides/gastown-config-recipes) and a closing ramp checklist.

If you want the system-level mental model before any of this, read the [Architecture Overview](/concepts/architecture-overview) and the [Primitives Reference](/concepts/primitives) first.

## Gas Town Recap

Before mapping anything, here is what Gas Town gives you — the parts this page is going to remap onto Gas City. If you have not touched Gas Town recently, or you are still deciding whether to migrate, this is the meaning to recover first.

Gas Town is shaped around a _role taxonomy_ and a _filesystem layout_. Work flows through named roles, state lives in a `~/gt/...` directory tree, and features like plugins, convoys, and path-derived identity are wired into that shape.

The next five groups are the domains we will map, in the same order as the mapping tables below.

### Roles

What each Gas Town role does operationally:

- **Mayor** — the planner and coordinator. The mayor takes high-level intent, breaks it into work, assigns it, and monitors overall progress across the town. It is the human's primary point of contact.
- **Deacon** — the watchdog. The deacon detects stalled or dead agents, restarts them, and enforces SLAs and health thresholds so work keeps moving without human babysitting.
- **Witness** — the lifecycle observer. The witness tracks session health and lifecycle transitions and publishes events about what is happening, giving the rest of the system something to react to.
- **Refinery** — the post-processor. The refinery takes raw agent output and transforms it into structured, usable results — cleaning, summarizing, or reshaping work products before they move on.
- **Polecat** — the ephemeral, on-demand worker. A polecat is spawned for a specific task and exits when done; polecats scale up and down with demand and often run in isolated worktrees.
- **Crew** — the persistent worker pool. Crew are long-lived agents that claim work from a queue and stay around between tasks, the standing workforce of the town.
- **Dog** — the integration / external-messaging relay. Dogs connect the town to the outside world, relaying messages and bridging external systems.

### Mechanisms and behaviors

This domain mixes two related-but-different things: *behaviors* (what the roles did) and the *mechanisms* Gas Town shipped to make them possible. Each bullet is tagged with which it is.

- **Watchdog** *(behavior)* — stall detection, restart, and SLA enforcement (the deacon's job).
- **Lifecycle tracking** *(behavior)* — observing session health and transitions and publishing events about them (the witness's job).
- **Plugins** *(mechanism)* — Gas Town's mechanism for "run something automatically": on a schedule, on an event, or when a condition holds.
- **Convoys** *(mechanism)* — a grouped, tracked set of related work with shared lineage. The mental model carries over to Gas City, but the implementation does not: in Gas City a convoy is bead-backed grouping, not a separate orchestration runtime.
- **Formula running** *(mechanism)* — Gas Town has a built-in formula runner that executes multi-step formula workflows itself.
- **Path-derived identity** *(mechanism)* — who an agent is, inferred from its directory path.

### Filesystem and state

Gas Town **encodes architecture into the directory layout**:

- state lives in a `~/gt/...` tree
- each role gets a home directory
- roles carry role-specific startup files and local settings.

This is one of the bigger shifts: Gas City treats directories as an implementation detail rather than part of the system contract, so much of the "which folder does this role live in" thinking does not carry over.

### Operator workflows

These are the **operator verbs** — the things you actually type to get work done:

- spin up a worker
- send a task to the mayor
- inspect what is stuck
- restart a stalled agent
- etc.

They are *not* Gas Town formulas; formulas are a mechanism (see above). This domain is about the day-to-day moves an operator makes, and it maps to a table of its own.

### Commands

Everything is driven through the `gt` CLI — install, rig, session, sling, convoy, formula, mail, and dozens more.

The full command-by-command translation lives in [Commands → Gas City Equivalents](#commands-→-gas-city-equivalents).

So we covered the parts Gas Town gives you. Next: the small set of building blocks Gas City offers in their place — and then the domain-by-domain map.

## How Gas City Works

The single most important thing to understand about Gas City is that **orchestration is a thin layer on top of work tracking**.

Gas City does not hardcode any roles. There is no built-in mayor, deacon, or polecat baked into the binary — every role you knew in Gas Town is supplied as configuration. The SDK provides only the **infrastructure**: the role-agnostic machinery every orchestration needs no matter what the agents are actually for.

Gas City gives you a small set of building blocks. There are **five primitives**:

- **Session** — start, stop, prompt, and observe agents, regardless of provider.
- **Beads Store** — CRUD over work units. Everything durable is a bead: tasks, mail, molecules, convoys.
- **Event Bus** — an append-only pub/sub log of all system activity.
- **Config** — TOML files that activate capabilities progressively.
- **Prompt Templates** — the behavioral specification for what each role does.

…and **four derived mechanisms** composed from them:

- **Messaging** — mail and nudges.
- **Formulas & Molecules** — workflow templates and their runtime instances.
- **Dispatch** (`gc sling`) — find or spawn an agent and route work to it.
- **Health Patrol** — stall detection and restart-with-backoff.

The **controller** is the engine that keeps these in sync — it owns SDK infrastructure operations such as reconciliation, scaling, order evaluation, and health patrol.

For more in depth details, see the [Primitives Reference](/concepts/primitives).

So when you move from Gas Town to Gas City, the default mental model becomes:

- reusable behavior lives in `pack.toml` plus pack directories
- deployment choices live in `city.toml`
- even the builtin packs are explicit: `gc init` writes their includes into
  `city.toml` (`[workspace] includes = [".gc/system/packs/core", ...]`) —
  nothing is spliced into config composition implicitly
- machine-local bindings and runtime state live in `.gc/`
- every durable work item is a bead
- agents are generic; roles come from prompts, formulas, orders, and config
- the controller owns SDK infrastructure behavior
- directories are an implementation detail, not the architecture

For the full treatment of these building blocks, read the [Primitives Reference](/concepts/primitives) and the [Architecture Overview](/concepts/architecture-overview). The rest of this page assumes them and focuses on translation.

## Mapping Tables

You have seen what Gas Town gives you and the building blocks Gas City offers in their place. The five following tables connect the two, one domain at a time, in the same order as the recap above:

- [roles](#roles-→-gas-city-equivalents)
- [mechanisms](#mechanisms-/-behaviors-→-gas-city-equivalents)
- [filesystem/state](#filesystem-/-state-layout-→-gas-city-equivalents)
- [workflows](#workflows-→-gas-city-equivalents)
- [commands](#commands-→-gas-city-equivalents).

Each table is preceded by a one-sentence scope statement so you always know which domain you are in.

### Roles → Gas City Equivalents

*Scope: how each Gas Town role name maps to Gas City. Every entry on the right is configuration — a user-configured agent plus a prompt template — not a built-in SDK type.*

| Gas Town role | Gas City equivalent | What changes for you |
|---|---|---|
| Mayor | Configured agent + coordinating prompt template (e.g. the Gastown pack's `mayor`) | A role name in a pack, not an SDK primitive. Reachable with `gc session attach mayor`. |
| Deacon | Controller / supervisor infrastructure + config; optionally a configured agent | Watchdog behavior moves into the controller. You tune thresholds in config rather than running a role. |
| Witness | Event bus + waits, formulas, and session scale config | The SDK gives you the mechanisms; a pack decides whether to model a "witness" role at all. |
| Refinery | Configured agent + a formula or order post-processing step | Post-processing is a workflow step, not a standing role type. |
| Polecat | Scalable / transient agent config (a pool — `min`/`max_active_sessions`) | "Polecat" is an operating style — on-demand sessions, often with worktrees — expressed as agent config. |
| Crew | Persistent named agent config | "Crew" is an operating style — long-lived named agents — expressed as agent config. |
| Dog | Exec orders shipped by the builtin core pack; the Gastown pack owns its own `dog` utility pool | Most relay and housekeeping work needs no LLM session, so it ships as core-pack exec orders; the Gastown pack's `dog` pool covers the work that does need an agent. |

### Mechanisms / Behaviors → Gas City Equivalents

*Scope: what those Gas Town roles and features actually *do* — the behaviors — and where that logic lives in Gas City.*

| Gas Town behavior | Gas City equivalent | Notes |
|---|---|---|
| Deacon watchdog logic | Controller health patrol + reconciliation | Stall detection, restart-with-backoff, and reconcile-to-desired-state are controller concerns, not a role agent. |
| Witness lifecycle tracking | Waits, formulas, session scale config, controller wake/sleep, event bus | The mechanisms are first-class; modeling a "witness" on top of them is optional pack behavior. |
| Plugin (scheduled / event / conditional automation) | Order — exec order or formula order | Use an **exec order** for shell or controller-side logic; a **formula order** to instantiate agent-driven work. |
| Convoy as an orchestration runtime | Convoy beads + `gc sling` + formulas | Convoys stay bead-backed grouping and lineage; there is no special convoy runtime layer you must use. |
| Formula runner inside Town workflows | Formula resolution + backend-owned execution | Gas City resolves and dispatches formulas; multi-step execution is backend-dependent today. `bd` is the production path. |
| Path-derived identity | Explicit agent identity, rig scope, env, bead metadata | Do not port code or prompts that assume the directory path implies who the agent is. |

### Filesystem / State Layout → Gas City Equivalents

*Scope: where state lives. Gas Town encodes architecture into directories; Gas City treats directories as an implementation detail.*

| Gas Town location                                                 | Gas City equivalent | Notes |
|-------------------------------------------------------------------|---|---|
| `~/gt/...` directory tree                                         | City directory + `.gc/` runtime state | A city is a directory containing `city.toml` and `.gc/`. Rigs are registered in `city.toml`; each `[[rigs]]` entry has a `path` that defaults under `rigs/` but can point anywhere on disk (absolute, or relative to the city directory). Live state is observed from the runtime and queried from the bead store, not read from a fixed home tree. |
| Town config                                                       | `pack.toml` (reusable behavior) + `city.toml` (this city's deployment) | Gas Town's single town config splits along a definition/deployment seam. |
| Rig config                                                        | `city.toml` `[[rigs]]` entries + `.gc/` (machine-local rig path bindings) | *Which* rigs and their scale are deployment; *where* a rig lives on this machine is a local binding. |
| Role homes                                                        | `agents/<name>/` (`agent.toml` + `prompt.template.md`), in the root city pack (the city directory) or an imported shared pack | Only the agent *definition* lives here. There is no on-disk role "home"; runtime dirs are separate and identity is not path-derived. |
| Role home directories (e.g. `~/gt/mayor/`, `~/gt/<rig>/polecat/`) | `dir` (identity scope) + `work_dir` (session working directory, only when needed) | Set both in the agent's `agent.toml` (or patch per-rig in `city.toml`): use `dir` to carry scope/identity; use `work_dir` only when a role truly needs filesystem isolation. |
| Role-specific startup files and local settings dirs               | Prompt templates, overlays, provider hooks, `pre_start`, `session_setup`, `gc prime` | Startup shaping is explicit and provider-aware, not inferred from where a role lives on disk. |

### Workflows → Gas City Equivalents

*Scope: how you actually *do* things — the operator verbs, not formulas. If you used to perform a task in Gas Town, this is the Gas City way to do it now.*

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
`gc session peek` takes a `--lines` count for a point-in-time snapshot; there is no `--follow` flag. For a continuously updating live view, attach to the session with `gc session attach`. For the system-wide live feed, use `gc events --follow`.
</Note>

### Commands → Gas City Equivalents

See the **[Gas Town → Gas City Command Map](/reference/gastown-command-map)** page for the full `gt` → `gc`/`bd` command mapping table.

## What Usually Maps Cleanly

### Roles Become Pack Agents

If you would have added a new role in Gas Town, the Gas City move is usually:

1. start in your local `city.toml`
2. include a pack if one already solves most of the problem
3. override the stamped agent if you just need local behavior changes
4. edit the pack only when you are changing the shared default for everyone
5. add formulas or orders around the agent if it needs workflow automation

That keeps role behavior in configuration instead of hardcoding more role semantics into the SDK, while still making the common day-one workflow feel local and incremental.

### Start With The City Pack And `city.toml`

This is the main day-one habit to adopt.

Most Gas Town users should begin with the root city pack plus `city.toml`, not by editing an imported shared pack. The split is:

- `pack.toml` imports reusable packs and defines city-specific behavior
- `agents/<name>/` defines city-owned named agents
- `city.toml` declares deployment choices such as rigs, substrates, and scale
- `.gc/` stores site bindings such as local rig paths

Reach for a pack edit when the change should become the new reusable default for every consumer of that pack.

### Plugins Become Orders

This is the most important practical translation.

If the Gas Town idea is "something should run automatically on a schedule, on an event, or when a condition is true", you probably want an order.

- Use an **exec order** when the work is just shell or controller-side logic.
- Use a **formula order** when the work should instantiate agent-driven workflow.

That is the clean replacement for many Town "plugin" instincts. Exec orders are especially important because they can run non-agent commands with no prompt, no session, and no extra role agent.

### Convoys Stay Bead-Shaped

Gas Town teaches people to think in convoys. That mental model still transfers well, but the implementation boundary is different.

In Gas City:

- convoys are still bead-backed grouping and lineage
- `gc sling` can create convoy structure as part of routing
- formulas, orders, and waits compose around that bead graph

So keep the convoy mental model for tracking work, but do not assume it needs a special orchestration subsystem beyond beads plus dispatch.

### Crew and Polecats Are Operating Modes

In Gas Town, these feel like first-class worker types. In Gas City, they are best thought of as conventions:

- **crew**: persistent named agents you expect humans to reason about
- **polecats**: scalable or transient agents, often with dedicated worktrees

That distinction is real and useful, but the SDK does not force it. A pack can adopt the convention, relax it, or replace it.

## Where Gas City Deliberately Differs

### The Controller Owns Infrastructure Behavior

In Gas Town, some orchestration behavior is mediated through specific roles. In Gas City, the controller is the canonical owner of infrastructure operations like:

- reconcile desired sessions to running sessions
- session scaling
- order evaluation
- health patrol
- wisp garbage collection

If something is fundamentally SDK infrastructure, prefer putting it in the controller path instead of inventing another deacon-like role behavior.

### Filesystem Layout Is Not The Architecture

Gas Town uses directories as part of the system contract. Gas City tries not to.

The current rule of thumb is:

- use `dir` in the agent’s `agent.toml` (or patch per-rig in `city.toml`) to carry the agent's scope and identity context
- use `work_dir` in the same places as above when the session must run somewhere else
- use bead metadata for durable handoff state

Good reasons to use a separate `work_dir`:

- the role mutates a repo and needs an isolated worktree
- provider scratch files would collide with another role
- the role needs a durable sandbox independent from the canonical rig root

Bad reason:

- "Gas Town has a separate folder for this role"

### Roles Are Examples, Not SDK Law

The Gastown pack still ships familiar roles, but that is an example operating model, not a type system inside Gas City.

This matters when you change the system:

- adding a new behavior usually means editing a pack, formula, order, or prompt
- it usually does not mean adding a new hardcoded role to the SDK

That is a feature, not a missing abstraction.

It is also worth separating two kinds of changes:

- **local city change**: edit `city.toml`, add rig overrides, add patches, or add a city-specific agent
- **shared product change**: edit the pack because you want a better default for everyone

Most onboarding work should live in the first category.

## Common Translation Patterns

### "I need a new dog"

Ask this first:

- Can this be an exec order?

If yes, prefer the order. That gives you trigger logic, history, and controller ownership without burning an agent slot.

Reach for a dog-like scalable agent config only if the task truly needs a long-lived session, rich interactive context, or repeated agent judgment.

### "I need a witness-like lifecycle manager"

Ask which parts are:

- controller infrastructure
- bead state transitions
- formula logic
- prompt guidance

Only the first category belongs in Go SDK infrastructure. The rest usually live better in the pack.

### "I need another special directory tree"

Usually you do not.

Start with:

- canonical repo root from the rig
- isolated `work_dir` only for roles that mutate repos or need provider-file isolation
- explicit env and metadata, not directory-path inference

### "I need to run something without an agent"

Use an exec order before inventing a plugin, helper role, or hidden session.

That is the direct Gas City answer to many old Town automation tasks.

### "How do I get to my mayor?"

```bash
gc session attach mayor
```

The Mayor session is the primary Gas Town experience — an interactive Claude session with full city context that coordinates everything. The CLI is plumbing; this is the product.

City-scoped agents from the Gastown pack — `mayor`, `deacon`, `boot` — are all accessible the same way. Use `gc session list` to see what is running.

This replaces `gt session at mayor/` or `tmux attach -t gt-mayor` from Gas Town.

## Editing Gastown Config

Ready to make changes? The common Gastown config edits — registering rigs, scaling pools, swapping providers, patching agents, tweaking prompts — live in [Gastown on Gas City: Config Recipes](/guides/gastown-config-recipes).

## What Not To Port Literally

These Gas Town habits usually create unnecessary complexity in Gas City:

- exact `~/gt/...` directory trees
- path-derived identity
- new hardcoded role names in SDK code
- plugin systems when an order is enough
- special helper agents for work that is really a shell command
- duplicating durable state outside beads when labels or metadata are enough

The most common architectural mistake is importing Town's surface area instead of re-expressing the intent in Gas City's primitives.

## Fast Ramp Checklist

If you already know Gas Town, this is the shortest path to becoming effective in Gas City:

1. Read the [Architecture Overview](/concepts/architecture-overview) for the top-down mental model, then the [Primitives Reference](/concepts/primitives) for the nine building blocks in user terms.
2. Skim the [CLI reference](/reference/cli) alongside the [Gas Town → Gas City Command Map](/reference/gastown-command-map) so the `gt` → `gc` muscle memory transfers.
3. Read [Tutorial 07 — Orders](/tutorials/07-orders) and mentally remap "plugins" to "orders".
4. Read [Tutorial 05 — Formulas](/tutorials/05-formulas) and remember that formulas are resolved by Gas City but executed by the configured beads backend.
5. Work through [Tutorial 02 — Agents](/tutorials/02-agents) and [Shareable Packs](/guides/shareable-packs) to see the PackV2 `agents/<name>/` layout end to end.
6. Read [A Complete Gastown Example](/guides/gastown-config-recipes#a-complete-gastown-example) — the city, root pack, and nested pack assembled into one runnable topology.

If you keep those points straight, most of the Gas Town to Gas City ramp goes quickly.
