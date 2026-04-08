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

A city is a directory that holds your agent configuration, prompts, and
workflows. You create a new city with `gc init`:

```shell

~
$ gc init ~/my-city
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
Created tutorial config (Level 1) in "my-city".
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

Gas City created the city directory, registered it, and started it. Let's look
at what's inside:

```shell
~
$ cd ~/my-city

~/my-city
$ ls
city.toml  formulas  hooks  orders  prompts
```

The main file is `city.toml` — it defines your city, using the contents of those
directories as well as containing some definitions and local config. Assuming
you chose the default `tutorial` config template and default provider,
`city.toml` looks like this:

```shell
~/my-city
$ cat city.toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[named_session]]
template = "mayor"
mode = "always"
```

The `[workspace]` section names your city and sets the default provider.

Each `[[agent]]` table you configure lets you create a named set of config
including things like the provider, the model, the prompt you want to use to
define its role, etc. An agent is named so that you can assign it work (aka
"sling"). Here we've created an agent called the `mayor` with a prompt template (the instructions for the mayor) and a default session where you instructions go.

Gas City also gives you an implicit agent for each supported provider — so
`claude`, `codex`, and `gemini` are available as agent names even though they're
not listed in `city.toml`. These use the provider's defaults with no custom
prompt.

## Slinging your first work

You assign work to agents by "slinging" it — think of it as tossing a task to
someone who knows what to do. The `gc sling` command takes an agent name and a
prompt:

```shell
~/my-city
$ gc sling claude "Write hello world in python to the file hello.py"
Created mc-tdr — "Write hello world in python to the file hello.py"
Attached wisp mc-jmb (default formula "mol-do-work") to mc-tdr
Auto-convoy mc-2pa
Slung mc-tdr → claude
```

The `gc sling` command created a work item in our city (called a "bead") and
dispatched it to the `claude` agent. You can watch it progress:

```shell
~/my-city
$ bd show mc-tdr --watch
✓ mc-tdr · Write hello world in python to the file hello.py   [● P2 · CLOSED]
Owner: Chris Sells · Assignee: claude-mc-208 · Type: task
Created: 2026-04-07 · Updated: 2026-04-07

NOTES
Done: created hello.py with print('Hello, World!')

PARENT
  ↑ ○ mc-2pa: sling-mc-tdr ● P2

Watching for changes... (Press Ctrl+C to exit)
```

Once the bead moves to `CLOSED`, you can see the results:

```shell
~/my-city
$ cat hello.py
print("Hello, World!")

~/my-city
$ python hello.py
Hello, World!
```

Success! You just dispatched work to an AI agent and gotten results back.

## Adding a rig

So far, the agent worked in the city directory itself. But your real projects
live somewhere else — in their own directories, probably as git repos. In Gas
City, a project directory registered with a city is called a "rig." Rigging a
project's directory lets agents work in it.

```shell
~/my-city
$ gc rig add ~/my-project
Adding rig 'my-project'...
  Prefix: mp
  Initialized beads database
  Generated routes.jsonl for cross-rig routing
  Registered in global rig index
Rig added.
```

Gas City derived the rig name from the directory basename (`my-project`) and set
up work tracking in it. You can see the new entry in `city.toml`:

```shell

~/my-city
$ cat city.toml
[workspace]
name = "my-city"
provider = "claude"

... # content elided

[[rigs]]
name = "my-project"
path = "/Users/csells/my-project"
```

If you want to sling work to be done in a rig, the easiest way to do that is
from inside a rig directory. Gas City figures out which rig and city you're in
based on your current working directory:

```shell
~/my-city
$ cd ~/my-project

~/my-project
$ gc sling claude "Add a README.md with a project description"
Created mp-ff9 — "Add a README.md with a project description"
Attached wisp mp-6yh (default formula "mol-do-work") to mp-ff9
Auto-convoy mp-4tl
Slung mp-ff9 → my-project/claude
```

Notice that the work was splung (slinged?) to `my-project/claude` — the agent is
scoped to this rig. Check the result:

```shell
~/my-project
$ ls
README.md
```

You can see all of your city's rigs with `gc rig list`:

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

## Managing your city

A few commands you'll use regularly:

To check which agents are running, you use `gc status`:

```shell
~/my-project
$ gc status
my-city  /Users/csells/my-city
  Controller: standalone (PID 83621)
  Suspended:  no

Agents:
  dog                     pool (min=0, max=3)
2026/04/06 21:20:22 tmux state cache: refreshed 2 sessions in 3.582ms
    dog-1                 stopped
    dog-2                 stopped
    dog-3                 stopped
  mayor                   pool (min=0, max=unlimited)
  claude                  pool (min=0, max=unlimited)
  my-project/claude       pool (min=0, max=unlimited)

1/4 agents running

Rigs:
  my-project              /Users/csells/my-project

Sessions: 2 active, 0 suspended
```

Sometimes you need agents to stop what they're doing — you're reorganizing a
directory tree, making a large manual commit, or taking a snapshot you don't
want agents to interfere with. In that case, you can suspend the city:

```shell
~/my-project
$ gc suspend
City suspended (/Users/csells/my-city)
```

This pauses all agent activity while keeping the city registered and its
resources intact. Resume when you're ready:

```shell
~/my-project
$ gc resume
City resumed (/Users/csells/my-city)
```

You can do the same thing with a rig via `gc rig suspect` and `gc rig resume`.

To stop the city entirely and release resources:

```shell
~/my-city
$ gc stop
Unregistered city 'my-city' (/Users/csells/my-city)
Reconciliation triggered.
City stopped.

~/my-city (master)
$ gc start
Registered city 'my-city' (/Users/csells/my-city)
Installed launchd service: /Users/csells/Library/LaunchAgents/com.gascity.supervisor.plist
[8/8] Waiting for supervisor to start city
City started under supervisor.
```

## What's next

You've created a city, slung work to agents, added a project as a rig, and
slung work to that rig. From here:

- **[Agents](/tutorials/02-agents)** — go deeper on agent configuration: prompts,
  sessions, scope, working directories
- **[Sessions](/tutorials/03-sessions)** — interactive conversations with agents,
  session lifecycle, inter-agent communication
- **[Formulas](/tutorials/04-formulas)** — multi-step workflow templates with
  dependencies and variables
