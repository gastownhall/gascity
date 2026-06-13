---
title: Architecture Overview
description: How Gas City's pieces — cities, rigs, agents, sessions, the beads store, the event bus, and the controller — hang together to route and process multi-agent work.
---

Gas City is an orchestration-builder SDK: a toolkit for composing multi-agent
coding workflows. This page gives you the mental model you need before diving
into the [Tutorials](/tutorials/index) — what the major parts are, how work
travels through the system, and how agents get spawned and talk to each other.

You do **not** need to read any internal engineering notes to follow along.
Everything here maps onto commands you can run with `gc`. Once you have the
mental model, the [Primitives Reference](/concepts/primitives) is the deeper,
per-concept companion to this page — dip into it for any single building block.

## The core idea: work is the primitive

The single most important thing to understand about Gas City is that
**orchestration is a thin layer on top of work tracking**.

The system does not hardcode any roles — there is no built-in "manager" or "reviewer" baked into
the binary. Instead, every role is supplied as configuration, and the SDK
provides only the **infrastructure** — the role-agnostic machinery that every
orchestration needs no matter what the agents are actually *for* — which consists of:

- a place to store work (the **beads store**)
- a way to run agents (**sessions**)
- a way to observe what happens (the **event bus**)
- an engine that keeps them all in sync (the **controller**).

None of this machinery knows or cares what your agents do. That is what we mean
throughout this page by "infrastructure": the plumbing the SDK owns, as opposed
to the role behavior you supply as configuration.

## The major pieces

### City

A **city** is the top-level unit of deployment. Concretely, it is a directory
on disk that contains:

- a `city.toml` config file
- a `.gc/` directory of runtime state
- a `.beads/` directory holding the city's own work store.

It also keeps track of the rigs you have registered — but a rig's own directory
can live anywhere on disk, inside or outside the city directory (see
[Rig](#rig)).

You create a city with `gc init`.

A city has exactly one long-running **controller** process that keeps everything reconciled.

### Rig

A **rig** is an external project registered with the city — usually a git
repository you want agents to work in.

A rig's directory can live anywhere on
disk; it does **not** have to sit inside the city directory. You register one
with `gc rig add <path>`, which records the rig by its absolute path.

Each rig gets its own beads namespace and routing context, so work slung inside
one rig stays logically isolated from the others. That isolation is by
`issue_prefix`, not by a separate database: the city and all its rigs share one
underlying store, and `bd` filters every read and write to the current scope's
prefix. See [Beads Storage Topology](/reference/internal/beads-topology) for the details.

### Agent

An **agent** is a configured worker. Agents are pure configuration:

- a name
- the provider that backs them (for example `claude`)
- a prompt template that defines their behavior
- a query that says which work routes to them
- and many more operational knobs (see [Config reference](/reference/config) for the full list)...

Because agents are configuration, you can define as many as you like, and the SDK never assumes any particular one exists.

### Session

A **session** is a single running instance of an agent — a live process (by
default a `tmux` pane) that the SDK can start, stop, prompt, and observe.

Sessions are ephemeral: they come and go, and the work they were doing survives
them, because work lives in the beads store, not in the session.

Two particular controller actions are worth spelling out:

- **Adoption.** If the controller restarts and finds agent processes still
  running from before, it *adopts* them — reconnecting to the live panes and
  recording a session bead for each — instead of killing and respawning them.
  Nothing is lost across a controller restart.
- **Scaling up and down.** An agent can be configured as a *pool*. On each tick
  the controller runs the agent's `scale_check` query to size the pool to
  demand: more pending work spawns more sessions (up to a configured max), and
  idle capacity is retired (down to a configured min).

### Beads store

The **beads store** is the universal persistence substrate.

*Everything* is a bead, meaning a row in the same beads store:

- tasks
- mail messages
- molecules
- convoys.

The store offers a single interface — create, read, update, close, list,
query by label, and walk parent/child relationships.

By default it is backed by Dolt through the `bd` CLI. Physically there is **one** Dolt server per city.
The city root and every rig each hold a `.beads/` configuration directory, but they all resolve to
that single server, and their data is kept logically separate by `issue_prefix`.

Because all domain state flows through one interface, the system converges to correct
outcomes even as sessions churn. See
[Beads Storage Topology](/reference/internal/beads-topology) for where the files live and
how the prefix scoping works.

### Event bus

The **event bus** is the universal observation substrate: an append-only
pub/sub log of everything that happens in the system.

It has two tiers:

- critical events on a bounded queue for infrastructure
- optional fire-and-forget events for audit.

Other parts of the system watch the bus
reactively rather than polling.

### Controller

The **controller** is the per-city reconciliation runtime — the engine that drives all infrastructure.

On a steady ticker (every 30 seconds by default), and
immediately whenever `city.toml` changes, it compares the running sessions
against the state your config *declares* and drives reality toward that
declaration:

- spawning missing sessions
- scaling agent pools
- dispatching automations
- garbage-collecting expired ephemeral work
- restarting stalled sessions.

There is no separate "desired state" file you maintain. The declaration **is**
`city.toml` — which agents should exist, and how many instances each pool should
run — and reconciliation is simply how the controller keeps the live system
matching it.

Crucially, the controller can do all of this with no
user-configured agent running: keeping the infrastructure healthy is the SDK's
job, and user agents only execute work.

## How the pieces fit together

Structurally, a city wraps a controller and a beads store, registers one or
more rigs, and runs agents as live sessions. The event bus sits alongside as
the observation channel everything writes to.

![Structural diagram of a Gas City: the city wraps the controller, beads store,
and event bus, with rigs and live agent sessions inside it. Config declares the
desired state to the controller; the controller reconciles sessions — spawning,
stopping, and restarting them — and reads/writes the store and event bus;
sessions create, claim, and update work in the store and emit activity to the
event bus. No arrow runs from a session back to the controller: agents signal it
only indirectly, through the store and event
bus.](../diagrams/excalidraw-rendered/architecture-structure.svg)

Notice what the diagram does *not* contain: any specific role.

The controller reconciles whatever agents the config declares. Remove an agent from
`city.toml` and the infrastructure keeps working — only that agent's work stops
flowing.

Notice, too, that **no arrow runs from a session back to the controller.** Agents
never call the controller directly. They influence it only by writing to the
beads store and event bus, which the controller reads on its next tick — the
loop closes through shared state, not direct calls. That is why the controller
can keep running even while every agent comes and goes.

## End-to-end: the life of a piece of work

Here we trace the life of **one single bead** — the simplest unit of work — from
the command line to a finished result: (after the diagram below is a list of some
more complex ways work enters the system)

1. **You sling.** `gc sling <agent> "<description>"` kicks off the work from the
   command line.
2. **The beads store records and routes.** A work bead is created and routed by
   running the target agent's routing query (which typically just assigns or
   labels the bead).
3. **The controller reconciles.** On its next reconciliation tick, the
   controller sees ready work routed to an agent that has no live session, and
   spawns one through its runtime provider.
4. **The session receives its prompt.** The new session is handed its rendered
   priming prompt and, following the system's "if it's on your hook, run it"
   principle, queries the beads store for the work hooked to it.
5. **The agent executes.** It does the work — editing files in the rig, running
   commands, and so on — emitting events on the bus as it goes so observers
   (including you, via `bd show --watch`) see the live state.
6. **The bead is updated and closed.** The agent records progress and closes the
   bead when done. The session may shut down or stay warm for the next item;
   either way the result persists in the store.

![Lifecycle diagram of one piece of work: you sling it, the beads store records
and routes the bead, the controller reconciles and spawns a session, the session
receives its rendered priming prompt and queries its hooked work, the agent
executes in the rig, and the bead is updated and closed. Each step is recorded
on the event bus, and you watch live status with bd show
--watch.](../diagrams/excalidraw-rendered/work-lifecycle.svg)

A lone bead _is not_ the only way work enters the system. It is just the
clearest place to start: the same infrastructure carries the richer shapes.
For instance you can also:

- sling a *formula* that expands into a multi-step **molecule** (a root bead plus one bead per step)
- group related work into a **convoy**.

## Agent spawning, lifecycle, and communication

### Spawning

Agents are never started by name in Go code. The controller spawns
a session when reconciliation determines one is needed — for a fixed agent
declared in config, or for an additional pool instance when an agent's
`scale_check` query reports more work.

The prompt template is rendered at spawn
time and is the entire behavioral specification for that session.

### Lifecycle

Sessions are designed to be disposable.

The controller probes
them for liveness, and if one stalls it can restart it with backoff. If a
session crashes, the controller can replace it. If the
controller crashes and a session is alive and well, the controller can adopt it.

Because the work is a
bead and the assignment is a hook on that bead, nothing is lost when a session
dies — a fresh session picks up exactly where the work record says to.

### Communication

Agents coordinate through two derived mechanisms, neither of
which is a new primitive:

- **Mail** is just a bead with a `message` type. An agent's inbox is a query for
  open message beads addressed to it; archiving a message is closing that bead.
- **Nudge** is a session-layer operation: text typed directly into a running
  agent's session to prod it. It is fire-and-forget.

Both reduce to the two primitives you already met — mail is the beads store,
nudge is the session. There is nothing else to learn.

## A runnable example

<Warning>
You will need to [install Gas City](/getting-started/installation) before running
this example.

If `gc` opens a git commit editor instead of running, see the Oh My
Zsh note in
[Troubleshooting](/getting-started/troubleshooting#oh-my-zsh-git-plugin-hides-gc).
</Warning>

Everything above is reachable from a handful of commands. This is the smallest
end-to-end path — create a city, register a rig, route work, and watch it run:

```bash
# 1. Create and start a city (controller comes up automatically)
gc init ~/bright-lights
cd ~/bright-lights

# 2. Register a project directory as a rig
mkdir ~/hello-world && cd ~/hello-world && git init && cd -
gc rig add ~/hello-world

# 3. Sling a work item to an agent — this creates a bead and routes it
cd ~/hello-world
gc sling claude "Create a script that prints hello world"

# 4. Watch the work bead progress as the agent executes it
bd show <bead-id> --watch
```

## Where to go next

- [Primitives Reference](/concepts/primitives) — the deeper, per-concept
  reference for the nine building blocks introduced above.
- [Quickstart](/getting-started/quickstart) — the same path above, in a few
  minutes.
- [Tutorial 01: Cities and Rigs](/tutorials/01-cities-and-rigs) — start the
  guided, end-to-end walkthrough that teaches the full user model.
- [Tutorial 06: Beads](/tutorials/06-beads) — go deeper on the work store that
  underpins everything here.
- [Beads Storage Topology](/reference/internal/beads-topology) — how a city and its rigs
  share one store under the hood.
- [Reference](/reference/index) — command, config, formula, and provider lookup.
