---
title: Tutorial 01 - Cities and Rigs
description: Create a city, sling work to an agent, add a project, and configure multiple agents.
---

# Tutorial 01: Cities and Rigs

<!-- chris: "good fodder for a tutorial, but doesn't take the time to explain concepts up front... dives deeply into concepts that belong in other parts of the docs" → addressed: rewritten as a walkthrough in the style of 01-beads.md. Conceptual material (taxonomy, composition pipeline, pack internals) moved to cities-draft.md for future reference docs. -->

Welcome to the Gas City tutorials. These guides walk you through Gas City from the ground up — creating a workspace, dispatching work to AI coding agents, and scaling up to multi-agent orchestration.

In this tutorial, you'll create a city, sling work to an agent, add a project directory, and configure multiple agents with different providers.

## Installing Gas City

<!-- chris: "skips over the entire onboarding wizard" → addressed: full wizard shown below -->

First, you'll need at least one CLI coding agent installed and on your PATH. Gas City works with Claude Code (`claude`), Codex CLI (`codex`), Gemini CLI (`gemini`), and others. Make sure you've configured your agent with the appropriate API key so it can run and do work for you.

Next, install the Gas City CLI:

```shell
~
$ brew install gastownhall/gascity/gascity
...
==> Summary
🍺  /opt/homebrew/Cellar/gascity/0.13.3: 6 files, 53.1MB, built in 2 seconds
```

Now we're ready to create our first city.

## Creating a city

<!-- chris: "what's a workspace?" → addressed: avoided the word entirely. chris: "supervisor is undefined" → addressed: not explained in the tutorial, just implied by the output. Details belong in reference docs. -->

A city is a directory that holds your agent configuration, prompts, and workflows. It's where agents live and work gets done. You create a new city with `gc init`:

```shell
~
$ gc init ~/my-city
Welcome to Gas City!

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
  6. Custom command
Agent [1]:
[1/8] Creating runtime scaffold
[2/8] Installing hooks (Claude Code)
[3/8] Writing default prompts
[4/8] Writing default formulas
[5/8] Writing city configuration
Created tutorial config in "my-city".
[6/8] Checking provider readiness
[7/8] Registering city with supervisor
Registered city 'my-city' (/Users/you/my-city)
[8/8] Waiting for supervisor to start city
```

You can avoid the prompts and just specify what provider you want. Here's the same call, just providing the provider specifically.

```shell
~
$ gc init ~/my-city --provider claude
```

Gas City created the city directory, registered it, and started it. Let's look at what's inside:

```shell
~
$ cd ~/my-city

~/my-city
$ ls
city.toml  formulas  hooks  orders  packs  prompts
```

<!-- chris: "run-on sentence" → addressed: rewritten -->
<!-- chris: "at this point, we're off the deep end of what should be in a tutorial into what belongs in conceptual docs" → addressed: taxonomy table, composition pipeline, pack internals all moved out to cities-draft.md. Only essential config shown here. -->
<!-- chris: "I don't agree that we should have a city.local.toml" → addressed: removed from tutorial entirely. -->
The main file is `city.toml` — it defines your city, using the contents of those directories as well as containing some definitions and local config. With the tutorial template, `city.toml` looks like this:

```toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
```

<!-- chris: "use 'rigs' for consistency, even when introducing new terms" → addressed: terminology introduced inline below. Rigs covered in their own section later. -->

The `[workspace]` section names your city and sets a default provider ("provider" is Gas City's term for a model backend — Claude, Codex, Gemini, etc.).

Agents are how you give an AI model a specific role. The `[[agent]]` entry defines one called `mayor` — the name you'll use to sling work to it. The `prompt_template` points to a markdown file that tells the agent what it is and how it should behave. Different agents can have different prompts, different providers, and different configuration — a reviewer agent and a coding agent might use the same model but have very different instructions.

Gas City also gives you an implicit agent for each supported provider — so `claude`, `codex`, and `gemini` are available as agent names even though they're not listed in `city.toml`. These use the provider's defaults with no custom prompt.

## Slinging your first work

<!-- chris: "what's an agent? what does it mean to dispatch work? what's up with 'sling'?" → addressed: agents explained in city.toml section, sling metaphor explained inline below -->
<!-- chris: "you should be able to sling work to the default provider by default" → noted: I think we now explain what an agent is and how there's an implicit one for every provider. currently you must name the agent. This is a product question, not a tutorial issue. my first usage wanted the agent name to be optional (e.g., `gc sling "Do stuff"` but it does create a little magic. It would make this tutorial much short though. In short, I'm torn.  FWIW, the last I checked the default provier shows up as an agent with `gc status`) -->

You assign work to agents by "slinging" it — think of it as tossing a task to someone who knows what to do. The `gc sling` command takes an agent name and a prompt:

```shell
~/my-city
$ gc sling claude "Write hello world in python to the file hello.py"
Created my-1 — "Write hello world in python to the file hello.py"
Slung my-1 → claude
```

<!-- chris: "misses the opportunity to tell them how to monitor an agent in progress" → addressed: bd show --watch shown below -->

The `gc sling` command created a work item in our city (called a "bead") and dispatched it to the `claude` agent. You can watch it progress:

```shell
~/my-city
$ bd show my-1 --watch
✓ my-1 · Write hello world in python to the file hello.py   [● P2 · CLOSED]
Owner: you · Type: task

NOTES
Done: wrote hello world in Python (hello.py)

Watching for changes... (Press Ctrl+C to exit)
```

> **Issue:** gc sling on a new city fails to dispatch — [details](issues.md#sling-after-init) · [#286](https://github.com/gastownhall/gascity/issues/286), [#287](https://github.com/gastownhall/gascity/issues/287)

Once the bead closes, you will see the results:

```shell
~/my-city
$ cat hello.py
print("Hello, World!")

~/my-city
$ python hello.py
Hello, World!
```

Success! You just dispatched work to an AI agent and got code back.

## Adding a rig

So far, the agent worked in the city directory itself. But your real projects live somewhere else — in their own directories, probably as git repos. In Gas City, a project directory registered with a city is called a "rig." Rigging a project's directory lets agents work in it.

```shell
~/my-city
$ gc rig add ~/my-project
Added rig 'my-project' to city 'my-city'
  Prefix: mp
  Beads:  initialized
  Hooks:  installed (claude)
```

Gas City derived the rig name from the directory basename (`my-project`) and set up work tracking in it. You can see the new entry in `city.toml`:

```toml
[[rigs]]
name = "my-project"
path = "/Users/you/my-project"
```

Now sling work from within the rig directory. Gas City figures out which rig and city you're in based on your current directory:

```shell
~/my-city
$ cd ~/my-project

~/my-project
$ gc sling claude "Add a README.md with a project description"
Created mp-1 — "Add a README.md with a project description"
Slung mp-1 → my-project/claude
```

Notice the target is `my-project/claude` — the agent is scoped to this rig. Check the result:

```shell
~/my-project
$ ls
README.md
```

You can see all of your city's rigs with `gc rig list`:

```shell
~/my-project
$ gc rig list
NAME          PATH                    PREFIX  SUSPENDED
my-project    /Users/you/my-project   mp      no
```

## Multiple agents and providers

<!-- chris: "'claude' is called a 'provider' when creating a city but an 'agent' when slinging work — what's up with that?" → addressed: explained that implicit provider agents are created automatically, and that explicit agents let you customize roles/prompts -->

Your city starts with one explicitly configured agent (`mayor`) and implicit agents for each supported provider (`claude`, `codex`, `gemini`). The implicit agents are convenient for quick work, but as you use Gas City more, you'll want to define agents with specific roles and prompts.

Open `city.toml` and add a second agent. This one uses Codex instead of Claude:

```toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
provider = "codex"
```

You'll need to create a prompt for the new agent:

```shell
~/my-city
$ cat > prompts/reviewer.md << 'EOF'
# Code Reviewer

You review code changes. When given a file or PR, read the code
and provide feedback on bugs, security issues, and style.
EOF
```

Restart the city to pick up the new agent:

```shell
~/my-city
$ gc restart
```

Now you can sling work to either agent — same command, different provider handling it behind the scenes:

```shell
~/my-project
$ gc sling mayor "Plan the next feature for my-project"
Slung my-2 → mayor

~/my-project
$ gc sling reviewer "Review hello.py for issues"
Slung my-3 → my-project/reviewer
```

One request went to Claude (the mayor's default provider), the other to Codex (the reviewer's). You don't have to think about which CLI to invoke or how each provider wants its arguments. Gas City handles the differences.

## Managing your city

A few commands you'll use regularly:

To check which agents are running, you use `gc status`:

```shell
~/my-city
$ gc status
my-city  /Users/you/my-city
  Controller: running (PID 12345)

Agents:
  mayor                      running
  my-project/claude          running
  my-project/reviewer        running

Sessions: 3 active, 0 suspended
```

See all the cities you have registered on your machine, use `gc cities`:

```shell
~/my-city
$ gc cities
NAME       PATH
my-city    /Users/you/my-city
```

Pause a rig when you're doing disruptive work and don't want agents interfering:

```shell
~/my-city
$ gc rig suspend my-project
Suspended rig 'my-project'
```

When you're ready, bring it back:

```shell
~/my-city
$ gc rig resume my-project
Resumed rig 'my-project'
```

Stop the city entirely, which both quiesces activity and releases most of the resources consumed by that city:

```shell
~/my-city
$ gc stop
City stopped.
```

Start it back up:

```shell
~/my-city
$ gc start
City started.
```

## What's next

You've created a city, slung work to agents, added a project as a rig, and configured multiple agents with different providers. From here:

- **[Agents](agents.md)** — go deeper on agent configuration: prompts, sessions, scope, working directories
- **[Sessions](sessions.md)** — interactive conversations with agents, session lifecycle, inter-agent communication
- **[Formulas](formulas.md)** — multi-step workflow templates with dependencies and variables
- **Packs** — reusable agent configurations that you can share across cities (coming soon)

<!--
BONEYARD — material moved out of the tutorial. Belongs in reference docs or a packs tutorial.

See cities-draft.md for the full previous version, which includes:
- Three-category file taxonomy (definitions / local bindings / managed state)
- Composition pipeline (7-step list)
- Gastown and Maintenance pack descriptions
- Pack includes, where packs live, remote git includes
- Supervisor and controller architecture
- Health checks (gc doctor)
- Full command reference table
-->
