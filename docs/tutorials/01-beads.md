---
title: Tutorial 01 - Beads
description: Boot a city, add a rig, create a bead, sling a bead and watch an agent do the work.
---

# Tutorial 01: Beads

Hello and welcome to the first tutorial for [Gas City](https://github.com/gastownhall/GasCity)! The tutorials are designed to get you started using Gas City from the ground up.

## Setup

First, you'll need to install at least one CLI coding agent and make sure that they're on the PATH. It's common for agentic engineers to use a combination of Claude Code (`claude`), Codex CLI (`codex`) and/or Gemini CLI (`gemini`). Gas City is multi-agent, so I recommend having more than one CLI coding agent installed. Also, make sure you've configured each of them with the appropriate token and/or API key so that they can each run and do things for you.

Next, you'll need to get the Gas City CLI installed and on your PATH:

```shell
~
$ brew install gastownhall/gascity/gascity
...
==> Summary
🍺  /opt/homebrew/Cellar/gascity/0.13.3-rc7: 6 files, 53.1MB, built in 2 seconds
```

Now we're ready to create our first city.

## Configuration

A city is a folder containing your specific configuration of agents (and their prompts), orchestrations ("formula"), automations ("orders"), extensions ("packs") and the list of your project folders ("rigs"). You can create your new city using the newly installed `gc` command. If you're following along at home, pick the "tutorial" config template:

```shell

~
$ gc init river-city
Welcome to Gas City SDK!

Choose a config template:
  1. tutorial  — default coding agent (default)
  2. gastown   — multi-agent orchestration pack
  3. custom    — empty workspace, configure it yourself
Template [1]:

Choose your coding agent:
  1. Claude Code  (default)
  2. Codex CLI
  3. Gemini CLI
  4. Cursor Agent
  5. GitHub Copilot
  6. Sourcegraph AMP
  7. OpenCode
  8. Auggie CLI
  9. Pi Coding Agent
  10. Oh My Pi (OMP)
  11. Custom command
Agent [1]:
[1/8] Creating runtime scaffold
[2/8] Installing hooks (Claude Code)
[3/8] Writing default prompts
[4/8] Writing default formulas
[5/8] Writing city configuration
Created tutorial config (Level 1) in "river-city".
[6/8] Checking provider readiness
[7/8] Registering city with supervisor
Registered city 'river-city' (/Users/csells/river-city)
Installed launchd service: /Users/csells/Library/LaunchAgents/com.gascity.supervisor.plist
[8/8] Waiting for supervisor to start city
  Adopting sessions...
  Starting agents...
```

By convention, your first city is often called `gc`, but to avoid confusion in the tutorial and because of my love of classic musicals, with `river-city` today. At this point, GC has created your city directory (`~/river-city`), registered it with the city supervision process and created the configuration file that describes your city (`city.toml`) as well as several configuration folders.

```shell
$ cd river-city

~/river-city
$ ls
city.toml  formulas  hooks  orders  packs  prompts
```

 Assuming that you chose the "tutorial" configuration, your new city's configuration will look like this:

```
[workspace]
name = "river-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
```

You'll see the `workspace` table, which defines the name of your city and the default provider. Out of the box, you'll see just one explicitly configured `agent`: the mayor (we'll talk to the mayor in the next tutorial). You'll also get an implicitly configured agent for each of the CLI agents that GC supports, e.g. `claude`, `codex` and `gemini`. You assign work to your agents by their name, as we'll see.

But before we create work, let's create a place for the results of that work.

## Rigs

A project in GC is called a "rig." A rig is a folder for agents to work in, most often mapped to a git repo. A rig can live anywhere in your file system. However, for `gc` to find the appropriate `city.toml` when  working in your rig folders, it needs to know where to look. So, to keep things simple during this tutorial, we'll put our rigs into a `river-city/rigs` folder. You can add a rig to your city like so:

```shell
~/river-city
$ gc rig add rigs/hello-world
Adding rig 'hello-world'...
  Prefix: hw
  Initialized beads database
  Generated routes.jsonl for cross-rig routing
Rig added.
```

Adding a rig causes it to be governed by the configuration of that city. And so, after adding a rig, you can see it in the `city.toml`:

```
[workspace]
name = "river-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[rigs]]
name = "hello-world"
path = "/Users/csells/river-city/rigs/hello-world"
```

Adding a rig also creates a beads database:

```shell
~/river-city
$ cd rigs/hello-world

~/river-city/rigs/hello-world
$ ls -la
drwxr-x---@  - csells 21 Mar 17:28 .beads
.rw-r--r--@ 54 csells 21 Mar 17:28 .gitignore
```

The `.beads` folder contains a database (with beads-related `.gitignore` entries) that tracks the work for that rig. With our city and agents in place, and a rig to work on, we're ready to create some beads.

## Beads

The unit of work in GC is called a "bead" and it represents a prompt for an agent to execute. We can create a bead with the `bd` command:

```shell
~/river-city/rigs/hello-world
$ bd create "Write hello world in the language of your choice"
...
✓ Created issue: hw-c18 — Write hello world in the language of your choice
  Priority: P2
  Status: open
```

The prefix comes from the name of the rig, e.g. "hello-world" shortens to "hw". To see a bead, you can show it:

```shell
~/river-city/rigs/hello-world
$ bd show hw-c18
○ hw-c18 · Write hello world in the language of your choice   [● P2 · OPEN]
Owner: Chris Sells · Type: task
Created: 2026-03-22 · Updated: 2026-03-22
```

You can see the prompt we gave it and that the status is `OPEN`. To execute it, you "sling" it to an agent:

```shell
~/river-city/rigs/hello-world
$ gc sling claude hw-c18
Attached wisp hw-c18.1 (default formula "mol-do-work") to hw-c18
Auto-convoy hw-ruz
Slung hw-c18 → hello-world/claude
```

We'll talk about wisps and formulas in future tutorials, but for now you can see that we've got an agent (`claude`) working on executing our bead. You can watch it progress through the bead stages like so:

```shell
# Bead hw-c18: OPEN → IN_PROGRESS → CLOSED
$ bd show hw-c18 --watch
✓ hw-c18 · Write hello world in the language of your choice   [● P2 · CLOSED]
Owner: Chris Sells · Assignee: Chris Sells · Type: task
Created: 2026-03-22 · Updated: 2026-03-22

NOTES
Done: wrote hello world in Python (hello.py)


LABELS: pool:hello-world/claude

PARENT
  ↑ ○ hw-ruz: sling-hw-c18 ● P2

CHILDREN
  ↳ ○ hw-c18.1: mol-do-work ● P2


Watching for changes... (Press Ctrl+C to exit)
```

There's a lot of detail in that output that we'll cover in future tutorials, but you can see that the bead is now `CLOSED` and contains the agent's summary of the work it did under `NOTES`. Even more importantly, you can check your rig folder to see what the agent produced:

```shell
~/river-city/rigs/hello-world
$ ls
hello.py

~/river-city/rigs/hello-world
$ cat hello.py
print("Hello, World!")

~/river-city/rigs/hello-world
$ python hello.py
Hello, World!
```

Success!

## Simply Slinging

Of course, you may wonder why we went through all of that work to execute a single prompt. And if that's all you want to do, you'd be right – Gas City is optimized for parallelizing thousands of beads through 100+ agents acting on dozens of rigs. We'll see how to scale on this foundation in future tutorials.

Even so, there is a shortcut if you find yourself just wanting to sling a prompt to an agent on your rig:

```shell
~/river-city/rigs/hello-world
$ gc sling claude "Write hello-world.cpp"
Created hw-1oy — "Write hello-world.cpp"
Attached wisp hw-1oy.1 (default formula "mol-do-work") to hw-1oy
Auto-convoy hw-51p
Slung hw-1oy → hello-world/claude
```

Here you're specifying the agent and the prompt in a single command, which GC uses to create a bead and sling it for you.
