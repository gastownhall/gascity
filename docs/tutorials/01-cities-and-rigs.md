---
title: Tutorial 01 - Cities and Rigs
sidebarTitle: 01 - Cities and Rigs
description: Create a city, add a project as a rig, and sling your first work to an agent.
---

## Setup

First, you'll need to install at least one CLI coding agent (which Gas City
calls "providers") and make sure that they're on the PATH. Gas City supports
many providers, including but not limited to Claude Code (`claude`), Codex
(`codex`), Gemini (`gemini`), Grok Build (`grok`), OpenCode (`opencode`),
Groq (`groq`), and Cerebras (`cerebras`). Make sure you've configured each of your chosen
providers (the more the merrier!) with the appropriate token and/or API key so
that they can each run and do things for you.

Next, you'll need to get the Gas City CLI installed and on your PATH:

```shell
~
$ brew install gascity
...

~
$ gc version
1.2.1
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
  1. Claude Code
  2. Codex
  3. Gemini CLI
If you don't see your coding agent, configure it and restart the wizard.
Agent: 1
[1/8] Creating runtime scaffold
[2/8] Installing hooks (Claude Code)
[3/8] Writing default prompts
[4/8] Writing pack.toml
[5/8] Writing city configuration
Created minimal config (Level 1) in "my-city".
[6/8] Checking provider readiness
[7/8] Registering city with supervisor
Registered city 'my-city' (/Users/csells/my-city)
Installed launchd service: /Users/csells/Library/LaunchAgents/com.gascity.supervisor.plist
[8/8] Waiting for supervisor to start city

~
$ gc cities
NAME     PATH
my-city  /Users/csells/my-city
```

The agent menu lists only the coding agents the wizard finds configured on
your machine — today it can probe Claude Code, Codex, Gemini CLI, and
Antigravity. If exactly one is configured, the wizard selects it without
asking.

You can avoid the prompts and just specify what provider you want. Here's the
same command, with the provider supplied explicitly.

```shell
~
$ gc init ~/my-city --default-provider claude
```

Gas City created the city directory, registered it, and started it. A city
created with `gc init` comes with `pack.toml`, `city.toml`, and the standard
top-level directories, so let's look at what's inside:

```shell
~
$ cd ~/my-city

~/my-city
$ ls
agents  assets  city.toml  commands  doctor  formulas  orders  overlays  pack.toml  template-fragments
```

At the top level of the city directory:

- `pack.toml` — the portable pack definition layer
- `city.toml` — city-local deployment and runtime settings

This city comes with a built-in `mayor` agent. The mayor's prompt lives at
`agents/mayor/prompt.template.md`, and `pack.toml` defines the always-on mayor
session that uses it. Assuming you chose the default `minimal` config
template and Claude Code, `city.toml` keeps the shared runtime settings:

```shell
~/my-city
$ cat city.toml
[workspace]
provider = "claude"

[providers]
[providers.claude]
base = "builtin:claude"
ready_delay_ms = 0

[daemon]
formula_v2 = true

... # commented mail-retention example elided
```

The portable pack definition lives next to it:

```shell
~/my-city
$ cat pack.toml
[pack]
name = "my-city"
schema = 2

[imports.core]
source = "https://github.com/gastownhall/gascity.git//internal/bootstrap/packs/core"
version = "sha:<pinned commit>"

[imports.bd]
source = "https://github.com/gastownhall/gascity.git//examples/bd"
version = "sha:<pinned commit>"

[[named_session]]
template = "mayor"
mode = "always"
```

The `[workspace]` section in `city.toml` sets shared runtime defaults such as
the provider. The `[providers.claude]` table registers your chosen provider
against the builtin `claude` preset, and `[daemon]`'s `formula_v2 = true`
turns on the v2 formula compiler — the default for new cities (you'll meet
formulas in [Tutorial 05](/tutorials/05-formulas)). The `[imports]` entries
in `pack.toml` pin the builtin packs bundled with the `gc` binary: `core`
(housekeeping orders, doctor checks, default prompts, and core formulas) and
— for cities on the default `bd` beads provider — `bd`, which brings in its
`dolt` helper pack. They resolve offline from the user-global pack cache,
which the binary keeps hydrated with its own embedded content. If these
imports go missing, `gc doctor --fix` restores them. The machine-local
workspace identity lives in `.gc/site.toml` instead, which is how `gc
cities`, `gc status`, and other commands still know this city is named
`my-city`.

The built-in `mayor` comes from the scaffolded `agents/mayor/` content, and
`[[named_session]]` keeps a `mayor` session running so you can talk to it at
any time. When you add more agents later, Gas City creates `agents/<name>/`,
with `prompt.template.md` for the prompt and `agent.toml` for any per-agent
overrides.

Gas City also gives you an implicit agent for each provider declared in
`city.toml`'s `[providers]` table — so `claude` is available as an agent name
(and, once you add a rig, `<rig>/claude`) even though it's not listed in
`pack.toml`. Implicit agents use the core pack's stock pool-worker prompt and
get `mol-do-work` as their default sling formula — more on that in a moment.

To check on the status of your city, use `gc status`:

```shell
~/my-city
$ gc status
my-city  /Users/csells/my-city
  Controller: supervisor-managed (PID 83621)
  API:        http://127.0.0.1:8372
  Authority: supervisor process PID 83621
  Suspended:  no

Agents:
  dolt.dog                scaled (min=0, max=2)
    dolt.dog-1            stopped
    dolt.dog-2            stopped
  control-dispatcher      stopped

0/3 agents running

Named sessions:
  mayor                   reserved-unmaterialized (always)
  control-dispatcher      reserved-unmaterialized (on_demand)
```

A named session shows `reserved-unmaterialized` until the controller
materializes it; once the mayor session is up, its state reads `awake` (or
`active` — the two are equivalent).

The `dolt.dog` pool is a background utility agent from the bundled `dolt` pack
(pulled in through the `bd` include you saw in `city.toml` — the `dolt.`
prefix is the import binding it arrived through). It handles Dolt database
housekeeping for the beads backend. `control-dispatcher` is SDK
infrastructure: the controller uses it to advance formula workflows. You don't
need to interact with either — ignore them for now.

## Adding a rig

<Note>
If another Gas City workspace is already registered (check `gc cities`),
commands inside `~/my-city` may resolve to that city and fail. Pass `--city
~/my-city` explicitly when that happens. These examples assume a single
registered city.
</Note>

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

[daemon]
formula_v2 = true
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
someone who knows what to do. To sling work on a rig, target the rig-scoped
agent explicitly; we'll also hop into the rig directory so we can inspect the
results:

```shell
~/my-city
$ cd ~/my-project

~/my-project
$ gc sling my-project/claude "Write hello world in python to the file hello.py"
Created mp-ff9 — "Write hello world in python to the file hello.py"
Attached workflow mp-6yh (formula "mol-do-work") to mp-ff9
```

Because the target is `my-project/claude`, the work stays scoped to this rig.

The `gc sling` command created a work item in our city (called a "bead") and
dispatched it to the `claude` agent. Two more things happened behind the
scenes: sling instantiated a workflow from the agent's default formula
(`mol-do-work` — read the bead, do the work, close it), and it created an
input convoy that tracks your bead so the workflow knows what to act on. You
can watch the bead progress:

```shell
~/my-project
$ gc bd show mp-ff9 --watch
○ mp-ff9 · Write hello world in python to the file hello.py   [● P2 · OPEN]
Owner: Chris Sells · Type: task
Created: 2026-04-07 · Updated: 2026-04-07

BLOCKS
  ← ○ mp-4tl: input convoy for mp-ff9 ● P2

Watching for changes... (Press Ctrl+C to exit)
```

The `BLOCKS` line shows the input convoy that sling created to track the bead.
When the agent finishes the work, the watch view updates and the bead's status
flips from `OPEN` to `CLOSED`.

Once the bead moves to `CLOSED`, you can see the results:

```shell
~/my-project
$ ls
hello.py
```

Success! You just dispatched work to an AI agent and got results back.

## What's next

You've created a city, added a project as a rig, and slung work to an agent on
that rig. From here:

- **[Agents](/tutorials/02-agents)** — go deeper on agent configuration:
  prompts, sessions, scope, working directories
- **[Sessions](/tutorials/03-sessions)** — interactive conversations with
  agents, polecats and crew
- **[Formulas](/tutorials/05-formulas)** — how multi-step work should be
  done: steps, dependencies, and variables
