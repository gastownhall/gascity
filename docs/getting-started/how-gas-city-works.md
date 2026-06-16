---
title: How Gas City Works
description: The orientation for Gas City — how it orchestrates fleets of agents, and the six primitives that compose into that orchestration.
---

Gas City **orchestrates fleets of coding agents** through real engineering work.
You write a **formula** — a method for how a job gets done — and the
**orchestrator** runs it as a graph: it decomposes the job into beads, fans the
ready ones out to as many agents as the work allows, holds each step back until
its dependencies close, retries what fails, drains convoys in parallel, and
drives the whole graph to completion *outside your session*.
[Orders](/tutorials/07-orders) trigger formulas on a schedule or an event;
health patrol keeps the fleet alive. **This orchestration is the point.**

What makes it a *platform* and not one fixed orchestrator: the orchestrator hardcodes
**zero roles** — no built-in "manager" or "reviewer." Every role is
configuration supplied through a **Pack**, and the whole orchestration is
composed from six primitives, so the same engine becomes Gas Town, Ralph, or
whatever you configure.

## The machinery underneath

Three pieces of role-agnostic plumbing run the primitives, and you configure no
role around any of them.

| Machinery | What it does |
| --- | --- |
| **Orchestrator** | runs formulas, drives each bead graph forward, and reconciles live agents against what your config declares |
| **Bead store** | durable work — every unit of work is a bead that survives an agent crash, so the orchestrator always has ground truth to resume from |
| **Event bus** | fires activity outward so humans and agents can watch what's happening |

None of this machinery knows what your agents do. It's the substrate the six
primitives sit on. Notice the shape of the loop: the orchestrator acts on
sessions — spawning, stopping, restarting them — but reads their progress from
the bead store and event bus rather than being called back directly. The loop
closes through shared state, which is why work survives a crash on either side.

## The six primitives

| Primitive | Role | Is | Key idea |
| --- | --- | --- | --- |
| **Agent** | WHO | a configured worker — name, provider, prompt template, scope | pure configuration, so define as many as you like; the platform assumes none exists |
| **Bead** | WHAT | one unit of work — ID, title, status, type | the universal substrate: tasks, mail, sessions, convoys are all beads differing only by `type` |
| **Formula** | HOW | a reusable, written-down method applied over work | applying it *produces* work: a formula materializes as beads that outlive the file and any session |
| **Rig** | WHERE | an external project (usually a git repo) registered with the city | each rig gets its own bead namespace and agent scope |
| **Pack** | CONFIGURES | the unit of configuration — declares agents, formulas, orders | the City *is* a pack: the one rooted at this deployment |
| **Event** | OBSERVE | an outbound notification fired by activity | *fired, not polled*; humans and agents both watch the stream |

![The six primitives and how they relate: Packs declare agents, formulas, and orders; a Formula operates over a convoy of Beads, fanning work out to Agents that execute in a Rig; Events are fired so humans and agents can observe.](/diagrams/excalidraw-rendered/primitives.svg)

**Packs** declare the agents, formulas, and orders; the local pack is your
**City**, which can pull in shared packs through imports. A **Formula** operates
over a convoy of **Beads**, fanning work out to **Agents** that execute in a
**Rig**; an **Order** automates *when* a formula runs; and **Events** fire so
humans and agents can observe the whole thing.

### Agent

An **agent** is *who* does the work — a worker a pack defines as a prompt plus a
scope and a provider. The prompt template is its entire behavioral spec; because
the platform has no hardcoded roles, a "reviewer" or a "planner" is nothing more
than the prompt you wrote for it. When an agent is *running* it is a **session** —
a live process the platform can start, stop, prompt, and observe. The engine backing
that session is its **provider**; when the orchestrator restarts, it *adopts* the
live sessions it finds — creating a session bead for each — rather than respawning
them. A single agent can be scaled into a **pool** of identical workers sharing one
queue: each tick the orchestrator runs the agent's `scale_check` query to measure
demand and sizes the pool to it — up to `max_active_sessions`, never below a
`min_active_sessions` floor — retiring sessions that fall idle. Sessions are
disposable; the work they did survives them, because work lives in beads.

### Bead

A **bead** is *what* the work is — one unit with an ID, title, status, and type,
moving `open` → `in_progress` → `closed`. Beads are also the universal store:
tasks, inter-agent mail, running sessions, and convoys are all beads that differ
only by `type`, sharing one query interface. A **convoy** is a container bead
that groups related work so you track a batch as a unit. **Dependencies** are
blocking `needs` edges: a bead with an open blocker is invisible to agents until
that blocker closes — which is how ordering happens with no central scheduler.
Because work persists in beads, the system converges: if an agent dies, its beads
stay open and a fresh agent picks up the same work.

### Formula

A **formula** is *how* a job gets done — a reusable, written-down method, a TOML
file of steps and their dependencies. Applying it compiles the steps into a graph
and materializes them as beads; from that moment a **run** is independent of the
file and of any session. The orchestrator drives the run outside your session,
fanning ready steps out to many agents at once and gating each on its
dependencies. **Sling** (`gc sling`) is the dispatch op that creates *and* routes
in one motion. An **Order** automates *when* a formula runs, pairing a trigger
(cooldown, cron, condition, event, or manual) with the formula to fire — no human
runs a verb. **Health patrol** is one kind of order: each tick the orchestrator
evaluates due triggers and fires them.

### Rig

A **rig** is *where* the work happens — an external project, usually a git repo,
registered with the city with `gc rig add <path>`. A rig carries a repo, its own
**bead namespace**, and an **agent scope**. Its directory can live anywhere on
disk, inside or outside the city. Isolation is by bead-ID prefix, not a separate
database: the city and all its rigs share one underlying store, and reads and
writes are filtered to the current scope's prefix. Work slung in one rig stays
logically isolated from the others, and rig-scoped agents are instantiated once
per rig.

### Pack

A **pack** is what *configures* the system — a directory with a `pack.toml` that
declares agents, formulas, and orders, plus the support files they need. The
local (root) pack is your **City**: the pack rooted at the city directory, where
the city keeps its own definitions alongside its deployment settings. **Imports**
are named dependencies on shared packs; an imported pack's agents, formulas, and
orders read exactly like locally declared ones, so a city reuses behavior defined
elsewhere without copying files. The same engine becomes a different orchestrator
purely by swapping which packs it loads.

### Event

An **event** is how you *observe* what's happening — an immutable, append-only
record fired by city activity, not something the other primitives consume. Every
event carries a monotonically increasing sequence number, so a watcher can replay
the stream from any point. Beads
fire `bead.created` / `bead.closed`, sessions fire `session.woke` /
`session.crashed`, convoys fire `convoy.created` / `convoy.closed`, and orders
fire `order.fired` / `order.completed`. Humans watch the stream with `bd show
--watch`, the `gc events --follow` CLI, or the dashboard; agents and bd hooks
observe and emit too. Events also close the automation loop: an event-triggered
order *reads* the stream to decide when its formula runs — so the same
notifications humans watch can drive the fleet, with no specific agent role
required.

## Where to go next

- [Tutorials](/tutorials/index) — the guided, end-to-end path through every
  primitive above.
- [Understanding Formulas](/guides/understanding-formulas) — the full guide to
  formulas, runs, sling, and orders.
- [Understanding Packs](/guides/understanding-packs) — how packs, cities, and
  imports compose.
- [Reference](/reference/index) — command, config, formula, and provider lookup.
