---
title: Quickstart
description: Create a city, add a rig, and route work in a few minutes.
---

<Note>
This guide assumes you have already installed Gas City and its
prerequisites. If you haven't, start with the
[Installation](/getting-started/installation) page.
</Note>

You will need `gc`, `tmux`, `git`, `jq`, and a beads provider (`bd` + `dolt`
by default, or set `GC_BEADS=file` to skip them).

<Tip>
Oh My Zsh's `git` plugin defines a `gc` alias for `git commit --verbose`. If
`gc version` or `gc init` opens git commit instead of Gas City, use
`command gc ...` temporarily and remove the alias after Oh My Zsh loads.
See [Troubleshooting](/getting-started/troubleshooting#oh-my-zsh-git-plugin-hides-gc).
</Tip>

## 1. Create a City

```bash
gc init ~/bright-lights
cd ~/bright-lights
```

`gc init` bootstraps the city directory, registers it with the supervisor, and
starts the orchestrator. The city is running as soon as init completes.

## 2. Add a Rig

```bash
mkdir ~/hello-world && cd ~/hello-world && git init && cd -
gc rig add ~/hello-world
```

A rig is an external project directory registered with the city. It gets its
own beads database, hook installation, and routing context.

## 3. Sling Work

```bash
cd ~/hello-world
gc sling claude "Create a script that prints hello world"
```

`gc sling` creates a work item (a bead) and routes it to an agent. Gas City
starts a session, delivers the task, and the agent executes it.

This is the smallest possible job: one bead, one agent. Gas City's real power
is orchestration -- you write a **formula** (the method for getting a job done)
and the **orchestrator** runs it as a graph: decomposing the work into beads,
fanning the ready ones out to many agents at once, gating each step on its
dependencies, retrying failures, and driving the whole thing to completion
outside your session. See [Formulas](/tutorials/05-formulas) and
[Orders](/tutorials/07-orders).

## 4. Watch an Agent Work

```bash
bd show <bead-id> --watch
```

For a fuller walkthrough of cities and rigs, continue to
[Tutorial 01](/tutorials/01-cities-and-rigs). To see Gas City do the thing it
exists for -- the orchestrator running a formula as a graph across many agents --
jump to [Formulas](/tutorials/05-formulas) and then
[Orders](/tutorials/07-orders), which trigger formulas on a schedule or event.
