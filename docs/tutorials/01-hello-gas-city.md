---
title: Tutorial 01 - Hello, Gas City
description: Boot a city, add a rig, create a bead, and attach to the mayor.
---

# Tutorial 01 — Hello, Gas City

Let's say that you're using Claude Code on a significant feature implementation.
You've described the feature, pointed the agent at the right files, and it's
making progress. Then — mid-flight — the context window fills up. The session is
either over or you're at the mercy of the compactation to save the important
details. This is the fundamental problem with AI coding agents: their memory is
the context window, and context windows are finite.

Gas City fixes this with **beads** — tracked work units that persist outside the
agent. When the agent uses beads to record what's done and what's left, running
out of context is no longer catastrophic. In fact, it can be beneficial to clear
out the context before it rots. A fresh session queries beads and picks up right
where the last one left off. The state is in the store, not in the agent's head.

This tutorial builds the simplest possible Gas City orchestration: a named agent
(the "Mayor") to capture the work, an anonymous coding agent to build it and
the beads system that ties it all together.

---

## Starting your city

Before starting, follow the source-based
[installation guide](/getting-started/installation). Once `gc` is
available, initialize a city:

```shell
$ make install
```

A city is a particular set of rules for how your orchestration works and a set
of projects configured for that orchestration. The configuration for a city is
stored in a folder on your computer set aside for that purpose. You can define
multiple cities with multiple configurations, but we'll start with just one for
now. By convention, each city goes into your home folder. Initialize one like
so:

```shell
$ gc init ~/bright-lights
Welcome to Gas City!
Initialized city "bright-lights" with default mayor agent.
```

This creates the city directory with everything you need: a `.gc/` runtime
directory, prompt templates, and a `city.toml` with a default mayor agent
configured.

Now start the city to launch the mayor:

```shell
$ cd ~/bright-lights
$ gc start
City started under supervisor.
```

Starting a city uses the configuration to ensure that you have the agents you
need to do your work. You can update the configuration at any time, stop and
restart your city for new configurations to take effect. If you specify no
configuration, you'll get the default which is what we'll use for the rest of
this tutorial.

## Adding a project

A project (called a "rig") is a git repo associated with a city. Let's create
a simple one:

```shell
$ mkdir hello-world
$ cd hello-world
$ git init
Initialized empty Git repository in /home/user/bright-lights/hello-world/.git/
```

Now register it with the city. Because hello-world is inside the city
directory, `gc` discovers the city automatically:

```shell
$ gc rig add .
Adding rig 'hello-world'...
  Detected git repo at /home/user/bright-lights/hello-world
  Initialized beads database
Rig added.

$ gc rig list

Rigs in /home/user/bright-lights:

  bright-lights (HQ):
    Prefix: brightlights
    Beads:  initialized

  hello-world:
    Path:   /home/user/bright-lights/hello-world
    Prefix: helloworld
    Beads:  initialized
```

Because we're getting the default GC configuration, we have only a single agent
— the Mayor — which is who you'll talk to for planning and coordination.

## Create a Task

To give an agent something to do, you'll want to create a bead that represents a
task. You can do that manually or use the mayor to do that work. Creating a bead
manually looks like this — run from inside the rig directory so `bd` targets
the right beads database:

```shell
$ cd hello-world

# create the bead
$ bd create "create a script that prints 'hello world'"
Created bead: gc-1  (status: open)

# list the beads ready to work on
$ bd ready
ID    STATUS   ASSIGNEE   TITLE
gc-1  open     —          Create a script that prints 'hello world'
```

A new bead starts with a status of `open` — available for claiming. No assignee
yet.

```
$ bd show gc-1
ID:       gc-1
Status:   open
Type:     task
Title:    Create a script that prints 'hello world'
Rig:      hello-world
Created:  2026-02-16 10:30:00
Assignee: —
```

> **Two things to notice:**
>
> 1. The bead has an ID (`gc-1`). Every bead in this city gets a unique ID.
> 2. The bead is stored on disk — not in the agent's context window. Agents come
>    and go. Beads persist.

We created this bead via the CLI. If you'd rather have a conversation instead
of remembering the CLI args, you talk to the Mayor instead:

```shell
$ gc session attach mayor
Attaching to agent 'mayor' (tmux session: bright-lights/mayor)...

╭────────────────────────────────────────╮
│ ✻ Welcome to Claude Code!              │
│   /help for help                       │
│                                        │
│   cwd: ~/bright-lights                 │
╰────────────────────────────────────────╯

You: Can you create a bead in the hello-world rig to create a script that
prints 'hello world'?

Mayor: Sure! I'll create that bead for you.

  $ cd hello-world && bd create "Create a script that prints 'hello world'"
  Created bead: gc-1  (status: open)

Done — gc-1 is in the backlog and ready for a worker to pick up.

You: Can you list the ready beads?

Mayor: Of course.

  $ bd ready
  ID    STATUS   TITLE
  gc-1  open     Create a script that prints 'hello world'

Just the one bead in the backlog right now.
```

The act of "attaching" to the mayor via `gc session attach` brings up the single
instance of that agent running in a tmux session. By using tmux, the mayor's
session is long-lived — it persists even if you close your terminal. You can
detach from it any time with `Ctrl-b d` and reattach later with `gc session attach
mayor`. This is how Gas City keeps agents running in the background while you do
other things.

---

## Let's get to work!

Now let's use a CLI coding agent to pick up that work for our rig. Detach from
the mayor's session (`Ctrl-b d`) and start a coding agent in the rig directory.
Use `gc prime` to give the agent its behavioral prompt — it tells the agent how
to find and execute beads:

```shell
$ tmux new -s hello-world-worker
$ cd hello-world
$ claude "$(gc prime)"
```

Replace `claude` with your preferred agent (`codex --prompt "$(gc prime)"`,
`gemini`, etc.). The `gc prime` command outputs instructions that teach any
agent how to use `bd` commands.

You can watch it build your app, or detach from the tmux session (`Ctrl-b d`)
and let it cook.

Check the bead status from another terminal any time you like:

```shell
$ bd list
ID    STATUS   ASSIGNEE              TITLE
gc-1  active   hello-world-claude    Create a script that prints 'hello world'
```

When the agent finishes, it closes the bead:

```shell
$ bd close gc-1
Closed bead: gc-1

$ bd list
ID    STATUS   ASSIGNEE              TITLE
gc-1  closed   hello-world-claude    Create a script that prints 'hello world'
```

That's it. The coding agent has now written your script. The bead records that
the work happened, who did it, and when it closed.

---

## Starting and stopping

When you're done for the day, stop the city:

```shell
$ gc stop
City stopped.
```

To resume later, start it again:

```shell
$ gc start
City started under supervisor.
```

If you point `gc start` at a directory that is not bootstrapped yet, it now
fails fast and tells you to run `gc init` first. Bootstrap happens in
`gc init`; `gc start` is for starting an existing city.

---

## What You Learned

This tutorial used four of Gas City's five primitives:

| Primitive              | What You Used It For                                           |
| ---------------------- | -------------------------------------------------------------- |
| **Config**             | Default city configuration — one mayor, beads backend          |
| **Agent Protocol**     | `gc init` / `gc start` / `gc stop` / `gc session attach` — managed the mayor |
| **Task Store (Beads)** | `bd create` / `bd list` — tracked the work                     |
| **Prompt Templates**   | `gc prime` — gave agents their behavioral prompts at startup   |

The remaining primitive (Event Bus) is not needed yet. For the current published
tutorial set, use the [Tutorials index](/tutorials/index) instead of the older
numbered progression plan.

---

## What's Next

At this point, you've got yourself a working orchestration system. You can use
the mayor to create beads and hand them off to a coding agent on demand.

But right now you're doing the routing manually — you told the agent to check
beads yourself. Ideally we'd like the agent to know it has outstanding work and
to get to it without any nudging from us.

The old intermediate numbered tutorials were removed instead of being left
stale. For the next maintained walkthrough, jump to
[Tutorial 06 — Formulas](/tutorials/06-formulas), or browse the published list in the
[Tutorials index](/tutorials/index).

---

## Spec Changes Needed

> This section tracks DX decisions from the tutorial that need to flow back
> into `gas-city-spec.md`. Don't delete until the spec is updated.

- **City-as-directory model.** A city is a folder (`~/bright-lights`), not a
  config file embedded in a project repo. `gc init <path>` creates and
  bootstraps a city at that directory; `gc start` starts it. The spec
  currently assumes workspace.toml inside the project.

- **`gc rig add <path>`** — new command to associate a project with a city.
  Creates rig infrastructure (beads, routes). Not in the spec at all.

- **`gc rig list`** — new command to list rigs in a city. Not in the spec.

- **`gc session attach <name>`** — starts or reattaches to a named agent's tmux
  session. Spec has `gc agent start` but not `attach`.

- **Default agent naming: `<rig-name>-<process-name>`** — when a coding agent
  picks up a bead without an explicit agent name, the assignee defaults to
  `<rig-name>-<cli-process-name>` (e.g. `hello-world-claude`). Not in spec.

- **Mayor as overseer, not worker.** The default config creates a mayor whose
  role is planning and coordination, not coding. Workers are separate agents
  started in rig directories. Spec doesn't distinguish mayor from worker role.

- **`gc prime [agent-name]`** — new command that outputs the agent's behavioral
  prompt. Used inline to prime any CLI coding agent: `claude "$(gc prime)"`.
  No AGENTS.md or CLAUDE.md is written into rigs — the prompt is passed
  directly at launch time via the agent's prompt argument.

- **`gc init` / `gc start` semantics.** `gc init [path]` creates and
  bootstraps a complete city (like `git init`). `gc start [path]` starts an
  existing city. Spec doesn't distinguish init from start.

- **`bd claim` is implicit.** Agents pick up beads by working on them; the
  `open → active` transition happens internally. No explicit `bd claim` command
  needed in the basic flow.

- **City discovery via `.gc/` walk-up.** Commands find the city by walking up
  from cwd looking for a `.gc/` directory. No `--city` flag needed in the
  common case.

- **Tutorial reordering.** The original plan had Tutorial 02 = Ralph loop.
  New order: 02 = Named Crew + routing, 03 = Ralph Loop. The Ralph loop
  requires routing as a prerequisite so that beads land on the agent's hook
  and the loop just clears context and picks up the next hooked bead.

- **"Crew" terminology.** Named agents assigned to rigs. New concept not in
  spec. Relates to the existing agent config but adds rig-scoped naming.
