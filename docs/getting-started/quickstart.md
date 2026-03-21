---
title: Quickstart
description: Create a city, add a rig, and route work in a few minutes.
---

This quickstart assumes you already have `gc`, `bd`, `tmux`, and `jq`
available.

## 1. Create a City

```bash
$ gc init ~/bright-lights
$ gc start
```

`gc init` bootstraps the city directory. `gc start` runs the controller under
the supervisor and begins reconciling configured agents.

## 2. Add a Rig

```bash
mkdir hello-world
cd hello-world
git init
$ gc rig add .
```

A rig is an external project directory managed by the city. It gets its own
beads database, hook installation, and routing context.

## 3. Create Work

```bash
$ bd create "Create a script that prints hello world"
$ bd ready
```

Gas City uses beads as the durable work substrate. The controller and agents
coordinate through the store instead of depending on process-local state.

## 4. Watch an Agent Work

```bash
$ gc session attach mayor
```

For a fuller walkthrough of the same path, continue to
[Tutorial 01](../tutorials/01-hello-gas-city.md).
