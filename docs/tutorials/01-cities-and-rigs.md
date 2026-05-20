---
title: Tutorial 01 - Cities and Rigs
sidebarTitle: 01 - Cities and Rigs
description: Create a city, sling work to an agent, add a rig, and configure multiple agents.
---

## Setup

First, you'll need to install at least one CLI coding agent (which Gas City
calls "providers") and make sure that they're on the PATH. Gas City supports
many providers, including but not limited to Claude Code (`claude`), Codex
(`codex`) and Gemini (`gemini`). Make sure you've configured each of your chosen
providers (the more the merrier!) with the appropriate token and/or API key so
that they can each run and do things for you.

Next, you'll need to get the Gas City CLI installed and on your PATH:

```shell
~
$ brew install gascity
...

~
$ gc version
0.13.4
```

> NOTE: the gascity installation is a great way to get the right dependencies in
> place, but may not be enough to keep up with the changes we're making on the
> way to 1.0. Best practice right now is to build your own `gc` binary from HEAD
> on the `main` branch of [the gascity
> repo](https://github.com/gastownhall/gascity) to get the latest and greatest
> bits before running these tutorials.

Now we're ready to create our first city.

## Creating a city

A city is a directory that holds your pack definition, deployment config, agent
prompts, and workflows. You create a new city with `gc init`:

A useful mental model is:

- A **city** is the whole working folder for one Gas City environment. It
  combines your agents, formulas, rigs, orders, and the local settings that
  tell Gas City how to run them on this machine.
- A **pack** is the reusable part of that city. It holds the Gas City
  definitions that are portable and worth sharing with other cities or other
  people.

Another way to say it: a city is a pack plus deployment details.

```shell

~
$ gc init ~/my-city
Welcome to Gas City SDK!

Choose a config template:
  1. minimal   — default coding agent (default)
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
[3/8] Scaffolding agent prompts
[4/8] Writing pack.toml
[5/8] Writing city configuration
Created minimal config (Level 1) in "my-city".
[6/8] Checking provider readiness
[7/8] Registering city with supervisor
Registered city 'my-city' (/Users/csells/my-city)
Installed launchd service: /Users/csells/Library/LaunchAgents/com.gascity.supervisor.plist
[8/8] Waiting for supervisor to start city
  Adopting sessions...
  Starting agents...

~
$ gc cities
NAME        PATH
my-city     /Users/csells/my-city
```

You can avoid the prompts and just specify what provider you want. Here's the
same command, just providing the provider explicitly.

```shell
~
$ gc init ~/my-city --provider claude
```

Gas City created the city directory, registered it, and started it. A city
created with `gc init` comes with `pack.toml`, `city.toml`, and the standard
top-level directories, so let's look at what's inside:

```shell
~
$ cd ~/my-city

~/my-city
$ ls
agents  assets  city.toml  commands  doctor  formulas  orders  overlay  pack.toml  template-fragments
```

At the top level of the city directory:

- `pack.toml` — the portable pack definition layer
- `city.toml` — city-local deployment and runtime settings

This city comes with a built-in `mayor` agent. The mayor's prompt lives at
`agents/mayor/prompt.template.md`, and `pack.toml` defines the always-on mayor
session that uses it. Assuming you chose the default `minimal` config
template and default provider, `city.toml` keeps the shared runtime settings:

```shell
~/my-city
$ cat city.toml
[workspace]
provider = "claude"
```

The portable pack definition lives next to it:

```shell
~/my-city
$ cat pack.toml
[pack]
name = "my-city"
schema = 2

[[named_session]]
template = "mayor"
mode = "always"
```

The `[workspace]` section in `city.toml` sets shared runtime defaults such as
the provider. The machine-local workspace identity lives in `.gc/site.toml`
instead, which is how `gc cities`, `gc status`, and other commands still know
this city is named `my-city`.

The built-in `mayor` comes from the scaffolded `agents/mayor/` content, and
`[[named_session]]` keeps a `mayor` session running so you can talk to it at
any time. When you add more agents later, Gas City creates `agents/<name>/`,
with `prompt.template.md` for the prompt and `agent.toml` for any per-agent
overrides.

Gas City also gives you an implicit agent for each supported provider — so
`claude`, `codex`, and `gemini` are available as agent names even though they're
not listed in `pack.toml`. These use the provider's defaults with no custom
prompt.

To check on the status of your city, use `gc status`:

```shell
~/my-city
$ gc status
my-city  /Users/csells/my-city
  Controller: supervisor-managed (PID 83621)
  Authority: supervisor process PID 83621
  Suspended:  no

Agents:
  dog                     scaled (min=0, max=3)
    dog-1                 stopped
    dog-2                 stopped
    dog-3                 stopped

0/3 agents running

Named sessions:
  mayor                   reserved-unmaterialized (always)
```

The `dog` pool is a background utility agent from the built-in maintenance
pack. It handles internal housekeeping like shutdown coordination. You don't
need to interact with it — ignore it for now.

## Adding a rig

In Gas City, a project directory registered with a city is called a "rig."
Rigging a project's directory lets agents work in it.

```shell
~/my-city
$ gc rig add ~/my-project
Adding rig 'my-project'...
  Prefix: mp
  Initialized beads database
  Generated routes.jsonl for cross-rig routing
Rig added.
```

Gas City derived the rig name from the directory basename (`my-project`) and set
up work tracking in it. The shared rig declaration lives in `city.toml`:

```shell

~/my-city
$ cat city.toml
[workspace]
provider = "claude"

... # content elided

[[rigs]]
name = "my-project"
```

The machine-local workspace identity and path binding live in `.gc/site.toml`:

```toml
workspace_name = "my-city"

[[rig]]
name = "my-project"
path = "/Users/csells/my-project"
```

You can also see your city's rigs with `gc rig list`:

```shell
~/my-project
$ gc rig list

Rigs in /Users/csells/my-city:

  my-city (HQ):
    Prefix: mc
    Beads:  initialized

  my-project:
    Path:   /Users/csells/my-project
    Prefix: mp
    Beads:  initialized
```

## Slinging your first work

You assign work to agents by "slinging" it — think of it as tossing a task to
someone who knows what to do. To sling work on a rig, start from inside the rig
directory and target the rig-scoped agent explicitly:

```shell
~/my-city
$ cd ~/my-project

~/my-project
$ gc sling my-project/claude "Write hello world in python to the file hello.py"
Created mp-ff9 — "Write hello world in python to the file hello.py"
Attached wisp mp-6yh (default formula "mol-do-work") to mp-ff9
Auto-convoy mp-4tl
Slung mp-ff9 → my-project/claude
```

Because the target is `my-project/claude`, the work stays scoped to this rig.

The `gc sling` command created a work item in our city (called a "bead") and
dispatched it to the `claude` agent. You can watch it progress:

```shell
~/my-city
$ gc bd show mp-ff9 --watch
✓ mp-ff9 · Write hello world in python to the file hello.py   [● P2 · CLOSED]
Owner: Chris Sells · Assignee: claude-mp-208 · Type: task
Created: 2026-04-07 · Updated: 2026-04-07

NOTES
Done: created hello.py

PARENT
  ↑ ○ mp-6yh: sling-mp-ff9 ● P2

Watching for changes... (Press Ctrl+C to exit)
```

Once the bead moves to `CLOSED`, you can see the results:

```shell
~/my-project
$ ls
hello.py
```

Success! You just dispatched work to an AI agent and gotten results back.

## What's next

You've created a city, slung work to agents, added a project as a rig, and slung
work to that rig. From here:

- **[Agents](/tutorials/02-agents)** — go deeper on agent configuration:
  prompts, sessions, scope, working directories
- **[Sessions](/tutorials/03-sessions)** — interactive conversations with
  agents, polecats and crew
- **[Formulas](/tutorials/05-formulas)** — multi-step workflow templates with
  dependencies and variables
