---
title: Tutorial 01 - Cities and Rigs
description: Create a city, sling work to an agent, add a rig, and configure multiple agents.
---

# Tutorial 01: Cities and Rigs

Hello and welcome to the first tutorial for [Gas City](https://github.com/gastownhall/GasCity)! The tutorials are designed to get you started using Gas City from the ground up.

## Setup

First, you'll need to install at least one CLI coding agent (which Gas City calls "providers) and make sure that they're on the PATH. Gas City supports many providers, including but not limited to Claude Code (`claude`), Codex CLI (`codex`) and Gemini CLI (`gemini`). Also, make sure you've configured each of your chosen providers (the more the merrier!) with the appropriate token and/or API key so that they can each run and do things for you.

Next, you'll need to get the Gas City CLI installed and on your PATH:

```shell
~
$ brew install gastownhall/gascity/gascity
...
==> Summary
🍺  /opt/homebrew/Cellar/gascity/0.13.3: 6 files, 53.1MB, built in 2 seconds
```

Now we're ready to create our first city.

## Creating a city

A city is a directory that holds your agent configuration, prompts, and workflows. You create a new city with `gc init`:

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

You can avoid the prompts and just specify what provider you want. Here's the same command, just providing the provider explicitly.

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

The main file is `city.toml` — it defines your city, using the contents of those directories as well as containing some definitions and local config. Assuming you chose the default `tutorial` config template and default provider, `city.toml` looks like this:

```toml
[workspace]
name = "my-city"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
```

The `[workspace]` section names your city and sets the default provider.

Each `[[agent]]` table you configure lets you create a named set of config including things like the provider, the model, the prompt you want to use to define its role, etc. An agent is named so that you can assign it work (aka "sling"). Here we've created an agent called the `mayor` with a prompt template. The `prompt_template` setting points to a markdown file that tells the agent what it is and how it should behave. Different agents can have different prompts, different providers, and different configuration — a reviewer agent and a coding agent might use the same provider but have very different instructions.

Gas City also gives you an implicit agent for each supported provider — so `claude`, `codex`, and `gemini` are available as agent names even though they're not listed in `city.toml`. These use the provider's defaults with no custom prompt.

## Slinging your first work

You assign work to agents by "slinging" it — think of it as tossing a task to someone who knows what to do. The `gc sling` command takes an agent name and a prompt:

```shell
~/my-city
$ gc sling claude "Write hello world in python to the file hello.py"
Created my-1 — "Write hello world in python to the file hello.py"
Slung my-1 → claude
```

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

Your city starts with one explicitly configured agent (`mayor`) and implicit agents for each supported provider (`claude`, `codex`, `gemini`, etc.). The implicit agents are convenient for quick work, but as you use Gas City more, you'll want to define agents with specific roles and prompts.

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

> ***donna to chris:** You brought up the distuptive work thing.  I dug around a bit more on city vs. rig suspension. For this flow, it should have been ities anyway (likely a surgical error on my part). I did log an issue on whether supending a rig should cause all work to cease on the rig's directory. It's unclear based on the bits.*

Sometimes you need agents to stop what they're doing — you're reorganizing a directory tree, making a large manual commit, or taking a snapshot you don't want agents to interfere with. Suspend the city:

```shell
~/my-city
$ gc suspend
City suspended.
```

This pauses all agent activity while keeping the city registered and its resources intact. Resume when you're ready:

```shell
~/my-city
$ gc resume
City resumed.
```

If you only want to pause work in one rig while the rest of the city keeps running, you can suspend and resume individual rigs:

```shell
~/my-city
$ gc rig suspend my-project
Suspended rig 'my-project'

~/my-city
$ gc rig resume my-project
Resumed rig 'my-project'
```
> **Issue:** City-level maintenance orders still run against suspended rigs — [details](../../../work/tutorials/city/issues.md#orders-ignore-rig-suspension)

To stop the city entirely and release resources:

```shell
~/my-city
$ gc stop
City stopped.

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
