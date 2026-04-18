---
title: Quickstart
description: Create a city, add a rig, and route work in a few minutes.
---

<Note>
This guide assumes you have already installed Gas City and its
prerequisites for the default stack. If you haven't, start with the
[Installation](/getting-started/installation) page.
</Note>

This walkthrough uses the default backends: `tmux` for session management and
the `bd` + `dolt` + `flock` stack for beads. If your city uses
`GC_SESSION=subprocess`, `GC_SESSION=acp`, `GC_SESSION=k8s`,
`GC_SESSION=exec:<script>`, or `GC_BEADS=file`, see
[Installation](/getting-started/installation) for the rows you can skip.

## 1. Create a City

```bash
gc init ~/bright-lights
cd ~/bright-lights
```

`gc init` bootstraps the city directory, registers it with the supervisor, and
starts the controller. The city is running as soon as init completes.

## 2. Add a Rig

```bash
mkdir ~/hello-world && cd ~/hello-world && git init
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

## 4. Watch an Agent Work

```bash
bd show <bead-id> --watch
```

For a fuller walkthrough of the same path, continue to
[Tutorial 01](/tutorials/01-cities-and-rigs).
